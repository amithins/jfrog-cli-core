package transferconfig

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	commonTests "github.com/jfrog/jfrog-cli-core/v2/common/tests"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	utilsTests "github.com/jfrog/jfrog-cli-core/v2/utils/tests"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/stretchr/testify/assert"
)

const nativeImportPath = "/" + configTransferImportRestApi

// nativeRecordedRequest is a request received by the mock target server
type nativeRecordedRequest struct {
	method      string
	uri         string
	body        string
	contentType string
	user        string
}

// nativeMock is a mock target server that records the requests and answers them using a per-request responder
type nativeMock struct {
	mu       sync.Mutex
	requests []nativeRecordedRequest
}

func (m *nativeMock) recorded() []nativeRecordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]nativeRecordedRequest(nil), m.requests...)
}

// Creates a native importer on top of a mock server. The responder gets the 0-based index of the request.
func newNativeImporterWithMock(t *testing.T, responder func(index int, w http.ResponseWriter, r *http.Request)) (*nativeImporter, *TransferConfigCommand, *nativeMock) {
	mock := &nativeMock{}
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		user, _, _ := r.BasicAuth()
		mock.mu.Lock()
		index := len(mock.requests)
		mock.requests = append(mock.requests, nativeRecordedRequest{method: r.Method, uri: r.RequestURI, body: string(body), contentType: r.Header.Get("Content-Type"), user: user})
		mock.mu.Unlock()
		responder(index, w, r)
	})
	t.Cleanup(testServer.Close)
	serverDetails.User, serverDetails.Password = "target-user", "target-pass"
	cmd := createTransferConfigCommand(t, &config.ServerDetails{Url: "dummy-url", User: "source-user", Password: "source-pass"}, serverDetails)
	// The HTTP client has its own retries on 5xx, which would multiply the requests counted by the tests.
	// They are disabled here, and enabled back by the tests that cover their interaction with the importer.
	setTargetClientRetries(t, cmd, 0)
	importer := newNativeImporter(cmd)
	// Do not wait between the retries of start()
	importer.retryIntervalMilliSecs = 0
	return importer, cmd, mock
}

// Rebuilds the target service manager with the given number of HTTP client retries (negative - the client default)
func setTargetClientRetries(t *testing.T, cmd *TransferConfigCommand, retries int) {
	manager, err := utils.CreateServiceManager(cmd.TargetServerDetails, retries, 0, false)
	assert.NoError(t, err)
	cmd.TargetArtifactoryManager = manager
}

func respondNative(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// Drops the connection of the current request without answering it, as a network failure or a restarting server would
func dropNativeConnection(t *testing.T, w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !assert.True(t, ok, "the response writer must support hijacking") {
		return
	}
	conn, _, err := hijacker.Hijack()
	if assert.NoError(t, err) {
		assert.NoError(t, conn.Close())
	}
}

func TestNativeImporterImplementsConfigImporter(t *testing.T) {
	var _ configImporter = (*nativeImporter)(nil)
}

// The tests set the retry interval to 0, so the production value has to be pinned separately
func TestNewNativeImporterRetryInterval(t *testing.T) {
	cmd := createTransferConfigCommand(t, &config.ServerDetails{Url: "dummy-url"}, &config.ServerDetails{Url: "dummy-url"})
	importer := newNativeImporter(cmd)
	assert.Equal(t, importStartRetriesIntervalMilliSecs, importer.retryIntervalMilliSecs)
	// The tests shorten the request time limits, so the production values are pinned here
	assert.Equal(t, nativePollRequestTimeout, importer.pollRequestTimeout)
	assert.Equal(t, nativeStartBaseTimeout, importer.startBaseTimeout)
	assert.Same(t, cmd, importer.tcc)
}

func TestNativeImporterVerifyMakesNoServerCalls(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		respondNative(w, http.StatusInternalServerError, "unexpected")
	})
	assert.NoError(t, importer.verify())
	assert.Empty(t, mock.recorded())
}

// The native importer ignores the target working dir (the plugin importer sends it to the plugin), which must not go unnoticed
func TestNativeImporterVerifyWarnsOnTargetWorkingDir(t *testing.T) {
	testCases := []struct {
		name       string
		workingDir string
		expectWarn bool
	}{
		{"working dir set", "/opt/jfrog/work", true},
		{"working dir not set", "", false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			importer, cmd, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				respondNative(w, http.StatusInternalServerError, "unexpected")
			})
			cmd.targetWorkingDir = testCase.workingDir
			stdout, stderr, previousLog := utilsTests.RedirectLogOutputToBuffer()
			defer log.SetLogger(previousLog)

			assert.NoError(t, importer.verify())
			logged := stdout.String() + stderr.String()
			if testCase.expectWarn {
				assert.Contains(t, logged, "[Warn]")
				assert.Contains(t, logged, "target working dir")
				assert.Contains(t, logged, testCase.workingDir)
			} else {
				assert.NotContains(t, logged, "[Warn]")
			}
			assert.Empty(t, mock.recorded(), "verify() must not call the server")
		})
	}
}

func TestNativeImporterStart(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		respondNative(w, http.StatusAccepted, `{"id":"import-42"}`)
	})
	ref, err := importer.start(bytes.NewBufferString("the-zip"))
	assert.NoError(t, err)
	assert.Equal(t, "import-42", ref)

	requests := mock.recorded()
	if assert.Len(t, requests, 1) {
		assert.Equal(t, http.MethodPost, requests[0].method)
		assert.Equal(t, nativeImportPath, requests[0].uri)
		assert.Equal(t, "the-zip", requests[0].body)
		assert.Equal(t, "application/octet-stream", requests[0].contentType)
		assert.Equal(t, "target-user", requests[0].user)
	}
}

// The import id may be a JSON string or a JSON number. Anything else is not an id.
func TestNativeImporterStartDecodesIdTolerantly(t *testing.T) {
	testCases := []struct {
		name        string
		body        string
		expectedRef string
		expectErr   bool
	}{
		{"string id", `{"id":"abc-1"}`, "abc-1", false},
		{"numeric string id", `{"id":"42"}`, "42", false},
		{"integer id", `{"id":42}`, "42", false},
		{"zero id", `{"id":0}`, "0", false},
		{"big integer id keeps all digits", `{"id":12345678901234567890}`, "12345678901234567890", false},
		{"fractional id", `{"id":1.5}`, "1.5", false},
		{"extra fields are ignored", `{"id":7,"status":"RUNNING"}`, "7", false},
		{"null id", `{"id":null}`, "", true},
		{"empty id", `{"id":""}`, "", true},
		{"missing id", `{"other":"x"}`, "", true},
		{"boolean id", `{"id":true}`, "", true},
		{"object id", `{"id":{"a":1}}`, "", true},
		{"array id", `{"id":[1]}`, "", true},
		{"bad json", `{"id":`, "", true},
		{"empty body", ``, "", true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				respondNative(w, http.StatusAccepted, testCase.body)
			})
			ref, err := importer.start(bytes.NewBufferString("zip"))
			assert.Equal(t, testCase.expectedRef, ref)
			assert.Equal(t, testCase.expectErr, err != nil, "error: %v", err)
			assert.Len(t, mock.recorded(), 1, "an accepted start must not be retried")
		})
	}
}

// A numeric id is polled as its decimal text
func TestNativeImporterPollsNumericId(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		if index == 0 {
			respondNative(w, http.StatusAccepted, `{"id":42}`)
			return
		}
		respondNative(w, http.StatusOK, "done")
	})
	action := startNativeImporter(t, importer, "42")
	stop, _, err := action()
	assert.NoError(t, err)
	assert.True(t, stop)
	requests := mock.recorded()
	if assert.Len(t, requests, 2) {
		assert.Equal(t, nativeImportPath+"/42", requests[1].uri)
	}
}

func TestNativeImporterStartInvalidAcceptedResponse(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{"empty id", `{"id":""}`},
		{"missing id", `{}`},
		{"empty body", ``},
		{"bad json", `not-json`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				respondNative(w, http.StatusAccepted, testCase.body)
			})
			ref, err := importer.start(bytes.NewBufferString("zip"))
			assert.Error(t, err)
			assert.Empty(t, ref)
			assert.Len(t, mock.recorded(), 1, "a malformed 202 must not be retried, the import may already be running")
		})
	}
}

// 403 (not admin), 423 (import already running) and any other non 5xx failure are surfaced as is and never retried
func TestNativeImporterStartFailsWithoutRetry(t *testing.T) {
	testCases := []struct {
		status int
		body   string
	}{
		{http.StatusForbidden, "admin required"},
		{http.StatusLocked, "import in progress"},
		{http.StatusNotFound, "no such endpoint"},
		{http.StatusBadRequest, "bad zip"},
		{http.StatusOK, "unexpected-200"},
	}
	for _, testCase := range testCases {
		t.Run(http.StatusText(testCase.status), func(t *testing.T) {
			importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				respondNative(w, testCase.status, testCase.body)
			})
			ref, err := importer.start(bytes.NewBufferString("zip"))
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), testCase.body, "the server error must be surfaced as is")
			}
			assert.Empty(t, ref)
			assert.Len(t, mock.recorded(), 1)
		})
	}
}

func TestNativeImporterStartRetriesOnServerError(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		if index < 2 {
			respondNative(w, http.StatusServiceUnavailable, "busy")
			return
		}
		respondNative(w, http.StatusAccepted, `{"id":"after-retry"}`)
	})
	ref, err := importer.start(bytes.NewBufferString("zip"))
	assert.NoError(t, err)
	assert.Equal(t, "after-retry", ref)
	requests := mock.recorded()
	if assert.Len(t, requests, 3) {
		// The body is sent in full on every attempt
		for _, request := range requests {
			assert.Equal(t, "zip", request.body)
		}
	}
}

func TestNativeImporterStartGivesUpAfterRetries(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		respondNative(w, http.StatusInternalServerError, "boom")
	})
	ref, err := importer.start(bytes.NewBufferString("zip"))
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "boom")
	}
	assert.Empty(t, ref)
	assert.Len(t, mock.recorded(), importStartRetries+1)
}

// A dropped connection is a transport error: start() must retry it, and succeed once the server answers
func TestNativeImporterStartRetriesOnTransportError(t *testing.T) {
	const droppedConnections = 2
	importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		if index < droppedConnections {
			dropNativeConnection(t, w)
			return
		}
		respondNative(w, http.StatusAccepted, `{"id":"after-drops"}`)
	})
	ref, err := importer.start(bytes.NewBufferString("zip"))
	assert.NoError(t, err)
	assert.Equal(t, "after-drops", ref)
	requests := mock.recorded()
	if assert.Len(t, requests, droppedConnections+1, "every dropped connection must be retried") {
		for _, request := range requests {
			assert.Equal(t, "zip", request.body, "the body is sent in full on every attempt")
		}
	}
}

// Transport errors are retried a bounded number of times, and then reported
func TestNativeImporterStartGivesUpOnTransportError(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		dropNativeConnection(t, w)
	})
	ref, err := importer.start(bytes.NewBufferString("zip"))
	assert.Error(t, err)
	assert.Empty(t, ref)
	assert.Len(t, mock.recorded(), importStartRetries+1)
}

// The Content-Type of the import must not leak into the client details shared with the polling
func TestNativeImporterStartDoesNotMutateSharedHeaders(t *testing.T) {
	var pollContentType string
	importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondNative(w, http.StatusAccepted, `{"id":"abc"}`)
			return
		}
		pollContentType = r.Header.Get("Content-Type")
		respondNative(w, http.StatusOK, "done")
	})
	ref, err := importer.start(bytes.NewBufferString("zip"))
	assert.NoError(t, err)
	if assert.NotNil(t, importer.rtDetails) {
		assert.NotContains(t, importer.rtDetails.Headers, "Content-Type")
	}

	stop, _, err := importer.pollingAction(ref)()
	assert.NoError(t, err)
	assert.True(t, stop)
	assert.Len(t, mock.recorded(), 2)
	assert.NotEqual(t, "application/octet-stream", pollContentType)
}

// Starts the importer against the mock server, and returns the polling action
func startNativeImporter(t *testing.T, importer *nativeImporter, id string) func() (bool, []byte, error) {
	importer.rtDetails = nil
	ref, err := importer.start(bytes.NewBufferString("zip"))
	assert.NoError(t, err)
	assert.Equal(t, id, ref)
	return importer.pollingAction(ref)
}

func TestNativeImporterPolling(t *testing.T) {
	statuses := []int{http.StatusAccepted, http.StatusAccepted, http.StatusOK}
	importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		if index == 0 {
			respondNative(w, http.StatusAccepted, `{"id":"my id/1"}`)
			return
		}
		respondNative(w, statuses[index-1], "log line")
	})
	action := startNativeImporter(t, importer, "my id/1")

	for i := 0; i < 2; i++ {
		stop, body, err := action()
		assert.NoError(t, err)
		assert.False(t, stop)
		assert.Nil(t, body)
	}
	stop, body, err := action()
	assert.NoError(t, err)
	assert.True(t, stop)
	assert.Equal(t, "log line", string(body))

	requests := mock.recorded()
	if assert.Len(t, requests, 4) {
		for _, request := range requests[1:] {
			assert.Equal(t, http.MethodGet, request.method)
			assert.Equal(t, nativeImportPath+"/my%20id%2F1", request.uri, "the id must be path-escaped")
			assert.Equal(t, "target-user", request.user)
		}
	}
}

func TestNativeImporterPollingStopsWithError(t *testing.T) {
	testCases := []struct {
		name          string
		status        int
		body          string
		expectedInErr []string
	}{
		{"unknown or expired id", http.StatusNotFound, "no such import", []string{"404"}},
		{"server error with body", http.StatusInternalServerError, "import crashed: details", []string{"500", "import crashed: details"}},
		{"unexpected status", http.StatusTeapot, "teapot", []string{"418"}},
		{"unexpected 200-range status", http.StatusNoContent, "", []string{"204"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
				if index == 0 {
					respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
					return
				}
				respondNative(w, testCase.status, testCase.body)
			})
			action := startNativeImporter(t, importer, "i1")
			stop, body, err := action()
			if assert.Error(t, err) {
				for _, expected := range testCase.expectedInErr {
					assert.Contains(t, err.Error(), expected)
				}
			}
			assert.True(t, stop, "polling must stop on %d", testCase.status)
			assert.Nil(t, body)
			assert.Len(t, mock.recorded(), 2)
		})
	}
}

// 401 and 403 mean that the target user was replaced by the import: the credentials are switched to the source ones, once
func TestNativeImporterPollingSwitchesCredentialsOnce(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			importer, cmd, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
				switch index {
				case 0:
					respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
				case 1:
					respondNative(w, status, "user deleted")
				case 2:
					respondNative(w, http.StatusAccepted, "")
				default:
					respondNative(w, http.StatusOK, "done")
				}
			})
			action := startNativeImporter(t, importer, "i1")
			assert.False(t, importer.credentialsSwitched)

			stop, body, err := action()
			assert.NoError(t, err)
			assert.False(t, stop)
			assert.Nil(t, body)
			assert.True(t, importer.credentialsSwitched)
			assert.Equal(t, "source-user", cmd.TargetServerDetails.GetUser())

			stop, _, err = action()
			assert.NoError(t, err)
			assert.False(t, stop)
			stop, body, err = action()
			assert.NoError(t, err)
			assert.True(t, stop)
			assert.Equal(t, "done", string(body))

			var users []string
			for _, request := range mock.recorded() {
				users = append(users, request.user)
			}
			assert.Equal(t, []string{"target-user", "target-user", "source-user", "source-user"}, users)
		})
	}
}

// A second 401/403, after the switch, means that the source credentials are not accepted either: stop instead of polling forever
func TestNativeImporterPollingSecondUnauthorizedFails(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		if index == 0 {
			respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
			return
		}
		respondNative(w, http.StatusUnauthorized, "nope")
	})
	action := startNativeImporter(t, importer, "i1")

	stop, _, err := action()
	assert.NoError(t, err)
	assert.False(t, stop)

	stop, body, err := action()
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "401")
	}
	assert.True(t, stop)
	assert.Nil(t, body)
	assert.Len(t, mock.recorded(), 3)
}

// A failure to rebuild the clients with the source credentials stops the polling with the error
func TestNativeImporterPollingFailedCredentialSwitchStops(t *testing.T) {
	importer, cmd, _ := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		if index == 0 {
			respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
			return
		}
		respondNative(w, http.StatusUnauthorized, "")
	})
	action := startNativeImporter(t, importer, "i1")
	cmd.TargetServerDetails.ClientCertPath = "/non/existing/client.crt"
	cmd.TargetServerDetails.ClientCertKeyPath = "/non/existing/client.key"

	stop, body, err := action()
	assert.Error(t, err)
	assert.True(t, stop)
	assert.Nil(t, body)
}

// Across the whole lifecycle (verify, start with a retry, polling with a credentials switch) the only requests that reach
// the target are the ones of the config transfer API. The config-import user plugin endpoint is never used.
func TestNativeImporterUsesImportEndpointOnly(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		switch index {
		case 0:
			respondNative(w, http.StatusServiceUnavailable, "busy")
		case 1:
			respondNative(w, http.StatusAccepted, `{"id":"abc"}`)
		case 2:
			respondNative(w, http.StatusAccepted, "")
		case 3:
			respondNative(w, http.StatusUnauthorized, "user replaced")
		default:
			respondNative(w, http.StatusOK, "done")
		}
	})
	assert.NoError(t, importer.verify())
	ref, err := importer.start(bytes.NewBufferString("zip"))
	assert.NoError(t, err)
	action := importer.pollingAction(ref)
	for _, expectedStop := range []bool{false, false, true} {
		stop, _, err := action()
		assert.NoError(t, err)
		assert.Equal(t, expectedStop, stop)
	}

	requests := mock.recorded()
	if assert.Len(t, requests, 5) {
		for _, request := range requests[:2] {
			assert.Equal(t, http.MethodPost, request.method)
			assert.Equal(t, nativeImportPath, request.uri)
		}
		for _, request := range requests[2:] {
			assert.Equal(t, http.MethodGet, request.method)
			assert.Equal(t, nativeImportPath+"/abc", request.uri)
		}
	}
	for _, request := range requests {
		assert.NotContains(t, request.uri, "plugins")
	}
}

// The HTTP client retries 5xx by itself, and then returns the last response together with a retry-timeout error.
// The server error must be surfaced as is, and not be replaced by the timeout error.
func TestNativeImporterServerErrorBodySurvivesClientRetries(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		importer, cmd, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			respondNative(w, http.StatusInternalServerError, "start crashed: details")
		})
		setTargetClientRetries(t, cmd, -1)
		ref, err := importer.start(bytes.NewBufferString("zip"))
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "500")
			assert.Contains(t, err.Error(), "start crashed: details")
		}
		assert.Empty(t, ref)
		assert.GreaterOrEqual(t, len(mock.recorded()), importStartRetries+1)
	})
	t.Run("polling", func(t *testing.T) {
		importer, cmd, _ := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
			if index == 0 {
				respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
				return
			}
			respondNative(w, http.StatusInternalServerError, "import crashed: details")
		})
		action := startNativeImporter(t, importer, "i1")
		setTargetClientRetries(t, cmd, -1)
		stop, body, err := action()
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "500")
			assert.Contains(t, err.Error(), "import crashed: details")
		}
		assert.True(t, stop)
		assert.Nil(t, body)
	})
}

// A transport error on the status request is transient (a restarting server, a network blip): the polling keeps going,
// and completes once the server answers again. The polling request itself is not retried.
func TestNativeImporterPollingTransportErrorContinues(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		switch index {
		case 0:
			respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
		case 1, 2:
			dropNativeConnection(t, w)
		default:
			respondNative(w, http.StatusOK, "done")
		}
	})
	action := startNativeImporter(t, importer, "i1")
	for i := 0; i < 2; i++ {
		stop, body, err := action()
		assert.NoError(t, err)
		assert.False(t, stop, "polling must continue on a transport error")
		assert.Nil(t, body)
	}
	stop, body, err := action()
	assert.NoError(t, err)
	assert.True(t, stop)
	assert.Equal(t, "done", string(body))
	assert.Len(t, mock.recorded(), 4, "a polling request is not retried by the importer")
}

// 502, 503 and 504 are transient (a gateway or a restarting server): the polling keeps going and completes on 200.
// This holds with the default HTTP client retries too, which return the last response together with a retry-timeout error.
func TestNativeImporterPollingTransientStatusContinues(t *testing.T) {
	statuses := []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	for _, clientRetries := range []int{0, -1} {
		for _, status := range statuses {
			t.Run(fmt.Sprintf("%d with client retries %d", status, clientRetries), func(t *testing.T) {
				importer, cmd, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
					switch {
					case index == 0:
						respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
					case index < 5 && clientRetries != 0:
						// The default HTTP client makes 4 attempts at the first polling tick
						respondNative(w, status, "temporarily unavailable")
					case index == 1 && clientRetries == 0:
						respondNative(w, status, "temporarily unavailable")
					default:
						respondNative(w, http.StatusOK, "done")
					}
				})
				action := startNativeImporter(t, importer, "i1")
				setTargetClientRetries(t, cmd, clientRetries)

				stop, body, err := action()
				assert.NoError(t, err)
				assert.False(t, stop, "polling must continue on %d", status)
				assert.Nil(t, body)

				stop, body, err = action()
				assert.NoError(t, err)
				assert.True(t, stop)
				assert.Equal(t, "done", string(body))
				assert.GreaterOrEqual(t, len(mock.recorded()), 3)
			})
		}
	}
}

// Transient failures do not make the polling unbounded: the polling executor still gives up when its timeout elapses.
func TestNativeImporterPollingPersistentTransientErrorIsBoundedByExecutor(t *testing.T) {
	importer, _, _ := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
		if index == 0 {
			respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
			return
		}
		respondNative(w, http.StatusServiceUnavailable, "down")
	})
	action := startNativeImporter(t, importer, "i1")
	executor := &httputils.PollingExecutor{Timeout: time.Second, PollingInterval: time.Second, PollingAction: action}
	_, err := executor.Execute()
	assert.Error(t, err, "a server that stays unavailable must end the polling at the executor timeout")
}

// 401/403 switch the credentials, and a transient error right after the switch is not fatal either
func TestNativeImporterPollingTransientErrorAfterCredentialSwitchContinues(t *testing.T) {
	transientResponders := map[string]func(t *testing.T, w http.ResponseWriter){
		"transport error": func(t *testing.T, w http.ResponseWriter) { dropNativeConnection(t, w) },
		"503": func(_ *testing.T, w http.ResponseWriter) {
			respondNative(w, http.StatusServiceUnavailable, "reloading")
		},
	}
	for name, transient := range transientResponders {
		t.Run(name, func(t *testing.T) {
			importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, _ *http.Request) {
				switch {
				case index == 0:
					respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
				case index == 1:
					respondNative(w, http.StatusUnauthorized, "user replaced")
				case index < 6:
					// The switch rebuilds the clients with the default HTTP client retries, which make 4 attempts
					transient(t, w)
				default:
					respondNative(w, http.StatusOK, "done")
				}
			})
			action := startNativeImporter(t, importer, "i1")
			for i := 0; i < 2; i++ {
				stop, _, err := action()
				assert.NoError(t, err)
				assert.False(t, stop)
			}
			assert.True(t, importer.credentialsSwitched)
			stop, body, err := action()
			assert.NoError(t, err)
			assert.True(t, stop)
			assert.Equal(t, "done", string(body))

			for i, request := range mock.recorded() {
				if i < 2 {
					assert.Equal(t, "target-user", request.user)
				} else {
					assert.Equal(t, "source-user", request.user)
				}
			}
		})
	}
}

// The polling action creates the client details by itself when start() did not run on this importer
func TestNativeImporterPollingCreatesClientDetailsLazily(t *testing.T) {
	importer, _, mock := newNativeImporterWithMock(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		respondNative(w, http.StatusOK, "done")
	})
	assert.Nil(t, importer.rtDetails)
	stop, body, err := importer.pollingAction("i1")()
	assert.NoError(t, err)
	assert.True(t, stop)
	assert.Equal(t, "done", string(body))
	if assert.NotNil(t, importer.rtDetails) {
		assert.NotContains(t, importer.rtDetails.Headers, "Content-Type")
	}
	requests := mock.recorded()
	if assert.Len(t, requests, 1) {
		assert.Equal(t, nativeImportPath+"/i1", requests[0].uri)
		assert.Equal(t, "target-user", requests[0].user)
	}
}

// A responder that accepts the request and never answers it, until the client gives up or the test ends.
// The test server waits for its handlers on Close, so it must be released before that.
func hangNativeRequest(r *http.Request, release <-chan struct{}) {
	select {
	case <-r.Context().Done():
	case <-release:
	}
}

// Returns a channel that releases the hanging responders when the test ends. Must be called after the mock is created, so that
// it is released before the mock server is closed (cleanups run last-in first-out).
func releaseHangingOnCleanup(t *testing.T) chan struct{} {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return release
}

// Runs the function, and fails the test instead of hanging it when it does not return within the limit
func runNativeWithin(t *testing.T, limit time.Duration, name string, f func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s: a request that is never answered hangs the importer", name, limit)
	}
}

const hangTestGuard = 10 * time.Second

// A poll request that is accepted and never answered must not hang the importer. It fails like a transport error, so the polling goes on.
func TestNativeImporterPollingRequestTimesOut(t *testing.T) {
	var release <-chan struct{}
	importer, cmd, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, r *http.Request) {
		switch index {
		case 0:
			respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
		case 1:
			hangNativeRequest(r, release)
		default:
			respondNative(w, http.StatusOK, "done")
		}
	})
	release = releaseHangingOnCleanup(t)
	importer.pollRequestTimeout = 200 * time.Millisecond
	action := startNativeImporter(t, importer, "i1")

	runNativeWithin(t, hangTestGuard, "the poll", func() {
		stop, body, err := action()
		assert.NoError(t, err, "a timed-out poll is transient")
		assert.False(t, stop)
		assert.Nil(t, body)
	})
	stop, body, err := action()
	assert.NoError(t, err)
	assert.True(t, stop)
	assert.Equal(t, "done", string(body))
	assert.Len(t, mock.recorded(), 3)
	// The limit belongs to the native importer only: the shared target manager keeps its unlimited requests
	assert.Zero(t, cmd.TargetArtifactoryManager.GetConfig().GetOverallRequestTimeout())
}

// The bound must also hold on the service manager that switchTargetToSourceCredentials rebuilds
func TestNativeImporterPollingRequestTimeoutSurvivesCredentialSwitch(t *testing.T) {
	var release <-chan struct{}
	importer, cmd, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, r *http.Request) {
		switch {
		case index == 0:
			respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
		case index == 1:
			respondNative(w, http.StatusUnauthorized, "user replaced")
		case index == 2:
			respondNative(w, http.StatusAccepted, "")
		case index < 7:
			// The rebuilt manager retries with the default client retries: 4 attempts, each of them hangs
			hangNativeRequest(r, release)
		default:
			respondNative(w, http.StatusOK, "done")
		}
	})
	release = releaseHangingOnCleanup(t)
	importer.pollRequestTimeout = 150 * time.Millisecond
	action := startNativeImporter(t, importer, "i1")

	for i := 0; i < 2; i++ {
		stop, _, err := action()
		assert.NoError(t, err)
		assert.False(t, stop)
	}
	assert.True(t, importer.credentialsSwitched)
	runNativeWithin(t, hangTestGuard, "the poll after the credentials switch", func() {
		stop, _, err := action()
		assert.NoError(t, err)
		assert.False(t, stop)
	})
	stop, body, err := action()
	assert.NoError(t, err)
	assert.True(t, stop)
	assert.Equal(t, "done", string(body))
	for i, request := range mock.recorded() {
		if i >= 2 {
			assert.Equal(t, "source-user", request.user)
		}
	}
	assert.Zero(t, cmd.TargetArtifactoryManager.GetConfig().GetOverallRequestTimeout())
}

// A polling that never gets an answer ends at the executor timeout instead of hanging
func TestNativeImporterPollingNeverAnsweredIsBoundedByExecutor(t *testing.T) {
	var release <-chan struct{}
	importer, _, _ := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, r *http.Request) {
		if index == 0 {
			respondNative(w, http.StatusAccepted, `{"id":"i1"}`)
			return
		}
		hangNativeRequest(r, release)
	})
	release = releaseHangingOnCleanup(t)
	importer.pollRequestTimeout = 100 * time.Millisecond
	action := startNativeImporter(t, importer, "i1")
	executor := &httputils.PollingExecutor{Timeout: time.Second, PollingInterval: 200 * time.Millisecond, PollingAction: action}
	runNativeWithin(t, hangTestGuard, "the polling", func() {
		_, err := executor.Execute()
		assert.Error(t, err)
	})
}

// A start request that is accepted and never answered times out, and is retried like any other transport error
func TestNativeImporterStartRequestTimesOut(t *testing.T) {
	var release <-chan struct{}
	importer, _, mock := newNativeImporterWithMock(t, func(index int, w http.ResponseWriter, r *http.Request) {
		if index == 0 {
			hangNativeRequest(r, release)
			return
		}
		respondNative(w, http.StatusAccepted, `{"id":"after-timeout"}`)
	})
	release = releaseHangingOnCleanup(t)
	importer.startBaseTimeout = 200 * time.Millisecond
	runNativeWithin(t, hangTestGuard, "start()", func() {
		ref, err := importer.start(bytes.NewBufferString("zip"))
		assert.NoError(t, err)
		assert.Equal(t, "after-timeout", ref)
	})
	assert.Len(t, mock.recorded(), 2)
}

// A start that is never answered gives up after the retries
func TestNativeImporterStartNeverAnsweredGivesUp(t *testing.T) {
	var release <-chan struct{}
	importer, _, mock := newNativeImporterWithMock(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
		hangNativeRequest(r, release)
	})
	release = releaseHangingOnCleanup(t)
	importer.startBaseTimeout = 100 * time.Millisecond
	runNativeWithin(t, hangTestGuard, "start()", func() {
		ref, err := importer.start(bytes.NewBufferString("zip"))
		assert.Error(t, err)
		assert.Empty(t, ref)
	})
	assert.Len(t, mock.recorded(), importStartRetries+1)
}

// The upload of a big ZIP must get more time than the base: the limit grows with the size
func TestNativeImporterStartTimeoutGrowsWithZipSize(t *testing.T) {
	cmd := createTransferConfigCommand(t, &config.ServerDetails{Url: "dummy-url"}, &config.ServerDetails{Url: "dummy-url"})
	importer := newNativeImporter(cmd)
	assert.Equal(t, nativeStartBaseTimeout, importer.startTimeout(0))
	assert.Equal(t, nativeStartBaseTimeout+time.Second, importer.startTimeout(nativeStartMinUploadBytesPerSec))
	// 100 MiB at the minimal upload speed
	hundredMiB := 100 * 1024 * 1024
	expected := nativeStartBaseTimeout + time.Duration(hundredMiB/nativeStartMinUploadBytesPerSec)*time.Second
	assert.Equal(t, expected, importer.startTimeout(hundredMiB))
	assert.Greater(t, importer.startTimeout(hundredMiB), importer.startTimeout(1024))

	// The base is a field, so that tests can shorten it
	importer.startBaseTimeout = time.Millisecond
	assert.Equal(t, time.Millisecond, importer.startTimeout(0))
}
