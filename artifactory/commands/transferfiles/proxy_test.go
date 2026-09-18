package transferfiles

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
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
				assert.Contains(t, err.Error(), "NO_PROXY")
				assert.NotContains(t, err.Error(), "secret")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "proxy:3128", parsed.Host)
			assert.NotContains(t, redactProxyKey(tc.raw), "secret")
		})
	}
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

func TestParseProxyKeyURL_schemeLessFailsWithoutSourceLookup(t *testing.T) {
	var sourceLookupHits atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceLookupHits.Add(1)
	}))
	t.Cleanup(source.Close)

	_, err := parseProxyKeyURL("corp-egress")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"corp-egress"`)
	assert.Contains(t, err.Error(), "including scheme")
	assert.Contains(t, err.Error(), "http://proxy:3128")
	assert.Contains(t, err.Error(), "HTTPS_PROXY")
	assert.Contains(t, err.Error(), "NO_PROXY")
	assert.Zero(t, sourceLookupHits.Load())
}

func TestRedactProxyKey_parseFailureNeverReturnsRawInput(t *testing.T) {
	const malformed = "http://user:secret@proxy.example/%zz"
	assert.Equal(t, "<unparseable proxy key>", redactProxyKey(malformed))
	_, err := parseProxyKeyURL(malformed)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), malformed)
	assert.NotContains(t, err.Error(), "secret")
}

func TestNewTargetProxyTransport_matchesClientGoDefaults(t *testing.T) {
	proxyURL, err := url.Parse("http://proxy:3128")
	require.NoError(t, err)
	roundTripper, err := newTargetProxyTransport(proxyURL, &config.ServerDetails{})
	require.NoError(t, err)
	transport := roundTripper.(*http.Transport)
	assert.False(t, transport.ForceAttemptHTTP2)
	assert.NotNil(t, transport.DialContext)
	assert.Equal(t, 100, transport.MaxIdleConns)
	assert.Equal(t, 90*time.Second, transport.IdleConnTimeout)
	assert.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)
	assert.Equal(t, time.Second, transport.ExpectContinueTimeout)
	require.NotNil(t, transport.TLSClientConfig)
	assert.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
}

func TestNewTargetProxyTransport_loadsClientCertificate(t *testing.T) {
	certPath, keyPath := writeClientCertificate(t)
	proxyURL, err := url.Parse("http://proxy:3128")
	require.NoError(t, err)
	roundTripper, err := newTargetProxyTransport(proxyURL, &config.ServerDetails{
		ClientCertPath:    certPath,
		ClientCertKeyPath: keyPath,
	})
	require.NoError(t, err)
	transport := roundTripper.(*http.Transport)
	require.NotNil(t, transport.TLSClientConfig)
	assert.Len(t, transport.TLSClientConfig.Certificates, 1)
	assert.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
}

func TestFlaglessProxyUsesEnvironmentOnly(t *testing.T) {
	if os.Getenv("JFROG_TEST_FLAGLESS_ENV_PROXY") == "1" {
		req, err := http.NewRequest(http.MethodGet, "https://target.example/artifactory", nil)
		require.NoError(t, err)
		proxyURL, err := newDefaultTargetTransport().Proxy(req)
		require.NoError(t, err)
		require.NotNil(t, proxyURL)
		assert.Equal(t, "http://env-proxy:3128", proxyURL.String())
		return
	}

	cmd, err := NewTransferFilesCommand(nil, nil)
	require.NoError(t, err)
	assert.Nil(t, cmd.targetServiceHTTPClient())

	testCmd := exec.Command(os.Args[0], "-test.run=^TestFlaglessProxyUsesEnvironmentOnly$")
	testCmd.Env = append(os.Environ(),
		"JFROG_TEST_FLAGLESS_ENV_PROXY=1",
		"HTTPS_PROXY=http://env-proxy:3128",
		"NO_PROXY=",
	)
	output, err := testCmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func writeClientCertificate(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "proxy-client"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)

	certPath := filepath.Join(t.TempDir(), "client-cert.pem")
	keyPath := filepath.Join(filepath.Dir(certPath), "client-key.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: certDER,
	}), 0600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}), 0600))
	return certPath, keyPath
}
