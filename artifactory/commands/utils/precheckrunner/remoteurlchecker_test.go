package precheckrunner

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	commandsUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	commonTests "github.com/jfrog/jfrog-cli-core/v2/common/tests"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/artifactory/services"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var remoteUrlCheckerTestDir = filepath.Join("testdata", "remoteurlchecker")

func TestRemoteUrlRequest(t *testing.T) {
	// Read mock "GET artifactory/api/repository/nuget-remote" response
	nugetRepo, err := fileutils.ReadFile(filepath.Join(remoteUrlCheckerTestDir, "nuget_repo.json"))
	assert.NoError(t, err)

	// Create RemoteRepositoryCheck test object
	var remoteRepositories []interface{}
	var remoteRepository services.RemoteRepositoryBaseParams
	assert.NoError(t, json.Unmarshal(nugetRepo, &remoteRepository))
	remoteRepositoryCheck := NewRemoteRepositoryCheck(nil, append(remoteRepositories, remoteRepository))

	// Run and verify createRemoteUrlRequest
	remoteUrlRequest, err := remoteRepositoryCheck.createRemoteUrlRequest()
	assert.NoError(t, err)
	assert.Len(t, remoteUrlRequest, 1)
	assert.Equal(t, "nuget-remote", remoteUrlRequest[0].Key)
	assert.Equal(t, "https://www.nuget.org/", remoteUrlRequest[0].Url)
	assert.Equal(t, "nuget", remoteUrlRequest[0].RepoType)
	assert.Equal(t, "admin", remoteUrlRequest[0].Username)
	assert.Equal(t, "password", remoteUrlRequest[0].Password)
}

func TestEmptyRemoteUrlRequest(t *testing.T) {
	// Create RemoteRepositoryCheck test object
	remoteRepositoryCheck := NewRemoteRepositoryCheck(nil, []interface{}{})

	// Run and verify createRemoteUrlRequest
	remoteUrlRequest, err := remoteRepositoryCheck.createRemoteUrlRequest()
	assert.NoError(t, err)
	assert.Empty(t, remoteUrlRequest)
}

const (
	nativeCheckStartPath = "/api/configTransfer/remoteRepositoriesCheck"
	nativeCheckId        = "7b1c6a52-3f3e-4b1e-9a43-0c5c2d1f8e90"
	nativeCheckPollPath  = nativeCheckStartPath + "/" + nativeCheckId
	pluginCheckStartPath = "/" + commandsUtils.PluginsExecuteRestApi + "remoteRepositoriesCheck"
	pluginCheckPollPath  = "/" + commandsUtils.PluginsExecuteRestApi + "remoteRepositoriesCheckStatus"
)

// A request that the mock Artifactory received
type checkRequest struct {
	method string
	uri    string
	body   string
}

// A mock target Artifactory that records the requests and answers each with the responder. The index is per method and path.
type checkMock struct {
	mu       sync.Mutex
	requests []checkRequest
}

func (m *checkMock) all() []checkRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]checkRequest(nil), m.requests...)
}

// The requests of the given method and path
func (m *checkMock) requestsTo(method, uri string) (matching []checkRequest) {
	for _, request := range m.all() {
		if request.method == method && request.uri == uri {
			matching = append(matching, request)
		}
	}
	return
}

func respondCheck(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// Creates a remote repository check on top of a mock Artifactory. The responder gets the 0-based index of the request among
// the requests with the same method and path. The check does not wait between the retries and the polls.
func newCheckWithMock(t *testing.T, native bool, responder func(index int, w http.ResponseWriter, r *http.Request)) (*RemoteRepositoryCheck, RunArguments, *checkMock) {
	// The CSV summary of the inaccessible repositories is written to the JFrog home
	t.Setenv(coreutils.HomeDir, t.TempDir())
	mock := &checkMock{}
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		mock.mu.Lock()
		index := 0
		for _, previous := range mock.requests {
			if previous.method == r.Method && previous.uri == r.RequestURI {
				index++
			}
		}
		mock.requests = append(mock.requests, checkRequest{method: r.Method, uri: r.RequestURI, body: string(body)})
		mock.mu.Unlock()
		responder(index, w, r)
	})
	t.Cleanup(testServer.Close)
	// The HTTP client has its own retries on 5xx, which would multiply the requests counted by the tests
	manager, err := utils.CreateServiceManager(serverDetails, 0, 0, false)
	require.NoError(t, err)
	remoteRepositories := []interface{}{
		// As in the command, the remote repositories are not necessarily typed: they can be maps
		map[string]interface{}{"key": "remote-1", "url": "https://example.invalid/one", "packageType": "generic"},
		map[string]interface{}{"key": "remote-2", "url": "https://example.invalid/two", "packageType": "maven"},
	}
	var check *RemoteRepositoryCheck
	if native {
		check = NewNativeRemoteRepositoryCheck(&manager, remoteRepositories)
	} else {
		check = NewRemoteRepositoryCheck(&manager, remoteRepositories)
	}
	check.retryIntervalMilliSecs = 0
	check.pollingInterval = time.Millisecond
	check.pollingTimeout = 5 * time.Second
	return check, RunArguments{Context: context.Background(), ServerDetails: serverDetails}, mock
}

func assertNoPluginRequests(t *testing.T, mock *checkMock) {
	for _, request := range mock.all() {
		assert.NotContains(t, request.uri, commandsUtils.PluginsExecuteRestApi, "unexpected plugin request: %s %s", request.method, request.uri)
	}
}

const (
	checkProgressBody = `{"status":"running","checked_repositories":1,"total_repositories":2}`
	checkAllReachable = `{"status":"completed","checked_repositories":2,"total_repositories":2}`
	checkInaccessible = `{"status":"completed","checked_repositories":2,"total_repositories":2,"inaccessible_repositories":[` +
		`{"repo_key":"remote-2","status_code":404,"reason":"Not Found","url":"https://example.invalid/two"}]}`
)

// Runs the check with a bound on its time, so that a regression that keeps polling fails fast and does not hang
func executeCheckWithin(t *testing.T, check *RemoteRepositoryCheck, args RunArguments) (passed bool, err error) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		passed, err = check.ExecuteCheck(args)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the remote repositories check did not finish")
	}
	return
}

func TestNativeRemoteCheckPollsProgressUntilAllReachable(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.RequestURI == nativeCheckStartPath:
			respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
		case r.Method == http.MethodGet && r.RequestURI == nativeCheckPollPath && index < 2:
			respondCheck(w, http.StatusAccepted, checkProgressBody)
		case r.Method == http.MethodGet && r.RequestURI == nativeCheckPollPath:
			respondCheck(w, http.StatusOK, checkAllReachable)
		default:
			assert.Fail(t, "Unexpected request: "+r.Method+" "+r.RequestURI)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), 1)
	assert.Len(t, mock.requestsTo(http.MethodGet, nativeCheckPollPath), 3, "two progress (202) polls and the final (200) one are expected")
	assertNoPluginRequests(t, mock)
}

func TestNativeRemoteCheckStartSendsTheRepositoriesRequest(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
			return
		}
		respondCheck(w, http.StatusOK, checkAllReachable)
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	starts := mock.requestsTo(http.MethodPost, nativeCheckStartPath)
	require.Len(t, starts, 1)
	var sent []remoteRepoSettings
	require.NoError(t, json.Unmarshal([]byte(starts[0].body), &sent))
	assert.Equal(t, []remoteRepoSettings{
		{Key: "remote-1", Url: "https://example.invalid/one", RepoType: "generic"},
		{Key: "remote-2", Url: "https://example.invalid/two", RepoType: "maven"},
	}, sent, "the request body is the same as the plugin one")
}

func TestNativeRemoteCheckInaccessibleRepositories(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
			return
		}
		respondCheck(w, http.StatusOK, checkInaccessible)
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err, "inaccessible repositories fail the check, but are not an error")
	assert.False(t, passed)
	assertNoPluginRequests(t, mock)
	csvFiles := findInaccessibleRepositoriesCsvFiles(t)
	if assert.Len(t, csvFiles, 1, "the summary CSV is expected, as in the plugin path") {
		content, err := os.ReadFile(csvFiles[0])
		assert.NoError(t, err)
		assert.Contains(t, string(content), "remote-2")
		assert.Contains(t, string(content), "https://example.invalid/two")
	}
}

func TestNativeRemoteCheckPollingFailsFastOnErrorStatus(t *testing.T) {
	testCases := []struct {
		name    string
		status  int
		body    string
		message string
	}{
		{"unknown check id", http.StatusNotFound, `{"errors":[{"status":404,"message":"unknown check id"}]}`, "unknown check id"},
		{"check failed", http.StatusInternalServerError, `{"errors":[{"status":500,"message":"check failed"}]}`, "check failed"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
					return
				}
				respondCheck(w, testCase.status, testCase.body)
			})
			// A regression that keeps polling would run until this timeout
			check.pollingTimeout = 3 * time.Second
			check.pollingInterval = 50 * time.Millisecond

			started := time.Now()
			passed, err := executeCheckWithin(t, check, args)
			assert.False(t, passed)
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), strconv.Itoa(testCase.status), "the status is reported")
				assert.Contains(t, err.Error(), testCase.message, "the server message is reported")
			}
			assert.Len(t, mock.requestsTo(http.MethodGet, nativeCheckPollPath), 1, "no polling is expected after the error")
			assert.Less(t, time.Since(started), 2*time.Second)
			assertNoPluginRequests(t, mock)
		})
	}
}

func TestNativeRemoteCheckPollingServerErrorBodySurvivesClientRetries(t *testing.T) {
	// With the HTTP client retries on, a 500 is returned by the client together with a retry-timeout error. The check failure
	// that the server reported must not be hidden behind it.
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
			return
		}
		respondCheck(w, http.StatusInternalServerError, `{"errors":[{"status":500,"message":"check failed"}]}`)
	})
	manager, err := utils.CreateServiceManager(args.ServerDetails, 2, 0, false)
	require.NoError(t, err)
	check.targetServicesManager = &manager

	passed, err := executeCheckWithin(t, check, args)
	assert.False(t, passed)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "500")
		assert.Contains(t, err.Error(), "check failed")
	}
	assert.NotEmpty(t, mock.requestsTo(http.MethodGet, nativeCheckPollPath))
}

func TestNativeRemoteCheckStartRetriesOnServerError(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && index < 2:
			respondCheck(w, http.StatusInternalServerError, "try later")
		case r.Method == http.MethodPost:
			respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
		default:
			respondCheck(w, http.StatusOK, checkAllReachable)
		}
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), 3)
}

func TestNativeRemoteCheckStartGivesUpAfterRetries(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		respondCheck(w, http.StatusServiceUnavailable, "unavailable")
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.False(t, passed)
	assert.Error(t, err)
	assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), remoteUrlCheckRetries+1)
	assert.Empty(t, mock.requestsTo(http.MethodGet, nativeCheckPollPath), "nothing to poll without a started check")
}

func TestNativeRemoteCheckStartDoesNotRetryOnNotFound(t *testing.T) {
	// Unlike the plugin, 404 is a real answer of the native API and not a plugin flakiness
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		respondCheck(w, http.StatusNotFound, "no such endpoint")
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.False(t, passed)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "404")
	}
	assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), 1)
	assertNoPluginRequests(t, mock)
}

func TestNativeRemoteCheckStartFailsOnUnexpectedStatus(t *testing.T) {
	// The plugin answers the start with 200. The native API answers it with 202 only
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
				respondCheck(w, status, `{"id":"`+nativeCheckId+`"}`)
			})

			passed, err := executeCheckWithin(t, check, args)
			assert.False(t, passed)
			assert.Error(t, err)
			assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), 1, "not retried")
			assert.Empty(t, mock.requestsTo(http.MethodGet, nativeCheckPollPath))
		})
	}
}

func TestNativeRemoteCheckStartInvalidResponse(t *testing.T) {
	for name, body := range map[string]string{"not json": "<html>", "no id": `{}`, "empty id": `{"id":""}`} {
		t.Run(name, func(t *testing.T) {
			check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
				respondCheck(w, http.StatusAccepted, body)
			})

			passed, err := executeCheckWithin(t, check, args)
			assert.False(t, passed)
			assert.Error(t, err)
			assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), 1, "the check was accepted, so a start is not retried")
			assert.Len(t, mock.all(), 1, "nothing but the start is expected, and in particular no poll")
		})
	}
}

func TestNativeRemoteCheckMakesNoPluginRequests(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.RequestURI == nativeCheckStartPath:
			respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
		case r.Method == http.MethodGet && r.RequestURI == nativeCheckPollPath:
			respondCheck(w, http.StatusOK, checkAllReachable)
		default:
			// A plugin request gets the plugin 404, and would end up as an error
			respondCheck(w, http.StatusNotFound, "no such endpoint")
		}
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.all(), 2)
	assertNoPluginRequests(t, mock)
}

func TestPluginRemoteCheckSuccess(t *testing.T) {
	check, args, mock := newCheckWithMock(t, false, func(index int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.RequestURI == pluginCheckStartPath:
			respondCheck(w, http.StatusOK, `{"status":"running","total_repositories":2}`)
		case r.Method == http.MethodGet && r.RequestURI == pluginCheckPollPath && index == 0:
			respondCheck(w, http.StatusAccepted, checkProgressBody)
		case r.Method == http.MethodGet && r.RequestURI == pluginCheckPollPath:
			respondCheck(w, http.StatusOK, checkAllReachable)
		default:
			assert.Fail(t, "Unexpected request: "+r.Method+" "+r.RequestURI)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.requestsTo(http.MethodPost, pluginCheckStartPath), 1)
	assert.Len(t, mock.requestsTo(http.MethodGet, pluginCheckPollPath), 2)
	for _, request := range mock.all() {
		assert.NotContains(t, request.uri, "configTransfer", "the plugin check must not use the native API: %s", request.uri)
	}
}

func TestPluginRemoteCheckInaccessibleRepositories(t *testing.T) {
	check, args, _ := newCheckWithMock(t, false, func(index int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondCheck(w, http.StatusOK, `{"status":"running","total_repositories":2}`)
			return
		}
		respondCheck(w, http.StatusOK, checkInaccessible)
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.False(t, passed)
}

func TestPluginRemoteCheckStartRetriesOnNotFound(t *testing.T) {
	// The plugin sometimes answers 404 on the start, although it is installed
	check, args, mock := newCheckWithMock(t, false, func(index int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && index == 0:
			respondCheck(w, http.StatusNotFound, "not found")
		case r.Method == http.MethodPost:
			respondCheck(w, http.StatusOK, `{"status":"running","total_repositories":2}`)
		default:
			respondCheck(w, http.StatusOK, checkAllReachable)
		}
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.requestsTo(http.MethodPost, pluginCheckStartPath), 2)
}

func TestPluginRemoteCheckStartDoesNotAcceptAccepted(t *testing.T) {
	// The plugin start contract is 200. 202 is the native one, and must not be mixed up with it
	check, args, mock := newCheckWithMock(t, false, func(index int, w http.ResponseWriter, r *http.Request) {
		respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.False(t, passed)
	assert.Error(t, err)
	assert.Len(t, mock.requestsTo(http.MethodPost, pluginCheckStartPath), remoteUrlCheckRetries+1)
}

func TestPluginRemoteCheckPollingKeepsPollingOnOtherStatuses(t *testing.T) {
	// The plugin polling treats any status other than 200 and 202 as "not done yet". Kept as is for the plugin path.
	check, args, mock := newCheckWithMock(t, false, func(index int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			respondCheck(w, http.StatusOK, `{"status":"running","total_repositories":2}`)
		case index < 2:
			respondCheck(w, http.StatusNotFound, "not found")
		default:
			respondCheck(w, http.StatusOK, checkAllReachable)
		}
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.requestsTo(http.MethodGet, pluginCheckPollPath), 3)
}

// The inaccessible repositories CSV summaries that were written to the JFrog home
func findInaccessibleRepositoriesCsvFiles(t *testing.T) (csvFiles []string) {
	assert.NoError(t, filepath.WalkDir(os.Getenv(coreutils.HomeDir), func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasPrefix(entry.Name(), "inaccessible-repositories-") && strings.HasSuffix(entry.Name(), ".csv") {
			csvFiles = append(csvFiles, path)
		}
		return err
	}))
	return
}

func TestNativeRemoteCheckEscapesTheCheckIdInThePollUrl(t *testing.T) {
	const unsafeId = "a/b c"
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondCheck(w, http.StatusAccepted, `{"id":"`+unsafeId+`"}`)
			return
		}
		respondCheck(w, http.StatusOK, checkAllReachable)
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.requestsTo(http.MethodGet, nativeCheckStartPath+"/a%2Fb%20c"), 1, "%v", mock.all())
}

func TestNativeRemoteCheckAcceptsNumericId(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondCheck(w, http.StatusAccepted, `{"id":12345678901234567890}`)
			return
		}
		respondCheck(w, http.StatusOK, checkAllReachable)
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.requestsTo(http.MethodGet, nativeCheckStartPath+"/12345678901234567890"), 1, "%v", mock.all())
}

func TestNativeRemoteCheckStartRejectsNonScalarId(t *testing.T) {
	for name, body := range map[string]string{"object": `{"id":{"a":1}}`, "array": `{"id":[1]}`, "null": `{"id":null}`} {
		t.Run(name, func(t *testing.T) {
			check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
				respondCheck(w, http.StatusAccepted, body)
			})
			passed, err := executeCheckWithin(t, check, args)
			assert.False(t, passed)
			assert.Error(t, err)
			assert.Len(t, mock.all(), 1)
		})
	}
}

// Drops the connection of the current request without answering it, as a network failure would
func dropCheckConnection(t *testing.T, w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !assert.True(t, ok, "the response writer must support hijacking") {
		return
	}
	conn, _, err := hijacker.Hijack()
	if assert.NoError(t, err) {
		assert.NoError(t, conn.Close())
	}
}

func TestNativeRemoteCheckStartRetriesOnTransportError(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && index == 0:
			dropCheckConnection(t, w)
		case r.Method == http.MethodPost:
			respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
		default:
			respondCheck(w, http.StatusOK, checkAllReachable)
		}
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.NoError(t, err)
	assert.True(t, passed)
	assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), 2)
}

func TestNativeRemoteCheckStartGivesUpOnTransportError(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		dropCheckConnection(t, w)
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.False(t, passed)
	assert.Error(t, err)
	assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), remoteUrlCheckRetries+1)
}

func TestNativeRemoteCheckPollingStopsOnTransportError(t *testing.T) {
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondCheck(w, http.StatusAccepted, `{"id":"`+nativeCheckId+`"}`)
			return
		}
		dropCheckConnection(t, w)
	})

	passed, err := executeCheckWithin(t, check, args)
	assert.False(t, passed)
	assert.Error(t, err)
	assert.Len(t, mock.requestsTo(http.MethodGet, nativeCheckPollPath), 1, "the polling does not continue after a transport error")
}

func TestNativeRemoteCheckStartServerErrorBodySurvivesClientRetries(t *testing.T) {
	check, args, _ := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		respondCheck(w, http.StatusServiceUnavailable, "the check service is down")
	})
	manager, err := utils.CreateServiceManager(args.ServerDetails, 2, 0, false)
	require.NoError(t, err)
	check.targetServicesManager = &manager

	passed, err := executeCheckWithin(t, check, args)
	assert.False(t, passed)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "503")
		assert.Contains(t, err.Error(), "the check service is down")
	}
}

func TestNativeRemoteCheckStartStopsRetryingWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	check, args, mock := newCheckWithMock(t, true, func(index int, w http.ResponseWriter, r *http.Request) {
		cancel()
		respondCheck(w, http.StatusInternalServerError, "try later")
	})
	args.Context = ctx

	passed, err := executeCheckWithin(t, check, args)
	assert.False(t, passed)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Len(t, mock.requestsTo(http.MethodPost, nativeCheckStartPath), 1, "no retry is expected after the cancellation")
}

func TestCheckProgress(t *testing.T) {
	testCases := []struct {
		name          string
		checked       uint
		currentBarVal int64
		expectedDelta int64
	}{
		{"first update", 1, 0, 1},
		{"advance", 5, 2, 3},
		{"no change", 4, 4, 0},
		{"server count behind the bar", 2, 5, -3},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			delta, err := checkProgressDelta(testCase.checked, testCase.currentBarVal)
			assert.NoError(t, err)
			assert.Equal(t, testCase.expectedDelta, delta)
		})
	}

	t.Run("overflow", func(t *testing.T) {
		_, err := checkProgressDelta(math.MaxUint64, 0)
		assert.Error(t, err)
		_, err = checkProgressTotal(math.MaxUint64)
		assert.Error(t, err)
	})
	t.Run("total", func(t *testing.T) {
		total, err := checkProgressTotal(7)
		assert.NoError(t, err)
		assert.EqualValues(t, 7, total)
	})
}

// With no remote repositories there is nothing to check, so no request should be sent to the target
func TestRemoteCheckWithoutRemoteRepositoriesSendsNoRequest(t *testing.T) {
	for _, native := range []bool{true, false} {
		name := "plugin"
		if native {
			name = "native"
		}
		t.Run(name, func(t *testing.T) {
			check, args, mock := newCheckWithMock(t, native, func(index int, w http.ResponseWriter, r *http.Request) {
				assert.Fail(t, "Unexpected request: "+r.Method+" "+r.RequestURI)
				w.WriteHeader(http.StatusInternalServerError)
			})
			for _, remoteRepositories := range [][]interface{}{nil, {}} {
				check.remoteRepositories = remoteRepositories
				passed, err := executeCheckWithin(t, check, args)
				assert.NoError(t, err)
				assert.True(t, passed)
			}
			assert.Empty(t, mock.all())
		})
	}
}
