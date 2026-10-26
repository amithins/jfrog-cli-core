//go:build !windows

package transferconfig

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

// Default locations of the Artifactory var directory: native installation and Docker image.
var defaultArtifactoryVarDirs = []string{"/opt/jfrog/artifactory/var", "/var/opt/jfrog/artifactory"}

// Locations of the Artifactory var directory, by precedence. Its group is the Artifactory service group (shared.group in system.yaml).
func artifactoryVarDirs() []string {
	var dirs []string
	if jfrogHome := os.Getenv("JFROG_HOME"); jfrogHome != "" {
		dirs = append(dirs, filepath.Join(jfrogHome, "artifactory", "var"))
	}
	return append(dirs, defaultArtifactoryVarDirs...)
}

// Find the Artifactory var directory by precedence. Return its path and owner details, or a nil stat if it wasn't found.
// A directory owned by root doesn't identify the Artifactory service user, so it is skipped.
func findArtifactoryVarDir() (string, *syscall.Stat_t) {
	for _, varDir := range artifactoryVarDirs() {
		info, err := os.Stat(varDir)
		if err != nil || !info.IsDir() {
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 {
			return varDir, stat
		}
	}
	return "", nil
}

// Return the uid of the Artifactory service user: the owner of the Artifactory var directory, or -1 if it wasn't found.
// It identifies the service user whether or not the group of the export directory was set from the var directory.
func lookupServiceOwner() int {
	if _, stat := findArtifactoryVarDir(); stat != nil {
		return int(stat.Uid)
	}
	return -1
}

// Give the export directory to the Artifactory service group, so Artifactory can write to it with mode 0770.
// Only root does it: other users can't read the keys Artifactory exports with mode 0600 anyway.
// Best effort: if the group isn't applied, the export fails later with exportDirNotFoundErr, unless the CLI runs as the service user.
func setServiceGroup(exportPath string) {
	// A setgid base directory means the operator already chose the group (e.g. the container's service GID), keep it
	if parentHasSetgid(exportPath) {
		log.Debug("The base directory of '" + exportPath + "' has the setgid bit, keeping the group of the export directory as is")
		return
	}
	if geteuid() != 0 {
		log.Debug("Not running as root, keeping the group of the export directory as is")
		return
	}
	varDir, stat := findArtifactoryVarDir()
	if stat == nil {
		log.Debug("Couldn't find the Artifactory var directory (JFROG_HOME isn't set or has no artifactory/var), keeping the group of the export directory as is")
		return
	}
	gid := int(stat.Gid)
	if err := os.Chown(exportPath, -1, gid); err != nil {
		log.Warn(fmt.Sprintf("Couldn't set the group of '%s' to the group of '%s' (gid %d): %s", exportPath, varDir, gid, err.Error()))
		return
	}
	log.Info(fmt.Sprintf("Set the group of the export directory '%s' to %s, the group of the Artifactory var directory '%s'", exportPath, describeGroup(gid), varDir))
}

// Return the uid of the owner of a file, if its owner details are available.
func fileOwnerUid(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	return int(stat.Uid), true
}

func describeGroup(gid int) string {
	if group, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
		return fmt.Sprintf("'%s' (gid %d)", group.Name, gid)
	}
	return fmt.Sprintf("gid %d", gid)
}

// The owners allowed for the entries of the export tree, so entries planted by other members of the export directory group are rejected.
func allowedExportOwners(serviceOwner int) []int {
	return expectedExportOwners(serviceOwner, os.Geteuid())
}

// Root, the user running the CLI and the Artifactory service user (serviceOwner) if known.
// Only root knows the service user: the owner of the var directory, or else the owner of the directory Artifactory exported to,
// recorded right after the export. The owner of that directory is never read at archive time, since a group member may replace it.
// A non-root CLI trusts only itself and root: it can only succeed as the service user anyway (the keys are exported with mode 0600).
func expectedExportOwners(serviceOwner, euid int) []int {
	owners := []int{0}
	for _, uid := range []int{euid, serviceOwner} {
		if uid >= 0 && !slices.Contains(owners, uid) {
			owners = append(owners, uid)
		}
	}
	return owners
}

// Ensure an entry of the export tree is owned by one of the allowed owners. No allowed owners means no check.
func validateEntryOwner(path string, info os.FileInfo, allowedOwners []int) error {
	if len(allowedOwners) == 0 {
		return nil
	}
	uid, ok := fileOwnerUid(info)
	if !ok {
		return errorutils.CheckErrorf("refusing to read the export entry '%s': couldn't determine its owner", path)
	}
	if slices.Contains(allowedOwners, uid) {
		return nil
	}
	return errorutils.CheckErrorf("refusing to read the export entry '%s': it is owned by uid %d, but expected one of uids %v "+
		"(root, the user running this command and the Artifactory service user). "+
		"Another user may have written to the export directory: make sure only the Artifactory service user and root can write to it, and run transfer-config again",
		path, uid, allowedOwners)
}

// Ensure the directory is owned by the user running the CLI, so a directory swapped in by another user is rejected.
func validateOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errorutils.CheckErrorf("refusing to use the export directory '%s': couldn't determine its owner", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return errorutils.CheckErrorf("refusing to use the export directory '%s': it is owned by another user (uid %d) and not by the user running this command (uid %d)", path, stat.Uid, os.Geteuid())
	}
	return nil
}
