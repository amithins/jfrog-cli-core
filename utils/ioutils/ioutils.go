package ioutils

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"golang.org/x/term"
)

// disallowUsingSavedPassword - Prevent changing username or url without changing the password.
// False if the user changed the username or the url.
func ReadCredentialsFromConsole(details, savedDetails coreutils.Credentials, disallowUsingSavedPassword bool) error {
	if details.GetUser() == "" {
		tempUser := ""
		ScanFromConsole("JFrog username", &tempUser, savedDetails.GetUser())
		details.SetUser(tempUser)
		disallowUsingSavedPassword = true
	}
	if details.GetPassword() == "" {
		password, err := ScanJFrogPasswordFromConsole()
		if err != nil {
			return err
		}
		details.SetPassword(password)
		if details.GetPassword() == "" && !disallowUsingSavedPassword {
			details.SetPassword(savedDetails.GetPassword())
		}
	}

	return nil
}

func ScanJFrogPasswordFromConsole() (string, error) {
	return ScanPasswordFromConsole("JFrog password or Reference Token: ")
}

func ScanPasswordFromConsole(message string) (string, error) {
	fmt.Print(coreutils.PrintLink(message))
	bytePassword, err := term.ReadPassword(int(syscall.Stdin)) //nolint:unconvert
	if err != nil {
		return "", errorutils.CheckError(err)
	}
	// New-line required after the password input:
	log.Output()
	return string(bytePassword), nil
}

func ScanFromConsole(caption string, scanInto *string, defaultValue string) {
	caption = coreutils.PrintLink(caption)
	if defaultValue != "" {
		caption = caption + " [" + defaultValue + "]"
	}
	fmt.Print(caption + ": ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	*scanInto = scanner.Text()
	if *scanInto == "" {
		*scanInto = defaultValue
	}
	*scanInto = strings.TrimSpace(*scanInto)
}

func DoubleWinPathSeparator(filePath string) string {
	return strings.ReplaceAll(filePath, "\\", "\\\\")
}

func UnixToWinPathSeparator(filePath string) string {
	return strings.ReplaceAll(filePath, "/", "\\\\")
}

func WinToUnixPathSeparator(filePath string) string {
	return strings.ReplaceAll(filePath, "\\", "/")
}

// BackupFile creates a backup of the file in filePath. The backup will be found at backupPath.
// The returned restore function can be called to restore the file's state - the file in filePath will be replaced by the backup in backupPath.
// If there is no file at filePath, a backup file won't be created, and the restore function will delete the file at filePath.
// The backup file is always created exclusively. If a symlink or any other non-regular entry already exists at the backup path
// (for example, one committed to an untrusted repository), an error is returned and nothing is written through it.
// A stale regular backup file (e.g. left over from a previous run that was interrupted) is removed and recreated.
// The returned restore function is never nil. If an error is returned, it is a no-op.
func BackupFile(filePath, backupFileName string) (restore func() error, err error) {
	restore = func() error { return nil }
	fileInfo, err := os.Stat(filePath)
	if errorutils.CheckError(err) != nil {
		if os.IsNotExist(err) {
			restore = createRestoreFileFunc(filePath, backupFileName, false)
			err = nil
		}
		return
	}

	if err = cloneFile(filePath, backupFileName, fileInfo.Mode().Perm()); err != nil {
		return
	}
	log.Debug("The file", filePath, "was backed up successfully to", backupFileName)
	restore = createRestoreFileFunc(filePath, backupFileName, true)
	return
}

func cloneFile(origFile, newName string, fileMode os.FileMode) (err error) {
	from, err := os.Open(origFile)
	if errorutils.CheckError(err) != nil {
		return
	}
	defer func() {
		err = errors.Join(err, from.Close())
	}()

	newPath := filepath.Join(filepath.Dir(origFile), newName)
	if err = removeStaleRegularFile(newPath); err != nil {
		return
	}
	// O_EXCL guarantees that a new file is created. The call fails if any entry (including a symlink, even a dangling one) exists at newPath.
	to, err := os.OpenFile(newPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if errorutils.CheckError(err) != nil {
		return
	}
	defer func() {
		err = errors.Join(err, to.Close())
	}()

	if _, err = io.Copy(to, from); err != nil {
		err = errorutils.CheckError(err)
	}
	return
}

// removeStaleRegularFile prepares path to be created exclusively.
// If nothing exists at path, it does nothing. If a regular file exists at path (e.g. a leftover backup from a previous run), it is removed.
// If a symlink or any other non-regular entry exists at path, an error is returned, without following or modifying it.
func removeStaleRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errorutils.CheckError(err)
	}
	if !info.Mode().IsRegular() {
		return errorutils.CheckErrorf("refusing to use '%s' as a backup file: a non-regular file (%s) already exists at this path. Remove it and try again", path, describeFileType(info.Mode()))
	}
	log.Debug("Removing a stale backup file found at", path)
	return errorutils.CheckError(os.Remove(path))
}

func describeFileType(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "directory"
	default:
		return mode.Type().String()
	}
}

// createRestoreFileFunc creates a function for restoring a file from its backup.
// If backupCreated is true, the returned function replaces the file in filePath with the backup in backupPath.
// Otherwise (there was no file at filePath when BackupFile() was called), the function deletes the file at filePath.
// The function does not rely on the presence of a file at backupPath to make this decision, so a backup file that was not created by BackupFile() is never restored.
func createRestoreFileFunc(filePath, backupFileName string, backupCreated bool) func() error {
	return func() error {
		if !backupCreated {
			// We verify the existence of the file in the specified filePath before initiating its deletion in order to prevent errors that might occur when attempting to remove a non-existent file
			fileExists, err := fileutils.IsFileExists(filePath, false)
			if err != nil {
				err = fmt.Errorf("failed to check for the existence of '%s' before deleting the file: %s", filePath, err.Error())
				return errorutils.CheckError(err)
			}
			if fileExists {
				err = os.Remove(filePath)
			}
			return errorutils.CheckError(err)
		}

		backupPath := filepath.Join(filepath.Dir(filePath), backupFileName)
		backupInfo, err := os.Lstat(backupPath)
		if err != nil {
			return errors.New(createRestoreErrorPrefix(filePath, backupPath) + err.Error())
		}
		if !backupInfo.Mode().IsRegular() {
			return errors.New(createRestoreErrorPrefix(filePath, backupPath) + fmt.Sprintf("the backup file was replaced by a non-regular file (%s)", describeFileType(backupInfo.Mode())))
		}

		if err = fileutils.MoveFile(backupPath, filePath); err != nil {
			return errorutils.CheckError(err)
		}
		log.Debug("Restored the file", filePath, "successfully")
		return nil
	}
}

func createRestoreErrorPrefix(filePath, backupPath string) string {
	return fmt.Sprintf("An error occurred while restoring the file: %s\n"+
		"To restore the file manually: delete %s and rename the backup file at %s (if exists) to '%s'.\n"+
		"Failure cause: ",
		filePath, filePath, backupPath, filePath)
}
