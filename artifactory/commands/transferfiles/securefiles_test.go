//go:build !windows

package transferfiles

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-cli-core/v2/utils/reposnapshot"
	"github.com/jfrog/jfrog-cli-core/v2/utils/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUniqueFilePathSkipsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	prefix := func() string { return "errors" }
	predicted := filepath.Join(dir, "errors-0.json")
	require.NoError(t, os.Symlink(victim, predicted))

	chosen, err := getUniqueErrorOrDelayFilePath(dir, prefix)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "errors-1.json"), chosen)

	// Even when writing to a path that is a symlink, the link must be replaced and not followed.
	src := filepath.Join(dir, "src")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0600))
	require.NoError(t, reposnapshot.MoveFileAtomic(src, predicted))
	_, err = os.Lstat(victim)
	assert.True(t, os.IsNotExist(err))
	info, err := os.Lstat(predicted)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestEnsureOwnerOnlyDirUnderPermissiveUmask(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	base := t.TempDir()
	existing := filepath.Join(base, "existing")
	require.NoError(t, os.Mkdir(existing, 0777))
	require.NoError(t, os.Chmod(existing, 0777))
	nested := filepath.Join(existing, "a")

	require.NoError(t, ensureOwnerOnlyDir(existing))
	require.NoError(t, ensureOwnerOnlyDir(nested))
	require.NoError(t, makeDirIfDoesNotExists(filepath.Join(nested, "errors")))
	for _, d := range []string{existing, nested, filepath.Join(nested, "errors")} {
		info, err := os.Stat(d)
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0077, d)
	}
}

func TestKeepRecordsOfRepoRejectsOtherRepos(t *testing.T) {
	records := []api.FileRepresentation{
		{Repo: "repo-a", Path: "p", Name: "ok"},
		{Repo: "other-repo", Path: "p", Name: "planted"},
	}
	kept := keepRecordsOfRepo(records, "repo-a", "f.json", func(r api.FileRepresentation) string { return r.Repo })
	require.Len(t, kept, 1)
	assert.Equal(t, "ok", kept[0].Name)
}

// A transfer root relocated to another volume through a symlink must keep working, and its target must be tightened.
func TestInitTransferDirFollowsSymlinkedRoot(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	home := t.TempDir()
	t.Setenv(coreutils.HomeDir, home)
	target := filepath.Join(t.TempDir(), "bigdisk-transfer")
	require.NoError(t, os.Mkdir(target, 0777))
	require.NoError(t, os.Symlink(target, filepath.Join(home, coreutils.JfrogTransferDirName)))

	require.NoError(t, (&TransferFilesCommand{}).initTransferDir())
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
}

func TestInitTransferDirRejectsRootSymlinkToFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv(coreutils.HomeDir, home)
	target := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(target, nil, 0600))
	require.NoError(t, os.Symlink(target, filepath.Join(home, coreutils.JfrogTransferDirName)))

	assert.Error(t, (&TransferFilesCommand{}).initTransferDir())
}

// Directories below the root are always created by the CLI, so a symlink there is still refused.
func TestMakeDirIfDoesNotExistsRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.Mkdir(target, 0700))
	link := filepath.Join(base, "errors")
	require.NoError(t, os.Symlink(target, link))

	assert.Error(t, makeDirIfDoesNotExists(link))
}

// Symlinks in the errors directory must not be read as records, whether they point outside the directory or dangle.
func TestGetErrorsFilesSkipsSymlinks(t *testing.T) {
	cleanUpJfrogHome, err := tests.SetJfrogHome()
	require.NoError(t, err)
	defer cleanUpJfrogHome()

	writeEmptyErrorsFile(t, repo1Key, true, 0, 0)
	errorsDirPath, err := getJfrogTransferRepoRetryableDir(repo1Key)
	require.NoError(t, err)
	outside := filepath.Join(t.TempDir(), "outside.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"errors":[]}`), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(errorsDirPath, "planted-0.json")))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing.json"), filepath.Join(errorsDirPath, "dangling-0.json")))

	paths, err := getErrorsFiles([]string{repo1Key}, true)
	require.NoError(t, err)
	require.Len(t, paths, 1)
	info, err := os.Lstat(paths[0])
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
}
