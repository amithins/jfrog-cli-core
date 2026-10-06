package transferconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	commandUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	commonTests "github.com/jfrog/jfrog-cli-core/v2/common/tests"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/tests"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/stretchr/testify/assert"
)

// Characterization tests for the target-side config import flow (JFMIG-97).
// They pin the HTTP calls, their order, the retry behaviour, the credentials used on each call and the
// user-visible log lines, through the command-level entry points only (validateTargetServer and
// importToTargetArtifactory), so they hold both before and after the configImporter refactoring.

const (
	characterizationTargetUser = "target-user"
	characterizationTargetPass = "target-pass"
	characterizationSourceUser = "source-user"
	characterizationSourcePass = "source-pass"
	characterizationWorkingDir = "/tmp/target-wd"
)

// recordedRequest is a single HTTP call received by the mock target server.
type recordedRequest struct {
	method string
	uri    string
	user   string
	pass   string
	body   string
}

func (r recordedRequest) String() string {
	return fmt.Sprintf("%s %s as %s", r.method, r.uri, r.user)
}

// requestRecorder is a goroutine-safe list of the HTTP calls received by a mock server.
type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (rec *requestRecorder) record(r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		body = []byte("<read error: " + err.Error() + ">")
	}
	user, pass, _ := r.BasicAuth()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.requests = append(rec.requests, recordedRequest{method: r.Method, uri: r.RequestURI, user: user, pass: pass, body: string(body)})
}

func (rec *requestRecorder) all() []recordedRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]recordedRequest(nil), rec.requests...)
}

// createCharacterizationCommand creates a transfer config command whose target is the given mock server details.
// The target and the source have different credentials, so tests can tell which of them was used on each call.
func createCharacterizationCommand(t *testing.T, targetServerDetails *config.ServerDetails) *TransferConfigCommand {
	targetServerDetails.User = characterizationTargetUser
	targetServerDetails.Password = characterizationTargetPass
	sourceServerDetails := &config.ServerDetails{Url: "http://source-dummy/", ArtifactoryUrl: "http://source-dummy/artifactory/", User: characterizationSourceUser, Password: characterizationSourcePass}
	cmd := createTransferConfigCommand(t, sourceServerDetails, targetServerDetails)
	cmd.SetTargetWorkingDir(characterizationWorkingDir)
	return cmd
}

func redirectLogs(t *testing.T) (logs *bytes.Buffer) {
	_, logs, previousLog := tests.RedirectLogOutputToBuffer()
	t.Cleanup(func() { log.SetLogger(previousLog) })
	return logs
}

// The import start is retried also on 404 (and not only on 5xx), with the same request. The retry interval is a
// constant, so this test sleeps for a single interval (~10s). The status polling sequence (202 in progress, 401/403 and
// the switch to the source credentials) is covered without sleeping by the pollingAction tests in importer_test.go.
func TestImportToTargetArtifactoryCharacterizeRetryOn404(t *testing.T) {
	logs := redirectLogs(t)
	recorder := &requestRecorder{}
	importCalls := 0
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		switch {
		case strings.HasPrefix(r.RequestURI, "/"+commandUtils.PluginsExecuteRestApi+"configImportStatus"):
			_, err := w.Write([]byte("import finished"))
			assert.NoError(t, err)
		case strings.HasPrefix(r.RequestURI, "/"+commandUtils.PluginsExecuteRestApi+"configImport"):
			importCalls++
			if importCalls == 1 {
				// The config-import plugin sometimes returns an unexpected 404, and the import start is retried
				w.WriteHeader(http.StatusNotFound)
				_, err := w.Write([]byte("plugin hiccup"))
				assert.NoError(t, err)
				return
			}
			_, err := w.Write([]byte("123456"))
			assert.NoError(t, err)
		default:
			assert.Fail(t, "Unexpected request: "+r.RequestURI)
		}
	})
	defer testServer.Close()

	cmd := createCharacterizationCommand(t, serverDetails)
	err := cmd.importToTargetArtifactory(bytes.NewBufferString("zip-content"))
	assert.NoError(t, err)

	importUri := "/" + commandUtils.PluginsExecuteRestApi + "configImport?params=workingDir=" + characterizationWorkingDir
	statusUri := "/" + commandUtils.PluginsExecuteRestApi + "configImportStatus?params=workingDir=" + characterizationWorkingDir
	expected := []recordedRequest{
		// Start, rejected with 404 and retried
		{method: http.MethodPost, uri: importUri, user: characterizationTargetUser, pass: characterizationTargetPass, body: "zip-content"},
		{method: http.MethodPost, uri: importUri, user: characterizationTargetUser, pass: characterizationTargetPass, body: "zip-content"},
		// Polling, with the timestamp of the successful attempt
		{method: http.MethodPost, uri: statusUri, user: characterizationTargetUser, pass: characterizationTargetPass, body: "123456"},
	}
	assert.Equal(t, expected, recorder.all())

	output := logs.String()
	assert.Contains(t, output, "[Config import](Attempt 1) - Failed to start the config import process in "+serverDetails.ArtifactoryUrl)
	assert.Equal(t, 1, strings.Count(output, "Config import timestamp: 123456"))
	assert.Contains(t, output, "Logs from Artifactory:\nimport finished")
	// The timestamp is logged only for the successful attempt, and before the final logs
	assert.Less(t, strings.Index(output, "Config import timestamp: 123456"), strings.Index(output, "Logs from Artifactory:"))
}

func TestImportToTargetArtifactoryCharacterizeErrorInImportLogs(t *testing.T) {
	logs := redirectLogs(t)
	recorder := &requestRecorder{}
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		if strings.HasPrefix(r.RequestURI, "/"+commandUtils.PluginsExecuteRestApi+"configImportStatus") {
			_, err := w.Write([]byte("line 1\n[ERROR] something failed\nline 3"))
			assert.NoError(t, err)
			return
		}
		_, err := w.Write([]byte("42"))
		assert.NoError(t, err)
	})
	defer testServer.Close()

	cmd := createCharacterizationCommand(t, serverDetails)
	err := cmd.importToTargetArtifactory(bytes.NewBufferString("zip-content"))
	assert.EqualError(t, err, "Errors detected during config import. Hint: You can skip transferring some Artifactory repositories by using the '--exclude-repos' command option. Run 'jf rt transfer-config -h' for more information.")

	// The logs from Artifactory are printed before the error is returned
	assert.Contains(t, logs.String(), "Logs from Artifactory:\nline 1\n[ERROR] something failed\nline 3")
	requests := recorder.all()
	if assert.Len(t, requests, 2) {
		assert.Contains(t, requests[0].uri, "configImport?")
		assert.Contains(t, requests[1].uri, "configImportStatus?")
		assert.Equal(t, "42", requests[1].body)
	}
}

func TestImportToTargetArtifactoryCharacterizeNoWorkingDir(t *testing.T) {
	recorder := &requestRecorder{}
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		_, err := w.Write([]byte("42"))
		assert.NoError(t, err)
	})
	defer testServer.Close()

	cmd := createCharacterizationCommand(t, serverDetails)
	cmd.SetTargetWorkingDir("")
	assert.NoError(t, cmd.importToTargetArtifactory(bytes.NewBufferString("zip-content")))
	requests := recorder.all()
	if assert.Len(t, requests, 2) {
		assert.Equal(t, "/"+commandUtils.PluginsExecuteRestApi+"configImport", requests[0].uri)
		assert.Equal(t, "/"+commandUtils.PluginsExecuteRestApi+"configImportStatus", requests[1].uri)
	}
}

func TestValidateTargetServerCharacterizeCallsOrderAndLogs(t *testing.T) {
	logs := redirectLogs(t)
	recorder := &requestRecorder{}
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		switch {
		case strings.HasPrefix(r.RequestURI, "/"+commandUtils.PluginsExecuteRestApi+"configImportVersion"):
			content, err := json.Marshal(commandUtils.VersionResponse{Version: "1.2.3"})
			assert.NoError(t, err)
			_, err = w.Write(content)
			assert.NoError(t, err)
		case strings.HasPrefix(r.RequestURI, "/"+commandUtils.PluginsExecuteRestApi+"checkPermissions"):
			w.WriteHeader(http.StatusOK)
		default:
			_, err := w.Write([]byte("[]"))
			assert.NoError(t, err)
		}
	})
	defer testServer.Close()

	cmd := createCharacterizationCommand(t, serverDetails)
	assert.NoError(t, cmd.validateTargetServer())

	requests := recorder.all()
	if assert.Len(t, requests, 3) {
		// The version URL does not carry the working dir, the permissions URL does
		assert.Equal(t, recordedRequest{method: http.MethodGet, uri: "/" + commandUtils.PluginsExecuteRestApi + "configImportVersion", user: characterizationTargetUser, pass: characterizationTargetPass}, requests[0])
		assert.Equal(t, recordedRequest{method: http.MethodGet, uri: "/" + commandUtils.PluginsExecuteRestApi + "checkPermissions?params=workingDir=" + characterizationWorkingDir, user: characterizationTargetUser, pass: characterizationTargetPass}, requests[1])
		assert.Equal(t, http.MethodGet, requests[2].method)
		assert.Equal(t, "/api/security/users", requests[2].uri)
	}

	output := logs.String()
	verifyingIdx := strings.Index(output, "Verifying config-import plugin is installed in the target server...")
	versionIdx := strings.Index(output, "config-import plugin version: 1.2.3")
	emptyIdx := strings.Index(output, "Verifying target server is empty...")
	assert.NotEqual(t, -1, verifyingIdx)
	assert.Less(t, verifyingIdx, versionIdx)
	assert.Less(t, versionIdx, emptyIdx)
}

func TestValidateTargetServerCharacterizeForceSkipsUsersCheck(t *testing.T) {
	logs := redirectLogs(t)
	recorder := &requestRecorder{}
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		if strings.HasPrefix(r.RequestURI, "/"+commandUtils.PluginsExecuteRestApi+"configImportVersion") {
			content, err := json.Marshal(commandUtils.VersionResponse{Version: "1.2.3"})
			assert.NoError(t, err)
			_, err = w.Write(content)
			assert.NoError(t, err)
		}
	})
	defer testServer.Close()

	cmd := createCharacterizationCommand(t, serverDetails).SetForce(true)
	assert.NoError(t, cmd.validateTargetServer())

	requests := recorder.all()
	if assert.Len(t, requests, 2) {
		assert.Contains(t, requests[0].uri, "configImportVersion")
		assert.Contains(t, requests[1].uri, "checkPermissions")
	}
	assert.NotContains(t, logs.String(), "Verifying target server is empty...")
}

func TestValidateTargetServerCharacterizeCheckPermissionsFailure(t *testing.T) {
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.RequestURI, "/"+commandUtils.PluginsExecuteRestApi+"configImportVersion") {
			content, err := json.Marshal(commandUtils.VersionResponse{Version: "1.2.3"})
			assert.NoError(t, err)
			_, err = w.Write(content)
			assert.NoError(t, err)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, err := w.Write([]byte("An admin user is required"))
		assert.NoError(t, err)
	})
	defer testServer.Close()

	cmd := createCharacterizationCommand(t, serverDetails)
	err := cmd.validateTargetServer()
	assert.EqualError(t, err, "Target server response: 403 Forbidden.\nAn admin user is required")
}
