package precheckrunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jfrog/gofrog/safeconvert"
	"net/http"
	"net/url"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	"github.com/jfrog/jfrog-cli-core/v2/utils/progressbar"
	"github.com/jfrog/jfrog-client-go/artifactory"
	"github.com/jfrog/jfrog-client-go/artifactory/services"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

type RemoteUrlCheckStatus string

const (
	remoteUrlCheckName              = "Remote repositories URL connectivity"
	remoteUrlCheckPollingTimeout    = 30 * time.Minute
	remoteUrlCheckPollingInterval   = 5 * time.Second
	remoteUrlCheckRetries           = 3
	remoteUrlCheckIntervalMilliSecs = 10000
)

type remoteRepoSettings struct {
	Key         string `json:"key,omitempty"`
	Url         string `json:"url,omitempty"`
	RepoType    string `json:"repo_type,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"` // #nosec G117 -- API struct for remote repo settings
	QueryParams string `json:"query_params,omitempty"`
}

type remoteUrlResponse struct {
	Status                   RemoteUrlCheckStatus     `json:"status,omitempty"`
	InaccessibleRepositories []inaccessibleRepository `json:"inaccessible_repositories,omitempty"`
	CheckedRepositories      uint                     `json:"checked_repositories,omitempty"`
	TotalRepositories        uint                     `json:"total_repositories,omitempty"`
}

type inaccessibleRepository struct {
	RepoKey    string `json:"repo_key,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Url        string `json:"url,omitempty"`
}

// Run remote repository URLs accessibility test before transferring configuration from one Artifactory to another
type RemoteRepositoryCheck struct {
	targetServicesManager *artifactory.ArtifactoryServicesManager
	remoteRepositories    []interface{}
	// The API of the target Artifactory that the check runs through: the config-import plugin, or the native config transfer API
	api remoteCheckApi
	// The URL of the status requests, which is set when the check is started
	statusUrl string
	// The time settings of the check. Fields and not the constants themselves, so that tests do not have to wait.
	retryIntervalMilliSecs int
	pollingTimeout         time.Duration
	pollingInterval        time.Duration
}

// NewRemoteRepositoryCheck creates the check that runs through the config-import plugin of the target Artifactory
func NewRemoteRepositoryCheck(targetServicesManager *artifactory.ArtifactoryServicesManager, remoteRepositories []interface{}) *RemoteRepositoryCheck {
	return newRemoteRepositoryCheck(targetServicesManager, remoteRepositories, pluginRemoteCheckApi{})
}

// NewNativeRemoteRepositoryCheck creates the check that runs through the native config transfer API of the target Artifactory,
// which does not require the config-import plugin
func NewNativeRemoteRepositoryCheck(targetServicesManager *artifactory.ArtifactoryServicesManager, remoteRepositories []interface{}) *RemoteRepositoryCheck {
	return newRemoteRepositoryCheck(targetServicesManager, remoteRepositories, nativeRemoteCheckApi{})
}

func newRemoteRepositoryCheck(targetServicesManager *artifactory.ArtifactoryServicesManager, remoteRepositories []interface{}, api remoteCheckApi) *RemoteRepositoryCheck {
	return &RemoteRepositoryCheck{
		targetServicesManager:  targetServicesManager,
		remoteRepositories:     remoteRepositories,
		api:                    api,
		retryIntervalMilliSecs: remoteUrlCheckIntervalMilliSecs,
		pollingTimeout:         remoteUrlCheckPollingTimeout,
		pollingInterval:        remoteUrlCheckPollingInterval,
	}
}

func (rrc *RemoteRepositoryCheck) Name() string {
	return remoteUrlCheckName
}

func (rrc *RemoteRepositoryCheck) ExecuteCheck(args RunArguments) (passed bool, err error) {
	if len(rrc.remoteRepositories) == 0 {
		log.Debug("No remote repositories to check.")
		return true, nil
	}
	remoteUrlRequest, err := rrc.createRemoteUrlRequest()
	if err != nil {
		return false, err
	}
	inaccessibleRepositories, err := rrc.doCheckRemoteRepositories(args, remoteUrlRequest)
	if err != nil {
		return false, err
	}
	if len(*inaccessibleRepositories) == 0 {
		return true, nil
	}
	return false, handleFailureRun(*inaccessibleRepositories)
}

// Create the remote URL request from the received remote repository details from Artifactory
func (rrc *RemoteRepositoryCheck) createRemoteUrlRequest() ([]remoteRepoSettings, error) {
	remoteUrlRequests := make([]remoteRepoSettings, len(rrc.remoteRepositories))
	for i, remoteRepository := range rrc.remoteRepositories {
		// The remote repository interface is not necessarily of RemoteRepositoryBaseParams
		// type (can be a map) and therefore we marshal and unmarshal it.
		remoteRepositoryBytes, err := json.Marshal(remoteRepository)
		if err != nil {
			return nil, errorutils.CheckError(err)
		}
		var remoteRepositoryParams services.RemoteRepositoryBaseParams
		if err = json.Unmarshal(remoteRepositoryBytes, &remoteRepositoryParams); err != nil {
			return nil, errorutils.CheckError(err)
		}

		remoteUrlRequests[i] = remoteRepoSettings{
			Key:         remoteRepositoryParams.Key,
			Url:         remoteRepositoryParams.Url,
			RepoType:    remoteRepositoryParams.PackageType,
			Username:    remoteRepositoryParams.Username,
			Password:    remoteRepositoryParams.Password,
			QueryParams: remoteRepositoryParams.QueryParams,
		}
	}
	return remoteUrlRequests, nil
}

func (rrc *RemoteRepositoryCheck) doCheckRemoteRepositories(args RunArguments, remoteUrlRequest []remoteRepoSettings) (inaccessibleRepositories *[]inaccessibleRepository, err error) {
	artifactoryUrl := clientutils.AddTrailingSlashIfNeeded(args.ServerDetails.ArtifactoryUrl)

	body, err := json.Marshal(remoteUrlRequest) // #nosec G117 -- credentials sent to Artifactory API
	if err != nil {
		return nil, errorutils.CheckError(err)
	}

	// Create rtDetails
	rtDetails, err := utils.CreateArtifactoryClientDetails(*rrc.targetServicesManager)
	if err != nil {
		return nil, err
	}

	progressBar, err := rrc.startCheckRemoteRepositories(rtDetails, artifactoryUrl, args, body)
	if err != nil {
		return nil, err
	}
	defer func() {
		if progressBar != nil {
			progressBar.GetBar().Abort(true)
		}
	}()

	// Wait for remote repositories check completion
	return rrc.waitForRemoteReposCheckCompletion(rtDetails, artifactoryUrl, progressBar)
}

func (rrc *RemoteRepositoryCheck) startCheckRemoteRepositories(rtDetails *httputils.HttpClientDetails, artifactoryUrl string, args RunArguments, requestBody []byte) (*progressbar.TasksProgressBar, error) {
	started, err := rrc.api.start(rrc, args, artifactoryUrl, rtDetails, requestBody)
	if err != nil {
		return nil, err
	}
	rrc.statusUrl = started.statusUrl

	if args.ProgressMng == nil {
		return nil, nil
	}
	total, err := checkProgressTotal(started.totalRepositories)
	if err != nil {
		return nil, err
	}
	return args.ProgressMng.NewTasksProgressBar(total, "Remote repositories"), nil
}

func (rrc *RemoteRepositoryCheck) waitForRemoteReposCheckCompletion(rtDetails *httputils.HttpClientDetails, artifactoryUrl string, progressBar *progressbar.TasksProgressBar) (*[]inaccessibleRepository, error) {
	pollingExecutor := &httputils.PollingExecutor{
		Timeout:         rrc.pollingTimeout,
		PollingInterval: rrc.pollingInterval,
		MsgPrefix:       "Waiting for remote repositories check completion in Artifactory server at " + artifactoryUrl,
		PollingAction:   rrc.createImportPollingAction(rtDetails, progressBar),
	}

	body, err := pollingExecutor.Execute()
	if err != nil {
		return nil, err
	}
	response, err := unmarshalRemoteUrlResponse(body)
	if err != nil {
		return nil, err
	}
	return &response.InaccessibleRepositories, nil
}

func (rrc *RemoteRepositoryCheck) createImportPollingAction(rtDetails *httputils.HttpClientDetails, progressBar *progressbar.TasksProgressBar) httputils.PollingAction {
	return func() (shouldStop bool, responseBody []byte, err error) {
		// Get remote repositories check status
		resp, body, _, err := (*rrc.targetServicesManager).Client().SendGet(rrc.statusUrl, true, rtDetails)
		return rrc.api.handlePollResponse(resp, body, err, progressBar)
	}
}

// 202 - Update the progress of the check. Shared by the plugin and the native API, which return the same progress body.
func updateCheckProgress(body []byte, progressBar *progressbar.TasksProgressBar) error {
	response, err := unmarshalRemoteUrlResponse(body)
	if err != nil {
		return err
	}
	if progressBar != nil {
		delta, err := checkProgressDelta(response.CheckedRepositories, progressBar.GetBar().Current())
		if err != nil {
			return err
		}
		progressBar.GetBar().IncrInt64(delta)
	}
	return nil
}

// The total of the progress bar, from the number of the repositories to check
func checkProgressTotal(totalRepositories uint) (int64, error) {
	signedTotalRepositories, err := safeconvert.UintToInt(totalRepositories)
	if err != nil {
		return 0, fmt.Errorf("failed to convert total repositories count to int: %w", err)
	}
	return int64(signedTotalRepositories), nil
}

// The increment that brings the progress bar from its current value to the number of the repositories checked so far
func checkProgressDelta(checkedRepositories uint, currentBarValue int64) (int64, error) {
	signedCheckedRepositories, err := safeconvert.UintToInt(checkedRepositories)
	if err != nil {
		return 0, fmt.Errorf("failed to convert checked repositories count to int: %w", err)
	}
	return int64(signedCheckedRepositories) - currentBarValue, nil
}

// The API of the target Artifactory that runs the remote repositories check
type remoteCheckApi interface {
	// Starts the check, and returns what is needed to follow it
	start(rrc *RemoteRepositoryCheck, args RunArguments, artifactoryUrl string, rtDetails *httputils.HttpClientDetails, requestBody []byte) (startedCheck, error)
	// Handles the result of a status request, as the action of the polling executor does
	handlePollResponse(resp *http.Response, body []byte, requestErr error, progressBar *progressbar.TasksProgressBar) (shouldStop bool, responseBody []byte, err error)
}

type startedCheck struct {
	// The number of the remote repositories that are checked, for the progress bar
	totalRepositories uint
	// The URL of the status requests
	statusUrl string
}

// The remote repositories check of the config-import plugin:
//
//	POST api/plugins/execute/remoteRepositoriesCheck        - starts the check. 200 returns the remoteUrlResponse (total_repositories).
//	GET  api/plugins/execute/remoteRepositoriesCheckStatus  - 202 in progress, 200 completed. Any other status is polled again.
type pluginRemoteCheckApi struct{}

func (pluginRemoteCheckApi) start(rrc *RemoteRepositoryCheck, args RunArguments, artifactoryUrl string, rtDetails *httputils.HttpClientDetails, requestBody []byte) (startedCheck, error) {
	var response *remoteUrlResponse
	// Sometimes, POST api/plugins/execute/remoteRepositoriesCheck returns unexpectedly 404 errors, although the config-import plugin is installed.
	// To overcome this issue, we use a custom retryExecutor and not the default retry executor that retries only on HTTP errors >= 500.
	retryExecutor := clientutils.RetryExecutor{
		Context:                  args.Context,
		MaxRetries:               remoteUrlCheckRetries,
		RetriesIntervalMilliSecs: rrc.retryIntervalMilliSecs,
		ErrorMessage:             fmt.Sprintf("Failed to start the remote repositories check in %s", artifactoryUrl),
		LogMsgPrefix:             "[Config import]",
		ExecutionHandler: func() (shouldRetry bool, err error) {
			// Start the remote repositories check process
			resp, responseBody, err := (*rrc.targetServicesManager).Client().SendPost(artifactoryUrl+utils.PluginsExecuteRestApi+"remoteRepositoriesCheck", requestBody, rtDetails)
			if err != nil {
				return false, err
			}
			if err = errorutils.CheckResponseStatusWithBody(resp, responseBody, http.StatusOK); err != nil {
				return true, err
			}

			response, err = unmarshalRemoteUrlResponse(responseBody)
			return false, err
		},
	}
	if err := retryExecutor.Execute(); err != nil {
		return startedCheck{}, err
	}
	return startedCheck{
		totalRepositories: response.TotalRepositories,
		statusUrl:         artifactoryUrl + utils.PluginsExecuteRestApi + "remoteRepositoriesCheckStatus",
	}, nil
}

func (pluginRemoteCheckApi) handlePollResponse(resp *http.Response, body []byte, requestErr error, progressBar *progressbar.TasksProgressBar) (shouldStop bool, responseBody []byte, err error) {
	if requestErr != nil {
		return true, nil, requestErr
	}

	// 200 - Check completed
	if resp.StatusCode == http.StatusOK {
		return true, body, nil
	}

	// 202 - Update status
	if resp.StatusCode == http.StatusAccepted {
		if err = updateCheckProgress(body, progressBar); err != nil {
			return true, nil, err
		}
	}

	return false, nil, nil
}

// The remote repositories check of the native config transfer API, which is built into the target Artifactory:
//
//	POST api/configTransfer/remoteRepositoriesCheck       - starts the check (body: the remote repositories, as for the plugin). 202 returns {"id":"<id>"}.
//	GET  api/configTransfer/remoteRepositoriesCheck/<id>  - 202 in progress, 200 completed (body: the remoteUrlResponse),
//	                                                        404 unknown id, 500 the check failed. Other statuses are failures as well.
const nativeRemoteCheckRestApi = "api/configTransfer/remoteRepositoriesCheck"

type nativeRemoteCheckApi struct{}

func (nativeRemoteCheckApi) start(rrc *RemoteRepositoryCheck, args RunArguments, artifactoryUrl string, rtDetails *httputils.HttpClientDetails, requestBody []byte) (started startedCheck, err error) {
	var checkId string
	// Unlike the plugin, the native API has no 404 flakiness to overcome: only transport errors and server errors (>= 500) are retried.
	retryExecutor := clientutils.RetryExecutor{
		Context:                  args.Context,
		MaxRetries:               remoteUrlCheckRetries,
		RetriesIntervalMilliSecs: rrc.retryIntervalMilliSecs,
		ErrorMessage:             fmt.Sprintf("Failed to start the remote repositories check in %s", artifactoryUrl),
		LogMsgPrefix:             "[Config import]",
		ExecutionHandler: func() (shouldRetry bool, err error) {
			resp, responseBody, err := (*rrc.targetServicesManager).Client().SendPost(artifactoryUrl+nativeRemoteCheckRestApi, requestBody, rtDetails)
			if err != nil && !responseReceived(resp, err) {
				// Transport error
				return true, err
			}
			if resp.StatusCode != http.StatusAccepted {
				// Only server errors are retried. Others, such as 403 (not an admin) and 404, are final.
				return resp.StatusCode >= http.StatusInternalServerError, errorutils.CheckResponseStatusWithBody(resp, responseBody, http.StatusAccepted)
			}

			// Not retried on failure: the check was accepted, and starting it again would run it twice.
			var startResponse struct {
				Id json.RawMessage `json:"id"`
			}
			log.Debug(fmt.Sprintf("Response from Artifactory:\n%s", responseBody))
			if err = json.Unmarshal(responseBody, &startResponse); err != nil {
				return false, errorutils.CheckErrorf("failed to parse the remote repositories check start response '%s': %s", responseBody, err.Error())
			}
			id, err := parseCheckId(startResponse.Id)
			if err != nil {
				return false, errorutils.CheckErrorf("failed to parse the check id of the remote repositories check start response '%s': %s", responseBody, err.Error())
			}
			if id == "" {
				return false, errorutils.CheckErrorf("the remote repositories check start response does not contain a check id: '%s'", responseBody)
			}
			checkId = id
			return false, nil
		},
	}
	if err = retryExecutor.Execute(); err != nil {
		return startedCheck{}, err
	}
	// The start response carries the check id only, so the progress total is the number of the repositories that were sent
	return startedCheck{
		totalRepositories: uint(len(rrc.remoteRepositories)),
		statusUrl:         artifactoryUrl + nativeRemoteCheckRestApi + "/" + url.PathEscape(checkId),
	}, nil
}

func (nativeRemoteCheckApi) handlePollResponse(resp *http.Response, body []byte, requestErr error, progressBar *progressbar.TasksProgressBar) (shouldStop bool, responseBody []byte, err error) {
	if requestErr != nil && !responseReceived(resp, requestErr) {
		return true, nil, requestErr
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// Check completed
		return true, body, nil
	case http.StatusAccepted:
		// Check in progress
		if err = updateCheckProgress(body, progressBar); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	default:
		// 404 - unknown or expired check id, 500 - the check failed, or any other unexpected status.
		// Unlike the plugin polling, there is nothing to wait for: stop, and report the server response.
		return true, nil, errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK)
	}
}

// The check id is a JSON string or a JSON number, and is used as text. A missing or null id returns an empty string.
// The same approach as the id of the native config import.
func parseCheckId(rawId json.RawMessage) (string, error) {
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
		return "", fmt.Errorf("the check id must be a string or a number, got %s", rawId)
	}
	return numberId.String(), nil
}

// When the HTTP client exhausts its own retries on a 5xx (or 429) response, it returns both the last response and a
// retry-timeout error. The response carries the server error, which must be reported as is and not hidden behind the timeout.
func responseReceived(resp *http.Response, err error) bool {
	var timeoutErr clientutils.RetryExecutorTimeoutError
	return resp != nil && errors.As(err, &timeoutErr)
}

// Unmarshal response from Artifactory to remoteUrlResponse
func unmarshalRemoteUrlResponse(body []byte) (*remoteUrlResponse, error) {
	log.Debug(fmt.Sprintf("Response from Artifactory:\n%s", body))
	var response remoteUrlResponse
	err := json.Unmarshal(body, &response)
	return &response, errorutils.CheckError(err)
}

// Create csv summary of all the files with inaccessible remote repositories and log the result
func handleFailureRun(inaccessibleRepositories []inaccessibleRepository) (err error) {
	// Create summary
	csvPath, err := utils.CreateCSVFile("inaccessible-repositories", inaccessibleRepositories, time.Now())
	if err != nil {
		log.Error("Couldn't create the inaccessible remote repository URLs CSV file", err)
		return
	}
	// Log result
	log.Info(fmt.Sprintf("Found %d inaccessible remote repository URLs. Check the summary CSV file in: %s", len(inaccessibleRepositories), csvPath))
	return
}
