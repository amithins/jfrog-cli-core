package commandsummary

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
)

const (
	applicationEvidenceFormat   = "%sui/applications/management/%s/versions/%s?activeVersionTab=Content+Graph"
	releaseBundleEvidenceFormat = "%sui/artifactory/lifecycle?range=Any+Time&bundleName=%s&repositoryKey=%s&releaseBundleVersion=%s&activeVersionTab=Content+Graph"
	buildEvidenceFormat         = "%sui/builds/%s/%s/%s/Evidence?buildRepo=%s"
	artifactEvidenceFormat      = "%sui/repos/tree/Evidence/%s?clearFilter=true"
)

func GenerateEvidenceUrlByType(data EvidenceSummaryData, section summarySection) (string, error) {
	switch data.SubjectType {
	// Currently, it is not possible to generate a link to the evidence tab for packages in the Artifactory UI.
	// The link will point to the lead artifact of the package instead.
	// This logic will be updated once UI support is available
	case SubjectTypePackage, SubjectTypeArtifact:
		return generateArtifactEvidenceUrl(data.Subject, section)
	case SubjectTypeReleaseBundle:
		return generateReleaseBundleEvidenceUrl(data, section)
	case SubjectTypeApplication:
		return generateApplicationEvidenceUrl(data, section)
	case SubjectTypeBuild:
		return generateBuildEvidenceUrl(data, section)
	default:
		return generateArtifactEvidenceUrl(data.Subject, section)
	}
}

func generateArtifactEvidenceUrl(pathInRt string, section summarySection) (string, error) {
	if err := validatePathSegments(strings.Split(pathInRt, "/")...); err != nil {
		return "", err
	}
	urlStr := fmt.Sprintf(artifactEvidenceFormat, StaticMarkdownConfig.GetPlatformUrl(), escapePathSegments(pathInRt))
	return addGitHubTrackingToUrl(urlStr, section)
}

func generateReleaseBundleEvidenceUrl(data EvidenceSummaryData, section summarySection) (string, error) {
	if data.ReleaseBundleName == "" || data.ReleaseBundleVersion == "" {
		return generateArtifactEvidenceUrl(data.Subject, section)
	}

	urlStr := fmt.Sprintf(releaseBundleEvidenceFormat,
		StaticMarkdownConfig.GetPlatformUrl(),
		url.QueryEscape(data.ReleaseBundleName),
		url.QueryEscape(data.RepoKey),
		url.QueryEscape(data.ReleaseBundleVersion))

	return addGitHubTrackingToUrl(urlStr, section)
}

func generateApplicationEvidenceUrl(data EvidenceSummaryData, section summarySection) (string, error) {
	if data.ApplicationKey == "" || data.ApplicationVersion == "" {
		return generateArtifactEvidenceUrl(data.Subject, section)
	}
	if err := validatePathSegments(data.ApplicationKey, data.ApplicationVersion); err != nil {
		return "", err
	}

	urlStr := fmt.Sprintf(applicationEvidenceFormat,
		StaticMarkdownConfig.GetPlatformUrl(),
		url.PathEscape(data.ApplicationKey),
		url.PathEscape(data.ApplicationVersion))

	return addGitHubTrackingToUrl(urlStr, section)
}

func generateBuildEvidenceUrl(data EvidenceSummaryData, section summarySection) (string, error) {
	if data.BuildName == "" || data.BuildNumber == "" || data.BuildTimestamp == "" {
		return generateArtifactEvidenceUrl(data.Subject, section)
	}
	if err := validatePathSegments(data.BuildName, data.BuildNumber, data.BuildTimestamp); err != nil {
		return "", err
	}

	urlStr := fmt.Sprintf(buildEvidenceFormat,
		StaticMarkdownConfig.GetPlatformUrl(),
		url.QueryEscape(data.BuildName),
		url.PathEscape(data.BuildNumber),
		url.PathEscape(data.BuildTimestamp),
		url.QueryEscape(data.RepoKey))

	return addGitHubTrackingToUrl(urlStr, section)
}

// escapePathSegments escapes each segment of a repository path, keeping the "/" separators.
func escapePathSegments(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// validatePathSegments rejects "." and ".." segments. Escaping does not help, since browsers also treat
// "%2e" and "%2e%2e" as dot segments and resolve them, which would point the link at a different page.
func validatePathSegments(segments ...string) error {
	for _, segment := range segments {
		if segment == "." || segment == ".." {
			return errorutils.CheckErrorf("cannot generate an evidence link for a path containing the %q segment", segment)
		}
	}
	return nil
}
