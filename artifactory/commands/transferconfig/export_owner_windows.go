package transferconfig

import "os"

// The export directory group is a Unix concept, nothing to do on Windows.
func setServiceGroup(string) {}

// The Artifactory service user is identified by Unix ownership, which isn't available on Windows.
func lookupServiceOwner() int {
	return -1
}

// Unix ownership isn't available on Windows.
func fileOwnerUid(os.FileInfo) (int, bool) {
	return -1, false
}

// Unix ownership isn't available on Windows.
func validateOwner(string, os.FileInfo) error {
	return nil
}

// Unix ownership isn't available on Windows, the owners of the export entries aren't checked.
func allowedExportOwners(int) []int {
	return nil
}

func validateEntryOwner(string, os.FileInfo, []int) error {
	return nil
}
