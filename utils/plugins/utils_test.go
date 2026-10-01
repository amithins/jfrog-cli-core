package plugins

import (
	"fmt"
	biutils "github.com/jfrog/build-info-go/utils"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	testsutils "github.com/jfrog/jfrog-client-go/utils/tests"
	"github.com/stretchr/testify/assert"
	"os"
	"path/filepath"
	"testing"
)

const pluginName = "rt-fs"

func TestConvertPluginsV0ToV1(t *testing.T) {
	if coreutils.IsWindows() {
		return
	}
	// Setup testing env
	cleanUpTempEnv := createTempEnvForPluginsTests(t)
	defer cleanUpTempEnv()
	testHomeDir := setupPluginsTestingEnv(t, "v0")
	// Migration- v0 to v1
	p, err := convertPluginsV0ToV1()
	assert.NoError(t, err)
	assert.Equal(t, p.Version, 1)
	// Verity plugins.yaml exists
	expectedYamlLocation := filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName, coreutils.JfrogPluginsFileName)
	exists, err := fileutils.IsFileExists(expectedYamlLocation, false)
	assert.NoError(t, err)
	assert.True(t, exists, fmt.Sprintf("expected file: %s doesn't exists", expectedYamlLocation))
	// Verify executable location according to v1 hierarchy
	exists, err = fileutils.IsFileExists(filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName, pluginName, coreutils.PluginsExecDirName, GetLocalPluginExecutableName(pluginName)), false)
	assert.NoError(t, err)
	assert.True(t, exists)
	// The migrated plugin's own directory must not be group/other writable.
	pluginDirInfo, err := os.Stat(filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName, pluginName))
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), pluginDirInfo.Mode().Perm())
	assert.NoError(t, os.RemoveAll(filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName)))
}

// Plugins directory is empty - only 'plugins.yaml' should be created.
func TestConvertPluginsV0ToV1EmptyDir(t *testing.T) {
	if coreutils.IsWindows() {
		return
	}
	// Setup testing env
	cleanUpTempEnv := createTempEnvForPluginsTests(t)
	defer cleanUpTempEnv()
	testHomeDir := setupPluginsTestingEnv(t, "empty")
	// Migration- v0 to v1
	p, err := convertPluginsV0ToV1()
	assert.NoError(t, err)
	assert.Equal(t, p.Version, 1)
	// Verity plugins.yaml exists
	expectedYamlLocation := filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName, coreutils.JfrogPluginsFileName)
	exists, err := fileutils.IsFileExists(expectedYamlLocation, false)
	assert.NoError(t, err)
	assert.True(t, exists, fmt.Sprintf("expected file: %s doesn't exists", expectedYamlLocation))
	assert.NoError(t, os.RemoveAll(filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName)))
}

// Plugins directory contains unexpected file (non executable)
func TestConvertPluginsV0ToV1WithUnexpectedFiles(t *testing.T) {
	if coreutils.IsWindows() {
		return
	}
	// Setup testing env
	cleanUpTempEnv := createTempEnvForPluginsTests(t)
	defer cleanUpTempEnv()
	testHomeDir := setupPluginsTestingEnv(t, "unexpectedFiles")
	// Migration- v0 to v1
	p, err := convertPluginsV0ToV1()
	assert.NoError(t, err)
	assert.Equal(t, p.Version, 1)
	// Verity plugins.yaml exists
	expectedYamlLocation := filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName, coreutils.JfrogPluginsFileName)
	exists, err := fileutils.IsFileExists(expectedYamlLocation, false)
	assert.NoError(t, err)
	assert.True(t, exists, fmt.Sprintf("expected file: %s doesn't exists", expectedYamlLocation))
	// Verify executable location according to v1 hierarchy
	exists, err = fileutils.IsFileExists(filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName, pluginName, coreutils.PluginsExecDirName, GetLocalPluginExecutableName(pluginName)), false)
	assert.NoError(t, err)
	assert.True(t, exists)
	assert.NoError(t, os.RemoveAll(filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName)))
}

// ChmodPluginsDirectoryContent previously applied 0777 unconditionally, making every plugin
// directory writable by any local account. Start from a deliberately insecure
// mode to prove the function actively tightens it, rather than merely happening to be fine.
func TestChmodPluginsDirectoryContentIsNotWorldWritable(t *testing.T) {
	if coreutils.IsWindows() {
		return
	}
	cleanUpTempEnv := createTempEnvForPluginsTests(t)
	defer cleanUpTempEnv()

	pluginsDir, err := coreutils.GetJfrogPluginsDir()
	assert.NoError(t, err)
	somePluginDir := filepath.Join(pluginsDir, "some-plugin")
	assert.NoError(t, os.MkdirAll(somePluginDir, 0777))

	assert.NoError(t, coreutils.ChmodPluginsDirectoryContent())

	info, err := os.Stat(somePluginDir)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

// CheckPluginsVersionAndConvertIfNeeded previously only re-tightened plugin directory
// permissions as a side effect of an actual V0->V1 migration. An installation that is already
// on V1 (plugins.yml already present) but predates the 0755 fix would then keep a
// world-writable plugin directory forever, unless the user happened to install or update a
// plugin again. This reproduces that case directly: a V1 layout, untouched by migration, with
// a pre-existing 0777 plugin directory - and asserts the directory is retightened anyway.
func TestCheckPluginsVersionAndConvertIfNeededRetightensAnAlreadyV1Directory(t *testing.T) {
	if coreutils.IsWindows() {
		return
	}
	cleanUpTempEnv := createTempEnvForPluginsTests(t)
	defer cleanUpTempEnv()

	pluginsDir, err := coreutils.GetJfrogPluginsDir()
	assert.NoError(t, err)
	assert.NoError(t, os.MkdirAll(pluginsDir, 0755))
	// plugins.yml already present marks this layout as already-V1, so the migration path
	// itself must not be what retightens the directory below - only the unconditional call
	// this test exists to verify.
	assert.NoError(t, os.WriteFile(filepath.Join(pluginsDir, coreutils.JfrogPluginsFileName), []byte(`{"version":1}`), 0600))

	preExistingPluginDir := filepath.Join(pluginsDir, "some-plugin")
	assert.NoError(t, os.MkdirAll(preExistingPluginDir, 0777))

	assert.NoError(t, CheckPluginsVersionAndConvertIfNeeded())

	info, err := os.Stat(preExistingPluginDir)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

func setupPluginsTestingEnv(t *testing.T, pluginsDirName string) string {
	testHomeDir, err := coreutils.GetJfrogHomeDir()
	assert.NoError(t, err)
	wd, err := os.Getwd()
	assert.NoError(t, err)
	err = biutils.CopyDir(filepath.Join(wd, "testdata", coreutils.JfrogPluginsDirName, pluginsDirName), filepath.Join(testHomeDir, coreutils.JfrogPluginsDirName), true, nil)
	assert.NoError(t, err)
	err = coreutils.ChmodPluginsDirectoryContent()
	assert.NoError(t, err)
	return testHomeDir
}

// Set JFROG_CLI_HOME_DIR environment variable to be a new temp directory
func createTempEnvForPluginsTests(t *testing.T) (cleanUp func()) {
	tmpDir, err := os.MkdirTemp("", "plugins_test")
	assert.NoError(t, err)
	oldHome := os.Getenv(coreutils.HomeDir)
	testsutils.SetEnvAndAssert(t, coreutils.HomeDir, tmpDir)
	return func() {
		testsutils.RemoveAllAndAssert(t, tmpDir)
		testsutils.SetEnvAndAssert(t, coreutils.HomeDir, oldHome)
	}
}
