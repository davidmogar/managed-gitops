/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package appstudioredhatcom

import (
	"context"
	"fmt"
	"github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	"github.com/kcp-dev/logicalcluster/v2"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"reflect"
	"time"

	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"

	appstudioshared "github.com/redhat-appstudio/managed-gitops/appstudio-shared/apis/appstudio.redhat.com/v1alpha1"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	apierr "k8s.io/apimachinery/pkg/api/errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// EnvironmentReconciler reconciles a Environment object
type EnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=environments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=environments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=environments/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = sharedutil.AddKCPClusterToContext(ctx, req.ClusterName)
	log := log.FromContext(ctx).WithValues("request", req)

	// The goal of this function is to ensure that if an Environment exists, and that Environment
	// has the 'kubernetesCredentials' field defined, that a corresponding
	// GitOpsDeploymentManagedEnvironment exists (and is up-to-date).
	environment := &appstudioshared.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(environment), environment); err != nil {
		if apierr.IsNotFound(err) {
			log.Info("Environment resource no longer exists")
			// A) The Environment resource could not be found: the owner reference on the GitOpsDeploymentManagedEnvironment
			// should ensure that it is cleaned up, so no more work is required.
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("unable to retrieve Environment: %v", err)
	}

	// Create sub-workspace if specified in the environment and make sure it's ready before continuing.
	if environment.Spec.UnstableConfigurationFields != nil && (environment.Spec.UnstableConfigurationFields.SubWorkspace != appstudioshared.SubWorkspace{}) {
		workspace, err := r.createWorkspace(ctx, environment)
		if err != nil {
			return ctrl.Result{}, err
		}

		klog.Infof("%+v", workspace)
		// Wait for the workspace to be ready
		if workspace.Status.Phase != v1alpha1.ClusterWorkspacePhaseReady {
			return ctrl.Result{RequeueAfter: time.Second * 5}, fmt.Errorf("workspace not ready yet. Requeueing after 5 seconds")
		}
	}

	return r.createOrUpdateManagedEnvironment(ctx, environment)
}

// createOrUpdateManagedEnvironment creates or update a GitOpsDeploymentManagedEnvironment using information from the
// given Environment.
func (r *EnvironmentReconciler) createOrUpdateManagedEnvironment(
	ctx context.Context, env *appstudioshared.Environment,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	managedEnvironment, err := r.generateManagedEnvironment(ctx, env)
	if err != nil {
		return ctrl.Result{}, err
	}

	if managedEnvironment == nil {
		return ctrl.Result{}, nil
	}

	existingEnvironment, err := r.getManagedEnvironmentFromEnvironment(ctx, env)
	if err != nil {
		return ctrl.Result{}, err
	}

	if existingEnvironment == nil {
		log.Info("Creating GitOpsDeploymentManagedEnvironment", "managedEnv", managedEnvironment.Name)

		if err := r.Client.Create(ctx, managedEnvironment); err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to create new GitOpsDeploymentManagedEnvironment: %v", err)
		}
		sharedutil.LogAPIResourceChangeEvent(managedEnvironment.Namespace, managedEnvironment.Name, managedEnvironment, sharedutil.ResourceCreated, log)
	} else {
		if reflect.DeepEqual(existingEnvironment.Spec, managedEnvironment.Spec) {
			// If the spec field is the same, no more work is needed.
			return ctrl.Result{}, nil
		}

		log.Info("Updating GitOpsDeploymentManagedEnvironment as a change was detected", "managedEnv", managedEnvironment.Name)

		// Update the current object to the desired state
		existingEnvironment.Spec = managedEnvironment.Spec

		if err := r.Client.Update(ctx, existingEnvironment); err != nil {
			return ctrl.Result{},
				fmt.Errorf("unable to update existing GitOpsDeploymentManagedEnvironment '%s': %v", existingEnvironment.Name, err)
		}
		sharedutil.LogAPIResourceChangeEvent(existingEnvironment.Namespace, existingEnvironment.Name, existingEnvironment, sharedutil.ResourceModified, log)
	}

	return ctrl.Result{}, nil
}

// createWorkspace creates a new Workspace in the cluster if it does not exist already.
func (r *EnvironmentReconciler) createWorkspace(ctx context.Context, env *appstudioshared.Environment) (*v1alpha1.ClusterWorkspace, error) {
	workspace, err := r.getSubWorkspaceFromEnvironment(ctx, env)
	if err != nil && !apierr.IsNotFound(err) {
		return nil, err
	}

	// Create the workspace if it doesn't exist already
	if err != nil {
		currentWorkspace, ok := logicalcluster.ClusterFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("subworkspaces should be defined exclusively on KCP deployments")
		}
		path := fmt.Sprintf("%s:%s", currentWorkspace, env.Spec.UnstableConfigurationFields.SubWorkspace.Name)
		workspace = r.generateWorkspace(env.Spec.UnstableConfigurationFields.SubWorkspace.Name, env.Namespace, "gitops-environment", path)
		err = r.Client.Create(ctx, workspace)
		if err != nil {
			return nil, err
		}
	}

	return workspace, nil
}

// generateEmptyManagedEnvironment returns a GitOpsDeploymentManagedEnvironment with its name and namespace set based
// on the environment information.
func generateEmptyManagedEnvironment(environmentName string, environmentNamespace string) *managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment {
	return &managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "managed-environment-" + environmentName,
			Namespace: environmentNamespace,
		},
	}
}

// generateManagedEnvironment returns a new GitOpsDeploymentManagedEnvironment based on the Environment's
// UnstableConfigurationFields information.
func (r *EnvironmentReconciler) generateManagedEnvironment(
	ctx context.Context, env *appstudioshared.Environment,
) (*managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, error) {
	if env.Spec.UnstableConfigurationFields == nil {
		return nil, nil
	}

	if (env.Spec.UnstableConfigurationFields.KubernetesClusterCredentials != appstudioshared.KubernetesClusterCredentials{}) {
		return r.generateManagedEnvironmentWithCredentials(ctx, env)
	} else if (env.Spec.UnstableConfigurationFields.SubWorkspace != appstudioshared.SubWorkspace{}) {
		return r.generateManagedEnvironmentWithWorkspace(env)
	}

	return nil, nil
}

// generateManagedEnvironmentWithCredentials returns a new GitOpsDeploymentManagedEnvironment with credentials set in the
// Environment's UnstableConfigurationFields.ClusterCredentialsSecret field.
func (r *EnvironmentReconciler) generateManagedEnvironmentWithCredentials(
	ctx context.Context, env *appstudioshared.Environment,
) (*managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, error) {
	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      env.Spec.UnstableConfigurationFields.ClusterCredentialsSecret,
		Namespace: env.Namespace,
	}, secret)

	if err != nil {
		if apierr.IsNotFound(err) {
			return nil, fmt.Errorf("the secret '%s' referenced by the Environment resource was not found: %v", secret.Name, err)
		}
		return nil, err
	}

	// 2) Generate (but don't apply) the corresponding GitOpsDeploymentManagedEnvironment resource
	managedEnv := generateEmptyManagedEnvironment(env.Name, env.Namespace)
	managedEnv.Spec = managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec{
		APIURL:                   env.Spec.UnstableConfigurationFields.KubernetesClusterCredentials.APIURL,
		ClusterCredentialsSecret: secret.Name,
	}

	err = ctrl.SetControllerReference(env, managedEnv, r.Client.Scheme())
	if err != nil {
		return nil, err
	}

	return managedEnv, nil
}

// generateManagedEnvironmentWithCredentials returns a new GitOpsDeploymentManagedEnvironment with a sub-workspace referenced
// in Environment's UnstableConfigurationFields.SubWorkspace field.
func (r *EnvironmentReconciler) generateManagedEnvironmentWithWorkspace(
	env *appstudioshared.Environment,
) (*managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, error) {
	managedEnv := generateEmptyManagedEnvironment(env.Name, env.Namespace)
	managedEnv.Spec = managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec{
		SubWorkspace: env.Spec.UnstableConfigurationFields.SubWorkspace.Name,
	}

	err := ctrl.SetControllerReference(env, managedEnv, r.Client.Scheme())
	if err != nil {
		return nil, err
	}

	return managedEnv, nil
}

// generateEmptyManagedEnvironment returns a ClusterWorkspace with information provided.
func (r *EnvironmentReconciler) generateWorkspace(name, namespace, clusterType, path string) *v1alpha1.ClusterWorkspace {
	return &v1alpha1.ClusterWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha1.ClusterWorkspaceSpec{
			Type: v1alpha1.ClusterWorkspaceTypeReference{
				Name: v1alpha1.ClusterWorkspaceTypeName(clusterType),
				Path: path,
			},
		},
	}
}

// getManagedEnvironmentFromEnvironment returns a GitOpsDeploymentManagedEnvironment referenced by the given Environment.
// If not found, a nil value will be returned.
func (r *EnvironmentReconciler) getManagedEnvironmentFromEnvironment(ctx context.Context, env *appstudioshared.Environment) (*managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, error) {
	managedEnvironment := generateEmptyManagedEnvironment(env.Name, env.Namespace)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(managedEnvironment), managedEnvironment)
	if err != nil {
		if apierr.IsNotFound(err) {
			return nil, nil
		}

		return nil, err
	}

	return managedEnvironment, nil
}

// getSubWorkspaceFromEnvironment returns a ClusterWorkspace referenced by the given Environment.
// If not found, a nil value will be returned.
func (r *EnvironmentReconciler) getSubWorkspaceFromEnvironment(ctx context.Context, env *appstudioshared.Environment) (*v1alpha1.ClusterWorkspace, error) {
	if env.Spec.UnstableConfigurationFields == nil || (env.Spec.UnstableConfigurationFields.SubWorkspace == appstudioshared.SubWorkspace{}) {
		return nil, nil
	}

	workspace := &v1alpha1.ClusterWorkspace{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      env.Spec.UnstableConfigurationFields.SubWorkspace.Name,
		Namespace: env.Namespace,
	}, workspace)

	if err != nil {
		return nil, err
	}

	return workspace, nil
}

const (
	SnapshotEnvironmentBindingConditionErrorOccurred = "ErrorOccurred"
	SnapshotEnvironmentBindingReasonErrorOccurred    = "ErrorOccurred"
)

// Update Status.Condition field of snapshotEnvironmentBinding
func updateStatusConditionOfEnvironmentBinding(ctx context.Context, client client.Client, message string,
	binding *appstudioshared.SnapshotEnvironmentBinding, conditionType string,
	status metav1.ConditionStatus, reason string) error {
	// Check if condition with same type is already set, if Yes then check if content is same,
	// If content is not same update LastTransitionTime
	index := -1
	for i, Condition := range binding.Status.BindingConditions {
		if Condition.Type == conditionType {
			index = i
			break
		}
	}

	now := metav1.Now()

	if index == -1 {
		binding.Status.BindingConditions = append(binding.Status.BindingConditions,
			metav1.Condition{
				Type:               conditionType,
				Message:            message,
				LastTransitionTime: now,
				Status:             status,
				Reason:             reason,
			})
	} else {
		if binding.Status.BindingConditions[index].Message != message &&
			binding.Status.BindingConditions[index].Reason != reason &&
			binding.Status.BindingConditions[index].Status != status {
			binding.Status.BindingConditions[index].LastTransitionTime = now
		}
		binding.Status.BindingConditions[index].Reason = reason
		binding.Status.BindingConditions[index].Message = message
		binding.Status.BindingConditions[index].LastTransitionTime = now
		binding.Status.BindingConditions[index].Status = status

	}

	if err := client.Status().Update(ctx, binding); err != nil {
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appstudioshared.Environment{}).
		Complete(r)
}
