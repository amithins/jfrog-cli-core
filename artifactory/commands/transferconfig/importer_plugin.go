package transferconfig

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	commandsUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

// pluginImporter imports the config using the config-import user plugin, installed in the target Artifactory
// and accessed through /api/plugins/execute/*.
type pluginImporter struct {
	tcc *TransferConfigCommand
	// The client details created by start(), which the polling action continues with.
	rtDetails *httputils.HttpClientDetails
}

func newPluginImporter(tcc *TransferConfigCommand) *pluginImporter {
	return &pluginImporter{tcc: tcc}
}

// Verify installation of the config-import plugin in the target server and make sure that the user is admin
func (p *pluginImporter) verify() error {
	log.Info("Verifying config-import plugin is installed in the target server...")
	artifactoryUrl := clientutils.AddTrailingSlashIfNeeded(p.tcc.TargetServerDetails.GetArtifactoryUrl())

	// Create rtDetails
	rtDetails, err := commandsUtils.CreateArtifactoryClientDetails(p.tcc.TargetArtifactoryManager)
	if err != nil {
		return err
	}

	// Get config-import plugin version
	configImportVersionUrl := artifactoryUrl + commandsUtils.PluginsExecuteRestApi + "configImportVersion"
	configImportPluginVersion, err := commandsUtils.GetTransferPluginVersion(p.tcc.TargetArtifactoryManager.Client(), configImportVersionUrl, "config-import", commandsUtils.Target, rtDetails)
	if err != nil {
		return err
	}
	log.Info("config-import plugin version: " + configImportPluginVersion)

	// Execute 'GET /api/plugins/execute/checkPermissions'
	resp, body, _, err := p.tcc.TargetArtifactoryManager.Client().SendGet(artifactoryUrl+commandsUtils.PluginsExecuteRestApi+"checkPermissions"+p.getWorkingDirParam(), false, rtDetails)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// Unexpected status received: 403 if the user is not admin, 500+ if there is a server error
	messageFormat := fmt.Sprintf("Target server response: %s.\n%s", resp.Status, body)
	return errors.New(messageFormat)
}

// Starts the config import in the target Artifactory. The returned reference is the timestamp of the import.
func (p *pluginImporter) start(zip *bytes.Buffer) (importRef string, err error) {
	artifactoryUrl := clientutils.AddTrailingSlashIfNeeded(p.tcc.TargetServerDetails.GetArtifactoryUrl())
	var timestamp []byte

	// Create rtDetails
	p.rtDetails, err = commandsUtils.CreateArtifactoryClientDetails(p.tcc.TargetArtifactoryManager)
	if err != nil {
		return "", err
	}

	// Sometimes, POST api/plugins/execute/configImport return unexpectedly 404 errors, although the config-import plugin is installed.
	// To overcome this issue, we use a custom retryExecutor and not the default retry executor that retries only on HTTP errors >= 500.
	retryExecutor := clientutils.RetryExecutor{
		MaxRetries:               importStartRetries,
		RetriesIntervalMilliSecs: importStartRetriesIntervalMilliSecs,
		ErrorMessage:             fmt.Sprintf("Failed to start the config import process in %s", artifactoryUrl),
		LogMsgPrefix:             "[Config import]",
		ExecutionHandler: func() (shouldRetry bool, err error) {
			// Start the config import async process
			resp, body, err := p.tcc.TargetArtifactoryManager.Client().SendPost(artifactoryUrl+commandsUtils.PluginsExecuteRestApi+"configImport"+p.getWorkingDirParam(), zip.Bytes(), p.rtDetails)
			if err != nil {
				return false, err
			}
			if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
				return true, err
			}

			log.Debug("Artifactory response:", resp.Status)
			timestamp = body
			log.Info("Config import timestamp: " + string(timestamp))
			return false, nil
		},
	}

	if err = retryExecutor.Execute(); err != nil {
		return "", err
	}
	return string(timestamp), nil
}

// Returns the action that polls the status of the import, identified by the timestamp returned from start().
// Precondition: start() was called successfully on this importer, since the polling continues with the client details it created.
func (p *pluginImporter) pollingAction(importRef string) httputils.PollingAction {
	artifactoryUrl := clientutils.AddTrailingSlashIfNeeded(p.tcc.TargetServerDetails.GetArtifactoryUrl())
	importTimestamp := []byte(importRef)
	rtDetails := p.rtDetails
	return func() (shouldStop bool, responseBody []byte, err error) {
		// Get config import status
		resp, body, err := p.tcc.TargetArtifactoryManager.Client().SendPost(artifactoryUrl+commandsUtils.PluginsExecuteRestApi+"configImportStatus"+p.getWorkingDirParam(), importTimestamp, rtDetails)
		if err != nil {
			return true, nil, err
		}

		// 200 - Import completed
		if resp.StatusCode == http.StatusOK {
			return true, body, nil
		}

		// 202 - Import in progress
		if resp.StatusCode == http.StatusAccepted {
			return false, nil, nil
		}

		// Unexpected status
		if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusUnauthorized, http.StatusForbidden); err != nil {
			return false, nil, err
		}

		// 401 or 403 - The user used for the target Artifactory server does not exist anymore.
		// This is perfectly normal, because the import caused the user to be deleted. We can now use the credentials of the source Artifactory server.
		rtDetails, err = p.tcc.switchTargetToSourceCredentials()
		if err != nil {
			return true, nil, err
		}

		// After 401 or 403, the server credentials are fixed, and therefore we can run again
		return false, nil, nil
	}
}

func (p *pluginImporter) getWorkingDirParam() string {
	if p.tcc.targetWorkingDir != "" {
		return "?params=workingDir=" + p.tcc.targetWorkingDir
	}
	return ""
}
