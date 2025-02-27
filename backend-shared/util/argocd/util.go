package argocd

import (
	"fmt"
	"strings"

	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
)

const (
	managedEnvPrefix = "managed-env-"

	// ArgoCDDefaultDestinationInCluster is 'in-cluster' which is the spec destination value that Argo CD recognizes
	// as indicating that Argo CD should deploy to the local cluster (the cluster that Argo CD is installed on).
	ArgoCDDefaultDestinationInCluster = "in-cluster"
)

// GenerateArgoCDClusterSecretName generates the name of the Argo CD cluster secret (and the name of the server within Argo CD).
func GenerateArgoCDClusterSecretName(managedEnv db.ManagedEnvironment) string {
	return managedEnvPrefix + managedEnv.Managedenvironment_id
}

func GenerateArgoCDApplicationName(gitopsDeploymentCRUID string) string {
	return "gitopsdepl-" + string(gitopsDeploymentCRUID)
}

// ConvertArgoCDClusterSecretNameToManagedIdDatabaseRowId takes the name of an Argo CD cluster secret as input.
// This name should correspond to the name of a Secret resource in the Argo CD namespace, which contains
// cluster credentials.
//
// All Argo CD cluster secrets generated by the GitOps Service are of the form managed-env-(uuid of managed env row in database)
// - We can thus use the name of the secret to find the corresponding ManagedEnvironment database row
//
// The secret name should:
// - either, be 'ArgoCDDefaultDestinationInCluster'
// - or, started with managed-env-(uuid of managed environment row in table)
// - otherwise it is not an Argo CD Cluster secret
func ConvertArgoCDClusterSecretNameToManagedIdDatabaseRowId(argoCDClusterSecretName string) (string, bool, error) {

	const (
		isLocalEnv_true  = true
		isLocalEnv_false = false
	)

	if argoCDClusterSecretName == ArgoCDDefaultDestinationInCluster {
		return "", isLocalEnv_true, nil
	}

	if !strings.HasPrefix(argoCDClusterSecretName, managedEnvPrefix) {
		return "", isLocalEnv_false, fmt.Errorf("secret name is not an GitOps Service Argo CD cluster secret")
	}

	managedEnvID := argoCDClusterSecretName[len(managedEnvPrefix):]

	return managedEnvID, isLocalEnv_false, nil

}
