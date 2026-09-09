package transferfiles

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testSourceRepo = "generic-local"
	testSourcePath = "folder"
	testSourceName = "file.txt"
)

func testSourceFile() api.FileRepresentation {
	return api.FileRepresentation{
		Repo: testSourceRepo,
		Path: testSourcePath,
		Name: testSourceName,
		Size: 11,
	}
}

func testSourceRelativePath() string {
	return testSourceRepo + "/" + testSourcePath + "/" + testSourceName
}

func TestIsSourceItemGone(t *testing.T) {
	assert.True(t, IsSourceItemGone(ErrSourceItemGone))
	assert.False(t, IsSourceItemGone(nil))
	assert.False(t, IsSourceItemGone(assert.AnError))
}

func TestGetFileMetadata_success(t *testing.T) {
	fileContent := "hello world"
	storagePath := "/api/storage/" + testSourceRelativePath()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			response := map[string]any{
				"repo":         testSourceRepo,
				"path":         testSourcePath + "/" + testSourceName,
				"created":      "2020-01-01T00:00:00.000Z",
				"createdBy":    "admin",
				"lastModified": "2020-01-02T00:00:00.000Z",
				"modifiedBy":   "deployer",
				"size":         strconv.Itoa(len(fileContent)),
				"checksums": map[string]string{
					"sha1":   "sha1-value",
					"sha256": "sha256-value",
					"md5":    "md5-value",
				},
			}
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(response))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			response := map[string]any{
				"properties": map[string][]string{
					"build.name": {"app"},
					"env":        {"prod", "release"},
				},
			}
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(response))
		case r.URL.Path == storagePath && r.URL.RawQuery == "stats":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"downloadCount":    0,
				"lastDownloaded":   0,
				"lastDownloadedBy": "",
			}))
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	require.NoError(t, err)
	require.NotNil(t, metadata)

	assert.Equal(t, testSourceRepo, metadata.Repo)
	assert.Equal(t, testSourcePath, metadata.Path)
	assert.Equal(t, testSourceName, metadata.Name)
	assert.Equal(t, int64(len(fileContent)), metadata.Size)
	assert.Equal(t, "sha1-value", metadata.Sha1)
	assert.Equal(t, "sha256-value", metadata.Sha256)
	assert.Equal(t, "md5-value", metadata.Md5)
	assert.Equal(t, "2020-01-01T00:00:00.000Z", metadata.Created)
	assert.Equal(t, "admin", metadata.CreatedBy)
	assert.Equal(t, "2020-01-02T00:00:00.000Z", metadata.LastModified)
	assert.Equal(t, "deployer", metadata.ModifiedBy)
	assert.Equal(t, map[string][]string{
		"build.name": {"app"},
		"env":        {"prod", "release"},
	}, metadata.Properties)
	assert.Zero(t, metadata.DownloadCount)
	assert.Zero(t, metadata.LastDownloaded)
	assert.Empty(t, metadata.LastDownloadedBy)
}

func TestGetFileMetadata_includesDownloadStats(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"size": "11",
				"checksums": map[string]string{
					"sha1": "sha1-value",
				},
			}))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"No properties could be found."}]}`))
		case r.URL.Path == storagePath && r.URL.RawQuery == "stats":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"downloadCount":    5,
				"lastDownloaded":   1788945284685,
				"lastDownloadedBy": "admin",
			}))
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	require.NoError(t, err)
	assert.Equal(t, int64(5), metadata.DownloadCount)
	assert.Equal(t, int64(1788945284685), metadata.LastDownloaded)
	assert.Equal(t, "admin", metadata.LastDownloadedBy)
}

func TestGetFileMetadata_statsNotFound_continuesWithoutStats(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"size": "11",
				"checksums": map[string]string{
					"sha1": "sha1-value",
				},
			}))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"No properties could be found."}]}`))
		case r.URL.Path == storagePath && r.URL.RawQuery == "stats":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Unable to find item"}]}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	require.NoError(t, err)
	assert.Zero(t, metadata.DownloadCount)
	assert.Empty(t, metadata.LastDownloadedBy)
}

func TestGetFileMetadata_statsForbidden_continuesWithoutStats(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"size": "11",
				"checksums": map[string]string{
					"sha1": "sha1-value",
				},
			}))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"No properties could be found."}]}`))
		case r.URL.Path == storagePath && r.URL.RawQuery == "stats":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Forbidden"}]}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	require.NoError(t, err)
	assert.Equal(t, "sha1-value", metadata.Sha1)
	assert.Zero(t, metadata.DownloadCount)
	assert.Empty(t, metadata.LastDownloadedBy)
}

func TestGetFileMetadata_notFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Not found"}]}`))
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	assert.Nil(t, metadata)
	assert.True(t, IsSourceItemGone(err))
}

func TestGetFileReader_success(t *testing.T) {
	fileContent := "streaming-bytes"
	downloadPath := "/" + testSourceRelativePath()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != downloadPath {
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(fileContent))
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	reader, err := client.GetFileReader(context.Background(), testSourceFile())
	require.NoError(t, err)
	require.NotNil(t, reader)
	defer func() {
		assert.NoError(t, reader.Close())
	}()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, fileContent, string(got))
}

func TestGetFileReader_notFound_closesBody(t *testing.T) {
	downloadPath := "/" + testSourceRelativePath()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != downloadPath {
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Not found"}]}`))
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	reader, err := client.GetFileReader(context.Background(), testSourceFile())
	assert.Nil(t, reader)
	assert.True(t, IsSourceItemGone(err))
}

func TestGetFileReader_cancellation_closesBody(t *testing.T) {
	downloadPath := "/" + testSourceRelativePath()
	requestCancelled := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != downloadPath {
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		for {
			select {
			case <-r.Context().Done():
				requestCancelled <- struct{}{}
				return
			default:
				_, _ = w.Write([]byte("x"))
				flusher.Flush()
				time.Sleep(5 * time.Millisecond)
			}
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader, err := client.GetFileReader(ctx, testSourceFile())
	require.NoError(t, err)
	require.NotNil(t, reader)

	buf := make([]byte, 1)
	_, err = reader.Read(buf)
	require.NoError(t, err)

	cancel()

	_, err = reader.Read(buf)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NoError(t, reader.Close())

	select {
	case <-requestCancelled:
	case <-time.After(time.Second):
		t.Fatal("expected server request to observe cancellation")
	}
}

func newTestSourceServerDetails(serverURL string) *config.ServerDetails {
	return &config.ServerDetails{
		Url:            serverURL + "/",
		ArtifactoryUrl: serverURL + "/",
	}
}

func TestNewSourceClient_streamServiceManagerHasNoOverallTimeoutOrRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	streamConfig := client.streamServiceManager.GetConfig()
	assert.Zero(t, streamConfig.GetOverallRequestTimeout())
	assert.Zero(t, streamConfig.GetHttpRetries())

	metadataConfig := client.metadataServiceManager.GetConfig()
	assert.Equal(t, time.Minute, metadataConfig.GetOverallRequestTimeout())
	assert.Equal(t, retries, metadataConfig.GetHttpRetries())
}

func TestGetFileReader_unrelatedErrorContaining404_notSourceGone(t *testing.T) {
	downloadPath := "/" + testSourceRelativePath()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != downloadPath {
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":[{"message":"upstream returned 404 from cache"}]}`))
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	reader, err := client.GetFileReader(context.Background(), testSourceFile())
	assert.Nil(t, reader)
	assert.Error(t, err)
	assert.False(t, IsSourceItemGone(err))
}

func TestGetFileReader_streamSurvivesSlowResponse(t *testing.T) {
	downloadPath := "/" + testSourceRelativePath()
	fileContent := "delayed-stream"
	streamDelay := 300 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != downloadPath {
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
		time.Sleep(streamDelay)
		_, _ = w.Write([]byte(fileContent))
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), streamDelay*3)
	defer cancel()

	reader, err := client.GetFileReader(ctx, testSourceFile())
	require.NoError(t, err)
	require.NotNil(t, reader)
	defer func() {
		assert.NoError(t, reader.Close())
	}()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, fileContent, string(got))
}
