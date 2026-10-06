package transferconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	commandsUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	"github.com/jfrog/jfrog-client-go/artifactory"
	clientConfig "github.com/jfrog/jfrog-client-go/config"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

// The config import REST API of the target Artifactory, relative to the Artifactory URL.
//
//	POST api/configTransfer/import        - starts the import of the config ZIP (body: the ZIP, as is). 202 returns {"id":"<id>"}.
//	GET  api/configTransfer/import/<id>   - 202 in progress, 200 completed (body: the import logs), 404 unknown id, 500 the import failed.
//	                                        A transport error and 502/503/504 are transient: the polling continues until its timeout.
const configTransferImportRestApi = "api/configTransfer/import"

// The per-request time limits of the native import requests. The target service manager has no overall request timeout,
// so a request that the server accepts but never answers would hang the command forever.
const (
	// A poll request is answered at once (202), or carries the import logs (200).
	nativePollRequestTimeout = time.Minute
	// The start request uploads the whole config ZIP, so its limit grows with the ZIP size: the base covers the connection,
	// the server-side handling and the response, and the upload gets one second per nativeStartMinUploadBytesPerSec bytes.
	nativeStartBaseTimeout          = 2 * time.Minute
	nativeStartMinUploadBytesPerSec = 64 * 1024
)

// nativeImporter imports the config using the config transfer API that is built into the target Artifactory,
// without the config-import user plugin.
type nativeImporter struct {
	tcc *TransferConfigCommand
	// The target user is replaced by the import, so the polling may be rejected with 401/403.
	// The credentials are then switched to the source ones, once.
	credentialsSwitched bool
	// The client details created by start(), which the polling action continues with.
	rtDetails *httputils.HttpClientDetails
	// The interval between the retries of start(). A field and not the constant itself, so that tests do not have to wait.
	retryIntervalMilliSecs int
	// The time limits of the requests, as fields so that tests do not have to wait for the real ones.
	pollRequestTimeout time.Duration
	startBaseTimeout   time.Duration
	// The service manager used by the polling requests, which limits their time. It is derived from the current target
	// service manager, and created again when the target service manager is replaced (by the credentials switch).
	pollManager *boundedManager
}

// A service manager that limits the time of every request, derived from the service manager it was created from
type boundedManager struct {
	source  artifactory.ArtifactoryServicesManager
	timeout time.Duration
	manager artifactory.ArtifactoryServicesManager
}

// Creates a service manager that is identical to the source one (target server, credentials, certificates, HTTP retries),
// except that each of its requests is limited to the given time. The service manager of the command has no such limit, since
// it serves requests of any length. A request that exceeds the limit fails like a transport error.
// The client API takes the time limit when the client is created, and not per request: hence the second client.
func newBoundedManager(source artifactory.ArtifactoryServicesManager, timeout time.Duration) (*boundedManager, error) {
	sourceConfig := source.GetConfig()
	if sourceConfig == nil {
		return nil, errorutils.CheckErrorf("expected full config, but no configuration exists")
	}
	boundedConfig, err := clientConfig.NewConfigBuilder().
		SetServiceDetails(sourceConfig.GetServiceDetails()).
		SetCertificatesPath(sourceConfig.GetCertificatesPath()).
		SetInsecureTls(sourceConfig.IsInsecureTls()).
		SetDryRun(sourceConfig.IsDryRun()).
		SetThreads(sourceConfig.GetThreads()).
		SetContext(sourceConfig.GetContext()).
		SetDialTimeout(sourceConfig.GetDialTimeout()).
		SetHttpRetries(sourceConfig.GetHttpRetries()).
		SetHttpRetryWaitMilliSecs(sourceConfig.GetHttpRetryWaitMilliSecs()).
		SetOverallRequestTimeout(timeout).
		Build()
	if err != nil {
		return nil, err
	}
	manager, err := artifactory.New(boundedConfig)
	if err != nil {
		return nil, err
	}
	return &boundedManager{source: source, timeout: timeout, manager: manager}, nil
}

// Returns the service manager of the polling requests. It follows the target service manager of the command, which is replaced
// when the credentials are switched: the limit must not be lost then, and the new credentials must be used.
func (n *nativeImporter) pollingManager() (artifactory.ArtifactoryServicesManager, error) {
	if n.pollManager == nil || n.pollManager.source != n.tcc.TargetArtifactoryManager || n.pollManager.timeout != n.pollRequestTimeout {
		bounded, err := newBoundedManager(n.tcc.TargetArtifactoryManager, n.pollRequestTimeout)
		if err != nil {
			return nil, err
		}
		n.pollManager = bounded
	}
	return n.pollManager.manager, nil
}

func newNativeImporter(tcc *TransferConfigCommand) *nativeImporter {
	return &nativeImporter{
		tcc:                    tcc,
		retryIntervalMilliSecs: importStartRetriesIntervalMilliSecs,
		pollRequestTimeout:     nativePollRequestTimeout,
		startBaseTimeout:       nativeStartBaseTimeout,
	}
}

// The native API needs no target-side preparation: the admin permission is enforced by the server on start(),
// and the emptiness of the target is checked by the command.
func (n *nativeImporter) verify() error {
	log.Info("Using the native config transfer API of the target Artifactory.")
	if n.tcc.targetWorkingDir != "" {
		log.Warn(fmt.Sprintf("The native config transfer API does not use the target working dir. The configured target working dir '%s' is ignored.", n.tcc.targetWorkingDir))
	}
	return nil
}

// The time limit of one start request, for a ZIP of the given size
func (n *nativeImporter) startTimeout(zipSize int) time.Duration {
	return n.startBaseTimeout + time.Duration(zipSize/nativeStartMinUploadBytesPerSec)*time.Second
}

// Starts the config import in the target Artifactory. The returned reference is the id of the import.
func (n *nativeImporter) start(zip *bytes.Buffer) (importRef string, err error) {
	artifactoryUrl := clientutils.AddTrailingSlashIfNeeded(n.tcc.TargetServerDetails.GetArtifactoryUrl())
	n.rtDetails, err = commandsUtils.CreateArtifactoryClientDetails(n.tcc.TargetArtifactoryManager)
	if err != nil {
		return "", err
	}

	// The Content-Type is needed by the start request only. It is added to a copy, so that the polling does not send it.
	startDetails := *n.rtDetails
	startDetails.Headers = make(map[string]string, len(n.rtDetails.Headers)+1)
	for name, value := range n.rtDetails.Headers {
		startDetails.Headers[name] = value
	}
	startDetails.Headers["Content-Type"] = "application/octet-stream"

	startManager, err := newBoundedManager(n.tcc.TargetArtifactoryManager, n.startTimeout(zip.Len()))
	if err != nil {
		return "", err
	}

	retryExecutor := clientutils.RetryExecutor{
		MaxRetries:               importStartRetries,
		RetriesIntervalMilliSecs: n.retryIntervalMilliSecs,
		ErrorMessage:             fmt.Sprintf("Failed to start the config import process in %s", artifactoryUrl),
		LogMsgPrefix:             "[Config import]",
		ExecutionHandler: func() (shouldRetry bool, err error) {
			resp, body, err := startManager.manager.Client().SendPost(artifactoryUrl+configTransferImportRestApi, zip.Bytes(), &startDetails)
			if err != nil && !responseReceived(resp, err) {
				// Transport error
				return true, err
			}
			if resp.StatusCode != http.StatusAccepted {
				// Only server errors are retried. Others, such as 403 (not an admin) and 423 (an import is already running), are final.
				return resp.StatusCode >= http.StatusInternalServerError, errorutils.CheckResponseStatusWithBody(resp, body, http.StatusAccepted)
			}
			log.Debug("Artifactory response:", resp.Status)

			// Not retried on failure: the import was accepted, and a second start would be rejected as already running.
			var startResponse struct {
				Id json.RawMessage `json:"id"`
			}
			if err = json.Unmarshal(body, &startResponse); err != nil {
				return false, errorutils.CheckErrorf("failed to parse the config import start response '%s': %s", body, err.Error())
			}
			id, err := parseImportId(startResponse.Id)
			if err != nil {
				return false, errorutils.CheckErrorf("failed to parse the import id of the config import start response '%s': %s", body, err.Error())
			}
			if id == "" {
				return false, errorutils.CheckErrorf("the config import start response does not contain an import id: '%s'", body)
			}
			importRef = id
			log.Info("Config import id: " + importRef)
			return false, nil
		},
	}
	if err = retryExecutor.Execute(); err != nil {
		return "", err
	}
	return importRef, nil
}

// Returns the action that polls the status of the import, identified by the id returned from start().
func (n *nativeImporter) pollingAction(importRef string) httputils.PollingAction {
	artifactoryUrl := clientutils.AddTrailingSlashIfNeeded(n.tcc.TargetServerDetails.GetArtifactoryUrl())
	statusUrl := artifactoryUrl + configTransferImportRestApi + "/" + url.PathEscape(importRef)
	return func() (shouldStop bool, responseBody []byte, err error) {
		if n.rtDetails == nil {
			if n.rtDetails, err = commandsUtils.CreateArtifactoryClientDetails(n.tcc.TargetArtifactoryManager); err != nil {
				return true, nil, err
			}
		}
		pollManager, err := n.pollingManager()
		if err != nil {
			return true, nil, err
		}
		resp, body, _, err := pollManager.Client().SendGet(statusUrl, false, n.rtDetails)
		if err != nil && !responseReceived(resp, err) {
			// Transport error. Transient: the target may be restarting, or the network may be flaky.
			// The import runs on the target regardless, so keep polling. The polling executor bounds the wait.
			log.Warn(fmt.Sprintf("[Config import] Failed to get the import status, will try again: %s", err.Error()))
			return false, nil, nil
		}

		switch resp.StatusCode {
		case http.StatusOK:
			// Import completed
			return true, body, nil
		case http.StatusAccepted:
			// Import in progress
			return false, nil, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			// The user used for the target Artifactory server does not exist anymore. This is perfectly normal,
			// because the import replaced the users. We can now use the credentials of the source Artifactory server, once.
			if n.credentialsSwitched {
				return true, nil, errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK)
			}
			if n.rtDetails, err = n.tcc.switchTargetToSourceCredentials(); err != nil {
				return true, nil, err
			}
			n.credentialsSwitched = true
			return false, nil, nil
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// The target, or a gateway in front of it, is temporarily unavailable. Keep polling, as for a transport error.
			log.Warn(fmt.Sprintf("[Config import] The target Artifactory is temporarily unavailable (%s), will try again.", resp.Status))
			return false, nil, nil
		default:
			// 404 - unknown or expired import id, 500 - the import failed, or any other unexpected status
			return true, nil, errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK)
		}
	}
}

// The import id is a JSON string or a JSON number, and is used as text. A missing or null id returns an empty string.
func parseImportId(rawId json.RawMessage) (string, error) {
	if len(rawId) == 0 {
		return "", nil
	}
	var textId string
	if err := json.Unmarshal(rawId, &textId); err == nil {
		return textId, nil
	}
	// json.Number keeps the number as it was written, with no float rounding of big ids
	var numberId json.Number
	if err := json.Unmarshal(rawId, &numberId); err != nil {
		return "", fmt.Errorf("the import id must be a string or a number, got %s", rawId)
	}
	return numberId.String(), nil
}

// When the HTTP client exhausts its own retries on a 5xx (or 429) response, it returns both the last response and a
// retry-timeout error. The response carries the server error, which must be reported as is and not hidden behind the timeout.
func responseReceived(resp *http.Response, err error) bool {
	var timeoutErr clientutils.RetryExecutorTimeoutError
	return resp != nil && errors.As(err, &timeoutErr)
}
