package charts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	bv1 "github.com/rancher/backup-restore-operator/pkg/apis/resources.cattle.io/v1"
	localConfig "github.com/rancher/observability-e2e/tests/helper/config"
	"github.com/rancher/observability-e2e/tests/helper/utils"
	catalogv1 "github.com/rancher/rancher/pkg/apis/catalog.cattle.io/v1"
	"github.com/rancher/rancher/tests/v2/actions/projects"
	"github.com/rancher/rancher/tests/v2/actions/secrets"
	"github.com/rancher/shepherd/clients/rancher"
	"github.com/rancher/shepherd/clients/rancher/catalog"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	v1 "github.com/rancher/shepherd/clients/rancher/v1"
	"github.com/rancher/shepherd/extensions/users"
	"github.com/rancher/shepherd/pkg/api/steve/catalog/types"
	namegen "github.com/rancher/shepherd/pkg/namegenerator"
	"github.com/rancher/shepherd/pkg/wait"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	RancherBackupRestoreNamespace     = "cattle-resources-system"
	RancherBackupRestoreName          = "rancher-backup"
	RancherBackupRestoreCRDName       = "rancher-backup-crd"
	BackupRestoreConfigurationFileKey = "../helper/yamls/inputBackupRestoreConfig.yaml"
	localStorageClass                 = "../helper/yamls/localStorageClass.yaml"
	EncryptionConfigFilePath          = "../helper/yamls/encrptionConfig.yaml"
	backupSteveType                   = "resources.cattle.io.backup"
	RestoreSteveType                  = "resources.cattle.io.restore"
	resourceCount                     = 2
	cniCalico                         = "calico"
)

var (
	rules = []management.PolicyRule{
		{
			APIGroups: []string{"management.cattle.io"},
			Resources: []string{"projects"},
			Verbs:     []string{"backupRole"},
		},
	}
)

type BackupOptions struct {
	Name                       string
	ResourceSetName            string
	RetentionCount             int64
	EncryptionConfigSecretName string
}

type ProvisioningConfig struct {
	Providers              []string `json:"providers,omitempty" yaml:"providers,omitempty"`
	NodeProviders          []string `json:"nodeProviders,omitempty" yaml:"nodeProviders,omitempty"`
	RKE2KubernetesVersions []string `json:"rke2KubernetesVersion,omitempty" yaml:"rke2KubernetesVersion,omitempty"`
	CNIs                   []string `json:"cni,omitempty" yaml:"cni,omitempty"`
}

// InstallRancherBackupRestoreChart installs the Rancher backup/restore chart with optional storage configuration.
func InstallRancherBackupRestoreChart(client *rancher.Client, installOpts *InstallOptions, chartOpts *RancherBackupRestoreOpts, withStorage bool, storageType string) error {
	serverSetting, err := client.Management.Setting.ByID(serverURLSettingID)
	if err != nil {
		return err
	}

	// Prepare the payload for chart installation.
	chartInstallActionPayload := &PayloadOpts{
		InstallOptions: *installOpts,
		Name:           RancherBackupRestoreName,
		Namespace:      RancherBackupRestoreNamespace,
		Host:           serverSetting.Value,
	}
	chartInstallAction := newBackupChartInstallAction(chartInstallActionPayload, withStorage, chartOpts, storageType)

	// Get the catalog client for the specified cluster.
	catalogClient, err := client.GetClusterCatalogClient(installOpts.Cluster.ID)
	if err != nil {
		return err
	}

	// Install the chart using the catalog client.
	if err = catalogClient.InstallChart(chartInstallAction, catalog.RancherChartRepo); err != nil {
		return err
	}

	// Watch for the App resource to ensure successful deployment.
	watchInterface, err := catalogClient.Apps(RancherBackupRestoreNamespace).Watch(context.TODO(), metav1.ListOptions{
		FieldSelector:  metadataName + RancherBackupRestoreName,
		TimeoutSeconds: &FiveMinuteTimeout,
	})
	if err != nil {
		return err
	}

	// Check function to validate the state of the app during the watch.
	checkFunc := func(event watch.Event) (bool, error) {
		app, ok := event.Object.(*catalogv1.App)
		if !ok {
			return false, fmt.Errorf("unexpected type %T", event.Object)
		}

		// Check the deployment state of the app.
		state := app.Status.Summary.State
		switch state {
		case string(catalogv1.StatusDeployed):
			return true, nil // Deployment succeeded.
		case string(catalogv1.StatusFailed):
			return false, fmt.Errorf("failed to install rancher-backup-restore chart") // Deployment failed.
		default:
			return false, nil // Continue waiting.
		}
	}

	// Wait for the app to be successfully deployed.
	err = wait.WatchWait(watchInterface, checkFunc)
	if err != nil {
		if err.Error() == wait.TimeoutError {
			return fmt.Errorf("timeout: rancher-backup-restore chart was not installed within 5 minutes")
		}
		return err
	}

	return nil // Successful installation.
}

// CreateOpaqueS3Secret creates an opaque Kubernetes secret for S3 credentials.
func CreateOpaqueS3Secret(steveClient *v1.Client, backupRestoreConfig *localConfig.BackupRestoreConfig) (string, error) {
	// Define the secret template with S3 access and secret keys.
	var SecretName = namegen.AppendRandomString("bro-secret")
	secretTemplate := secrets.NewSecretTemplate(
		SecretName,
		backupRestoreConfig.CredentialSecretNamespace,
		map[string][]byte{
			"accessKey": []byte(backupRestoreConfig.AccessKey),
			"secretKey": []byte(backupRestoreConfig.SecretKey),
		},
		corev1.SecretTypeOpaque,
	)
	// Create the secret using the Steve client.
	createdSecret, err := steveClient.SteveType(secrets.SecretSteveType).Create(secretTemplate)

	return createdSecret.Name, err
}

// CreateEncryptionConfigSecret creates an opaque Kubernetes secret for encryption configuration.
func CreateEncryptionConfigSecret(steveClient *v1.Client, yamlPath, secretName, namespace string) (string, error) {
	// Read the encryption config file
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return "", fmt.Errorf("failed to read encryption config file %s: %w", yamlPath, err)
	}

	// Define the secret template
	secretTemplate := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"encryption-provider-config.yaml": data,
		},
		Type: corev1.SecretTypeOpaque,
	}

	// Create the secret using the Steve client
	createdSecret, err := steveClient.SteveType("secret").Create(secretTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to create secret %s in namespace %s: %w", secretName, namespace, err)
	}

	return createdSecret.Name, nil
}

// newBackupChartInstallAction prepares the chart installation action with storage and payload options.
func newBackupChartInstallAction(p *PayloadOpts, withStorage bool, rancherBackupRestoreOpts *RancherBackupRestoreOpts, storageType string) *types.ChartInstallAction {
	// Configure backup values if storage is enabled.
	backupValues := map[string]interface{}{}
	if withStorage {
		switch storageType {
		case "s3":
			backupValues["s3"] = map[string]any{
				"bucketName":                rancherBackupRestoreOpts.BucketName,
				"credentialSecretName":      rancherBackupRestoreOpts.CredentialSecretName,
				"credentialSecretNamespace": rancherBackupRestoreOpts.CredentialSecretNamespace,
				"enabled":                   rancherBackupRestoreOpts.Enabled,
				"endpoint":                  rancherBackupRestoreOpts.Endpoint,
				"folder":                    rancherBackupRestoreOpts.Folder,
				"region":                    rancherBackupRestoreOpts.Region,
			}

		case "storageClass":
			backupValues["persistence"] = map[string]any{
				"enabled":      rancherBackupRestoreOpts.Enabled,
				"size":         "2Gi", // Default size, can be modified
				"storageClass": rancherBackupRestoreOpts.StorageClassName,
			}

		default:
			fmt.Printf("Unsupported storage type: %s\n", storageType)
			return nil
		}
	}

	// Prepare the chart installation actions for the backup and its CRDs.
	chartInstall := newChartInstall(
		p.Name,
		p.InstallOptions.Version,
		p.InstallOptions.Cluster.ID,
		p.InstallOptions.Cluster.Name,
		p.Host,
		rancherChartsName,
		p.ProjectID,
		p.DefaultRegistry,
		backupValues,
	)

	chartInstallCRD := newChartInstall(
		p.Name+"-crd",
		p.InstallOptions.Version,
		p.InstallOptions.Cluster.ID,
		p.InstallOptions.Cluster.Name,
		p.Host,
		rancherChartsName,
		p.ProjectID,
		p.DefaultRegistry,
		nil,
	)

	chartInstalls := []types.ChartInstall{*chartInstallCRD, *chartInstall}

	// Combine the chart installs into a single installation action.
	chartInstallAction := newChartInstallAction(p.Namespace, p.ProjectID, chartInstalls)

	return chartInstallAction
}

// Function to uninstall the backup-restore charts
func UninstallBackupRestoreChart(client *rancher.Client, clusterID string, namespace string) error {
	chartNames := []string{RancherBackupRestoreName, RancherBackupRestoreCRDName}

	for _, chartName := range chartNames {
		err := UninstallChart(client, clusterID, chartName, namespace)
		if err != nil {
			e2e.Failf("Failed to uninstall the chart %s. Error: %v", chartName, err)
			return err // Stop on first failure
		}
	}
	return nil
}

// CreateStorageResources handles the creation of resources based on StorageType
func CreateStorageResources(storageType string, client *rancher.Client, backupRestoreConfig *localConfig.BackupRestoreConfig) (string, error) {
	switch storageType {
	case "s3":
		secretName, err := CreateOpaqueS3Secret(client.Steve, backupRestoreConfig)
		if err != nil {
			return "", fmt.Errorf("failed to create opaque secret with S3 credentials: %v", err)
		}
		return secretName, nil

	case "storageClass":
		err := utils.DeployYamlResource(client, localStorageClass, RancherBackupRestoreNamespace)
		if err != nil {
			return "", fmt.Errorf("failed to create the storage class and pv: %v", err)
		}
		return "storage-class-resource", nil // Returning a placeholder name

	default:
		return "", fmt.Errorf("invalid storage type specified: %s", storageType)
	}
}

// Function to handle the delete of resources based on StorageType
func DeleteStorageResources(storageType string, client *rancher.Client, backupRestoreConfig *localConfig.BackupRestoreConfig) error {
	// Skip deletion if storageType is "s3" as this is handled in test suite level
	if storageType == "s3" {
		return nil
	}
	switch storageType {
	case "storageClass":
		err := utils.DeleteYamlResource(client, localStorageClass, RancherBackupRestoreNamespace)
		if err != nil {
			return fmt.Errorf("failed to delete the storage class and pv: %v", err)
		}
	default:
		return fmt.Errorf("invalid storage type specified: %s", storageType)
	}
	return nil
}

func setBackupObject(backupOptions BackupOptions) *bv1.Backup {
	// Create a Backup object using provided options
	backup := &bv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name: backupOptions.Name,
		},
		Spec: bv1.BackupSpec{
			ResourceSetName:            backupOptions.ResourceSetName,
			RetentionCount:             backupOptions.RetentionCount,
			EncryptionConfigSecretName: backupOptions.EncryptionConfigSecretName,
		},
	}
	return backup
}

func VerifyBackupCompleted(client *rancher.Client, steveType string, backup *v1.SteveAPIObject) (bool, string, error) {
	timeout := 3 * time.Minute
	interval := 2 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	timeoutChan := time.After(timeout)

	for {
		select {
		case <-ticker.C:
			backupObj, err := client.Steve.SteveType(steveType).ByID(backup.ID)
			if err != nil {
				return false, "", err
			}

			backupStatus := &bv1.BackupStatus{}
			err = utils.ConvertToStruct(backupObj.Status, backupStatus)
			if err != nil {
				return false, "", err
			}

			// Check if backup is ready
			for _, condition := range backupStatus.Conditions {
				if condition.Type == "Ready" && condition.Status == corev1.ConditionTrue {
					e2e.Logf("Backup is completed!")
					return true, backupStatus.Filename, nil
				}
			}

		case <-timeoutChan:
			return false, "", fmt.Errorf("timeout waiting for backup to complete")
		}
	}
}

func CreateRancherBackupAndVerifyCompleted(client *rancher.Client, backupOptions BackupOptions) (*v1.SteveAPIObject, string, error) {
	backup := setBackupObject(backupOptions)
	backupTemplate := bv1.NewBackup("", backupOptions.Name, *backup)
	client, err := client.ReLogin() // This needs to be done as the chart installed changed the schema
	if err != nil {
		return nil, "", err
	}
	completedBackup, err := client.Steve.SteveType(backupSteveType).Create(backupTemplate)
	if err != nil {
		return nil, "", err
	}
	_, backupFileName, err := VerifyBackupCompleted(client, backupSteveType, completedBackup)
	if err != nil {
		return nil, "", err
	}
	return completedBackup, backupFileName, err
}

func CreateRancherResources(client *rancher.Client, clusterID string, context string) ([]*management.User, []*management.Project, []*management.RoleTemplate, error) {
	userList := []*management.User{}
	projList := []*management.Project{}
	roleList := []*management.RoleTemplate{}

	for i := 0; i < resourceCount; i++ {
		u, err := users.CreateUserWithRole(client, users.UserConfig(), "user")
		if err != nil {
			return userList, projList, roleList, err
		}
		userList = append(userList, u)

		p, _, err := projects.CreateProjectAndNamespace(client, clusterID)
		if err != nil {
			return userList, projList, roleList, err
		}
		projList = append(projList, p)

		rt, err := client.Management.RoleTemplate.Create(
			&management.RoleTemplate{
				Context: context,
				Name:    namegen.AppendRandomString("bro-role"),
				Rules:   rules,
			})
		if err != nil {
			return userList, projList, roleList, err
		}
		roleList = append(roleList, rt)
	}

	return userList, projList, roleList, nil
}

func SetRestoreObject(backupName string, prune bool, encryptionConfigSecretName string) bv1.Restore {
	restore := bv1.Restore{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "restore-",
		},
		Spec: bv1.RestoreSpec{
			BackupFilename:             backupName,
			Prune:                      &prune,
			EncryptionConfigSecretName: encryptionConfigSecretName,
		},
	}
	return restore
}

func VerifyRestoreCompleted(client *rancher.Client, steveType string, restore *v1.SteveAPIObject) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err() // Timeout reached
		case <-ticker.C:
			restoreObj, err := client.Steve.SteveType(steveType).ByID(restore.ID)
			if err != nil {
				continue // Retry if there's an error
			}

			restoreStatus := &bv1.RestoreStatus{}
			if err := v1.ConvertToK8sType(restoreObj.Status, restoreStatus); err != nil {
				return false, err // Conversion error, stop polling
			}

			for _, condition := range restoreStatus.Conditions {
				if condition.Type == "Ready" && condition.Status == corev1.ConditionTrue {
					e2e.Logf("Restore is completed!")
					return true, nil
				}
			}
		}
	}
}

func VerifyRancherResources(client *rancher.Client, curUserList []*management.User, curProjList []*management.Project, curRoleList []*management.RoleTemplate) error {
	var errs []error

	e2e.Logf("Verifying user resources...")
	for _, user := range curUserList {
		userID, err := users.GetUserIDByName(client, user.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("user %s: %w", user.Name, err))
		} else if userID == "" {
			errs = append(errs, fmt.Errorf("user %s not found", user.Name))
		}
	}

	e2e.Logf("Verifying project resources...")
	for _, proj := range curProjList {
		_, err := client.Management.Project.ByID(proj.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("project %s: %w", proj.ID, err))
		}
	}

	e2e.Logf("Verifying role resources...")
	for _, role := range curRoleList {
		_, err := client.Management.RoleTemplate.ByID(role.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("role %s: %w", role.ID, err))
		}
	}

	return errors.Join(errs...)
}
