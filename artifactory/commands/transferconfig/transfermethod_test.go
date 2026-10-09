package transferconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	commandUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	commonTests "github.com/jfrog/jfrog-cli-core/v2/common/tests"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	utilsTests "github.com/jfrog/jfrog-cli-core/v2/utils/tests"
	"github.com/jfrog/jfrog-client-go/artifactory/services"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/stretchr/testify/assert"
)

// Target versions relative to minNativeConfigTransferVersion, which is a placeholder that no real target reaches yet.
const (
	targetBelowNativeMin = "7.100.0"
	targetAtNativeMin    = minNativeConfigTransferVersion
	targetAboveNativeMin = "1000.0.0"
	// Valid source version for all the target versions above (the source must not be newer than the target).
	// It is below 7.0.0, so that the Access connectivity check of the source is not part of these tests.
	methodTestSourceVersion = minTransferConfigArtifactoryVersion
)

func TestParseConfigTransferMethod(t *testing.T) {
	testCases := []struct {
		value    string
		expected ConfigTransferMethod
		wantErr  bool
	}{
		{value: "", expected: ConfigTransferMethodAuto},
		{value: "auto", expected: ConfigTransferMethodAuto},
		{value: "native", expected: ConfigTransferMethodNative},
		{value: "plugin", expected: ConfigTransferMethodPlugin},
		{value: "unknown", wantErr: true},
		{value: "NATIVE", wantErr: true},
		{value: " native", wantErr: true},
	}
	for _, testCase := range testCases {
		t.Run("value '"+testCase.value+"'", func(t *testing.T) {
			method, err := ParseConfigTransferMethod(testCase.value)
			if testCase.wantErr {
				// The error lists all the valid values
				assert.ErrorContains(t, err, testCase.value)
				for _, valid := range []string{"auto", "native", "plugin"} {
					assert.ErrorContains(t, err, valid)
				}
				assert.Empty(t, method)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, testCase.expected, method)
		})
	}
}

func TestConfigTransferMethodValues(t *testing.T) {
	// The exported values are part of the contract with jfrog-cli (the value of the --method flag)
	assert.Equal(t, ConfigTransferMethod("auto"), ConfigTransferMethodAuto)
	assert.Equal(t, ConfigTransferMethod("native"), ConfigTransferMethodNative)
	assert.Equal(t, ConfigTransferMethod("plugin"), ConfigTransferMethodPlugin)
}

func TestSetConfigTransferMethod(t *testing.T) {
	cmd := NewTransferConfigCommand(&config.ServerDetails{}, &config.ServerDetails{})
	assert.Equal(t, ConfigTransferMethodAuto, cmd.method, "the default method must be auto")
	assert.Same(t, cmd, cmd.SetConfigTransferMethod(ConfigTransferMethodNative), "the setter must be chainable")
	assert.Equal(t, ConfigTransferMethodNative, cmd.method)
}

// versionServers creates a source and a target mock servers, that answer the version requests with the given versions.
// All the requests that the servers receive are recorded.
type versionServers struct {
	sourceDetails, targetDetails *config.ServerDetails
	recorder                     *requestRecorder
}

func newVersionServers(t *testing.T, sourceVersion, targetVersion string, extra func(w http.ResponseWriter, r *http.Request) bool) *versionServers {
	servers := &versionServers{recorder: &requestRecorder{}}
	handler := func(version string) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			servers.recorder.record(r)
			if r.RequestURI == "/api/system/version" {
				content, err := json.Marshal(commandUtils.VersionResponse{Version: version})
				assert.NoError(t, err)
				_, err = w.Write(content)
				assert.NoError(t, err)
				return
			}
			if extra != nil && extra(w, r) {
				return
			}
			assert.Fail(t, "Unexpected request: "+r.Method+" "+r.RequestURI)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	sourceServer, sourceDetails, _ := commonTests.CreateRtRestsMockServer(t, handler(sourceVersion))
	t.Cleanup(sourceServer.Close)
	targetServer, targetDetails, _ := commonTests.CreateRtRestsMockServer(t, handler(targetVersion))
	t.Cleanup(targetServer.Close)
	servers.sourceDetails, servers.targetDetails = sourceDetails, targetDetails
	return servers
}

func (s *versionServers) pluginRequests() (result []recordedRequest) {
	for _, request := range s.recorder.all() {
		if strings.Contains(request.uri, commandUtils.PluginsExecuteRestApi) {
			result = append(result, request)
		}
	}
	return
}

// The resolution matrix: flag value x target version (below / at / above minNativeConfigTransferVersion)
func TestValidateMinVersionResolvesImporter(t *testing.T) {
	const (
		plugin = "plugin"
		native = "native"
	)
	testCases := []struct {
		name     string
		method   ConfigTransferMethod
		target   string
		expected string
		// A native method on a target that is below the minimum is a user's explicit decision: warn and continue
		expectWarn bool
	}{
		{name: "auto below min", method: ConfigTransferMethodAuto, target: targetBelowNativeMin, expected: plugin},
		{name: "auto at min", method: ConfigTransferMethodAuto, target: targetAtNativeMin, expected: native},
		{name: "auto above min", method: ConfigTransferMethodAuto, target: targetAboveNativeMin, expected: native},
		{name: "plugin below min", method: ConfigTransferMethodPlugin, target: targetBelowNativeMin, expected: plugin},
		{name: "plugin at min", method: ConfigTransferMethodPlugin, target: targetAtNativeMin, expected: plugin},
		{name: "plugin above min", method: ConfigTransferMethodPlugin, target: targetAboveNativeMin, expected: plugin},
		{name: "native below min", method: ConfigTransferMethodNative, target: targetBelowNativeMin, expected: native, expectWarn: true},
		{name: "native at min", method: ConfigTransferMethodNative, target: targetAtNativeMin, expected: native},
		{name: "native above min", method: ConfigTransferMethodNative, target: targetAboveNativeMin, expected: native},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			servers := newVersionServers(t, methodTestSourceVersion, testCase.target, nil)
			cmd := createTransferConfigCommand(t, servers.sourceDetails, servers.targetDetails).SetConfigTransferMethod(testCase.method)
			stdout, stderr, previousLog := utilsTests.RedirectLogOutputToBuffer()
			defer log.SetLogger(previousLog)

			sourceVersion, err := cmd.validateMinVersion()
			assert.NoError(t, err)
			assert.Equal(t, methodTestSourceVersion, sourceVersion)

			switch testCase.expected {
			case plugin:
				importer, ok := cmd.importer.(*pluginImporter)
				if assert.True(t, ok, "expected the plugin importer, got %T", cmd.importer) {
					assert.Same(t, cmd, importer.tcc)
				}
			case native:
				importer, ok := cmd.importer.(*nativeImporter)
				if assert.True(t, ok, "expected the native importer, got %T", cmd.importer) {
					assert.Same(t, cmd, importer.tcc)
				}
			}

			logged := stdout.String() + stderr.String()
			assert.Contains(t, logged, "Config transfer method: "+testCase.expected+" (target Artifactory version "+testCase.target+", native API minimum "+minNativeConfigTransferVersion+")")
			if testCase.expectWarn {
				assert.Contains(t, logged, "[Warn]")
				assert.Contains(t, logged, minNativeConfigTransferVersion)
			} else {
				assert.NotContains(t, logged, "[Warn]")
			}

			// The target version is read once, by the existing version validation: no extra GET version request
			versionRequests := 0
			for _, request := range servers.recorder.all() {
				if request.uri == "/api/system/version" {
					versionRequests++
				}
			}
			assert.Equal(t, 2, versionRequests, "expected one version request to the source and one to the target")
		})
	}
}

// An unset method behaves as auto
func TestValidateMinVersionUnsetMethodIsAuto(t *testing.T) {
	servers := newVersionServers(t, methodTestSourceVersion, targetAboveNativeMin, nil)
	cmd := createTransferConfigCommand(t, servers.sourceDetails, servers.targetDetails)
	_, err := cmd.validateMinVersion()
	assert.NoError(t, err)
	assert.IsType(t, &nativeImporter{}, cmd.importer)
}

func TestValidateMinVersionUnknownMethodFails(t *testing.T) {
	servers := newVersionServers(t, methodTestSourceVersion, targetAboveNativeMin, nil)
	cmd := createTransferConfigCommand(t, servers.sourceDetails, servers.targetDetails).SetConfigTransferMethod("bogus")
	_, err := cmd.validateMinVersion()
	assert.ErrorContains(t, err, "bogus")
	assert.ErrorContains(t, err, "auto")
}

// The importer is not resolved if the version validation fails
func TestValidateMinVersionFailureKeepsImporter(t *testing.T) {
	servers := newVersionServers(t, "7.1.0", "7.0.0", nil)
	cmd := createTransferConfigCommand(t, servers.sourceDetails, servers.targetDetails).SetConfigTransferMethod(ConfigTransferMethodNative)
	_, err := cmd.validateMinVersion()
	assert.ErrorContains(t, err, "can't be higher than the target Artifactory version")
	assert.IsType(t, &pluginImporter{}, cmd.importer)
}

// The method is resolved before the target is verified, since the verification belongs to the chosen importer.
// With the native method nothing is sent to the config-import plugin, and the --target-working-dir flag is reported as ignored.
func TestValidateServerPrerequisitesNative(t *testing.T) {
	testCases := []struct {
		name       string
		method     ConfigTransferMethod
		target     string
		workingDir string
	}{
		{name: "explicit native", method: ConfigTransferMethodNative, target: targetBelowNativeMin, workingDir: "/opt/jfrog/work"},
		{name: "auto resolved to native", method: ConfigTransferMethodAuto, target: targetAboveNativeMin, workingDir: "/opt/jfrog/work"},
		{name: "native without working dir", method: ConfigTransferMethodNative, target: targetAboveNativeMin},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			servers := newVersionServers(t, methodTestSourceVersion, testCase.target, func(w http.ResponseWriter, r *http.Request) bool {
				if r.RequestURI == "/api/security/users" {
					_, err := w.Write([]byte("[]"))
					assert.NoError(t, err)
					return true
				}
				return false
			})
			cmd := createTransferConfigCommand(t, servers.sourceDetails, servers.targetDetails).SetConfigTransferMethod(testCase.method)
			cmd.SetTargetWorkingDir(testCase.workingDir)
			stdout, stderr, previousLog := utilsTests.RedirectLogOutputToBuffer()
			defer log.SetLogger(previousLog)

			assert.NoError(t, cmd.validateServerPrerequisites())
			assert.IsType(t, &nativeImporter{}, cmd.importer)
			assert.Empty(t, servers.pluginRequests(), "the native method must not use the config-import plugin")

			// The target was inspected after the resolution (the users count is part of validateTargetServer)
			usersRequests := 0
			for _, request := range servers.recorder.all() {
				if request.uri == "/api/security/users" {
					usersRequests++
				}
			}
			assert.Equal(t, 1, usersRequests)

			logged := stdout.String() + stderr.String()
			if testCase.workingDir != "" {
				assert.Contains(t, logged, "[Warn]")
				assert.Contains(t, logged, "target working dir")
				assert.Contains(t, logged, testCase.workingDir)
			} else {
				assert.NotContains(t, logged, "target working dir")
			}
		})
	}
}

// The plugin method is not affected: the target is verified through the plugin, and --target-working-dir is used (no warning)
func TestValidateServerPrerequisitesPlugin(t *testing.T) {
	servers := newVersionServers(t, methodTestSourceVersion, targetAboveNativeMin, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.RequestURI {
		case "/" + commandUtils.PluginsExecuteRestApi + "configImportVersion":
			content, err := json.Marshal(commandUtils.VersionResponse{Version: "1.0.0"})
			assert.NoError(t, err)
			_, err = w.Write(content)
			assert.NoError(t, err)
		case "/" + commandUtils.PluginsExecuteRestApi + "checkPermissions?params=workingDir=/opt/jfrog/work":
			w.WriteHeader(http.StatusOK)
		case "/api/security/users":
			_, err := w.Write([]byte("[]"))
			assert.NoError(t, err)
		default:
			return false
		}
		return true
	})
	cmd := createTransferConfigCommand(t, servers.sourceDetails, servers.targetDetails).SetConfigTransferMethod(ConfigTransferMethodPlugin)
	cmd.SetTargetWorkingDir("/opt/jfrog/work")
	stdout, stderr, previousLog := utilsTests.RedirectLogOutputToBuffer()
	defer log.SetLogger(previousLog)

	assert.NoError(t, cmd.validateServerPrerequisites())
	assert.IsType(t, &pluginImporter{}, cmd.importer)
	assert.Len(t, servers.pluginRequests(), 2, "the plugin version and checkPermissions are expected")
	assert.NotContains(t, stdout.String()+stderr.String(), "[Warn]")
}

func remoteRepositoryCheckNames(checks []string) (found bool) {
	for _, name := range checks {
		if strings.Contains(strings.ToLower(name), "remote") {
			return true
		}
	}
	return false
}

func checkNames(cmd *TransferConfigCommand) (names []string) {
	for _, check := range cmd.buildPreChecks(map[utils.RepoType][]services.RepositoryDetails{}, nil) {
		names = append(names, check.Name())
	}
	return
}

func TestBuildPreChecksPerMethod(t *testing.T) {
	newCmd := func(method ConfigTransferMethod, version string) *TransferConfigCommand {
		servers := newVersionServers(t, methodTestSourceVersion, version, nil)
		cmd := createTransferConfigCommand(t, servers.sourceDetails, servers.targetDetails).SetConfigTransferMethod(method)
		_, err := cmd.validateMinVersion()
		assert.NoError(t, err)
		return cmd
	}

	t.Run("plugin runs all the checks", func(t *testing.T) {
		names := checkNames(newCmd(ConfigTransferMethodPlugin, targetAboveNativeMin))
		assert.Len(t, names, 2)
		assert.True(t, remoteRepositoryCheckNames(names), "%v", names)
	})
	t.Run("auto resolved to plugin runs all the checks", func(t *testing.T) {
		names := checkNames(newCmd(ConfigTransferMethodAuto, targetBelowNativeMin))
		assert.Len(t, names, 2)
		assert.True(t, remoteRepositoryCheckNames(names), "%v", names)
	})
	t.Run("native runs all the checks, and no warning about a skipped one", func(t *testing.T) {
		cmd := newCmd(ConfigTransferMethodNative, targetAboveNativeMin)
		stdout, stderr, previousLog := utilsTests.RedirectLogOutputToBuffer()
		defer log.SetLogger(previousLog)

		names := checkNames(cmd)
		assert.Len(t, names, 2, "%v", names)
		assert.True(t, remoteRepositoryCheckNames(names), "%v", names)
		assert.NotContains(t, stdout.String()+stderr.String(), "[Warn]")
	})
	t.Run("auto resolved to native runs all the checks", func(t *testing.T) {
		names := checkNames(newCmd(ConfigTransferMethodAuto, targetAboveNativeMin))
		assert.Len(t, names, 2, "%v", names)
		assert.True(t, remoteRepositoryCheckNames(names), "%v", names)
	})
}

// --prechecks with the native method, against a source that has a remote repository: the remote repositories check runs through
// the native API of the target, so nothing may reach /api/plugins/execute/* on either server.
func TestRunPreChecksNativeMakesNoPluginRequests(t *testing.T) {
	recorder := &requestRecorder{}
	respond := func(t *testing.T, w http.ResponseWriter, body string) {
		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	}
	sourceServer, sourceDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		switch r.RequestURI {
		case "/api/system/version":
			respond(t, w, `{"version":"`+methodTestSourceVersion+`"}`)
		case "/api/security/lockedUsers":
			respond(t, w, `["admin"]`)
		case "/api/repositories":
			respond(t, w, `[{"key":"remote1","type":"REMOTE","packageType":"generic"}]`)
		case "/api/repositories/remote1":
			respond(t, w, `{"key":"remote1","rclass":"remote","packageType":"generic","url":"https://example.invalid/"}`)
		case "/api/system/configuration":
			respond(t, w, "<config></config>")
		case "/api/system/decrypt", "/api/system/encrypt":
			w.WriteHeader(http.StatusOK)
		default:
			assert.Fail(t, "Unexpected source request: "+r.Method+" "+r.RequestURI)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer sourceServer.Close()
	targetServer, targetDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		switch r.RequestURI {
		case "/api/system/version":
			respond(t, w, `{"version":"`+targetAboveNativeMin+`"}`)
		case "/api/security/users", "/api/repositories":
			respond(t, w, "[]")
		case "/api/configTransfer/remoteRepositoriesCheck":
			w.WriteHeader(http.StatusAccepted)
			respond(t, w, `{"id":"check-1"}`)
		case "/api/configTransfer/remoteRepositoriesCheck/check-1":
			respond(t, w, `{"status":"completed","checked_repositories":1,"total_repositories":1}`)
		default:
			assert.Fail(t, "Unexpected target request: "+r.Method+" "+r.RequestURI)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer targetServer.Close()

	cmd := NewTransferConfigCommand(sourceDetails, targetDetails).
		SetConfigTransferMethod(ConfigTransferMethodNative).SetPreChecks(true)
	_, _, previousLog := utilsTests.RedirectLogOutputToBuffer()
	defer log.SetLogger(previousLog)

	assert.NoError(t, cmd.Run())
	assert.IsType(t, &nativeImporter{}, cmd.importer)
	for _, request := range recorder.all() {
		assert.NotContains(t, request.uri, commandUtils.PluginsExecuteRestApi, "unexpected plugin request: %s", request)
	}
	// The remote repository was in the pre-checks input, so the native remote repositories check was started and polled on the target
	var sawRemoteRepo, sawCheckStart, sawCheckPoll bool
	for _, request := range recorder.all() {
		sawRemoteRepo = sawRemoteRepo || request.uri == "/api/repositories/remote1"
		sawCheckStart = sawCheckStart || (request.method == http.MethodPost && request.uri == "/api/configTransfer/remoteRepositoriesCheck")
		sawCheckPoll = sawCheckPoll || (request.method == http.MethodGet && request.uri == "/api/configTransfer/remoteRepositoriesCheck/check-1")
	}
	assert.True(t, sawRemoteRepo, "the source remote repository was expected to be read")
	assert.True(t, sawCheckStart, "the native remote repositories check was expected to be started on the target")
	assert.True(t, sawCheckPoll, "the native remote repositories check was expected to be polled on the target")
}

// The plugin twin of TestRunPreChecksNativeMakesNoPluginRequests: with the plugin method, the remote repositories check runs
// through the config-import plugin, and nothing may reach /api/configTransfer/*.
func TestRunPreChecksPluginMakesNoNativeRequests(t *testing.T) {
	remoteRepositories := []interface{}{map[string]interface{}{"key": "remote1", "url": "https://example.invalid/", "packageType": "generic"}}
	servers := newVersionServers(t, methodTestSourceVersion, targetAboveNativeMin, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && r.RequestURI == "/"+commandUtils.PluginsExecuteRestApi+"remoteRepositoriesCheck":
			_, err := w.Write([]byte(`{"status":"running","total_repositories":1}`))
			assert.NoError(t, err)
		case r.Method == http.MethodGet && r.RequestURI == "/"+commandUtils.PluginsExecuteRestApi+"remoteRepositoriesCheckStatus":
			_, err := w.Write([]byte(`{"status":"completed","checked_repositories":1,"total_repositories":1}`))
			assert.NoError(t, err)
		default:
			return false
		}
		return true
	})
	cmd := createTransferConfigCommand(t, servers.sourceDetails, servers.targetDetails).SetConfigTransferMethod(ConfigTransferMethodPlugin)
	_, err := cmd.validateMinVersion()
	assert.NoError(t, err)
	assert.IsType(t, &pluginImporter{}, cmd.importer)
	_, _, previousLog := utilsTests.RedirectLogOutputToBuffer()
	defer log.SetLogger(previousLog)

	assert.NoError(t, cmd.NewPreChecksRunner(map[utils.RepoType][]services.RepositoryDetails{}, remoteRepositories).Run(context.Background(), cmd.TargetServerDetails))

	var sawStart bool
	for _, request := range servers.recorder.all() {
		assert.NotContains(t, request.uri, "api/configTransfer/", "unexpected native request: %s", request)
		sawStart = sawStart || (request.method == http.MethodPost && request.uri == "/"+commandUtils.PluginsExecuteRestApi+"remoteRepositoriesCheck")
	}
	assert.True(t, sawStart, "the plugin remote repositories check was expected to be started on the target")
}
