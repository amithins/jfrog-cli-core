//go:build !windows

package transferconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"

	biutils "github.com/jfrog/build-info-go/utils"
	commonTests "github.com/jfrog/jfrog-cli-core/v2/common/tests"
	"github.com/jfrog/jfrog-cli-core/v2/utils/tests"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Keep all tests, including the ones reaching createExportPath, away from the machine's Artifactory var directories.
func TestMain(m *testing.M) {
	defaultArtifactoryVarDirs = nil
	if err := os.Unsetenv("JFROG_HOME"); err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// Point the Artifactory var directory lookup to the given JFROG_HOME and default locations only.
func setArtifactoryVarDirs(t *testing.T, jfrogHome string, defaults ...string) {
	t.Setenv("JFROG_HOME", jfrogHome)
	oldDefaults := defaultArtifactoryVarDirs
	defaultArtifactoryVarDirs = defaults
	t.Cleanup(func() {
		defaultArtifactoryVarDirs = oldDefaults
	})
}

// Run the root-only logic of setServiceGroup as if the effective user ID was euid.
func setEuid(t *testing.T, euid int) {
	oldGeteuid := geteuid
	geteuid = func() int { return euid }
	t.Cleanup(func() {
		geteuid = oldGeteuid
	})
}

func getStat(t *testing.T, path string) *syscall.Stat_t {
	info, err := os.Stat(path)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return stat
}

func getGid(t *testing.T, path string) int {
	return int(getStat(t, path).Gid)
}

// Return a group ID that differs from the excluded ones and that the current user can chown to.
// Root can use any gid. Other users need a supplementary group, otherwise the test is skipped.
func getOtherGid(t *testing.T, excluded ...int) int {
	var candidates []int
	if os.Geteuid() == 0 {
		for gid := 65534; gid > 65534-1-len(excluded); gid-- {
			candidates = append(candidates, gid)
		}
	} else {
		groups, err := os.Getgroups()
		require.NoError(t, err)
		candidates = groups
	}
	for _, gid := range candidates {
		if !slices.Contains(excluded, gid) {
			return gid
		}
	}
	t.Skipf("Requires root or membership in a group besides %v", excluded)
	return -1
}

// A uid other than the current user's, for fake file owners.
func otherUid() uint32 {
	if os.Geteuid() == 65534 {
		return 65533
	}
	return 65534
}

// Create an Artifactory var directory group-owned by gid.
func createVarDir(t *testing.T, varDir string, gid int) string {
	require.NoError(t, os.MkdirAll(varDir, 0750))
	require.NoError(t, os.Chown(varDir, nonRootOwner(), gid))
	return varDir
}

// A non-root owner for var directories created by tests, since root-owned var directories are skipped.
// Not running as root, the directories are already owned by the current user, so keep the owner (-1).
func nonRootOwner() int {
	if os.Geteuid() == 0 {
		return 65534
	}
	return -1
}

// Create the export directory the way createExportPath does, inside a fresh base directory.
func createTestExportDir(t *testing.T) string {
	exportPath := filepath.Join(t.TempDir(), "export")
	require.NoError(t, os.Mkdir(exportPath, exportDirMode))
	require.NoError(t, os.Chmod(exportPath, exportDirMode))
	return exportPath
}

// Create a base directory with the setgid bit, like the documented --source-working-dir setup. Skip if the bit can't be set.
func createSetgidBaseDir(t *testing.T) string {
	baseDir := t.TempDir()
	require.NoError(t, os.Chmod(baseDir, os.ModeSetgid|exportDirMode))
	info, err := os.Stat(baseDir)
	require.NoError(t, err)
	if info.Mode()&os.ModeSetgid == 0 {
		t.Skip("Couldn't set the setgid bit on the base directory")
	}
	return baseDir
}

// Capture the log, for asserting on warnings and info messages.
func captureLog(t *testing.T) fmt.Stringer {
	_, logs, previousLog := tests.RedirectLogOutputToBuffer()
	t.Cleanup(func() {
		log.SetLogger(previousLog)
	})
	return logs
}

func TestFindArtifactoryVarDirPrecedence(t *testing.T) {
	jfrogHome := t.TempDir()
	jfrogHomeVarDir := filepath.Join(jfrogHome, "artifactory", "var")
	require.NoError(t, os.MkdirAll(jfrogHomeVarDir, 0750))
	require.NoError(t, os.Chown(jfrogHomeVarDir, nonRootOwner(), -1))
	defaultVarDir := filepath.Join(t.TempDir(), "var")
	require.NoError(t, os.Mkdir(defaultVarDir, 0750))
	require.NoError(t, os.Chown(defaultVarDir, nonRootOwner(), -1))
	notDir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDir, nil, 0600))
	notExist := filepath.Join(t.TempDir(), "not-exist")

	testCases := []struct {
		name      string
		jfrogHome string
		defaults  []string
		expected  string
	}{
		{"JFROG_HOME before the defaults", jfrogHome, []string{defaultVarDir}, jfrogHomeVarDir},
		{"first existing default directory", "", []string{notExist, notDir, defaultVarDir}, defaultVarDir},
		{"JFROG_HOME without a var directory", t.TempDir(), []string{defaultVarDir}, defaultVarDir},
		{"root-owned directory", "", []string{"/", defaultVarDir}, defaultVarDir},
		{"not found", "", []string{notExist, notDir}, ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			setArtifactoryVarDirs(t, testCase.jfrogHome, testCase.defaults...)
			varDir, stat := findArtifactoryVarDir()
			assert.Equal(t, testCase.expected, varDir)
			assert.Equal(t, testCase.expected != "", stat != nil)
		})
	}
}

func TestSetServiceGroupFromJfrogHome(t *testing.T) {
	setEuid(t, 0)
	exportPath := createTestExportDir(t)
	serviceGid := getOtherGid(t, getGid(t, exportPath))
	jfrogHome := t.TempDir()
	varDir := createVarDir(t, filepath.Join(jfrogHome, "artifactory", "var"), serviceGid)
	setArtifactoryVarDirs(t, jfrogHome)
	logs := captureLog(t)

	setServiceGroup(exportPath)
	assert.Equal(t, serviceGid, getGid(t, exportPath))
	info, err := os.Stat(exportPath)
	require.NoError(t, err)
	assert.Equal(t, exportDirMode, info.Mode().Perm())
	// The applied group and where it came from are logged
	assert.Contains(t, logs.String(), "gid "+strconv.Itoa(serviceGid))
	assert.Contains(t, logs.String(), varDir)
}

func TestSetServiceGroupRequiresRoot(t *testing.T) {
	setEuid(t, 4321)
	exportPath := createTestExportDir(t)
	originalGid := getGid(t, exportPath)
	jfrogHome := t.TempDir()
	createVarDir(t, filepath.Join(jfrogHome, "artifactory", "var"), getOtherGid(t, originalGid))
	setArtifactoryVarDirs(t, jfrogHome)

	setServiceGroup(exportPath)
	assert.Equal(t, originalGid, getGid(t, exportPath), "a non-root user must not change the group of the export directory")
}

func TestSetServiceGroupKeepsSetgidParentGroup(t *testing.T) {
	setEuid(t, 0)
	baseDir := createSetgidBaseDir(t)
	exportPath := filepath.Join(baseDir, "export")
	require.NoError(t, os.Mkdir(exportPath, exportDirMode))
	inheritedGid := getGid(t, exportPath)

	jfrogHome := t.TempDir()
	createVarDir(t, filepath.Join(jfrogHome, "artifactory", "var"), getOtherGid(t, inheritedGid))
	setArtifactoryVarDirs(t, jfrogHome)

	setServiceGroup(exportPath)
	assert.Equal(t, inheritedGid, getGid(t, exportPath), "the group chosen by the operator with a setgid base directory must be kept")
}

func TestSetServiceGroupNoVarDir(t *testing.T) {
	setEuid(t, 0)
	exportPath := createTestExportDir(t)
	originalGid := getGid(t, exportPath)
	setArtifactoryVarDirs(t, "", filepath.Join(t.TempDir(), "not-exist"))

	setServiceGroup(exportPath)
	assert.Equal(t, originalGid, getGid(t, exportPath))
}

func TestLookupServiceOwner(t *testing.T) {
	varDir := filepath.Join(t.TempDir(), "var")
	require.NoError(t, os.Mkdir(varDir, 0750))
	require.NoError(t, os.Chown(varDir, nonRootOwner(), -1))
	notExist := filepath.Join(t.TempDir(), "not-exist")

	testCases := []struct {
		name     string
		defaults []string
		expected int
	}{
		{"var directory owner", []string{notExist, varDir}, int(getStat(t, varDir).Uid)},
		{"root-owned var directory", []string{"/"}, -1},
		{"no var directory", []string{notExist}, -1},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			setArtifactoryVarDirs(t, "", testCase.defaults...)
			assert.Equal(t, testCase.expected, lookupServiceOwner())
		})
	}
}

func TestExpectedExportOwners(t *testing.T) {
	testCases := []struct {
		name               string
		serviceOwner, euid int
		expected           []int
	}{
		{"known service user, CLI as root", 1030, 0, []int{0, 1030}},
		{"known service user, CLI as another user", 1030, 502, []int{0, 502, 1030}},
		{"root service user", 0, 0, []int{0}},
		// Root records the owner of the export root right after the export, the owner at archive time isn't trusted
		{"unknown service user, CLI as root", -1, 0, []int{0}},
		{"unknown service user, CLI as the service user", -1, 1030, []int{0, 1030}},
		// A non-root CLI must not trust the owner of the export root, which a group member may replace
		{"unknown service user, non-root CLI", -1, 502, []int{0, 502}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.ElementsMatch(t, testCase.expected, expectedExportOwners(testCase.serviceOwner, testCase.euid))
		})
	}
}

// os.FileInfo with a custom owner, to test the owner checks without root.
type ownedFileInfo struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (i ownedFileInfo) Sys() any { return i.stat }

func TestAllowedExportOwners(t *testing.T) {
	assert.ElementsMatch(t, expectedExportOwners(-1, os.Geteuid()), allowedExportOwners(-1))
	assert.ElementsMatch(t, expectedExportOwners(1030, os.Geteuid()), allowedExportOwners(1030))
}

// Without a known service user, the owner of the export root at archive time is never trusted (root included, see
// TestArchiveConfigRejectsReplacedExportRoot for root's recorded owner).
func TestNonRootRejectsForeignExportRoot(t *testing.T) {
	path := t.TempDir()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	foreignRoot := ownedFileInfo{FileInfo: info, stat: &syscall.Stat_t{Uid: otherUid()}}

	err = validateEntryOwner(path, foreignRoot, allowedExportOwners(-1))
	assert.ErrorContains(t, err, fmt.Sprintf("uid %d", otherUid()))
}

func TestValidateEntryOwner(t *testing.T) {
	path := t.TempDir()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	ownedBy := func(uid uint32) os.FileInfo {
		return ownedFileInfo{FileInfo: info, stat: &syscall.Stat_t{Uid: uid}}
	}

	assert.NoError(t, validateEntryOwner(path, ownedBy(1030), []int{0, 1030}))
	assert.NoError(t, validateEntryOwner(path, ownedBy(4242), nil), "no allowed owners means the owner isn't checked")
	err = validateEntryOwner(path, ownedBy(4242), []int{0, 1030})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), path)
		assert.Contains(t, err.Error(), "uid 4242")
		assert.Contains(t, err.Error(), "1030")
	}
}

func TestValidateOwner(t *testing.T) {
	dir := t.TempDir()
	info, err := os.Lstat(dir)
	require.NoError(t, err)
	assert.NoError(t, validateOwner(dir, info))

	otherOwner := &syscall.Stat_t{Uid: otherUid()}
	err = validateOwner(dir, ownedFileInfo{FileInfo: info, stat: otherOwner})
	assert.ErrorContains(t, err, "is owned by another user")
	assert.ErrorContains(t, err, dir)
}

func TestValidateExportBaseDirRejectsOtherOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Changing the owner of a directory requires root")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, exportDirMode))
	assert.NoError(t, validateExportBaseDir(dir))
	require.NoError(t, os.Chown(dir, 65534, -1))
	assert.ErrorContains(t, validateExportBaseDir(dir), "is owned by another user")
}

// Change the owner of the export directory and all of its entries.
func chownTree(t *testing.T, dir string, uid int) {
	require.NoError(t, lchownTree(dir, uid))
}

// Like chownTree, for goroutines other than the test's (mock server handlers), which must not call require.
func lchownTree(dir string, uid int) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, root.Close())
	}()
	return fs.WalkDir(root.FS(), ".", func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return root.Lchown(path, uid, -1)
	})
}

func TestArchiveConfigRejectsForeignOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Changing the owner of a file requires root")
	}
	const serviceUid, otherUid = 4242, 4343
	keyPath := filepath.Join("etc", "security", "artifactory.key")

	t.Run("group applied from the var directory", func(t *testing.T) {
		exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
		defer cleanupFunc()
		require.NoError(t, os.Chown(filepath.Join(exportDir, keyPath), serviceUid, -1))

		// Owned by the var directory owner
		_, err := archiveConfig(exportDir, "<config></config>", serviceUid)
		assert.NoError(t, err)

		// Owned by a user other than root, the CLI user and the var directory owner
		buf, err := archiveConfig(exportDir, "<config></config>", otherUid)
		assert.Nil(t, buf)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), filepath.Join(exportDir, keyPath))
			assert.Contains(t, err.Error(), fmt.Sprintf("uid %d", serviceUid))
			assert.Contains(t, err.Error(), strconv.Itoa(otherUid))
		}
	})

	t.Run("unknown service user", func(t *testing.T) {
		exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
		defer cleanupFunc()

		// Without a recorded owner, root trusts only root, not the owner of the export root at archive time
		chownTree(t, exportDir, serviceUid)
		_, err := archiveConfig(exportDir, "<config></config>", -1)
		assert.ErrorContains(t, err, fmt.Sprintf("uid %d", serviceUid))
	})

	t.Run("typo file planted by another user", func(t *testing.T) {
		exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
		defer cleanupFunc()
		etc := filepath.Join(exportDir, "etc")
		require.NoError(t, os.Rename(filepath.Join(etc, "access.bootstrap.json"), filepath.Join(etc, "access.boostrap.json")))
		require.NoError(t, os.Chown(filepath.Join(etc, "access.boostrap.json"), otherUid, -1))

		_, err := archiveConfig(exportDir, "<config></config>", -1)
		assert.ErrorContains(t, err, fmt.Sprintf("uid %d", otherUid))
		assert.NoFileExists(t, filepath.Join(etc, "access.bootstrap.json"), "a foreign typo file must not be renamed")
	})
}

func TestCreateExportPathSetsServiceGroup(t *testing.T) {
	setEuid(t, 0)
	baseDir := t.TempDir()
	serviceGid := getOtherGid(t, getGid(t, baseDir))
	jfrogHome := t.TempDir()
	varDir := createVarDir(t, filepath.Join(jfrogHome, "artifactory", "var"), serviceGid)
	setArtifactoryVarDirs(t, jfrogHome)

	transferConfigBase := NewTransferConfigCommand(nil, nil)
	transferConfigBase.sourceWorkingDir = baseDir
	exportPath, unsetTempDir, err := transferConfigBase.createExportPath()
	unsetTempDir()
	require.NoError(t, err)
	assert.Equal(t, serviceGid, getGid(t, exportPath))
	assert.Equal(t, int(getStat(t, varDir).Uid), transferConfigBase.serviceOwner)
	info, err := os.Stat(exportPath)
	require.NoError(t, err)
	assert.Equal(t, exportDirMode, info.Mode().Perm())
	assert.NotZero(t, info.Mode()&os.ModeSticky, "group members must not rename or remove the entries of others")
}

func TestCreateExportPathKeepsSetgid(t *testing.T) {
	baseDir := createSetgidBaseDir(t)

	transferConfigBase := NewTransferConfigCommand(nil, nil)
	transferConfigBase.sourceWorkingDir = baseDir
	exportPath, unsetTempDir, err := transferConfigBase.createExportPath()
	unsetTempDir()
	require.NoError(t, err)
	info, err := os.Stat(exportPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSetgid, "entries created by Artifactory must keep the group chosen by the operator")
	assert.NotZero(t, info.Mode()&os.ModeSticky, "group members must not rename or remove the entries of others")
	assert.Equal(t, exportDirMode, info.Mode().Perm())
	assert.Equal(t, getGid(t, baseDir), getGid(t, exportPath))
	assert.Equal(t, -1, transferConfigBase.serviceOwner, "no var directory, the service user is unknown")
}

// With a setgid base directory, the group chosen by the operator is kept, but root still learns the service user from the var directory.
func TestCreateExportPathSetgidWithVarDir(t *testing.T) {
	baseDir := createSetgidBaseDir(t)
	varDir := createVarDir(t, filepath.Join(t.TempDir(), "var"), getOtherGid(t, getGid(t, baseDir)))
	setArtifactoryVarDirs(t, "", varDir)

	testCases := []struct {
		name     string
		euid     int
		expected int
	}{
		{"root", 0, int(getStat(t, varDir).Uid)},
		// Not root, only root and the CLI user are trusted
		{"not root", 4321, -1},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			setEuid(t, testCase.euid)
			transferConfigBase := NewTransferConfigCommand(nil, nil)
			transferConfigBase.sourceWorkingDir = baseDir
			exportPath, unsetTempDir, err := transferConfigBase.createExportPath()
			unsetTempDir()
			require.NoError(t, err)
			assert.Equal(t, testCase.expected, transferConfigBase.serviceOwner)
			assert.Equal(t, getGid(t, baseDir), getGid(t, exportPath), "the group chosen by the operator must be kept")
			info, err := os.Stat(exportPath)
			require.NoError(t, err)
			assert.NotZero(t, info.Mode()&os.ModeSetgid)
			assert.NotZero(t, info.Mode()&os.ModeSticky)
		})
	}
}

// Without a known service user, root records the owner of the export root right after the export.
func TestExportSourceArtifactoryRecordsExportOwner(t *testing.T) {
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// jfrog-ignore - false positive: test-only mock server, the export path is the temp dir created by this test.
		assert.NoError(t, os.Mkdir(filepath.Join(transferExportPathFromRequest(t, r), "20260101.000000"), 0750))
	})
	defer testServer.Close()
	// Owned by a user other than the creator of the export root when running as root
	varDir := filepath.Join(t.TempDir(), "var")
	require.NoError(t, os.Mkdir(varDir, 0750))
	require.NoError(t, os.Chown(varDir, nonRootOwner(), -1))

	testCases := []struct {
		name    string
		euid    int
		varDirs []string
		// Return the expected service owner, given the export root
		expected func(exportDir string) int
	}{
		{"root, unknown service user", 0, nil, func(exportDir string) int { return int(getStat(t, exportDir).Uid) }},
		{"root, service user from the var directory", 0, []string{varDir}, func(string) int { return int(getStat(t, varDir).Uid) }},
		{"not root", 4321, nil, func(string) int { return -1 }},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			setEuid(t, testCase.euid)
			setArtifactoryVarDirs(t, "", testCase.varDirs...)
			transferConfigCmd := createTransferConfigCommand(t, serverDetails, nil).SetSourceWorkingDir(t.TempDir())
			exportDir, cleanUp, err := transferConfigCmd.exportSourceArtifactory()
			require.NoError(t, err)
			defer func() {
				assert.NoError(t, cleanUp())
			}()
			assert.Equal(t, testCase.expected(exportDir), transferConfigCmd.serviceOwner)
		})
	}
}

// A group member who replaces the export root after the export (rename and plant) is rejected by root, which recorded the
// owner of the export root right after the export.
func TestArchiveConfigRejectsReplacedExportRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Changing the owner of a file requires root")
	}
	const replacingUid = 4343
	serviceUid := int(otherUid())
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Like an export written by the Artifactory service user
		exportRoot := filepath.Join(transferExportPathFromRequest(t, r), "20260101.000000")
		// jfrog-ignore - false positive: test-only mock server, the export path is the temp dir created by this test.
		assert.NoError(t, os.Mkdir(exportRoot, 0750))
		// jfrog-ignore - false positive: test-only mock server, the export path is the temp dir created by this test.
		assert.NoError(t, biutils.CopyDir(filepath.Join("..", "testdata", "artifactory_export"), exportRoot, true, nil))
		// jfrog-ignore - false positive: test-only mock server, the export path is the temp dir created by this test.
		assert.NoError(t, lchownTree(exportRoot, serviceUid))
	})
	defer testServer.Close()

	// The documented setup: a setgid --source-working-dir, and no var directory on this machine
	transferConfigCmd := createTransferConfigCommand(t, serverDetails, nil).SetSourceWorkingDir(createSetgidBaseDir(t))
	exportDir, cleanUp, err := transferConfigCmd.exportSourceArtifactory()
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, cleanUp())
	}()
	require.Equal(t, serviceUid, transferConfigCmd.serviceOwner, "the owner of the export root must be recorded right after the export")
	_, err = archiveConfig(exportDir, "<config></config>", transferConfigCmd.serviceOwner)
	assert.NoError(t, err)

	// Another user now owns the export root, as if it renamed the export and planted its own
	chownTree(t, exportDir, replacingUid)
	buf, err := archiveConfig(exportDir, "<config></config>", transferConfigCmd.serviceOwner)
	assert.Nil(t, buf)
	assert.ErrorContains(t, err, fmt.Sprintf("uid %d", replacingUid))
}

func TestExportSourceArtifactoryWarnsOnCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("Root can remove any directory")
	}
	var lockedDir string
	testServer, serverDetails, _ := commonTests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Like an export written by the Artifactory service user, which the CLI user can't remove
		exportPath := filepath.Join(transferExportPathFromRequest(t, r), "20260101.000000")
		// jfrog-ignore - false positive: test-only mock server, the export path is the temp dir created by this test.
		assert.NoError(t, os.MkdirAll(filepath.Join(exportPath, "etc"), 0700))
		// jfrog-ignore - false positive: test-only mock server, the export path is the temp dir created by this test.
		assert.NoError(t, os.WriteFile(filepath.Join(exportPath, "etc", "artifactory.key"), nil, 0600))
		lockedDir = filepath.Join(exportPath, "etc")
		// jfrog-ignore - false positive: test-only mock server, the export path is the temp dir created by this test.
		assert.NoError(t, os.Chmod(lockedDir, 0500))
	})
	defer testServer.Close()
	tmpDir, createTempDirCallback := tests.CreateTempDirWithCallbackAndAssert(t)
	defer createTempDirCallback()
	logs := captureLog(t)

	transferConfigCmd := createTransferConfigCommand(t, serverDetails, nil).SetSourceWorkingDir(tmpDir)
	exportDir, cleanUp, err := transferConfigCmd.exportSourceArtifactory()
	require.NoError(t, err)
	exportPath := filepath.Dir(exportDir)

	assert.Error(t, cleanUp())
	assert.Contains(t, logs.String(), exportPath)
	assert.Contains(t, logs.String(), "delete it")

	// Allow the test cleanup to remove the export
	// jfrog-ignore - false positive: test-only mock server, the export path is the temp dir created by this test.
	assert.NoError(t, os.Chmod(lockedDir, 0700))
	assert.NoError(t, cleanUp())
}
