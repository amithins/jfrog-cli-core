package transferfiles

import (
	"context"
	"net/http"
	"testing"

	"github.com/jfrog/jfrog-cli-core/v2/common/tests"
	"github.com/jfrog/jfrog-client-go/artifactory/services/utils"
	"github.com/stretchr/testify/assert"
)

func Test_shouldExcludeGeneratedFile(t *testing.T) {
	cases := []struct {
		packageType string
		path        string
		exclude     bool
	}{
		{packageType: "npm", path: ".npm/pkg", exclude: true},
		{packageType: "NPM", path: "pkg/-/pkg-1.0.0.tgz", exclude: false},
		{packageType: "docker", path: "repo/repository.catalog", exclude: true},
		{packageType: "docker", path: ".jfrog/repository_v2.catalog", exclude: true},
		{packageType: "docker", path: "busybox/_uploads/layer", exclude: true},
		{packageType: "docker", path: "_uploads/layer", exclude: true},
		{packageType: "docker", path: "busybox/manifest.json", exclude: false},
		{packageType: "oci", path: "img/repository_v2.catalog", exclude: true},
		{packageType: "helmoci", path: "chart/_uploads/blob", exclude: true},
		{packageType: "generic", path: ".jfrog/internal", exclude: true},
		{packageType: "debian", path: "dists/stable/Release", exclude: true},
		{packageType: "debian", path: "dists/stable/foo.deb", exclude: false},
		{packageType: "debian", path: "pool/main/foo.deb", exclude: false},
		{packageType: "debian", path: "foosdists/stable/Release", exclude: false},
		{packageType: "gems", path: "latest_specs.4.8.gz", exclude: true},
		{packageType: "gems", path: "info/foo", exclude: true},
		{packageType: "gems", path: "foo.gemspec.rz", exclude: true},
		{packageType: "cargo", path: "config.original.json", exclude: true},
		{packageType: "maven", path: "org/foo/1.0/maven-metadata.xml", exclude: true},
		{packageType: "gradle", path: "nexus-maven-repository-index.gz", exclude: true},
		{packageType: "maven", path: "org/foo/1.0/foo-1.0.jar", exclude: false},
		{packageType: "helm", path: "index.yaml", exclude: true},
		{packageType: "helm", path: "charts/mychart-1.0.0.tgz", exclude: false},
		{packageType: "alpine", path: "x86_64/APKINDEX.tar.gz", exclude: true},
		{packageType: "conan", path: "pkg/1.0/index.json", exclude: true},
		{packageType: "conan", path: ".conan/packages.ref.json", exclude: true},
		{packageType: "nuget", path: ".nuget/packages", exclude: true},
		{packageType: "pypi", path: ".pypi/simple.html", exclude: true},
		{packageType: "cocoapods", path: "Specs/a.podspec.json", exclude: true},
		{packageType: "cocoapods", path: "Specs/a.podspec", exclude: true},
		{packageType: "terraform", path: "mod/module.json", exclude: true},
		{packageType: "cargo", path: "config.json", exclude: true},
		{packageType: "cargo", path: ".git/HEAD", exclude: true},
		{packageType: "cargo", path: "index/ab/crate", exclude: true},
		{packageType: "cargo", path: "crates/foo.crate", exclude: false},
		{packageType: "gems", path: "specs.4.8.gz", exclude: true},
		{packageType: "gems", path: "pkg-1.0.0.gem", exclude: false},
		{packageType: "composer", path: ".composer/packages.json", exclude: true},
		{packageType: "opkg", path: "arm/Packages.gz", exclude: true},
		{packageType: "conda", path: "linux-64/repodata.json", exclude: true},
		{packageType: "rpm", path: "repodata/repomd.xml", exclude: true},
		{packageType: "yum", path: "repodata/repomd.xml", exclude: true},
		{packageType: "swift", path: "pkg/releases.json", exclude: true},
		{packageType: "swift", path: "Package.swift?swift-version=5", exclude: true},
		{packageType: "swift", path: ".swift/scope/pkg/1.0.0/Package.swift", exclude: true},
		{packageType: "swift", path: ".swift/scope/pkg/1.0.0/Package@swift-5.7.swift", exclude: true},
		{packageType: "swift", path: "Sources/foo.swift", exclude: false},
		{packageType: "releasebundles", path: "bundle.json.draft", exclude: true},
		{packageType: "releasebundles", path: "bundle.json", exclude: false},
		{packageType: "go", path: "github.com/foo/@v/list", exclude: false},
		{packageType: "generic", path: ".npm/pkg", exclude: false},
		{packageType: "chef", path: "any/path", exclude: false},
	}

	for _, tc := range cases {
		t.Run(tc.packageType+"/"+tc.path, func(t *testing.T) {
			assert.Equal(t, tc.exclude, shouldExcludeGeneratedFile(tc.packageType, tc.path))
		})
	}
}

func TestFilterLocallyGenerated_disabledUsesPluginPathTable(t *testing.T) {
	testServer, _, servicesManager := tests.CreateRtRestsMockServer(t, func(http.ResponseWriter, *http.Request) {})
	defer testServer.Close()
	disabled := NewLocallyGenerated(context.Background(), servicesManager, "7.54.5")

	items := []utils.ResultItem{
		{Repo: "npm-local", Path: ".", Name: "pkg-1.0.0.tgz"},
		{Repo: "npm-local", Path: ".npm", Name: "pkg"},
	}
	results, err := disabled.FilterLocallyGenerated(items, "npm")
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "pkg-1.0.0.tgz", results[0].Name)
}
