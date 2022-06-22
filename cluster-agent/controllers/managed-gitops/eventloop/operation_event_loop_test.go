package eventloop

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	operation "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
)

var _ = Describe("Operation Controller", func() {
	const (
		name      = "operation"
		namespace = "argocd"
		dbID      = "databaseID"
	)
	Context("Operation Controller Test", func() {

		var ctx context.Context
		var dbQueries db.AllDatabaseQueries
		var k8sClient client.WithWatch
		var task processOperationEventTask
		var logger logr.Logger
		var kubesystemNamespace *v1.Namespace
		var argocdNamespace *v1.Namespace
		var workspace *v1.Namespace
		var scheme *runtime.Scheme
		var testClusterUser *db.ClusterUser
		var err error

		BeforeEach(func() {
			ctx = context.Background()
			logger = log.FromContext(ctx)

			testClusterUser = &db.ClusterUser{
				Clusteruser_id: "test-user",
				User_name:      "test-user",
			}

			dbQueries, err = db.NewUnsafePostgresDBQueries(true, true)
			Expect(err).To(BeNil())

			scheme, argocdNamespace, kubesystemNamespace, workspace, err = eventlooptypes.GenericTestSetup()
			Expect(err).To(BeNil())

			err = appv1.AddToScheme(scheme)
			Expect(err).To(BeNil())

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
			}

			By("Initialize fake kube client")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitopsDepl, workspace, argocdNamespace, kubesystemNamespace).Build()

			task = processOperationEventTask{
				log: logger,
				event: operationEventLoopEvent{
					request: newRequest(namespace, name),
					client:  k8sClient,
				},
			}

		})
		It("Ensure that calling perform task on an operation CR that doesn't exist, it doesn't return an error, and retry is false", func() {
			defer dbQueries.CloseDatabase()
			defer testTeardown()

			err = db.SetupForTestingDBGinkgo()
			Expect(err).To(BeNil())

			_, _, _, gitopsEngineInstance, _, err := db.CreateSampleData(dbQueries)
			Expect(err).To(BeNil())

			By("creating Operation row in database")
			operationDB := &db.Operation{
				Operation_id:            "test-operation",
				Instance_id:             gitopsEngineInstance.Gitopsengineinstance_id,
				Resource_id:             "test-fake-resource-id",
				Resource_type:           db.OperationResourceType_Application,
				State:                   db.OperationState_Waiting,
				Operation_owner_user_id: testClusterUser.Clusteruser_id,
			}

			err = dbQueries.CreateOperation(ctx, operationDB, operationDB.Operation_owner_user_id)
			Expect(err).To(BeNil())

			retry, err := task.PerformTask(ctx)
			Expect(err).To(BeNil())
			Expect(retry).To(BeFalse())

		})

		It("ensures that if the operation row doesn't exist, an error is not returned, and retry is false", func() {
			By("Close database connection")
			defer dbQueries.CloseDatabase()
			defer testTeardown()

			err = db.SetupForTestingDBGinkgo()
			Expect(err).To(BeNil())

			_, _, _, gitopsEngineInstance, _, err := db.CreateSampleData(dbQueries)
			Expect(err).To(BeNil())

			operationDB := &db.Operation{
				Operation_id:            "test-operation",
				Instance_id:             gitopsEngineInstance.Gitopsengineinstance_id,
				Resource_id:             "test-fake-resource-id",
				Resource_type:           db.OperationResourceType_Application,
				State:                   db.OperationState_Waiting,
				Operation_owner_user_id: testClusterUser.Clusteruser_id,
			}

			err = dbQueries.CreateOperation(ctx, operationDB, operationDB.Operation_owner_user_id)
			Expect(err).To(BeNil())

			By("Operation row(test-wrong-operation) doesn't exists")
			operationCR := &operation.Operation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: operation.OperationSpec{
					OperationID: "test-wrong-operation",
				},
			}

			err = task.event.client.Create(ctx, operationCR)
			Expect(err).To(BeNil())

			retry, err := task.PerformTask(ctx)
			Expect(err).To(BeNil())
			Expect(retry).To(BeFalse())

		})

		It("ensures that if the kube-system namespace does not having a matching namespace uid, an error is not returned, but retry it true", func() {
			By("Close database connection")
			defer dbQueries.CloseDatabase()
			defer testTeardown()

			err = db.SetupForTestingDBGinkgo()
			Expect(err).To(BeNil())

			By("'kube-system' namespace has a UID that is not found in a corresponding row in GitOpsEngineCluster database")
			_, _, _, gitopsEngineInstance, _, err := db.CreateSampleData(dbQueries)
			Expect(err).To(BeNil())
			Expect(kubesystemNamespace.UID).ToNot(Equal(gitopsEngineInstance.Namespace_uid))

			By("creating Operation row in database")
			operationDB := &db.Operation{
				Operation_id:            "test-operation",
				Instance_id:             gitopsEngineInstance.Gitopsengineinstance_id,
				Resource_id:             "test-fake-resource-id",
				Resource_type:           db.OperationResourceType_Application,
				State:                   db.OperationState_Waiting,
				Operation_owner_user_id: testClusterUser.Clusteruser_id,
			}

			err = dbQueries.CreateOperation(ctx, operationDB, operationDB.Operation_owner_user_id)
			Expect(err).To(BeNil())

			By("Operation CR exists")
			operationCR := &operation.Operation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: operation.OperationSpec{
					OperationID: operationDB.Operation_id,
				},
			}

			err = task.event.client.Create(ctx, operationCR)
			Expect(err).To(BeNil())

			retry, err := task.PerformTask(ctx)
			Expect(err).To(BeNil())
			Expect(retry).To(BeTrue())

		})

		It("Ensures that if the GitopsEngineInstance's namespace_name field doesn't exist, an error is not returned, and retry is true", func() {
			By("Close database connection")
			defer dbQueries.CloseDatabase()

			clusterCredentials := db.ClusterCredentials{
				Clustercredentials_cred_id: string(uuid.NewUUID()),
			}

			err = dbQueries.CreateClusterCredentials(ctx, &clusterCredentials)
			Expect(err).To(BeNil())

			gitopsEngineCluster, _, err := dbutil.GetOrCreateGitopsEngineClusterByKubeSystemNamespaceUID(ctx, string(kubesystemNamespace.UID), dbQueries, logger)
			Expect(gitopsEngineCluster).ToNot(BeNil())
			Expect(err).To(BeNil())

			By("creating a gitops engine instance with a namespace name/uid that don't exist in fakeclient")
			gitopsEngineInstance := &db.GitopsEngineInstance{
				Gitopsengineinstance_id: "test-fake-engine-instance",
				Namespace_name:          "doesn't-exist",
				Namespace_uid:           string("doesnt-exist-uid"),
				EngineCluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
			}
			err = dbQueries.CreateGitopsEngineInstance(ctx, gitopsEngineInstance)
			Expect(err).To(BeNil())

			By("creating Operation row in database")
			operationDB := &db.Operation{
				Operation_id:            "test-operation",
				Instance_id:             gitopsEngineInstance.Gitopsengineinstance_id,
				Resource_id:             "test-fake-resource-id",
				Resource_type:           db.OperationResourceType_Application,
				State:                   db.OperationState_Waiting,
				Operation_owner_user_id: testClusterUser.Clusteruser_id,
			}

			err = dbQueries.CreateOperation(ctx, operationDB, operationDB.Operation_owner_user_id)
			Expect(err).To(BeNil())

			By("creating Operation CR")
			operationCR := &operation.Operation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: operation.OperationSpec{
					OperationID: operationDB.Operation_id,
				},
			}

			err = task.event.client.Create(ctx, operationCR)
			Expect(err).To(BeNil())

			retry, err := task.PerformTask(ctx)
			Expect(err).To(BeNil())
			Expect(retry).To(BeTrue())

			kubernetesToDBResourceMapping := db.KubernetesToDBResourceMapping{
				KubernetesResourceType: "Namespace",
				KubernetesResourceUID:  string(kubesystemNamespace.UID),
				DBRelationType:         "GitopsEngineCluster",
				DBRelationKey:          gitopsEngineCluster.Gitopsenginecluster_id,
			}

			By("deleting resources and cleaning up db entries created by test.")
			resourcesToBeDeleted := testResources{
				Operation_id:                  operationDB.Operation_id,
				Gitopsenginecluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
				Gitopsengineinstance_id:       gitopsEngineInstance.Gitopsengineinstance_id,
				ClusterCredentials_id:         gitopsEngineCluster.Clustercredentials_id,
				kubernetesToDBResourceMapping: kubernetesToDBResourceMapping,
			}

			deleteTestResources(ctx, dbQueries, resourcesToBeDeleted)

		})

		Context("Process Application Operation Test", func() {
			It("Verify that When an Operation row points to an Application row that doesn't exist, any Argo Application CR that relates to that Application row should be removed.", func() {
				By("Close database connection")
				defer dbQueries.CloseDatabase()

				applicationCR := &appv1.Application{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "my-user",
						Labels: map[string]string{
							dbID: "doesnt-exist",
						},
						DeletionTimestamp: &metav1.Time{
							Time: time.Now(),
						},
					},
				}

				err = task.event.client.Create(ctx, applicationCR)
				Expect(err).To(BeNil())

				go func() {
					err = wait.Poll(1*time.Second, 1*time.Minute, func() (bool, error) {
						if applicationCR.DeletionTimestamp != nil {
							err = k8sClient.Get(ctx, client.ObjectKeyFromObject(applicationCR), applicationCR)
							Expect(err).To(BeNil())

							applicationCR.Finalizers = nil

							err = k8sClient.Update(ctx, applicationCR)
							Expect(err).To(BeNil())

							err = task.event.client.Delete(ctx, applicationCR)
							return true, nil
						}
						return false, nil
					})
				}()

				clusterCredentials := db.ClusterCredentials{
					Clustercredentials_cred_id: string(uuid.NewUUID()),
				}

				err = dbQueries.CreateClusterCredentials(ctx, &clusterCredentials)
				Expect(err).To(BeNil())

				gitopsEngineCluster, _, err := dbutil.GetOrCreateGitopsEngineClusterByKubeSystemNamespaceUID(ctx, string(kubesystemNamespace.UID), dbQueries, logger)
				Expect(gitopsEngineCluster).ToNot(BeNil())
				Expect(err).To(BeNil())

				By("creating a gitops engine instance with a namespace name/uid that don't exist in fakeclient")
				gitopsEngineInstance := &db.GitopsEngineInstance{
					Gitopsengineinstance_id: "test-fake-engine-instance",
					Namespace_name:          workspace.Namespace,
					Namespace_uid:           string(workspace.UID),
					EngineCluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
				}

				err = dbQueries.CreateGitopsEngineInstance(ctx, gitopsEngineInstance)
				Expect(err).To(BeNil())

				managedEnvironment := db.ManagedEnvironment{
					Managedenvironment_id: "test-managed-env-914",
					Clustercredentials_id: clusterCredentials.Clustercredentials_cred_id,
					Name:                  "my env",
				}

				err = dbQueries.CreateManagedEnvironment(ctx, &managedEnvironment)
				Expect(err).To(BeNil())

				By("Creating Operation row in database")
				operationDB := &db.Operation{
					Operation_id:            "test-operation",
					Instance_id:             gitopsEngineInstance.Gitopsengineinstance_id,
					Resource_id:             "doesnt-exist",
					Resource_type:           "Application",
					State:                   db.OperationState_Waiting,
					Operation_owner_user_id: testClusterUser.Clusteruser_id,
				}

				err = dbQueries.CreateOperation(ctx, operationDB, operationDB.Operation_owner_user_id)
				Expect(err).To(BeNil())

				By("Creating Operation CR")
				operationCR := &operation.Operation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
					},
					Spec: operation.OperationSpec{
						OperationID: operationDB.Operation_id,
					},
				}

				err = task.event.client.Create(ctx, operationCR)
				Expect(err).To(BeNil())

				retry, err := task.PerformTask(ctx)
				Expect(err).To(BeNil())
				Expect(retry).To(BeFalse())

				kubernetesToDBResourceMapping := db.KubernetesToDBResourceMapping{
					KubernetesResourceType: "Namespace",
					KubernetesResourceUID:  string(kubesystemNamespace.UID),
					DBRelationType:         "GitopsEngineCluster",
					DBRelationKey:          gitopsEngineCluster.Gitopsenginecluster_id,
				}

				By("Verifying whether Application CR is deleted")
				err = task.event.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, applicationCR)
				Expect(err).ToNot(BeNil())

				By("deleting resources and cleaning up db entries created by test.")
				resourcesToBeDeleted := testResources{
					Operation_id:                  operationDB.Operation_id,
					Gitopsenginecluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
					Gitopsengineinstance_id:       gitopsEngineInstance.Gitopsengineinstance_id,
					ClusterCredentials_id:         gitopsEngineCluster.Clustercredentials_id,
					Managedenvironment_id:         managedEnvironment.Managedenvironment_id,
					kubernetesToDBResourceMapping: kubernetesToDBResourceMapping,
				}

				deleteTestResources(ctx, dbQueries, resourcesToBeDeleted)
			})

			It("Verify that when an Operation row points to an Application row that exists in the database, but doesn't exist in the Argo CD namespace, it should be created.", func() {
				By("Close database connection")
				defer dbQueries.CloseDatabase()

				dummyApplicationSpec, dummyApplicationSpecBytes, err := createDummyApplicationData()
				Expect(err).To(BeNil())

				clusterCredentials := db.ClusterCredentials{
					Clustercredentials_cred_id: string(uuid.NewUUID()),
				}

				err = dbQueries.CreateClusterCredentials(ctx, &clusterCredentials)
				Expect(err).To(BeNil())

				gitopsEngineCluster, _, err := dbutil.GetOrCreateGitopsEngineClusterByKubeSystemNamespaceUID(ctx, string(kubesystemNamespace.UID), dbQueries, logger)
				Expect(gitopsEngineCluster).ToNot(BeNil())
				Expect(err).To(BeNil())

				By("creating a gitops engine instance with a namespace name/uid that don't exist in fakeclient")
				gitopsEngineInstance := &db.GitopsEngineInstance{
					Gitopsengineinstance_id: "test-fake-engine-instance",
					Namespace_name:          workspace.Namespace,
					Namespace_uid:           string(workspace.UID),
					EngineCluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
				}
				err = dbQueries.CreateGitopsEngineInstance(ctx, gitopsEngineInstance)
				Expect(err).To(BeNil())

				managedEnvironment := db.ManagedEnvironment{
					Managedenvironment_id: "test-managed-env-914",
					Clustercredentials_id: clusterCredentials.Clustercredentials_cred_id,
					Name:                  "my env",
				}

				err = dbQueries.CreateManagedEnvironment(ctx, &managedEnvironment)
				Expect(err).To(BeNil())

				By("Create Application in Database")
				applicationDB := &db.Application{
					Application_id:          "test-my-application",
					Name:                    name,
					Spec_field:              dummyApplicationSpecBytes,
					Engine_instance_inst_id: gitopsEngineInstance.Gitopsengineinstance_id,
					Managed_environment_id:  managedEnvironment.Managedenvironment_id,
				}

				err = dbQueries.CreateApplication(ctx, applicationDB)
				Expect(err).To(BeNil())

				applicationCR := appv1.Application{
					ObjectMeta: metav1.ObjectMeta{
						Name:      applicationDB.Name,
						Namespace: "my-user",
					},
				}

				By("Creating Operation row in database")
				operationDB := &db.Operation{
					Operation_id:            "test-operation",
					Instance_id:             gitopsEngineInstance.Gitopsengineinstance_id,
					Resource_id:             applicationDB.Application_id,
					Resource_type:           "Application",
					State:                   db.OperationState_Waiting,
					Operation_owner_user_id: testClusterUser.Clusteruser_id,
				}

				err = dbQueries.CreateOperation(ctx, operationDB, operationDB.Operation_owner_user_id)
				Expect(err).To(BeNil())

				By("Creating Operation CR")
				operationCR := &operation.Operation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
					},
					Spec: operation.OperationSpec{
						OperationID: operationDB.Operation_id,
					},
				}

				err = task.event.client.Create(ctx, operationCR)
				Expect(err).To(BeNil())

				retry, err := task.PerformTask(ctx)
				Expect(err).To(BeNil())
				Expect(retry).To(BeFalse())

				By("Verifying whether Application CR is created")
				err = task.event.client.Get(ctx, types.NamespacedName{Namespace: applicationCR.Namespace, Name: name}, &applicationCR)
				Expect(err).To(BeNil())
				Expect(dummyApplicationSpec.Spec).To(Equal(applicationCR.Spec))

				kubernetesToDBResourceMapping := db.KubernetesToDBResourceMapping{
					KubernetesResourceType: "Namespace",
					KubernetesResourceUID:  string(kubesystemNamespace.UID),
					DBRelationType:         "GitopsEngineCluster",
					DBRelationKey:          gitopsEngineCluster.Gitopsenginecluster_id,
				}

				By("deleting resources and cleaning up db entries created by test.")
				resourcesToBeDeleted := testResources{
					Application_id:                applicationDB.Application_id,
					Operation_id:                  operationDB.Operation_id,
					Gitopsenginecluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
					Gitopsengineinstance_id:       gitopsEngineInstance.Gitopsengineinstance_id,
					ClusterCredentials_id:         gitopsEngineCluster.Clustercredentials_id,
					Managedenvironment_id:         managedEnvironment.Managedenvironment_id,
					kubernetesToDBResourceMapping: kubernetesToDBResourceMapping,
				}

				deleteTestResources(ctx, dbQueries, resourcesToBeDeleted)
			})

			It("Verify that Application CR should be updated to be consistent with the Application row", func() {
				By("Close database connection")
				defer dbQueries.CloseDatabase()

				_, dummyApplicationSpecBytes, err := createDummyApplicationData()
				Expect(err).To(BeNil())

				applicationCR := &appv1.Application{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "my-user",
						Labels: map[string]string{
							dbID: "test-my-application",
						},
						DeletionTimestamp: &metav1.Time{
							Time: time.Now(),
						},
					},
				}

				err = task.event.client.Create(ctx, applicationCR)
				Expect(err).To(BeNil())

				clusterCredentials := db.ClusterCredentials{
					Clustercredentials_cred_id: string(uuid.NewUUID()),
				}

				err = dbQueries.CreateClusterCredentials(ctx, &clusterCredentials)
				Expect(err).To(BeNil())

				gitopsEngineCluster, _, err := dbutil.GetOrCreateGitopsEngineClusterByKubeSystemNamespaceUID(ctx, string(kubesystemNamespace.UID), dbQueries, logger)
				Expect(gitopsEngineCluster).ToNot(BeNil())
				Expect(err).To(BeNil())

				By("creating a gitops engine instance with a namespace name/uid that don't exist in fakeclient")
				gitopsEngineInstance := &db.GitopsEngineInstance{
					Gitopsengineinstance_id: "test-fake-engine-instance",
					Namespace_name:          workspace.Namespace,
					Namespace_uid:           string(workspace.UID),
					EngineCluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
				}
				err = dbQueries.CreateGitopsEngineInstance(ctx, gitopsEngineInstance)
				Expect(err).To(BeNil())

				managedEnvironment := db.ManagedEnvironment{
					Managedenvironment_id: "test-managed-env-914",
					Clustercredentials_id: clusterCredentials.Clustercredentials_cred_id,
					Name:                  "my env",
				}

				err = dbQueries.CreateManagedEnvironment(ctx, &managedEnvironment)
				Expect(err).To(BeNil())

				databaseID := applicationCR.Labels[dbID]
				applicationDB := &db.Application{
					Application_id:          databaseID,
					Name:                    name,
					Spec_field:              dummyApplicationSpecBytes,
					Engine_instance_inst_id: gitopsEngineInstance.Gitopsengineinstance_id,
					Managed_environment_id:  managedEnvironment.Managedenvironment_id,
				}

				By("Create Application in Database")
				err = dbQueries.CreateApplication(ctx, applicationDB)
				Expect(err).To(BeNil())

				By("Creating Operation row in database")
				operationDB := &db.Operation{
					Operation_id:            "test-operation",
					Instance_id:             gitopsEngineInstance.Gitopsengineinstance_id,
					Resource_id:             applicationDB.Application_id,
					Resource_type:           "Application",
					State:                   db.OperationState_Waiting,
					Operation_owner_user_id: testClusterUser.Clusteruser_id,
				}

				err = dbQueries.CreateOperation(ctx, operationDB, operationDB.Operation_owner_user_id)
				Expect(err).To(BeNil())

				By("Creating Operation CR")
				operationCR := &operation.Operation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
					},
					Spec: operation.OperationSpec{
						OperationID: operationDB.Operation_id,
					},
				}

				err = task.event.client.Create(ctx, operationCR)
				Expect(err).To(BeNil())

				retry, err := task.PerformTask(ctx)
				Expect(err).To(BeNil())
				Expect(retry).To(BeFalse())

				applicationGet := &db.Application{
					Application_id: applicationDB.Application_id,
				}

				err = dbQueries.GetApplicationById(ctx, applicationGet)
				Expect(err).To(BeNil())
				Expect(applicationDB).Should(Equal(applicationGet))

				applicationUpdate := &db.Application{
					Application_id:          databaseID,
					Name:                    "test-application-updated",
					Spec_field:              dummyApplicationSpecBytes,
					Engine_instance_inst_id: gitopsEngineInstance.Gitopsengineinstance_id,
					Managed_environment_id:  managedEnvironment.Managedenvironment_id,
					SeqID:                   101,
				}

				By("Update Application in Database")
				err = dbQueries.UpdateApplication(ctx, applicationUpdate)
				Expect(err).To(BeNil())

				By("Call Perform task again and verify that update works")
				retry, err = task.PerformTask(ctx)
				Expect(err).To(BeNil())
				Expect(retry).To(BeFalse())
				Expect(applicationUpdate).ToNot(Equal(applicationGet))

				kubernetesToDBResourceMapping := db.KubernetesToDBResourceMapping{
					KubernetesResourceType: "Namespace",
					KubernetesResourceUID:  string(kubesystemNamespace.UID),
					DBRelationType:         "GitopsEngineCluster",
					DBRelationKey:          gitopsEngineCluster.Gitopsenginecluster_id,
				}

				By("deleting resources and cleaning up db entries created by test.")
				resourcesToBeDeleted := testResources{
					Application_id:                applicationDB.Application_id,
					Operation_id:                  operationDB.Operation_id,
					Gitopsenginecluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
					Gitopsengineinstance_id:       gitopsEngineInstance.Gitopsengineinstance_id,
					ClusterCredentials_id:         gitopsEngineCluster.Clustercredentials_id,
					Managedenvironment_id:         managedEnvironment.Managedenvironment_id,
					kubernetesToDBResourceMapping: kubernetesToDBResourceMapping,
				}

				deleteTestResources(ctx, dbQueries, resourcesToBeDeleted)

			})
		})
	})
})

func testTeardown() {
	err := db.SetupForTestingDBGinkgo()
	Expect(err).To(BeNil())
}

func newRequest(namespace, name string) reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		},
	}
}

// Used to list down resources for deletion which are created while running tests.
type testResources struct {
	Operation_id                  string
	Gitopsenginecluster_id        string
	Gitopsengineinstance_id       string
	ClusterCredentials_id         string
	Application_id                string
	Managedenvironment_id         string
	kubernetesToDBResourceMapping db.KubernetesToDBResourceMapping
}

// Delete resources from table
func deleteTestResources(ctx context.Context, dbQueries db.AllDatabaseQueries, resourcesToBeDeleted testResources) {
	var rowsAffected int
	var err error

	// Delete Application
	if resourcesToBeDeleted.Application_id != "" {
		rowsAffected, err = dbQueries.DeleteApplicationById(ctx, resourcesToBeDeleted.Application_id)
		Expect(err).To(BeNil())
		Expect(rowsAffected).To(Equal(1))
	}

	// Delete kubernetesToDBResourceMapping
	if resourcesToBeDeleted.kubernetesToDBResourceMapping.KubernetesResourceUID != "" {
		rowsAffected, err = dbQueries.DeleteKubernetesResourceToDBResourceMapping(ctx, &resourcesToBeDeleted.kubernetesToDBResourceMapping)
		Expect(err).To(BeNil())
		Expect(rowsAffected).To(Equal(1))
	}

	// Delete ManagedEnvironment
	if resourcesToBeDeleted.Managedenvironment_id != "" {
		rowsAffected, err = dbQueries.DeleteManagedEnvironmentById(ctx, resourcesToBeDeleted.Managedenvironment_id)
		Expect(err).To(BeNil())
		Expect(rowsAffected).To(Equal(1))
	}

	// Delete Operation
	if resourcesToBeDeleted.Operation_id != "" {
		rowsAffected, err = dbQueries.DeleteOperationById(ctx, resourcesToBeDeleted.Operation_id)
		Expect(err).To(BeNil())
		Expect(rowsAffected).To(Equal(1))
	}

	// Delete GitopsEngineInstance
	if resourcesToBeDeleted.Gitopsengineinstance_id != "" {
		rowsAffected, err = dbQueries.DeleteGitopsEngineInstanceById(ctx, resourcesToBeDeleted.Gitopsengineinstance_id)
		Expect(err).To(BeNil())
		Expect(rowsAffected).To(Equal(1))
	}

	// Delete GitopsEngineCluster
	if resourcesToBeDeleted.Gitopsenginecluster_id != "" {
		rowsAffected, err = dbQueries.DeleteGitopsEngineClusterById(ctx, resourcesToBeDeleted.Gitopsenginecluster_id)
		Expect(err).To(BeNil())
		Expect(rowsAffected).To(Equal(1))
	}

	// Delete ClusterCredentials
	if resourcesToBeDeleted.ClusterCredentials_id != "" {
		rowsAffected, err = dbQueries.DeleteClusterCredentialsById(ctx, resourcesToBeDeleted.ClusterCredentials_id)
		Expect(err).To(BeNil())
		Expect(rowsAffected).To(Equal(1))
	}

}

func createDummyApplicationData() (appv1.Application, string, error) {
	// Create dummy Application Spec to be saved in DB
	dummyApplicationSpec := appv1.Application{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Application",
			APIVersion: "argoproj.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "operation",
			Namespace: "my-user",
		},
		Spec: appv1.ApplicationSpec{
			Source: appv1.ApplicationSource{
				Path:           "guestbook",
				TargetRevision: "HEAD",
				RepoURL:        "https://github.com/argoproj/argocd-example-apps.git",
			},
			Destination: appv1.ApplicationDestination{
				Namespace: "guestbook",
				Server:    "https://kubernetes.default.svc",
			},
			Project: "default",
			SyncPolicy: &appv1.SyncPolicy{
				Automated: &appv1.SyncPolicyAutomated{},
			},
		},
	}

	dummyApplicationSpecBytes, err := yaml.Marshal(dummyApplicationSpec)

	if err != nil {
		return appv1.Application{}, "", err
	}

	return dummyApplicationSpec, string(dummyApplicationSpecBytes), nil
}
