package ioutils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testOrigFileName   = ".npmrc"
	testBackupFileName = "jfrog.npmrc.backup"
	testOrigContent    = "registry=https://original.example/\n"
	testUpdatedContent = "registry=https://updated.example/\n"
)

func TestBackupFileAndRestore(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, testOrigFileName)
	require.NoError(t, os.WriteFile(origPath, []byte(testOrigContent), 0600))

	restore, err := BackupFile(origPath, testBackupFileName)
	require.NoError(t, err)
	require.NotNil(t, restore)
	assertFileContent(t, filepath.Join(dir, testBackupFileName), testOrigContent)

	require.NoError(t, os.WriteFile(origPath, []byte(testUpdatedContent), 0600))
	require.NoError(t, restore())
	assertFileContent(t, origPath, testOrigContent)
	assert.NoFileExists(t, filepath.Join(dir, testBackupFileName))
}

func TestBackupFileNoOriginalFile(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, testOrigFileName)

	restore, err := BackupFile(origPath, testBackupFileName)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(dir, testBackupFileName))

	require.NoError(t, os.WriteFile(origPath, []byte(testUpdatedContent), 0600))
	require.NoError(t, restore())
	assert.NoFileExists(t, origPath)
}

// A stale regular backup file (e.g. from a previous interrupted run) must be replaced, not appended to or partially overwritten.
func TestBackupFileReplacesStaleRegularBackup(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, testOrigFileName)
	backupPath := filepath.Join(dir, testBackupFileName)
	require.NoError(t, os.WriteFile(origPath, []byte(testOrigContent), 0600))
	// Longer than the original content, to verify the backup is not left with trailing stale bytes.
	require.NoError(t, os.WriteFile(backupPath, []byte("stale content that is longer than the original file content\n"), 0600))

	restore, err := BackupFile(origPath, testBackupFileName)
	require.NoError(t, err)
	assertFileContent(t, backupPath, testOrigContent)

	require.NoError(t, os.WriteFile(origPath, []byte(testUpdatedContent), 0600))
	require.NoError(t, restore())
	assertFileContent(t, origPath, testOrigContent)
}

// A symlink planted at the backup path must not be followed: the symlink target must remain untouched.
func TestBackupFileRefusesSymlinkAtBackupPath(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	origPath := filepath.Join(dir, testOrigFileName)
	backupPath := filepath.Join(dir, testBackupFileName)
	require.NoError(t, os.WriteFile(origPath, []byte(testOrigContent), 0600))

	victimPath := filepath.Join(t.TempDir(), "victim")
	victimContent := "victim content must not change"
	require.NoError(t, os.WriteFile(victimPath, []byte(victimContent), 0600))
	require.NoError(t, os.Symlink(victimPath, backupPath))

	restore, err := BackupFile(origPath, testBackupFileName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
	// The returned restore function must be safe to call on error.
	require.NotNil(t, restore)
	assert.NoError(t, restore())

	assertFileContent(t, victimPath, victimContent)
	assertFileContent(t, origPath, testOrigContent)
	linkInfo, err := os.Lstat(backupPath)
	require.NoError(t, err)
	assert.NotZero(t, linkInfo.Mode()&os.ModeSymlink, "the planted symlink should be left as is")
}

// A dangling symlink planted at the backup path must not be followed to create a new file.
func TestBackupFileRefusesDanglingSymlinkAtBackupPath(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	origPath := filepath.Join(dir, testOrigFileName)
	require.NoError(t, os.WriteFile(origPath, []byte(testOrigContent), 0600))

	victimPath := filepath.Join(t.TempDir(), "should-not-be-created")
	require.NoError(t, os.Symlink(victimPath, filepath.Join(dir, testBackupFileName)))

	_, err := BackupFile(origPath, testBackupFileName)
	require.Error(t, err)
	assert.NoFileExists(t, victimPath)
}

func TestBackupFileRefusesDirectoryAtBackupPath(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, testOrigFileName)
	require.NoError(t, os.WriteFile(origPath, []byte(testOrigContent), 0600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, testBackupFileName), 0700))

	_, err := BackupFile(origPath, testBackupFileName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory")
}

// When there was no original file, a pre-existing file at the backup path was not created by BackupFile
// and must not be "restored" into the original path.
func TestRestoreIgnoresForeignBackupWhenNoOriginalFile(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	origPath := filepath.Join(dir, testOrigFileName)
	backupPath := filepath.Join(dir, testBackupFileName)

	secretPath := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("secret"), 0600))
	require.NoError(t, os.Symlink(secretPath, backupPath))

	restore, err := BackupFile(origPath, testBackupFileName)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(origPath, []byte(testUpdatedContent), 0600))
	require.NoError(t, restore())
	assert.NoFileExists(t, origPath)
	assertFileContent(t, secretPath, "secret")
}

// If the backup file is swapped with a symlink before restoring, the restore must fail instead of following it.
func TestRestoreRefusesSymlinkedBackup(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	origPath := filepath.Join(dir, testOrigFileName)
	backupPath := filepath.Join(dir, testBackupFileName)
	require.NoError(t, os.WriteFile(origPath, []byte(testOrigContent), 0600))

	restore, err := BackupFile(origPath, testBackupFileName)
	require.NoError(t, err)

	secretPath := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("secret"), 0600))
	require.NoError(t, os.Remove(backupPath))
	require.NoError(t, os.Symlink(secretPath, backupPath))
	require.NoError(t, os.WriteFile(origPath, []byte(testUpdatedContent), 0600))

	err = restore()
	require.Error(t, err)
	assertFileContent(t, origPath, testUpdatedContent)
	assertFileContent(t, secretPath, "secret")
}

func skipIfWindows(t *testing.T) {
	if coreutils.IsWindows() {
		t.Skip("Creating symlinks requires elevated privileges on Windows")
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, expected, string(content))
}
