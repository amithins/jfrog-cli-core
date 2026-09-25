package transferfiles

import (
	"context"
	"encoding/json"
	"io"
	"net"
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
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

func TestGetFileMetadata_statsContextCancel_returnsError(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	statsReceived := make(chan struct{})

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
			close(statsReceived)
			<-r.Context().Done()
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct {
		metadata *SourceFileMetadata
		err      error
	}, 1)
	go func() {
		metadata, err := client.GetFileMetadata(ctx, testSourceFile())
		done <- struct {
			metadata *SourceFileMetadata
			err      error
		}{metadata, err}
	}()

	select {
	case <-statsReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive stats request in time")
	}
	cancel()

	select {
	case result := <-done:
		assert.Nil(t, result.metadata)
		assert.ErrorIs(t, result.err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("GetFileMetadata did not return promptly after stats cancellation")
	}
}

// TestGetFileMetadata_propertiesNotFoundOtherBody_itemStillExists_notGone verifies that an
// ambiguous properties-404 (a body that isn't the pinned "No properties could be found"
// string) does NOT get misclassified as a deletion when a corroborating storage-info check
// finds the item still exists. This is the fix for B-13/B-14/B-20: previously any such 404
// was silently treated as ErrSourceItemGone, even while the item was confirmed to still be
// there. The correct outcome is "no properties available" (nil properties, no error), not a
// silent skip.
func TestGetFileMetadata_propertiesNotFoundOtherBody_itemStillExists_notGone(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	var storageInfoRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			storageInfoRequests++
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"size": "11",
				"checksums": map[string]string{
					"sha1": "sha1-value",
				},
			}))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Not found"}]}`))
		case r.URL.Path == storagePath && r.URL.RawQuery == "stats":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Unable to find item"}]}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	require.NoError(t, err)
	require.NotNil(t, metadata)
	assert.False(t, IsSourceItemGone(err))
	assert.Nil(t, metadata.Properties)
	// Corroborating check must have hit storage-info a second time (once for the initial
	// fetch, once to confirm the item still exists after the ambiguous properties 404).
	assert.Equal(t, 2, storageInfoRequests)
}

// TestGetFileMetadata_propertiesNotFoundOtherBody_itemActuallyGone verifies that when the
// corroborating storage-info re-check also 404s, the item is correctly classified as gone.
func TestGetFileMetadata_propertiesNotFoundOtherBody_itemActuallyGone(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	storageInfoCall := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			storageInfoCall++
			if storageInfoCall == 1 {
				w.WriteHeader(http.StatusOK)
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"size": "11",
					"checksums": map[string]string{
						"sha1": "sha1-value",
					},
				}))
				return
			}
			// Corroborating re-check: the item is gone by the time properties are fetched.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Not found"}]}`))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Not found"}]}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	assert.Nil(t, metadata)
	assert.True(t, IsSourceItemGone(err))
}

// TestGetFileMetadata_notFound_confirmedByCorroboratingCheck verifies that a storage-info 404
// is only classified as ErrSourceItemGone once a second, corroborating storage-info request
// also confirms the item is gone.
func TestGetFileMetadata_notFound_confirmedByCorroboratingCheck(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Not found"}]}`))
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	assert.Nil(t, metadata)
	assert.True(t, IsSourceItemGone(err))
	assert.Equal(t, 2, requests, "expected the initial storage-info 404 plus one corroborating re-check")
}

// TestGetFileMetadata_notFound_transient_notGone verifies that a storage-info 404 which does
// NOT hold up on corroborating re-check (e.g. a transient proxy hiccup) is surfaced as a plain
// error, not silently classified as a deletion.
func TestGetFileMetadata_notFound_transient_notGone(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != storagePath || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		requestCount++
		if requestCount == 1 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Not found"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"size": "11",
			"checksums": map[string]string{
				"sha1": "sha1-value",
			},
		}))
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	assert.Nil(t, metadata)
	require.Error(t, err)
	assert.False(t, IsSourceItemGone(err), "a 404 that doesn't hold up on corroboration must not be classified as a deletion")
}

func TestGetFileReader_success(t *testing.T) {
	fileContent := "streaming-bytes"
	downloadPath := "/" + testSourceRelativePath()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != downloadPath {
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
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

// TestGetFileReader_nonRespondingPeer_boundedByResponseHeaderTimeout reproduces B-17(b): a peer
// that accepts the TCP connection but never writes any response (unlike a slow-but-responding
// server, or a connection refused/reset). Without a ResponseHeaderTimeout on the streaming
// transport, GetFileReader would block forever on such a peer, since the streaming service
// manager's overall http.Client.Timeout is intentionally 0 (see
// TestNewSourceClient_streamServiceManagerHasNoOverallTimeoutOrRetries) to allow large file
// bodies to stream for as long as they need. streamResponseHeaderTimeout is shrunk here so the
// test doesn't have to wait out the multi-minute production value.
func TestGetFileReader_nonRespondingPeer_boundedByResponseHeaderTimeout(t *testing.T) {
	origTimeout := streamResponseHeaderTimeout
	streamResponseHeaderTimeout = 200 * time.Millisecond
	defer func() { streamResponseHeaderTimeout = origTimeout }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			// Accept the connection and drain whatever the client sends, but never write a
			// response and never close the connection - simulating a hung peer.
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				for {
					if _, readErr := c.Read(buf); readErr != nil {
						return
					}
				}
			}(conn)
		}
	}()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails("http://"+ln.Addr().String()))
	require.NoError(t, err)

	type result struct {
		reader io.ReadCloser
		err    error
	}
	done := make(chan result, 1)
	go func() {
		reader, getErr := client.GetFileReader(context.Background(), testSourceFile())
		done <- result{reader, getErr}
	}()

	select {
	case res := <-done:
		require.Error(t, res.err, "a non-responding peer must fail once the response-header timeout elapses, not succeed")
		assert.Nil(t, res.reader)
	case <-time.After(5 * time.Second):
		t.Fatal("GetFileReader hung indefinitely against a non-responding peer instead of being bounded by streamResponseHeaderTimeout")
	}
}

// TestGetFileReader_contextCancel_abortsRoundTrip verifies that cancelling the caller
// context while the server has not yet sent any response headers causes GetFileReader
// to return promptly with context.Canceled, not block until the server decides to reply.
func TestGetFileReader_contextCancel_abortsRoundTrip(t *testing.T) {
	downloadPath := "/" + testSourceRelativePath()
	serverReceivedRequest := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != downloadPath {
			t.Errorf("unexpected request: %s", r.URL.String())
			return
		}
		// Signal that the server has the request; block until it's cancelled.
		close(serverReceivedRequest)
		<-r.Context().Done()
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.GetFileReader(ctx, testSourceFile())
		done <- err
	}()

	// Wait for server to start handling the request, then cancel.
	select {
	case <-serverReceivedRequest:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive request in time")
	}
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("GetFileReader did not return promptly after context cancellation")
	}
}

// TestGetFileMetadata_contextCancel_abortsMetadataRoundTrip verifies that cancelling the
// caller context while fetchFileInfo is waiting for a response causes GetFileMetadata to
// return promptly with context.Canceled.
func TestGetFileMetadata_contextCancel_abortsMetadataRoundTrip(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	serverReceivedRequest := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != storagePath {
			return
		}
		close(serverReceivedRequest)
		<-r.Context().Done()
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.GetFileMetadata(ctx, testSourceFile())
		done <- err
	}()

	select {
	case <-serverReceivedRequest:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive metadata request in time")
	}
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("GetFileMetadata did not return promptly after context cancellation")
	}
}

// TestGetFileMetadata_sizeParseError_noFallback verifies that when the fileinfo size
// field is unparseable and the caller's FileRepresentation carries no positive size,
// GetFileMetadata returns a wrapped, path-specific error rather than silently using zero.
func TestGetFileMetadata_sizeParseError_noFallback(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"size": "not-a-number",
				"checksums": map[string]string{
					"sha1": "sha1-value",
				},
			}))
		default:
			// other endpoints won't be reached before error is returned
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	// Size == 0: no positive fallback, so a parse error must be returned.
	file := testSourceFile()
	file.Size = 0

	metadata, err := client.GetFileMetadata(context.Background(), file)
	assert.Nil(t, metadata)
	require.Error(t, err)
	assert.Contains(t, err.Error(), testSourceRelativePath())
}

// TestGetFileMetadata_sizeParseError_fallbackUsed verifies that when the fileinfo size
// is unparseable but the FileRepresentation carries a positive size, the positive size
// is used as a fallback and no error is returned.
func TestGetFileMetadata_sizeParseError_fallbackUsed(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"size": "not-a-number",
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
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	// testSourceFile() has Size=11 > 0, so the fallback must be used.
	metadata, err := client.GetFileMetadata(context.Background(), testSourceFile())
	require.NoError(t, err)
	assert.Equal(t, int64(11), metadata.Size)
}

func TestGetFileMetadata_folderHasNoSizeField(t *testing.T) {
	storagePath := "/api/storage/" + testSourceRelativePath()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"repo":         testSourceRepo,
				"path":         testSourcePath + "/" + testSourceName,
				"created":      "2020-01-01T00:00:00.000Z",
				"createdBy":    "admin",
				"lastModified": "2020-01-02T00:00:00.000Z",
				"modifiedBy":   "deployer",
				"children":     []map[string]any{{"uri": "/child", "folder": false}},
			}))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"No properties could be found."}]}`))
		case r.URL.Path == storagePath && r.URL.RawQuery == "stats":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Unable to find item"}]}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	file := testSourceFile()
	file.Size = 0

	metadata, err := client.GetFileMetadata(context.Background(), file)
	require.NoError(t, err)
	require.NotNil(t, metadata)
	assert.Zero(t, metadata.Size)
}

// TestGetFileMetadata_folderItem_skipsStatsGET verifies that a real folder item (Name == "",
// as produced for AQL folder results) resolves Size 0 and never issues the "stats" GET, since
// download statistics don't apply to folders.
func TestGetFileMetadata_folderItem_skipsStatsGET(t *testing.T) {
	folderRelativePath := testSourceRepo + "/" + testSourcePath
	storagePath := "/api/storage/" + folderRelativePath
	statsRequested := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"repo":     testSourceRepo,
				"path":     testSourcePath,
				"children": []map[string]any{{"uri": "/child", "folder": false}},
			}))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"No properties could be found."}]}`))
		case r.URL.Path == storagePath && r.URL.RawQuery == "stats":
			statsRequested = true
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{}))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client, err := NewSourceClient(context.Background(), newTestSourceServerDetails(server.URL))
	require.NoError(t, err)

	folder := api.FileRepresentation{Repo: testSourceRepo, Path: testSourcePath, Name: ""}
	metadata, err := client.GetFileMetadata(context.Background(), folder)
	require.NoError(t, err)
	require.NotNil(t, metadata)
	assert.Zero(t, metadata.Size)
	assert.False(t, statsRequested, "stats GET must be skipped for a folder item")
}

// TestSourceClient_metadataAndContentGET_usesOnlySourceCredentials exercises both
// metadata GET and content GET against a live source httptest while a distinct
// target httptest must receive zero traffic.
func TestSourceClient_metadataAndContentGET_usesOnlySourceCredentials(t *testing.T) {
	const sourceToken = "source-secret-token"
	const targetToken = "target-secret-token"
	storagePath := "/api/storage/" + testSourceRelativePath()
	downloadPath := "/" + testSourceRelativePath()
	fileContent := "hello world"

	var sourceAuth []string
	var sourcePaths []string
	var sawContentGET bool
	targetHits := 0

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		t.Errorf("source client contacted target: %s %s auth=%q", r.Method, r.URL.String(), r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourcePaths = append(sourcePaths, r.URL.Path)
		sourceAuth = append(sourceAuth, r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == storagePath && r.URL.RawQuery == "":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"size":      strconv.Itoa(len(fileContent)),
				"checksums": map[string]string{"sha1": "sha1-value"},
			}))
		case r.URL.Path == storagePath && r.URL.RawQuery == "properties":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"No properties could be found."}]}`))
		case r.URL.Path == storagePath && r.URL.RawQuery == "stats":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Unable to find item"}]}`))
		case r.URL.Path == downloadPath:
			sawContentGET = true
			_, _ = w.Write([]byte(fileContent))
		default:
			t.Errorf("unexpected source request: %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer source.Close()

	sourceDetails := newTestSourceServerDetails(source.URL)
	sourceDetails.AccessToken = sourceToken
	targetDetails := newTestSourceServerDetails(target.URL)
	targetDetails.AccessToken = targetToken
	require.NotEqual(t, sourceDetails.ArtifactoryUrl, targetDetails.ArtifactoryUrl)

	client, err := NewSourceClient(context.Background(), sourceDetails)
	require.NoError(t, err)

	_, err = client.GetFileMetadata(context.Background(), testSourceFile())
	require.NoError(t, err)

	reader, err := client.GetFileReader(context.Background(), testSourceFile())
	require.NoError(t, err)
	require.NotNil(t, reader)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, fileContent, string(got))
	assert.NoError(t, reader.Close())

	require.NotEmpty(t, sourceAuth, "expected source traffic")
	require.True(t, sawContentGET, "content GET must be exercised")
	for i, auth := range sourceAuth {
		assert.Equal(t, "Bearer "+sourceToken, auth, "source request %d", i)
		assert.NotContains(t, auth, targetToken)
	}
	assert.Zero(t, targetHits)
	for _, p := range sourcePaths {
		assert.NotContains(t, p, "/api/plugins/execute")
	}
}
