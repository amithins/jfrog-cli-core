package transferconfig

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

// Needed files for config import in SaaS
var neededFiles = []string{
	"artifactory.properties",
	filepath.Join("etc", "access.bootstrap.json"),
	filepath.Join("etc", "security", "artifactory.key"),
	filepath.Join("etc", "security", "url.signing.key"),
}

// The export directory written by Artifactory. Its entries are accessed only through root, so they can't be resolved outside of it.
type exportTree struct {
	root *os.Root
	// The owners allowed for the entries of the tree. Empty if the owner isn't checked (Windows).
	allowedOwners []int
}

// Archive the exportPath directory and the input artifactory.config.xml.
// serviceOwner is the uid of the Artifactory service user if known (root only: the owner of the var directory, or else the
// owner of the export root recorded right after the export), or -1.
func archiveConfig(exportPath, configXml string, serviceOwner int) (buffer *bytes.Buffer, retErr error) {
	buffer = &bytes.Buffer{}
	writer := zip.NewWriter(buffer)
	writer.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestCompression)
	})
	defer func() {
		retErr = errors.Join(retErr, errorutils.CheckError(writer.Close()))
	}()

	// The export directory itself must not be a symlink, os.OpenRoot would follow it
	exportInfo, err := os.Lstat(exportPath)
	if err != nil {
		return nil, errorutils.CheckError(wrapPermissionErr(exportPath, err))
	}
	if err = validateExportEntryInfo(exportPath, exportInfo, true); err != nil {
		return nil, err
	}
	tree := &exportTree{allowedOwners: allowedExportOwners(serviceOwner)}
	if err = validateEntryOwner(exportPath, exportInfo, tree.allowedOwners); err != nil {
		return nil, err
	}

	// Open the export directory once and access all entries through it
	if tree.root, err = os.OpenRoot(exportPath); err != nil {
		return nil, errorutils.CheckError(wrapPermissionErr(exportPath, err))
	}
	defer func() {
		_ = tree.root.Close()
	}()
	rootInfo, err := tree.root.Stat(".")
	if err != nil {
		return nil, errorutils.CheckError(err)
	}
	if !os.SameFile(exportInfo, rootInfo) {
		return nil, errorutils.CheckErrorf("refusing to read the export directory '%s': it was replaced while being read", exportPath)
	}
	if _, err = tree.lstatEntry("etc", true); err != nil {
		return nil, err
	}

	if err = handleTypoInAccessBootstrap(tree); err != nil {
		return nil, err
	}

	for _, neededFile := range neededFiles {
		log.Debug("Archiving " + neededFile)
		fileContent, err := readExportFile(tree, neededFile)
		if err != nil {
			if os.IsNotExist(err) && strings.Contains(neededFile, "security") {
				log.Info(neededFile + " file is missing in the source Artifactory server. Skipping...")
				continue
			}
			return nil, errorutils.CheckError(err)
		}
		fileWriter, err := writer.Create(neededFile)
		if err != nil {
			return nil, errorutils.CheckError(err)
		}
		if _, retErr = fileWriter.Write(fileContent); errorutils.CheckError(retErr) != nil {
			return
		}
	}
	fileWriter, err := writer.Create("artifactory.config.xml")
	if err != nil {
		return buffer, errorutils.CheckError(err)
	}
	_, retErr = fileWriter.Write([]byte(configXml))
	return
}

// The export directory must be writable by the Artifactory service account (group), but never accessible to others.
const exportDirMode os.FileMode = 0770

// Allows tests to run the root-only logic as another user.
var geteuid = os.Geteuid

// A setgid parent of the export directory means the operator already chose the group of the export directory.
func parentHasSetgid(exportPath string) bool {
	parent, err := os.Stat(filepath.Dir(exportPath))
	return err == nil && parent.Mode()&os.ModeSetgid != 0
}

// Ensure the base export directory is a real directory owned by the current user, with no permissions for others.
func validateExportBaseDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errorutils.CheckError(err)
	}
	if runtime.GOOS != "windows" && info.IsDir() && info.Mode().Perm()&0007 != 0 {
		return errorutils.CheckErrorf("refusing to use the export directory '%s': it is accessible to other users (mode %s)", path, info.Mode().Perm())
	}
	if err = validateExportEntryInfo(path, info, true); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	return validateOwner(path, info)
}

// Lstat the given entry of the export tree and ensure it is not a symlink, has the expected type and an allowed owner.
// Errors returned by Lstat are returned as-is (unless access was denied), so callers may use os.IsNotExist.
func (et *exportTree) lstatEntry(name string, isDir bool) (os.FileInfo, error) {
	path := filepath.Join(et.root.Name(), name)
	info, err := et.root.Lstat(name)
	if err != nil {
		return nil, wrapPermissionErr(path, err)
	}
	if err = validateExportEntryInfo(path, info, isDir); err != nil {
		return nil, err
	}
	return info, validateEntryOwner(path, info, et.allowedOwners)
}

// Make a permission error on an export entry actionable. Other errors are returned as-is.
func wrapPermissionErr(path string, err error) error {
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return errorutils.CheckError(fmt.Errorf("couldn't access '%s' in the Artifactory export. "+
		"Run this command as root or as the Artifactory service user (shared.user in system.yaml, 'artifactory' by default): %w", path, err))
}

func validateExportEntryInfo(path string, info os.FileInfo, isDir bool) error {
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		return errorutils.CheckErrorf("refusing to read the export entry '%s': it is a symbolic link", path)
	}
	if isDir && !mode.IsDir() {
		return errorutils.CheckErrorf("refusing to read the export entry '%s': it is not a directory", path)
	}
	if !isDir && !mode.IsRegular() {
		return errorutils.CheckErrorf("refusing to read the export entry '%s': it is not a regular file", path)
	}
	// Not an error: the export directory created by the CLI is 0770, so other users can't reach the entries below it
	if runtime.GOOS != "windows" && mode.Perm()&0002 != 0 {
		log.Warn(fmt.Sprintf("The export entry '%s' is writable by other users (mode %s). "+
			"Set a restrictive umask for the Artifactory service, for example UMASK=0027 for its Tomcat", path, mode.Perm()))
	}
	return nil
}

// Read a needed file from the export tree after validating every path component below its root.
// Lstat errors are returned as-is, so callers may use os.IsNotExist.
func readExportFile(tree *exportTree, relativePath string) ([]byte, error) {
	current := ""
	parts := strings.Split(relativePath, string(filepath.Separator))
	for _, dir := range parts[:len(parts)-1] {
		current = filepath.Join(current, dir)
		if _, err := tree.lstatEntry(current, true); err != nil {
			return nil, err
		}
	}
	info, err := tree.lstatEntry(relativePath, false)
	if err != nil {
		return nil, err
	}
	filePath := filepath.Join(tree.root.Name(), relativePath)
	file, err := tree.root.Open(relativePath)
	if err != nil {
		return nil, errorutils.CheckError(wrapPermissionErr(filePath, err))
	}
	defer func() {
		_ = file.Close()
	}()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, errorutils.CheckError(err)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, errorutils.CheckErrorf("refusing to read the export entry '%s': it was replaced while being read", filePath)
	}
	content, err := io.ReadAll(file)
	return content, errorutils.CheckError(err)
}

// In some versions of Artifactory, the file access.bootstrap.json has a typo in its name: access.boostrap.json.
// If this is the case, rename the file to the right name.
func handleTypoInAccessBootstrap(tree *exportTree) error {
	accessBootstrap := filepath.Join("etc", "access.bootstrap.json")
	accessBootstrapPath := filepath.Join(tree.root.Name(), accessBootstrap)
	_, err := tree.root.Lstat(accessBootstrap)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return errorutils.CheckError(wrapPermissionErr(accessBootstrapPath, err))
	}
	typo := filepath.Join("etc", "access.boostrap.json")
	if _, err = tree.lstatEntry(typo, false); err == nil {
		err = wrapPermissionErr(accessBootstrapPath, tree.root.Rename(typo, accessBootstrap))
	}
	if err != nil {
		if os.IsNotExist(err) {
			return errorutils.CheckErrorf("%s: the file was not found or is not accessible", accessBootstrapPath)
		}
		return errorutils.CheckError(err)
	}
	return nil
}
