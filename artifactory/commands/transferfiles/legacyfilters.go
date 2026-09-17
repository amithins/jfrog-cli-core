package transferfiles

import (
	"strings"
)

var packageTypePropertyPrefixes = map[string]string{
	"go":        "go.",
	"npm":       "npm.",
	"pub":       "pub.",
	"rpm":       "rpm.metadata.",
	"chef":      "chef.",
	"gems":      "gem.",
	"helm":      "chart.",
	"opkg":      "opkg.",
	"pypi":      "pypi.",
	"bower":     "bower.",
	"cargo":     "crate.",
	"conan":     "conan.",
	"conda":     "conda.",
	"nuget":     "nuget.",
	"swift":     "swift.",
	"alpine":    "alpine.",
	"debian":    "deb.",
	"puppet":    "puppet.",
	"composer":  "composer.",
	"cocoapods": "pods.",
	"terraform": "terraform.",
}

// isGeneratedPropertyKey reports whether a property key is package-generated and should be
// skipped on deploy. The production caller (filterEligibleProperties) lands in the target-client slice.
func isGeneratedPropertyKey(key, packageType string) bool {
	normalizedKey := strings.ToLower(key)
	normalizedPackageType := strings.ToLower(packageType)

	switch normalizedKey {
	case "artifactory.licenses", "artifactory.metadata.exclude", "package.lowercase":
		return true
	case "ruby":
		return normalizedPackageType == "gems"
	case "baseurl":
		return normalizedPackageType == "pub"
	}
	if strings.HasPrefix(normalizedKey, "conan.settings.") {
		return true
	}

	prefix, ok := packageTypePropertyPrefixes[normalizedPackageType]
	if ok && prefix != "" && strings.HasPrefix(normalizedKey, prefix) {
		return true
	}
	return false
}
