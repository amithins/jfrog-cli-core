package transferconfig

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	commandUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	commonTests "github.com/jfrog/jfrog-cli-core/v2/common/tests"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/stretchr/testify/assert"
)

// fakeImporter is a configImporter that records how the command drives it.
type fakeImporter struct {
	verifyErr       error
	startErr        error
	importRef       string
	pollingResponse []byte

	verifyCalls    int
	startCalls     int
	startedZip     string
	pollingCalls   int
	pollingRefUsed string
}

func (f *fakeImporter) verify() error {
	f.verifyCalls++
	return f.verifyErr
}

func (f *fakeImporter) start(zip *bytes.Buffer) (string, error) {
	f.startCalls++
	f.startedZip = zip.String()
	return f.importRef, f.startErr
}

func (f *fakeImporter) pollingAction(importRef string) httputils.PollingAction {
	f.pollingCalls++
	f.pollingRefUsed = importRef
	return func() (bool, []byte, error) {
		return true, f.pollingResponse, nil
	}
}

func TestNewTransferConfigCommandUsesPluginImporter(t *testing.T) {
	cmd := NewTransferConfigCommand(&config.ServerDetails{}, &config.ServerDetails{})
	importer, ok := cmd.importer.(*pluginImporter)
	if assert.True(t, ok, "the importer must always be the plugin importer for now, got %T", cmd.importer) {
		assert.Same(t, cmd, importer.tcc, "the plugin importer must operate on the command that owns it")
	}
}

func TestValidateTargetServerUsesImporterVerify(t *testing.T) {
	// The mock server fails the test on any HTTP call: with a fake importer, the plugin must not be contacted
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/api/security/users" {
			_, err := w.Write([]byte("[]"))
			assert.NoError(t, err)
			return
		}
		assert.Fail(t, "Unexpected request: "+r.RequestURI)
	})
	defer testServer.Close()

	cmd := createTransferConfigCommand(t, &config.ServerDetails{Url: "dummy-url"}, serverDetails)
	fake := &fakeImporter{}
	cmd.importer = fake
	assert.NoError(t, cmd.validateTargetServer())
	assert.Equal(t, 1, fake.verifyCalls)

	// A verification failure is returned as is, and the target is not inspected
	fake.verifyErr = errors.New("verify failed")
	assert.Same(t, fake.verifyErr, cmd.validateTargetServer())
	assert.Equal(t, 2, fake.verifyCalls)
}

func TestImportToTargetArtifactoryUsesImporter(t *testing.T) {
	cmd := createTransferConfigCommand(t, nil, &config.ServerDetails{Url: "http://target-dummy/", ArtifactoryUrl: "http://target-dummy/artifactory/"})
	fake := &fakeImporter{importRef: "ref-123", pollingResponse: []byte("all good")}
	cmd.importer = fake

	assert.NoError(t, cmd.importToTargetArtifactory(bytes.NewBufferString("zip-content")))
	assert.Equal(t, 1, fake.startCalls)
	assert.Equal(t, "zip-content", fake.startedZip)
	assert.Equal(t, 1, fake.pollingCalls)
	assert.Equal(t, "ref-123", fake.pollingRefUsed, "the polling action must be built from the reference returned by start")
}

func TestImportToTargetArtifactoryStartFailureSkipsPolling(t *testing.T) {
	cmd := createTransferConfigCommand(t, nil, &config.ServerDetails{Url: "http://target-dummy/", ArtifactoryUrl: "http://target-dummy/artifactory/"})
	fake := &fakeImporter{startErr: errors.New("start failed")}
	cmd.importer = fake

	assert.Same(t, fake.startErr, cmd.importToTargetArtifactory(bytes.NewBufferString("zip-content")))
	assert.Equal(t, 0, fake.pollingCalls)
}

// The [ERROR] check on the import logs is shared by all importers, and therefore stays in the command
func TestImportToTargetArtifactoryErrorInLogsWithImporter(t *testing.T) {
	cmd := createTransferConfigCommand(t, nil, &config.ServerDetails{Url: "http://target-dummy/", ArtifactoryUrl: "http://target-dummy/artifactory/"})
	cmd.importer = &fakeImporter{importRef: "ref", pollingResponse: []byte("a\n[ERROR] b")}

	err := cmd.importToTargetArtifactory(bytes.NewBufferString("zip-content"))
	assert.ErrorContains(t, err, "Errors detected during config import")
}

func TestSwitchTargetToSourceCredentials(t *testing.T) {
	testCases := []struct {
		name                string
		sourceDetails       config.ServerDetails
		expectedAuthHeader  func(r *http.Request) bool
		expectedUser        string
		expectedPassword    string
		expectedAccessToken string
	}{
		{
			name:          "user and password",
			sourceDetails: config.ServerDetails{User: "source-user", Password: "source-pass"},
			expectedAuthHeader: func(r *http.Request) bool {
				u, p, ok := r.BasicAuth()
				return ok && u == "source-user" && p == "source-pass"
			},
			expectedUser:        "source-user",
			expectedPassword:    "source-pass",
			expectedAccessToken: "",
		},
		{
			name:                "access token",
			sourceDetails:       config.ServerDetails{AccessToken: "source-token"},
			expectedAuthHeader:  func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer source-token" },
			expectedUser:        "",
			expectedPassword:    "",
			expectedAccessToken: "source-token",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			authenticatedWithSource := false
			testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
				authenticatedWithSource = testCase.expectedAuthHeader(r)
				w.WriteHeader(http.StatusOK)
			})
			defer testServer.Close()
			serverDetails.User, serverDetails.Password = "target-user", "target-pass"

			sourceDetails := testCase.sourceDetails
			sourceDetails.Url = "dummy-url"
			cmd := createTransferConfigCommand(t, &sourceDetails, serverDetails)
			oldManager := cmd.TargetArtifactoryManager

			rtDetails, err := cmd.switchTargetToSourceCredentials()
			assert.NoError(t, err)

			// The target server details carry the source credentials
			assert.Equal(t, testCase.expectedUser, cmd.TargetServerDetails.GetUser())
			assert.Equal(t, testCase.expectedPassword, cmd.TargetServerDetails.GetPassword())
			assert.Equal(t, testCase.expectedAccessToken, cmd.TargetServerDetails.GetAccessToken())

			// Both service managers were rebuilt
			assert.NotSame(t, oldManager, cmd.TargetArtifactoryManager)
			assert.NotNil(t, cmd.TargetAccessManager)

			// The returned client details are built from the new manager, and are therefore usable for the next call
			if assert.NotNil(t, rtDetails) {
				assert.Equal(t, testCase.expectedUser, rtDetails.User)
				assert.Equal(t, testCase.expectedPassword, rtDetails.Password)
				assert.Equal(t, testCase.expectedAccessToken, rtDetails.AccessToken)
			}
			resp, _, _, err := cmd.TargetArtifactoryManager.Client().SendGet(testServer.URL+"/ping", false, rtDetails)
			assert.NoError(t, err)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.True(t, authenticatedWithSource, "the call after the switch must be authenticated with the source credentials")
		})
	}
}

func TestPluginImporterStart(t *testing.T) {
	var gotUri, gotBody string
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotUri = r.RequestURI
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		gotBody = string(body)
		_, err = w.Write([]byte("987"))
		assert.NoError(t, err)
	})
	defer testServer.Close()

	cmd := createTransferConfigCommand(t, nil, serverDetails)
	cmd.SetTargetWorkingDir("/wd")
	ref, err := cmd.importer.start(bytes.NewBufferString("the-zip"))
	assert.NoError(t, err)
	assert.Equal(t, "987", ref)
	assert.Equal(t, "/"+commandUtils.PluginsExecuteRestApi+"configImport?params=workingDir=/wd", gotUri)
	assert.Equal(t, "the-zip", gotBody)
}

func TestPluginImporterPollingAction(t *testing.T) {
	// Statuses returned by the mock server for the configImportStatus calls, in order
	statuses := []int{http.StatusOK, http.StatusAccepted, http.StatusUnauthorized, http.StatusOK, http.StatusNotFound}
	var users, uris, bodies []string
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.RequestURI, "configImportStatus") {
			// configImport, called by start()
			_, err := w.Write([]byte("ts-1"))
			assert.NoError(t, err)
			return
		}
		user, _, _ := r.BasicAuth()
		users = append(users, user)
		uris = append(uris, r.RequestURI)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		bodies = append(bodies, string(body))
		w.WriteHeader(statuses[len(users)-1])
		_, err = w.Write([]byte("resp-body"))
		assert.NoError(t, err)
	})
	defer testServer.Close()
	serverDetails.User, serverDetails.Password = "target-user", "target-pass"

	cmd := createTransferConfigCommand(t, &config.ServerDetails{Url: "dummy-url", User: "source-user", Password: "source-pass"}, serverDetails)
	cmd.SetTargetWorkingDir("/wd")
	// start() creates the client details that the polling action continues with
	ref, err := cmd.importer.start(bytes.NewBufferString("zip"))
	assert.NoError(t, err)
	action := cmd.importer.pollingAction(ref)

	// 200 - completed
	stop, body, err := action()
	assert.NoError(t, err)
	assert.True(t, stop)
	assert.Equal(t, "resp-body", string(body))

	// 202 - in progress
	stop, body, err = action()
	assert.NoError(t, err)
	assert.False(t, stop)
	assert.Nil(t, body)

	// 401 - the credentials are switched to the source ones and the polling goes on
	stop, body, err = action()
	assert.NoError(t, err)
	assert.False(t, stop)
	assert.Nil(t, body)
	assert.Equal(t, "source-user", cmd.TargetServerDetails.GetUser())

	// The next polls use the source credentials
	stop, _, err = action()
	assert.NoError(t, err)
	assert.True(t, stop)

	// 404 - unexpected status
	stop, _, err = action()
	assert.Error(t, err)
	assert.False(t, stop)

	assert.Equal(t, []string{"target-user", "target-user", "target-user", "source-user", "source-user"}, users)
	for i := range uris {
		assert.True(t, strings.HasSuffix(uris[i], "configImportStatus?params=workingDir=/wd"), uris[i])
		assert.Equal(t, "ts-1", bodies[i])
	}
}

// 401 and 403 both mean that the target user was deleted by the import: the credentials are switched to the source ones
func TestPluginImporterPollingActionCredentialSwitchStatuses(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var users []string
			testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.RequestURI, "configImportStatus") {
					_, err := w.Write([]byte("ts-1"))
					assert.NoError(t, err)
					return
				}
				user, _, _ := r.BasicAuth()
				users = append(users, user)
				if len(users) == 1 {
					w.WriteHeader(status)
					return
				}
				_, err := w.Write([]byte("done"))
				assert.NoError(t, err)
			})
			defer testServer.Close()
			serverDetails.User, serverDetails.Password = "target-user", "target-pass"

			cmd := createTransferConfigCommand(t, &config.ServerDetails{Url: "dummy-url", User: "source-user", Password: "source-pass"}, serverDetails)
			ref, err := cmd.importer.start(bytes.NewBufferString("zip"))
			assert.NoError(t, err)
			action := cmd.importer.pollingAction(ref)

			stop, body, err := action()
			assert.NoError(t, err)
			assert.False(t, stop)
			assert.Nil(t, body)
			assert.Equal(t, "source-user", cmd.TargetServerDetails.GetUser())

			stop, body, err = action()
			assert.NoError(t, err)
			assert.True(t, stop)
			assert.Equal(t, "done", string(body))
			assert.Equal(t, []string{"target-user", "source-user"}, users)
		})
	}
}

// If the credentials cannot be switched after a 401, the polling stops with the error instead of polling forever
func TestPluginImporterPollingActionFailedCredentialSwitchStops(t *testing.T) {
	statusCalls := 0
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.RequestURI, "configImportStatus") {
			_, err := w.Write([]byte("ts-1"))
			assert.NoError(t, err)
			return
		}
		statusCalls++
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer testServer.Close()

	cmd := createTransferConfigCommand(t, &config.ServerDetails{Url: "dummy-url", User: "source-user", Password: "source-pass"}, serverDetails)
	ref, err := cmd.importer.start(bytes.NewBufferString("zip"))
	assert.NoError(t, err)
	action := cmd.importer.pollingAction(ref)

	// Building the service manager of the new credentials fails on a missing client certificate
	cmd.TargetServerDetails.ClientCertPath = "/non/existing/client.crt"
	cmd.TargetServerDetails.ClientCertKeyPath = "/non/existing/client.key"

	stop, body, err := action()
	assert.Error(t, err)
	assert.True(t, stop)
	assert.Nil(t, body)
	assert.Equal(t, 1, statusCalls)
}
