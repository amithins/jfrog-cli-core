package transferconfig

import (
	"bytes"

	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
)

// configImporter abstracts the target-side calls of the config import, so that different import
// implementations (currently only the config-import plugin) can be used by the transfer-config command.
// Waiting for the import completion and the validation of its logs are shared by all the implementations,
// and are therefore done by the TransferConfigCommand.
type configImporter interface {
	// Target-side checks before the import (plugin: version + checkPermissions)
	verify() error
	// Starts the async import of the config ZIP, returns an opaque reference used for polling
	start(zip *bytes.Buffer) (importRef string, err error)
	// Returns the polling action used by waitForImportCompletion
	pollingAction(importRef string) httputils.PollingAction
}
