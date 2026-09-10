package transferfiles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTargetRepo = testSourceRepo
)

func testTargetMetadata() *SourceFileMetadata {
	return &SourceFileMetadata{
		Repo:         testTargetRepo,
		Path:         testSourcePath,
		Name:         testSourceName,
		Size:         11,
		Sha1:         "sha1-value",
		Sha256:       "sha256-value",
		Md5:          "md5-value",
		Created:      "2020-01-01T00:00:00.000Z",
		CreatedBy:    "admin",
		LastModified: "2020-01-02T00:00:00.000Z",
		ModifiedBy:   "deployer",
		Properties: map[string][]string{
			"build.name": {"app"},
			"env":        {"prod", "release"},
		},
	}
}

func testTargetRelativePath() string {
	return testSourceRelativePath()
}

func defaultTargetDeployOptions() TargetDeployOptions {
	return TargetDeployOptions{
		MinChecksumDeploySize: 10,
	}
}

func newTestTargetServerDetails(serverURL string) *config.ServerDetails {
	return &config.ServerDetails{
		Url:            serverURL + "/",
		ArtifactoryUrl: serverURL + "/",
	}
}

func TestTargetClient_Ping_usesSystemPing(t *testing.T) {
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		_, _ = w.Write([]byte("OK"))
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	err = client.Ping(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/api/system/ping", requestPath)
}

func TestTargetClient_TryChecksumDeploy_hit_sendsNoBody(t *testing.T) {
	deployPath := "/" + testTargetRelativePath()
	var requestMethod string
	var requestPath string
	var checksumDeployHeader string
	var sha1Header string
	var bodyBytes []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMethod = r.Method
		requestPath = r.URL.Path
		checksumDeployHeader = r.Header.Get("X-Checksum-Deploy")
		sha1Header = r.Header.Get("X-Checksum-Sha1")
		if r.Body != nil {
			bodyBytes, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	outcome, err := client.TryChecksumDeploy(context.Background(), testTargetMetadata(), defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, ChecksumDeployHit, outcome)
	assert.Equal(t, http.MethodPut, requestMethod)
	assert.True(t, strings.HasPrefix(requestPath, deployPath))
	assert.Contains(t, requestPath, ";build.name=")
	assert.Equal(t, "true", checksumDeployHeader)
	assert.Empty(t, bodyBytes)
	assert.Equal(t, "sha1-value", sha1Header)
}

func TestTargetClient_TryChecksumDeploy_missOn404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	outcome, err := client.TryChecksumDeploy(context.Background(), testTargetMetadata(), defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, ChecksumDeployMiss, outcome)
}

func TestTargetClient_TryChecksumDeploy_conflictDoesNotFallThrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"errors":[{"message":"checksum conflict"}]}`))
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	outcome, err := client.TryChecksumDeploy(context.Background(), testTargetMetadata(), defaultTargetDeployOptions())
	require.Error(t, err)
	assert.Equal(t, ChecksumDeployMiss, outcome)
}

func TestTargetClient_TryChecksumDeploy_skipsBuildInfoRepo(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requestCount++
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	options := defaultTargetDeployOptions()
	options.BuildInfoRepo = true
	outcome, err := client.TryChecksumDeploy(context.Background(), testTargetMetadata(), options)
	require.NoError(t, err)
	assert.Equal(t, ChecksumDeployMiss, outcome)
	assert.Zero(t, requestCount)
}

func TestTargetClient_TryChecksumDeploy_filestoreOptionHeaders(t *testing.T) {
	var binaryExistenceHeader string
	var md5Header string
	var binarySizeHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		binaryExistenceHeader = r.Header.Get("X-Check-Binary-Existence-In-Filestore")
		md5Header = r.Header.Get("X-Checksum-Md5")
		binarySizeHeader = r.Header.Get("X-Binary-Size")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	options := defaultTargetDeployOptions()
	options.CheckExistenceInFilestore = true
	outcome, err := client.TryChecksumDeploy(context.Background(), testTargetMetadata(), options)
	require.NoError(t, err)
	assert.Equal(t, ChecksumDeployHit, outcome)
	assert.Equal(t, "true", binaryExistenceHeader)
	assert.Equal(t, "md5-value", md5Header)
	assert.Equal(t, strconv.FormatInt(testTargetMetadata().Size, 10), binarySizeHeader)
}

func TestTargetClient_Put_setsContentLengthAndChecksumHeaders(t *testing.T) {
	deployPath := "/" + testTargetRelativePath()
	payload := "hello world"
	var contentLength string
	var sha1Header string
	var sha256Header string
	var bodyBytes []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.URL.Path, deployPath))
		assert.Contains(t, r.URL.Path, ";build.name=")
		contentLength = r.Header.Get("Content-Length")
		sha1Header = r.Header.Get("X-Checksum-Sha1")
		sha256Header = r.Header.Get("X-Checksum")
		bodyBytes, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	metadata := testTargetMetadata()
	err = client.Put(context.Background(), metadata, strings.NewReader(payload), defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(metadata.Size, 10), contentLength)
	assert.Equal(t, metadata.Sha1, sha1Header)
	assert.Equal(t, metadata.Sha256, sha256Header)
	assert.Equal(t, payload, string(bodyBytes))
}

func TestTargetClient_ApplyProperties_setsEligibleProperties(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	skippedLargeProps, err := client.ApplyProperties(context.Background(), testTargetMetadata(), defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.False(t, skippedLargeProps)
	assert.Zero(t, requestCount, "short property maps are applied as deploy matrix params, not a follow-up properties PUT")
}

func TestTargetClient_ApplyProperties_omitsValuesLongerThan2400(t *testing.T) {
	longValue := strings.Repeat("x", 2401)
	metadata := testTargetMetadata()
	metadata.Properties = map[string][]string{
		"short": {"ok"},
		"long":  {longValue},
	}

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	skippedLargeProps, err := client.ApplyProperties(context.Background(), metadata, defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.True(t, skippedLargeProps)
	assert.Zero(t, requestCount)
	assert.Contains(t, propertyMatrixSuffix(metadata, defaultTargetDeployOptions()), ";short=")
	assert.NotContains(t, propertyMatrixSuffix(metadata, defaultTargetDeployOptions()), "long")
}

func TestTargetClient_ApplyProperties_usesPatchWhenEncodedPayloadExceeds4000(t *testing.T) {
	metadata := testTargetMetadata()
	metadata.Properties = largeEligiblePropertiesMap(t)

	var requestMethod string
	var requestPath string
	var rawQuery string
	var contentType string
	var bodyBytes []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMethod = r.Method
		requestPath = r.URL.Path
		rawQuery = r.URL.RawQuery
		contentType = r.Header.Get("Content-Type")
		bodyBytes, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	skippedLargeProps, err := client.ApplyProperties(context.Background(), metadata, defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.False(t, skippedLargeProps)
	assert.Equal(t, http.MethodPatch, requestMethod)
	assert.Equal(t, "/api/metadata/"+testTargetRelativePath(), requestPath)
	assert.Empty(t, rawQuery)
	assert.Contains(t, contentType, "application/json")

	var patchBody updateItemPropertiesBody
	require.NoError(t, json.Unmarshal(bodyBytes, &patchBody))
	assert.NotEmpty(t, patchBody.Props)
	assert.Contains(t, patchBody.Props, "transfer.prop.0000")
}

func TestTargetClient_ApplyProperties_usesPutQueryWhenEncodedPayloadAtMost4000(t *testing.T) {
	metadata := testTargetMetadata()
	suffix := propertyMatrixSuffix(metadata, defaultTargetDeployOptions())
	assert.True(t, strings.HasPrefix(suffix, ";"))
	assert.Contains(t, suffix, "build.name=")
	assert.LessOrEqual(t, len(strings.TrimPrefix(suffix, ";")), maxPropertyEncodedStringLength)

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	_, err = client.ApplyProperties(context.Background(), metadata, defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Zero(t, requestCount)
}

func TestTargetClient_ApplyProperties_stripsGeneratedPropertyKeys(t *testing.T) {
	metadata := testTargetMetadata()
	metadata.Properties = map[string][]string{
		"build.name":                   {"app"},
		"artifactory.licenses":         {"MIT"},
		"artifactory.metadata.exclude": {"*"},
		"package.lowercase":            {"true"},
		"ruby":                         {"x86_64-linux"},
		"baseUrl":                      {"http://example.com"},
		"conan.settings.os":            {"Linux"},
		"conan.settings.compiler":      {"gcc"},
	}

	suffix := propertyMatrixSuffix(metadata, defaultTargetDeployOptions())
	assert.Contains(t, suffix, "build.name=")
	assert.NotContains(t, suffix, "artifactory.licenses")
	assert.NotContains(t, suffix, "artifactory.metadata.exclude")
	assert.NotContains(t, suffix, "package.lowercase")
	assert.NotContains(t, suffix, "baseUrl")
	assert.NotContains(t, suffix, "conan.settings.os")

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requestCount++
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	skippedLargeProps, err := client.ApplyProperties(context.Background(), metadata, defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.False(t, skippedLargeProps)
	assert.Zero(t, requestCount)
}

func TestTargetClient_ApplyProperties_preservesMultiValues(t *testing.T) {
	metadata := testTargetMetadata()
	metadata.Properties = map[string][]string{
		"env": {"prod", "release"},
	}

	suffix := propertyMatrixSuffix(metadata, defaultTargetDeployOptions())
	assert.Contains(t, suffix, "env=prod")
	assert.Contains(t, suffix, "env=release")
}

func TestTargetClient_CreateFolder_putsZeroByteBody(t *testing.T) {
	folderMetadata := &SourceFileMetadata{
		Repo:         testTargetRepo,
		Path:         "empty-dir",
		Name:         "",
		Created:      "2020-01-01T00:00:00.000Z",
		CreatedBy:    "admin",
		LastModified: "2020-01-02T00:00:00.000Z",
		ModifiedBy:   "deployer",
	}
	deployPath := "/" + testTargetRepo + "/empty-dir/"
	var contentLength string
	var bodyBytes []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentLength = r.Header.Get("Content-Length")
		bodyBytes, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		require.Equal(t, deployPath, r.URL.Path)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	err = client.CreateFolder(context.Background(), folderMetadata, defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, "0", contentLength)
	assert.Empty(t, bodyBytes)
}

func TestTargetClient_noPluginExecuteURLs(t *testing.T) {
	var seenPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	require.NoError(t, client.Ping(context.Background()))
	_, err = client.TryChecksumDeploy(context.Background(), testTargetMetadata(), defaultTargetDeployOptions())
	require.NoError(t, err)

	for _, path := range seenPaths {
		assert.NotContains(t, path, "/api/plugins/execute")
	}
}

func TestTargetClient_checksumDeploy409DoesNotInvokePut(t *testing.T) {
	deployPath := "/" + testTargetRelativePath()
	checksumDeployCalls := 0
	fullPutCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, deployPath) || r.Method != http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Checksum-Deploy") == "true" {
			checksumDeployCalls++
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"errors":[{"message":"checksum conflict"}]}`))
			return
		}
		fullPutCalls++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	source := &mockTransferSource{
		metadata: testFileMetadata(),
		reader:   io.NopCloser(strings.NewReader("hello world")),
	}
	ft := NewFileTransfer(source, client, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), testFileCandidate())

	require.Error(t, result.Err)
	assert.Equal(t, api.Fail, result.Status)
	assert.Equal(t, 1, checksumDeployCalls)
	assert.Zero(t, fullPutCalls)
	assert.Zero(t, source.getReaderCalls)
}

func largeEligiblePropertiesMap(t *testing.T) map[string][]string {
	t.Helper()
	props := make(map[string][]string)
	for i := 0; ; i++ {
		key := fmt.Sprintf("transfer.prop.%04d", i)
		props[key] = []string{"value"}
		if encodedPropertiesLength(props) > maxPropertyEncodedStringLength {
			break
		}
	}
	require.Greater(t, encodedPropertiesLength(props), maxPropertyEncodedStringLength)
	return props
}

func encodedPropertiesLength(props map[string][]string) int {
	eligibleProps, _ := filterEligibleProperties(props, TargetDeployOptions{})
	return len(eligibleProps.ToEncodedString(false))
}

func TestNewTargetClient_streamServiceManagerHasNoOverallTimeoutOrRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	streamConfig := client.streamServiceManager.GetConfig()
	assert.Zero(t, streamConfig.GetOverallRequestTimeout())
	assert.Zero(t, streamConfig.GetHttpRetries())

	metadataConfig := client.metadataServiceManager.GetConfig()
	assert.Equal(t, time.Minute, metadataConfig.GetOverallRequestTimeout())
	assert.Equal(t, retries, metadataConfig.GetHttpRetries())
}

func TestTargetClient_TryChecksumDeploy_skipsWhenSha1Empty(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requestCount++
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	metadata := testTargetMetadata()
	metadata.Sha1 = ""
	outcome, err := client.TryChecksumDeploy(context.Background(), metadata, defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, ChecksumDeployMiss, outcome)
	assert.Zero(t, requestCount)
}

func TestTargetClient_emptySha1UsesPutStreamPath(t *testing.T) {
	deployPath := "/" + testTargetRelativePath()
	payload := "hello world"
	checksumDeployCalls := 0
	fullPutCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, deployPath) || r.Method != http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Checksum-Deploy") == "true" {
			checksumDeployCalls++
			w.WriteHeader(http.StatusOK)
			return
		}
		fullPutCalls++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	metadata := testTargetMetadata()
	metadata.Sha1 = ""
	outcome, err := client.TryChecksumDeploy(context.Background(), metadata, defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, ChecksumDeployMiss, outcome)
	assert.Zero(t, checksumDeployCalls)

	err = client.Put(context.Background(), metadata, strings.NewReader(payload), defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, 1, fullPutCalls)
}

func Test_addIdentityHeaders_convertsIsoTimestampsToEpochMilliseconds(t *testing.T) {
	var createdHeader string
	var lastModifiedHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		createdHeader = r.Header.Get("X-Artifactory-Created")
		lastModifiedHeader = r.Header.Get("X-Artifactory-Last-Modified")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	err = client.Put(context.Background(), testTargetMetadata(), strings.NewReader("hello world"), defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, "1577836800000", createdHeader)
	assert.Equal(t, "1577923200000", lastModifiedHeader)
}

func Test_addIdentityHeaders_preservesNumericTimestamps(t *testing.T) {
	var createdHeader string
	var lastModifiedHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		createdHeader = r.Header.Get("X-Artifactory-Created")
		lastModifiedHeader = r.Header.Get("X-Artifactory-Last-Modified")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	metadata := testTargetMetadata()
	metadata.Created = "1670000000000"
	metadata.LastModified = "1680000000000"
	err = client.Put(context.Background(), metadata, strings.NewReader("hello world"), defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Equal(t, "1670000000000", createdHeader)
	assert.Equal(t, "1680000000000", lastModifiedHeader)
}

func Test_addIdentityHeaders_omitsInvalidTimestamps(t *testing.T) {
	var createdHeader string
	var lastModifiedHeader string
	var createdByHeader string
	var modifiedByHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		createdHeader = r.Header.Get("X-Artifactory-Created")
		lastModifiedHeader = r.Header.Get("X-Artifactory-Last-Modified")
		createdByHeader = r.Header.Get("X-Artifactory-Created-By")
		modifiedByHeader = r.Header.Get("X-Artifactory-Modified-By")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	metadata := testTargetMetadata()
	metadata.Created = "not-a-timestamp"
	metadata.LastModified = "also-invalid"
	err = client.Put(context.Background(), metadata, strings.NewReader("hello world"), defaultTargetDeployOptions())
	require.NoError(t, err)
	assert.Empty(t, createdHeader)
	assert.Empty(t, lastModifiedHeader)
	assert.Equal(t, "admin", createdByHeader)
	assert.Equal(t, "deployer", modifiedByHeader)
}

func TestTargetClient_ApplyStatistics_putsItemStatisticsXML(t *testing.T) {
	var (
		requestMethod string
		requestPath   string
		requestBody   string
		contentType   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMethod = r.Method
		requestPath = r.URL.Path
		contentType = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		requestBody = string(body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	metadata := testTargetMetadata()
	metadata.DownloadCount = 5
	metadata.LastDownloaded = 1788945284685
	metadata.LastDownloadedBy = "admin"
	err = client.ApplyStatistics(context.Background(), metadata)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, requestMethod)
	assert.Equal(t, "/"+testTargetRelativePath()+":statistics", requestPath)
	assert.Equal(t, "application/xml", contentType)
	assert.Contains(t, requestBody, "<artifactory.stats>")
	assert.Contains(t, requestBody, "<downloadCount>5</downloadCount>")
	assert.Contains(t, requestBody, "<lastDownloaded>1788945284685</lastDownloaded>")
	assert.Contains(t, requestBody, "<lastDownloadedBy>admin</lastDownloadedBy>")
	assert.NotContains(t, requestPath, "/api/plugins/execute/")
}

func TestTargetClient_ApplyStatistics_rejectedStatus_doesNotFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasSuffix(r.URL.Path, ":statistics"))
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"errors":[{"message":"statistics rejected"}]}`))
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	metadata := testTargetMetadata()
	metadata.DownloadCount = 5
	metadata.LastDownloadedBy = "admin"
	err = client.ApplyStatistics(context.Background(), metadata)
	require.NoError(t, err)
}

func TestTargetClient_ApplyStatistics_skipsWhenEmpty(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requestCount++
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	err = client.ApplyStatistics(context.Background(), testTargetMetadata())
	require.NoError(t, err)
	assert.Zero(t, requestCount)
}

func TestTargetClient_ApplyStatistics_skipsFolders(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requestCount++
	}))
	defer server.Close()

	client, err := NewTargetClient(context.Background(), newTestTargetServerDetails(server.URL), nil)
	require.NoError(t, err)

	err = client.ApplyStatistics(context.Background(), &SourceFileMetadata{
		Repo:             testTargetRepo,
		Path:             "empty-dir",
		DownloadCount:    3,
		LastDownloadedBy: "admin",
	})
	require.NoError(t, err)
	assert.Zero(t, requestCount)
}
