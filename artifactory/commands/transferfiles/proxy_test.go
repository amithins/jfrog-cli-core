package transferfiles

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProxyKeyURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "http", raw: "http://proxy:3128"},
		{name: "https", raw: "https://proxy:3128"},
		{name: "legacy key", raw: "corp-egress", wantErr: true},
		{name: "empty scheme host", raw: "http://", wantErr: true},
		{name: "socks5", raw: "socks5://proxy:1080", wantErr: true},
		{name: "userinfo redacted", raw: "http://user:secret@proxy:3128", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseProxyKeyURL(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "HTTPS_PROXY")
				assert.NotContains(t, err.Error(), "secret")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "proxy:3128", parsed.Host)
		})
	}
}

func TestResolveProxyKeyURL_URLFormDoesNotRequireSource(t *testing.T) {
	resolvedURL, err := resolveProxyKeyURL(context.Background(), "http://proxy:3128", nil)
	require.NoError(t, err)
	assert.Equal(t, "http://proxy:3128", resolvedURL.String())
}

func TestTargetClient_routesThroughProxy_sourceDoesNot(t *testing.T) {
	var artifactoryHits atomic.Int32
	var sourceStorageHits atomic.Int32
	storagePath := "/api/storage/" + testSourceRelativePath()
	artifactory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactoryHits.Add(1)
		if r.URL.Path == "/api/system/ping" {
			_, _ = w.Write([]byte("OK"))
			return
		}
		if strings.HasPrefix(r.URL.Path, storagePath) {
			sourceStorageHits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"checksums":{},"size":"0"}`))
	}))
	t.Cleanup(artifactory.Close)

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		outReq := r.Clone(r.Context())
		outReq.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vv := range resp.Header {
			w.Header()[k] = vv
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)

	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	transport, err := newTargetProxyTransport(proxyURL, newTestTargetServerDetails(artifactory.URL))
	require.NoError(t, err)

	targetClient, err := NewTargetClient(context.Background(), newTestTargetServerDetails(artifactory.URL), transport)
	require.NoError(t, err)
	require.NoError(t, targetClient.Ping(context.Background()))
	assert.Greater(t, proxyHits.Load(), int32(0), "target traffic must route through the proxy")

	hitsAfterTarget := proxyHits.Load()
	sourceClient, err := NewSourceClient(context.Background(), newTestTargetServerDetails(artifactory.URL))
	require.NoError(t, err)
	_, err = sourceClient.GetFileMetadata(context.Background(), api.FileRepresentation{
		Repo: testSourceRepo,
		Path: testSourcePath,
		Name: testSourceName,
	})
	require.NoError(t, err)
	assert.Greater(t, sourceStorageHits.Load(), int32(0), "source GET must reach Artifactory")
	assert.Equal(t, hitsAfterTarget, proxyHits.Load(), "source traffic must not route through the --proxy-key transport")
	assert.Greater(t, artifactoryHits.Load(), int32(0))
}

func TestNamedProxyLookup_routesTargetPingThroughProxy_sourceLookupDoesNot(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		assert.Equal(t, "/api/system/ping", r.URL.Path)
		_, _ = w.Write([]byte("OK"))
	}))
	t.Cleanup(target.Close)

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		outReq := r.Clone(r.Context())
		outReq.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)

	parsedProxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	proxyPort := parsedProxyURL.Port()
	var sourceLookupHits atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceLookupHits.Add(1)
		assert.Equal(t, "/api/system/configuration/platform/proxies", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"proxies":[{"key":"corp-egress","host":"127.0.0.1","port":` +
			proxyPort + `}]}`))
	}))
	t.Cleanup(source.Close)

	resolvedURL, err := resolveProxyKeyURL(context.Background(), "corp-egress",
		newTestSourceServerDetails(source.URL))
	require.NoError(t, err)
	assert.Equal(t, proxy.URL, resolvedURL.String())
	assert.Equal(t, int32(1), sourceLookupHits.Load())
	assert.Zero(t, proxyHits.Load(), "source proxy lookup must use the source client's normal transport")

	transport, err := newTargetProxyTransport(resolvedURL, newTestTargetServerDetails(target.URL))
	require.NoError(t, err)
	targetClient, err := NewTargetClient(context.Background(), newTestTargetServerDetails(target.URL), transport)
	require.NoError(t, err)
	require.NoError(t, targetClient.Ping(context.Background()))
	assert.Equal(t, int32(1), proxyHits.Load(), "target ping must route through the named proxy")
	assert.Equal(t, int32(1), targetHits.Load())
}

func TestNamedProxyLookup_missingKeyFailsClosed(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/system/configuration/platform/proxies", r.URL.Path)
		_, _ = w.Write([]byte(`{"proxies":[{"key":"another-proxy","host":"proxy","port":3128}]}`))
	}))
	t.Cleanup(source.Close)

	_, err := resolveProxyKeyURL(context.Background(), "corp-egress", newTestSourceServerDetails(source.URL))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"corp-egress"`)
	assert.Contains(t, err.Error(), "looked up on the source Artifactory")
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), "http://proxy:3128")
	assert.Contains(t, err.Error(), "HTTPS_PROXY")
}

func TestProxyDescriptorURL_rejectsUsernameWithoutPassword(t *testing.T) {
	_, err := proxyDescriptorURL(sourceProxyDescriptor{
		Host:     "proxy.example",
		Port:     3128,
		Username: "proxy-user",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "omitted the proxy password")
	assert.Contains(t, err.Error(), "access token")
}

func TestNamedProxyLookup_usernameWithoutPasswordFailsClosed(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/system/configuration/platform/proxies", r.URL.Path)
		_, _ = w.Write([]byte(`{"proxies":[{"key":"corp-egress","host":"proxy","port":3128,"username":"proxy-user"}]}`))
	}))
	t.Cleanup(source.Close)

	_, err := resolveProxyKeyURL(context.Background(), "corp-egress", newTestSourceServerDetails(source.URL))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"corp-egress"`)
	assert.Contains(t, err.Error(), "looked up on the source Artifactory")
	assert.Contains(t, err.Error(), "omitted the proxy password")
	assert.Contains(t, err.Error(), "access token")
	assert.Contains(t, err.Error(), "HTTPS_PROXY")
}

func TestProxyDescriptorURL_rejectsEncryptedPassword(t *testing.T) {
	_, err := proxyDescriptorURL(sourceProxyDescriptor{
		Host:     "proxy.example",
		Port:     3128,
		Password: "**ENC**unusable",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encrypted proxy password")
	assert.NotContains(t, err.Error(), "unusable")
}

func TestProxyDescriptorURL_buildsHTTPSURLWithCredentials(t *testing.T) {
	proxyURL, err := proxyDescriptorURL(sourceProxyDescriptor{
		Host:     "proxy.example",
		Port:     8443,
		Username: "proxy-user",
		Password: "secret",
		Https:    true,
	})
	require.NoError(t, err)
	assert.Equal(t, "https://proxy-user:secret@proxy.example:8443", proxyURL.String())
}
