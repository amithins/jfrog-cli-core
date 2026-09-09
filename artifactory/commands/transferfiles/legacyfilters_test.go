package transferfiles

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_isGeneratedPropertyKey_crossPackageExactAndPrefix(t *testing.T) {
	generatedKeys := []string{
		"artifactory.licenses",
		"artifactory.metadata.exclude",
		"package.lowercase",
		"ruby",
		"baseUrl",
		"BASEURL",
		"conan.settings.os",
		"conan.settings.compiler.version",
	}
	for _, key := range generatedKeys {
		assert.True(t, isGeneratedPropertyKey(key, ""), "expected generated key: %s", key)
		assert.True(t, isGeneratedPropertyKey(key, "maven"), "expected generated key for maven: %s", key)
	}

	eligibleKeys := []string{
		"build.name",
		"env",
		"npm.name",
		"go.name",
		"chart.name",
	}
	for _, key := range eligibleKeys {
		assert.False(t, isGeneratedPropertyKey(key, ""), "expected eligible key: %s", key)
		assert.False(t, isGeneratedPropertyKey(key, "generic"), "expected eligible key for generic: %s", key)
	}
}

func Test_isGeneratedPropertyKey_packageTypePrefixes(t *testing.T) {
	cases := []struct {
		name        string
		packageType string
		generated   []string
		eligible    []string
	}{
		{
			name:        "go",
			packageType: "Go",
			generated:   []string{"go.name", "GO.version", "package.lowercase"},
			eligible:    []string{"build.name", "npm.name"},
		},
		{
			name:        "npm",
			packageType: "npm",
			generated:   []string{"npm.name", "npm.version", "artifactory.metadata.exclude"},
			eligible:    []string{"build.name", "go.name"},
		},
		{
			name:        "pub",
			packageType: "Pub",
			generated:   []string{"pub.name", "baseUrl"},
			eligible:    []string{"build.name", "npm.name"},
		},
		{
			name:        "rpm",
			packageType: "rpm",
			generated:   []string{"rpm.metadata.name", "rpm.metadata.version"},
			eligible:    []string{"build.name", "rpm.custom"},
		},
		{
			name:        "chef",
			packageType: "chef",
			generated:   []string{"chef.name", "chef.version"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "gems",
			packageType: "Gems",
			generated:   []string{"gem.name", "ruby"},
			eligible:    []string{"build.name", "gems.custom"},
		},
		{
			name:        "helm",
			packageType: "helm",
			generated:   []string{"chart.name", "chart.version"},
			eligible:    []string{"build.name", "helm.custom"},
		},
		{
			name:        "opkg",
			packageType: "opkg",
			generated:   []string{"opkg.name"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "pypi",
			packageType: "pypi",
			generated:   []string{"pypi.name", "pypi.version"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "bower",
			packageType: "bower",
			generated:   []string{"bower.name"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "cargo",
			packageType: "cargo",
			generated:   []string{"crate.name", "crate.version"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "conan",
			packageType: "conan",
			generated:   []string{"conan.name", "conan.settings.os", "conan.settings.compiler", "conan.author"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "conda",
			packageType: "conda",
			generated:   []string{"conda.name"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "nuget",
			packageType: "NuGet",
			generated:   []string{"nuget.id", "nuget.version"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "swift",
			packageType: "swift",
			generated:   []string{"swift.name"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "alpine",
			packageType: "alpine",
			generated:   []string{"alpine.name"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "debian",
			packageType: "debian",
			generated:   []string{"deb.name", "deb.version", "artifactory.licenses"},
			eligible:    []string{"build.name", "debian.custom"},
		},
		{
			name:        "puppet",
			packageType: "puppet",
			generated:   []string{"puppet.name"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "composer",
			packageType: "composer",
			generated:   []string{"composer.name"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "cocoapods",
			packageType: "cocoapods",
			generated:   []string{"pods.name", "pods.version"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "terraform",
			packageType: "terraform",
			generated:   []string{"terraform.name", "terraform.version"},
			eligible:    []string{"build.name"},
		},
		{
			name:        "maven-no-prefix",
			packageType: "maven",
			generated:   []string{"artifactory.licenses"},
			eligible:    []string{"build.name", "maven.name"},
		},
		{
			name:        "docker-no-prefix",
			packageType: "docker",
			generated:   []string{"conan.settings.os"},
			eligible:    []string{"build.name", "docker.manifest"},
		},
		{
			name:        "generic-no-prefix",
			packageType: "generic",
			generated:   []string{"package.lowercase"},
			eligible:    []string{"build.name", "npm.name", "go.name"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range tc.generated {
				assert.True(t, isGeneratedPropertyKey(key, tc.packageType), "expected generated key %q for %q", key, tc.packageType)
			}
			for _, key := range tc.eligible {
				assert.False(t, isGeneratedPropertyKey(key, tc.packageType), "expected eligible key %q for %q", key, tc.packageType)
			}
		})
	}
}

func Test_filterEligibleProperties_usesPackageType(t *testing.T) {
	properties := map[string][]string{
		"build.name":  {"app"},
		"npm.name":    {"pkg"},
		"npm.version": {"1.0.0"},
	}
	options := TargetDeployOptions{PackageType: "npm"}
	eligibleProps, skippedLargeProps := filterEligibleProperties(properties, options)
	assert.False(t, skippedLargeProps)
	assert.Equal(t, 1, eligibleProps.KeysLen())
	assert.Equal(t, map[string][]string{"build.name": {"app"}}, eligibleProps.ToMap())
}
