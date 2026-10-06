package transferconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfrog/gofrog/version"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/generic"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferconfig/configxmlutils"
	commandsUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils/precheckrunner"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	"github.com/jfrog/jfrog-cli-core/v2/common/commands"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/artifactory/services"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

const (
	importStartRetries                  = 3
	importStartRetriesIntervalMilliSecs = 10000
	importPollingTimeout                = 10 * time.Minute
	importPollingInterval               = 10 * time.Second
	interruptedByUserErr                = "Config transfer was cancelled"
	minTransferConfigArtifactoryVersion = "6.23.21"
)

type TransferConfigCommand struct {
	commandsUtils.TransferConfigBase
	dryRun           bool
	force            bool
	verbose          bool
	preChecks        bool
	sourceWorkingDir string
	targetWorkingDir string
	importer         configImporter
}

func NewTransferConfigCommand(sourceServer, targetServer *config.ServerDetails) *TransferConfigCommand {
	tcc := &TransferConfigCommand{TransferConfigBase: *commandsUtils.NewTransferConfigBase(sourceServer, targetServer)}
	// The config-import plugin is currently the only supported way to import the config to the target server
	tcc.importer = newPluginImporter(tcc)
	return tcc
}

func (tcc *TransferConfigCommand) CommandName() string {
	return "rt_transfer_config"
}

func (tcc *TransferConfigCommand) SetDryRun(dryRun bool) *TransferConfigCommand {
	tcc.dryRun = dryRun
	return tcc
}

func (tcc *TransferConfigCommand) SetForce(force bool) *TransferConfigCommand {
	tcc.force = force
	return tcc
}

func (tcc *TransferConfigCommand) SetVerbose(verbose bool) *TransferConfigCommand {
	tcc.verbose = verbose
	return tcc
}

func (tcc *TransferConfigCommand) SetPreChecks(preChecks bool) *TransferConfigCommand {
	tcc.preChecks = preChecks
	return tcc
}

func (tcc *TransferConfigCommand) SetSourceWorkingDir(workingDir string) *TransferConfigCommand {
	tcc.sourceWorkingDir = workingDir
	return tcc
}

func (tcc *TransferConfigCommand) SetTargetWorkingDir(workingDir string) *TransferConfigCommand {
	tcc.targetWorkingDir = workingDir
	return tcc
}

func (tcc *TransferConfigCommand) Run() (err error) {
	if err = tcc.CreateServiceManagers(tcc.dryRun); err != nil {
		return err
	}
	if tcc.preChecks {
		return tcc.runPreChecks()
	}

	tcc.LogTitle("Phase 1/5 - Preparations")
	err = tcc.printWarnings()
	if err != nil {
		return err
	}
	err = tcc.validateServerPrerequisites()
	if err != nil {
		return err
	}
	// Run export on the source Artifactory
	tcc.LogTitle("Phase 2/5 - Export configuration from the source Artifactory")
	exportPath, cleanUp, err := tcc.exportSourceArtifactory()
	defer func() {
		err = errors.Join(err, cleanUp())
	}()
	if err != nil {
		return
	}

	tcc.LogTitle("Phase 3/5 - Download and modify configuration")
	selectedRepos, err := tcc.GetSelectedRepositories()
	if err != nil {
		return
	}

	// Download and decrypt the config XML from the source Artifactory
	configXml, remoteRepos, err := tcc.getEncryptedItems(selectedRepos)
	if err != nil {
		return
	}

	// In Artifactory 7.49, the repositories configuration was moved from the artifactory-config.xml to the database.
	// this method removes the repositories from the artifactory-config.xml file, to be aligned with new Artifactory versions.
	configXml, err = configxmlutils.RemoveAllRepositories(configXml)
	if err != nil {
		return
	}

	// Create an archive of the source Artifactory export in memory
	archiveConfig, err := archiveConfig(exportPath, configXml)
	if err != nil {
		return
	}

	// Import the archive to the target Artifactory
	tcc.LogTitle("Phase 4/5 - Import configuration to the target Artifactory")
	err = tcc.importToTargetArtifactory(archiveConfig)
	if err != nil {
		return
	}

	// Update the server details of the target Artifactory in the CLI configuration
	err = tcc.updateServerDetails()
	if err != nil {
		return
	}

	tcc.LogTitle("Phase 5/5 - Import repositories to the target Artifactory")
	if err = tcc.TransferRepositoriesToTarget(selectedRepos, remoteRepos); err != nil {
		return
	}

	// If config transfer passed successfully, add conclusion message
	log.Output()
	log.Info("Config transfer completed successfully!")
	tcc.LogIfFederatedMemberRemoved()
	log.Info("☝️  Please make sure to disable configuration transfer in MyJFrog before running the 'jf transfer-files' command.")
	return
}

// Create the directory containing the Artifactory export content
// Return values:
// exportPath - The export path
// unsetTempDir - Clean up function
// err - Error if any
func (tcc *TransferConfigCommand) createExportPath() (exportPath string, unsetTempDir func(), err error) {
	if tcc.sourceWorkingDir != "" {
		// Set the base temp dir according to the value of the --source-working-dir flag
		oldTempDir := fileutils.GetTempDirBase()
		fileutils.SetTempDirBase(tcc.sourceWorkingDir)
		unsetTempDir = func() {
			fileutils.SetTempDirBase(oldTempDir)
		}
	} else {
		unsetTempDir = func() {}
	}

	// Create temp directory that will contain the export directory
	exportPath, err = fileutils.CreateTempDir()
	if err != nil {
		return "", unsetTempDir, err
	}

	return exportPath, unsetTempDir, errorutils.CheckError(os.Chmod(exportPath, 0777))
}

func (tcc *TransferConfigCommand) runPreChecks() error {
	// Warn if default admin:password credentials are exist in the source server
	_, err := tcc.IsDefaultCredentials()
	if err != nil {
		return err
	}

	if err = tcc.validateServerPrerequisites(); err != nil {
		return err
	}

	selectedRepos, err := tcc.GetSelectedRepositories()
	if err != nil {
		return err
	}

	// Download and decrypt the remote repositories list from the source Artifactory
	_, remoteRepositories, err := tcc.getEncryptedItems(selectedRepos)
	if err != nil {
		return err
	}

	return tcc.NewPreChecksRunner(selectedRepos, remoteRepositories).Run(context.Background(), tcc.TargetServerDetails)
}

func (tcc *TransferConfigCommand) printWarnings() (err error) {
	// Prompt message
	promptMsg := "This command will transfer Artifactory config data:\n" +
		fmt.Sprintf("From %s - <%s>\n", coreutils.PrintBold("Source"), tcc.SourceServerDetails.ArtifactoryUrl) +
		fmt.Sprintf("To %s - <%s>\n", coreutils.PrintBold("Target"), tcc.TargetServerDetails.ArtifactoryUrl) +
		"This action will wipe out all Artifactory content in the target.\n" +
		"Make sure that you're using strong credentials in your source platform (for example - having the default admin:password credentials isn't recommended).\n" +
		"Those credentials will be transferred to your SaaS target platform.\n" +
		"Are you sure you want to continue?"

	if !coreutils.AskYesNo(promptMsg, false) {
		return errorutils.CheckErrorf(interruptedByUserErr)
	}

	// Check if there is a configured user using default credentials in the source platform.
	isDefaultCredentials, err := tcc.IsDefaultCredentials()
	if err != nil {
		return err
	}
	if isDefaultCredentials && !coreutils.AskYesNo("Are you sure you want to continue?", false) {
		return errorutils.CheckErrorf(interruptedByUserErr)
	}
	return nil
}

// Make sure the target Artifactory is ready for the import, by verifying it with the config importer (for the plugin importer:
// the config-import plugin is installed and the user is admin).
// Unless the force flag is set, also make sure the target is empty, by counting the number of the users. If it is bigger than 2, return an error.
func (tcc *TransferConfigCommand) validateTargetServer() error {
	// Verify the target server is ready for the import (plugin: the config-import plugin is installed and the user is admin)
	if err := tcc.importer.verify(); err != nil {
		return err
	}

	if tcc.force {
		return nil
	}
	log.Info("Verifying target server is empty...")
	users, err := tcc.TargetArtifactoryManager.GetAllUsers()
	if err != nil {
		return err
	}
	// We consider an "empty" Artifactory as an Artifactory server that contains 2 users: the admin user and the anonymous.
	if len(users) > 2 {
		return errorutils.CheckErrorf("cowardly refusing to import the config to the target server, because it contains more than 2 users. By default, this command avoids transferring the config to a server which isn't empty. You can bypass this rule by providing the --force flag to the transfer-config command.")
	}
	return nil
}

// Creates the Pre-checks runner for the config import command
func (tcc *TransferConfigCommand) NewPreChecksRunner(selectedRepos map[utils.RepoType][]services.RepositoryDetails, remoteRepositories []interface{}) (runner *precheckrunner.PreCheckRunner) {
	runner = precheckrunner.NewPreChecksRunner()

	// Add pre-checks here
	runner.AddCheck(precheckrunner.NewRepositoryNamingCheck(selectedRepos))
	runner.AddCheck(precheckrunner.NewRemoteRepositoryCheck(&tcc.TargetArtifactoryManager, remoteRepositories))

	return
}

func (tcc *TransferConfigCommand) getEncryptedItems(selectedSourceRepos map[utils.RepoType][]services.RepositoryDetails) (configXml string, remoteRepositories []interface{}, err error) {
	reactivateKeyEncryption, err := tcc.DeactivateKeyEncryption()
	if err != nil {
		return "", nil, err
	}
	defer func() {
		err = errors.Join(err, reactivateKeyEncryption())
	}()

	// Download artifactory.config.xml from the source Artifactory server.
	// It is safer to not store the decrypted artifactory.config.xml file in the file system, and therefore we only keep it in memory.
	configXml, err = tcc.SourceArtifactoryManager.GetConfigDescriptor()
	if err != nil {
		return
	}

	// Get all remote repositories from the source Artifactory server.
	if remoteRepositoriesDetails, ok := selectedSourceRepos[utils.Remote]; ok && len(remoteRepositoriesDetails) > 0 {
		remoteRepositories = make([]interface{}, len(remoteRepositoriesDetails))
		for i, remoteRepositoryDetails := range remoteRepositoriesDetails {
			if err = tcc.SourceArtifactoryManager.GetRepository(remoteRepositoryDetails.Key, &remoteRepositories[i]); err != nil {
				return
			}
		}
	}

	return
}

// Export the config from the source Artifactory to a local directory.
// Return the path to the export directory, a cleanup function and an error.
func (tcc *TransferConfigCommand) exportSourceArtifactory() (string, func() error, error) {
	// Create temp directory that will contain the export directory
	exportPath, unsetTempDir, err := tcc.createExportPath()
	defer unsetTempDir()
	if err != nil {
		return "", func() error { return nil }, err
	}

	// Do export
	exportParams := services.ExportParams{
		ExportPath:      exportPath,
		IncludeMetadata: clientutils.Pointer(false),
		Verbose:         &tcc.verbose,
		ExcludeContent:  clientutils.Pointer(true),
	}
	cleanUp := func() error { return fileutils.RemoveTempDir(exportPath) }
	if err = tcc.SourceArtifactoryManager.Export(exportParams); err != nil {
		return "", cleanUp, err
	}

	// Make sure only the export directory contained in the temp directory
	files, err := fileutils.ListFiles(exportPath, true)
	if err != nil {
		return "", cleanUp, err
	}
	if len(files) == 0 {
		return "", cleanUp, errorutils.CheckErrorf("couldn't find the export directory in '%s'. Please make sure to run this command inside the source Artifactory machine", exportPath)
	}
	if len(files) > 1 {
		return "", cleanUp, errorutils.CheckErrorf("only the exported directory is expected to be in the export directory %s, but found %q", exportPath, files)
	}

	// Return the export directory and the cleanup function
	return files[0], cleanUp, nil
}

// Import from the input buffer to the target Artifactory
func (tcc *TransferConfigCommand) importToTargetArtifactory(buffer *bytes.Buffer) (err error) {
	importRef, err := tcc.importer.start(buffer)
	if err != nil {
		return err
	}

	// Wait for config import completion
	return tcc.waitForImportCompletion(tcc.importer.pollingAction(importRef))
}

func (tcc *TransferConfigCommand) waitForImportCompletion(pollingAction httputils.PollingAction) error {
	artifactoryUrl := clientutils.AddTrailingSlashIfNeeded(tcc.TargetServerDetails.GetArtifactoryUrl())

	pollingExecutor := &httputils.PollingExecutor{
		Timeout:         importPollingTimeout,
		PollingInterval: importPollingInterval,
		MsgPrefix:       "Waiting for config import completion in Artifactory server at " + artifactoryUrl,
		PollingAction:   pollingAction,
	}

	body, err := pollingExecutor.Execute()
	if err != nil {
		return err
	}
	log.Info(fmt.Sprintf("Logs from Artifactory:\n%s", body))
	if strings.Contains(string(body), "[ERROR]") {
		return errorutils.CheckErrorf("Errors detected during config import. Hint: You can skip transferring some Artifactory repositories by using the '--exclude-repos' command option. Run 'jf rt transfer-config -h' for more information.")
	}
	return nil
}

// After the config import, the user used for the target Artifactory server does not exist anymore, because the import replaces
// all users. Switch the target server details and service managers to use the credentials of the source Artifactory server.
// Returns the client details of the new target Artifactory service manager.
func (tcc *TransferConfigCommand) switchTargetToSourceCredentials() (*httputils.HttpClientDetails, error) {
	newServerDetails := tcc.TargetServerDetails
	newServerDetails.SetUser(tcc.SourceServerDetails.GetUser())
	newServerDetails.SetPassword(tcc.SourceServerDetails.GetPassword())
	newServerDetails.SetAccessToken(tcc.SourceServerDetails.GetAccessToken())

	var err error
	tcc.TargetArtifactoryManager, err = utils.CreateServiceManager(newServerDetails, -1, 0, false)
	if err != nil {
		return nil, err
	}
	tcc.TargetAccessManager, err = utils.CreateAccessServiceManager(newServerDetails, false)
	if err != nil {
		return nil, err
	}
	return commandsUtils.CreateArtifactoryClientDetails(tcc.TargetArtifactoryManager)
}

func (tcc *TransferConfigCommand) updateServerDetails() error {
	log.Info("Pinging the target Artifactory...")
	newTargetServerDetails := tcc.TargetServerDetails

	// Copy credentials from the source server details
	newTargetServerDetails.User = tcc.SourceServerDetails.User
	newTargetServerDetails.Password = tcc.SourceServerDetails.Password
	newTargetServerDetails.SshKeyPath = tcc.SourceServerDetails.SshKeyPath
	newTargetServerDetails.SshPassphrase = tcc.SourceServerDetails.SshPassphrase
	newTargetServerDetails.AccessToken = tcc.SourceServerDetails.AccessToken
	newTargetServerDetails.RefreshToken = tcc.SourceServerDetails.RefreshToken
	newTargetServerDetails.ArtifactoryRefreshToken = tcc.SourceServerDetails.ArtifactoryRefreshToken
	newTargetServerDetails.ArtifactoryTokenRefreshInterval = tcc.SourceServerDetails.ArtifactoryTokenRefreshInterval
	newTargetServerDetails.ClientCertPath = tcc.SourceServerDetails.ClientCertPath
	newTargetServerDetails.ClientCertKeyPath = tcc.SourceServerDetails.ClientCertKeyPath

	// Ping to validate the transfer ended successfully
	pingCmd := generic.NewPingCommand().SetServerDetails(newTargetServerDetails)
	err := pingCmd.Run()
	if err != nil {
		return err
	}
	log.Info("Ping to the target Artifactory was successful. Updating the server configuration in JFrog CLI.")

	// Update the server details in JFrog CLI configuration
	configCmd := commands.NewConfigCommand(commands.AddOrEdit, newTargetServerDetails.ServerId).SetInteractive(false).SetDetails(newTargetServerDetails)
	err = configCmd.Run()
	if err != nil {
		return err
	}
	tcc.TargetServerDetails = newTargetServerDetails
	return nil
}

// Make sure that the source Artifactory version is sufficient.
// Returns the source Artifactory version.
func (tcc *TransferConfigCommand) validateMinVersion() (sourceArtifactoryVersion string, err error) {
	log.Info("Verifying minimum version of the source server...")
	sourceArtifactoryVersion, err = tcc.SourceArtifactoryManager.GetVersion()
	if err != nil {
		return
	}
	var targetArtifactoryVersion string
	targetArtifactoryVersion, err = tcc.TargetArtifactoryManager.GetVersion()
	if err != nil {
		return
	}

	// Validate minimal Artifactory version in the source server
	err = clientutils.ValidateMinimumVersion(clientutils.Artifactory, sourceArtifactoryVersion, minTransferConfigArtifactoryVersion)
	if err != nil {
		return
	}

	// Validate that the target Artifactory server version is >= than the source Artifactory server version
	if !version.NewVersion(targetArtifactoryVersion).AtLeast(sourceArtifactoryVersion) {
		err = errorutils.CheckErrorf("The source Artifactory version (%s) can't be higher than the target Artifactory version (%s).", sourceArtifactoryVersion, targetArtifactoryVersion)
	}

	return
}

func (tcc *TransferConfigCommand) validateServerPrerequisites() (err error) {
	var sourceArtifactoryVersion string
	// Make sure that the source Artifactory version is sufficient.
	if sourceArtifactoryVersion, err = tcc.validateMinVersion(); err != nil {
		return
	}

	// Check connectivity to JFrog Access if the source Artifactory version is >= 7.0.0
	if versionErr := clientutils.ValidateMinimumVersion(clientutils.Projects, sourceArtifactoryVersion, commandsUtils.MinJFrogProjectsArtifactoryVersion); versionErr == nil {
		if err = tcc.ValidateAccessServerConnection(tcc.SourceServerDetails, tcc.SourceAccessManager); err != nil {
			return
		}
	}

	// Make sure source and target Artifactory URLs are different
	if err = tcc.ValidateDifferentServers(); err != nil {
		return
	}
	// Make sure that the target Artifactory is empty and the config-import plugin is installed
	return tcc.validateTargetServer()
}
