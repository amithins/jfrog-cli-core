package transferfiles

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/auth/cert"
	"github.com/jfrog/jfrog-client-go/http/httpclient"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
)

const proxyKeyUsageHint = "pass an HTTP or HTTPS proxy URL (for example http://proxy:3128), or configure HTTPS_PROXY and NO_PROXY on the CLI host"

func parseProxyKeyURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errorutils.CheckErrorf("--proxy-key value %q is not a valid URL. %s",
			redactProxyKey(raw), proxyKeyUsageHint)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" || parsed.Host == "" {
		return nil, errorutils.CheckErrorf("--proxy-key value %q must be a full HTTP or HTTPS proxy URL including scheme (for example http://proxy:3128). %s",
			redactProxyKey(raw), proxyKeyUsageHint)
	}
	return parsed, nil
}

func redactProxyKey(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<unparseable proxy key>"
	}
	if parsed.User == nil {
		return raw
	}
	return parsed.Redacted()
}

func newTargetProxyTransport(proxyURL *url.URL, serverDetails *config.ServerDetails) (http.RoundTripper, error) {
	insecureTls := false
	if serverDetails != nil {
		insecureTls = serverDetails.InsecureTls
	}
	transport := newDefaultTargetTransport()
	transport.Proxy = http.ProxyURL(proxyURL)

	certsPath, err := coreutils.GetJfrogCertsDir()
	if err != nil {
		return nil, fmt.Errorf("failed resolving JFrog certs dir for --proxy-key: %w", err)
	}
	transport, err = cert.GetTransportWithLoadedCert(certsPath, insecureTls, transport)
	if err != nil {
		return nil, fmt.Errorf("failed loading TLS certs for --proxy-key: %w", err)
	}
	if serverDetails != nil {
		err = httpclient.ClientBuilder().
			SetClientCertPath(serverDetails.ClientCertPath).
			SetClientCertKeyPath(serverDetails.ClientCertKeyPath).
			AddClientCertToTransport(transport)
		if err != nil {
			return nil, fmt.Errorf("failed loading client certificate for --proxy-key: %w", err)
		}
	}
	// GetTransportWithLoadedCert replaces TLSClientConfig and drops MinVersion.
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	return transport, nil
}

func newDefaultTargetTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   httpclient.DefaultDialTimeout,
			KeepAlive: 20 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
}

func httpClientWithTransport(transport http.RoundTripper, timeout time.Duration) *http.Client {
	if transport == nil {
		return nil
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func (tdc *TransferFilesCommand) targetServiceHTTPClient() *http.Client {
	return httpClientWithTransport(tdc.targetProxyTransport, time.Minute)
}

func (tdc *TransferFilesCommand) httpClientForServer(serverDetails *config.ServerDetails) *http.Client {
	if tdc.targetProxyTransport == nil || serverDetails != tdc.targetServerDetails {
		return nil
	}
	return tdc.targetServiceHTTPClient()
}
