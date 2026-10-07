package transferconfig

import (
	"fmt"

	"github.com/jfrog/gofrog/version"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

// ConfigTransferMethod is the way the config is imported to the target Artifactory.
type ConfigTransferMethod string

const (
	// Choose by the target Artifactory version: native from minNativeConfigTransferVersion, the config-import plugin below it.
	ConfigTransferMethodAuto ConfigTransferMethod = "auto"
	// The config transfer API that is built into the target Artifactory (api/configTransfer/import).
	ConfigTransferMethodNative ConfigTransferMethod = "native"
	// The config-import user plugin, installed in the target Artifactory (api/plugins/execute/*).
	ConfigTransferMethodPlugin ConfigTransferMethod = "plugin"
)

// The first target Artifactory version that includes the native config transfer API (RTDEV-100136).
// Placeholder: the version is not known yet, so it is set to one that no real target reaches. The auto method keeps choosing
// the plugin, while the native method can already be requested explicitly. To be replaced when the version is known.
const minNativeConfigTransferVersion = "999.0.0"

// ParseConfigTransferMethod parses the value of the config transfer method flag.
// An empty value is the default method (auto), and an unknown value is an error.
func ParseConfigTransferMethod(value string) (ConfigTransferMethod, error) {
	switch ConfigTransferMethod(value) {
	case "", ConfigTransferMethodAuto:
		return ConfigTransferMethodAuto, nil
	case ConfigTransferMethodNative:
		return ConfigTransferMethodNative, nil
	case ConfigTransferMethodPlugin:
		return ConfigTransferMethodPlugin, nil
	}
	return "", errorutils.CheckErrorf("unknown config transfer method '%s'. Valid values are: %s, %s, %s",
		value, ConfigTransferMethodAuto, ConfigTransferMethodNative, ConfigTransferMethodPlugin)
}

// resolveImporter chooses the importer that the command uses, according to the requested method and the target Artifactory version.
// The choice is deterministic: it does not probe the target for the native API or for the plugin.
func (tcc *TransferConfigCommand) resolveImporter(targetArtifactoryVersion string) error {
	method, err := ParseConfigTransferMethod(string(tcc.method))
	if err != nil {
		return err
	}
	nativeSupported := version.NewVersion(targetArtifactoryVersion).AtLeast(minNativeConfigTransferVersion)

	var useNative bool
	switch method {
	case ConfigTransferMethodNative:
		useNative = true
		if !nativeSupported {
			log.Warn(fmt.Sprintf("The target Artifactory version %s is lower than %s, which is the minimum version expected to include the native config transfer API. "+
				"Continuing with the native method, since it was requested explicitly.", targetArtifactoryVersion, minNativeConfigTransferVersion))
		}
	case ConfigTransferMethodPlugin:
		useNative = false
	default:
		useNative = nativeSupported
	}

	chosenMethod := ConfigTransferMethodPlugin
	tcc.importer = newPluginImporter(tcc)
	if useNative {
		chosenMethod = ConfigTransferMethodNative
		tcc.importer = newNativeImporter(tcc)
	}
	log.Info(fmt.Sprintf("Config transfer method: %s (target Artifactory version %s, native API minimum %s).", chosenMethod, targetArtifactoryVersion, minNativeConfigTransferVersion))
	return nil
}

// usesNativeImporter returns true if the command imports the config using the native config transfer API.
func (tcc *TransferConfigCommand) usesNativeImporter() bool {
	_, ok := tcc.importer.(*nativeImporter)
	return ok
}
