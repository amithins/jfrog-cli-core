package transferconfig

import (
	"archive/zip"
	"bytes"
	biutils "github.com/jfrog/build-info-go/utils"
	"github.com/jfrog/jfrog-cli-core/v2/utils/tests"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"golang.org/x/exp/slices"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveConfig(t *testing.T) {
	expectedConfigXml := "<config></config>"
	exportPath := filepath.Join("..", "testdata", "artifactory_export")
	// Trust the owner of the checkout, like the owner of the export root recorded by root right after the export
	exportInfo, err := os.Lstat(exportPath)
	require.NoError(t, err)
	serviceOwner, _ := fileOwnerUid(exportInfo)
	buf, err := archiveConfig(exportPath, expectedConfigXml, serviceOwner)
	require.NoError(t, err)

	zipReader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	assert.NoError(t, err)

	expectedFiles := append(slices.Clone(neededFiles), "artifactory.config.xml")
	assert.Len(t, zipReader.File, len(expectedFiles))
	for _, zipFile := range zipReader.File {
		assert.Contains(t, expectedFiles, zipFile.Name)
		if zipFile.Name == "artifactory.config.xml" {
			f, err := zipFile.Open()
			assert.NoError(t, err)
			defer func(file io.ReadCloser) {
				assert.NoError(t, file.Close())
			}(f)
			actualConfigXml, err := io.ReadAll(f)
			assert.NoError(t, err)
			assert.Equal(t, expectedConfigXml, string(actualConfigXml))
		}
	}
}

func initHandleTypoInAccessBootstrapTest(t *testing.T) (exportDir string, cleanupFunc func()) {
	exportDir, err := fileutils.CreateTempDir()
	assert.NoError(t, err)
	testDataPath := filepath.Join("..", "testdata", "artifactory_export")
	assert.NoError(t, biutils.CopyDir(testDataPath, exportDir, true, nil))
	cleanupFunc = func() {
		assert.NoError(t, fileutils.RemoveTempDir(exportDir), "Couldn't remove temp dir")
	}
	return
}

// Open the export directory as an export tree, without owner checks. The returned function must run before the directory
// is removed (required on Windows).
func openExportTree(t *testing.T, exportDir string) (*exportTree, func()) {
	root, err := os.OpenRoot(exportDir)
	require.NoError(t, err)
	return &exportTree{root: root}, func() {
		assert.NoError(t, root.Close())
	}
}

func TestHandleTypoInAccessBootstrapNoTypo(t *testing.T) {
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()
	tree, closeTree := openExportTree(t, exportDir)
	defer closeTree()

	// Test access.bootstrap.json
	assert.NoError(t, handleTypoInAccessBootstrap(tree))
	assert.FileExists(t, filepath.Join(exportDir, "etc", "access.bootstrap.json"))
}

func TestHandleTypoInAccessBootstrapWithTypo(t *testing.T) {
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()
	tree, closeTree := openExportTree(t, exportDir)
	defer closeTree()

	accessBootstrapPath := filepath.Join(exportDir, "etc", "access.bootstrap.json")
	assert.NoError(t, fileutils.MoveFile(accessBootstrapPath, filepath.Join(exportDir, "etc", "access.boostrap.json")))

	assert.NoError(t, handleTypoInAccessBootstrap(tree))
	assert.FileExists(t, accessBootstrapPath)
}

func TestHandleTypoInAccessBootstrapRejectsSymlinkedTypoFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Symlink creation requires privileges on Windows")
	}
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()
	tree, closeTree := openExportTree(t, exportDir)
	defer closeTree()

	planted := filepath.Join(t.TempDir(), "planted.json")
	assert.NoError(t, os.WriteFile(planted, []byte("{}"), 0600))
	assert.NoError(t, os.Remove(filepath.Join(exportDir, "etc", "access.bootstrap.json")))
	assert.NoError(t, os.Symlink(planted, filepath.Join(exportDir, "etc", "access.boostrap.json")))

	assert.ErrorContains(t, handleTypoInAccessBootstrap(tree), "symbolic link")
	assert.NoFileExists(t, filepath.Join(exportDir, "etc", "access.bootstrap.json"))
}

func TestArchiveConfigRejectsSymlinkedNeededFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Symlink creation requires privileges on Windows")
	}
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()

	planted := filepath.Join(t.TempDir(), "planted.key")
	assert.NoError(t, os.WriteFile(planted, []byte("not-a-real-secret"), 0600))
	keyPath := filepath.Join(exportDir, "etc", "security", "artifactory.key")
	assert.NoError(t, os.Remove(keyPath))
	assert.NoError(t, os.Symlink(planted, keyPath))

	buf, err := archiveConfig(exportDir, "<config></config>", -1)
	assert.ErrorContains(t, err, "symbolic link")
	assert.Nil(t, buf)
}

func TestArchiveConfigRejectsSymlinkedExportDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Symlink creation requires privileges on Windows")
	}
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()

	link := filepath.Join(t.TempDir(), "20260101.000000")
	assert.NoError(t, os.Symlink(exportDir, link))

	buf, err := archiveConfig(link, "<config></config>", -1)
	assert.ErrorContains(t, err, "symbolic link")
	assert.Nil(t, buf)
}

func TestArchiveConfigWarnsOnWorldWritableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()
	_, logs, previousLog := tests.RedirectLogOutputToBuffer()
	defer log.SetLogger(previousLog)

	bootstrapPath := filepath.Join(exportDir, "etc", "access.bootstrap.json")
	assert.NoError(t, os.Chmod(bootstrapPath, 0666))

	// The export directory is 0770, so other users can't reach the entry: warn and continue
	buf, err := archiveConfig(exportDir, "<config></config>", -1)
	assert.NoError(t, err)
	assert.NotNil(t, buf)
	assert.Contains(t, logs.String(), bootstrapPath)
	assert.Contains(t, logs.String(), "umask")
}

func TestArchiveConfigPermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("Root can read any file")
	}
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()

	// Like artifactory.key, which Artifactory writes with mode 0600 for its service user only
	keyPath := filepath.Join(exportDir, "etc", "security", "artifactory.key")
	assert.NoError(t, os.Chmod(keyPath, 0))
	defer func() {
		assert.NoError(t, os.Chmod(keyPath, 0600))
	}()

	buf, err := archiveConfig(exportDir, "<config></config>", -1)
	assert.Nil(t, buf)
	if assert.Error(t, err) {
		assert.ErrorIs(t, err, fs.ErrPermission)
		assert.Contains(t, err.Error(), keyPath)
		assert.Contains(t, err.Error(), "as root or as the Artifactory service user")
	}
}

func TestValidateExportBaseDirRejectsWorldAccessible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	assert.NoError(t, os.Chmod(dir, 0777))
	err := validateExportBaseDir(dir)
	assert.ErrorContains(t, err, "accessible to other users")
	// The CLI creates the base directory, the Artifactory service umask is not the cause
	assert.NotContains(t, err.Error(), "umask")
	assert.NoError(t, os.Chmod(dir, 0705))
	assert.ErrorContains(t, validateExportBaseDir(dir), "accessible to other users")
	assert.NoError(t, os.Chmod(dir, 0770))
	assert.NoError(t, validateExportBaseDir(dir))
}

func TestHandleTypoInAccessBootstrapNotExist(t *testing.T) {
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()
	tree, closeTree := openExportTree(t, exportDir)
	defer closeTree()

	assert.NoError(t, os.Remove(filepath.Join(exportDir, "etc", "access.bootstrap.json")))
	assert.ErrorContains(t, handleTypoInAccessBootstrap(tree), "the file was not found or is not accessible")
}

func TestArchiveConfigRejectsSymlinkedEtcDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Symlink creation requires privileges on Windows")
	}
	exportDir, cleanupFunc := initHandleTypoInAccessBootstrapTest(t)
	defer cleanupFunc()

	// Move etc outside the export directory and link to it
	outside := filepath.Join(t.TempDir(), "etc")
	assert.NoError(t, os.Rename(filepath.Join(exportDir, "etc"), outside))
	assert.NoError(t, os.Symlink(outside, filepath.Join(exportDir, "etc")))

	buf, err := archiveConfig(exportDir, "<config></config>", -1)
	assert.ErrorContains(t, err, "symbolic link")
	assert.Nil(t, buf)
}
