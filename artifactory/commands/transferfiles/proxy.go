package transferfiles

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/auth/cert"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
)

const proxyKeyUsageHint = "pass an HTTP or HTTPS proxy URL (for example http://proxy:3128) or set HTTPS_PROXY on the CLI host"

type sourceProxyDescriptor struct {
	Key      string `json:"key"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"` // #nosec G117 -- proxy credential returned by source Artifactory
	Https    bool   `json:"https"`
}

type sourceProxiesResponse struct {
	Proxies []sourceProxyDescriptor `json:"proxies"`
}

func resolveProxyKeyURL(ctx context.Context, raw string, sourceServerDetails *config.ServerDetails) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errorutils.CheckErrorf("--proxy-key value %q is not valid: %s. %s",
			redactProxyKey(raw), err.Error(), proxyKeyUsageHint)
	}
	if parsed.Scheme != "" {
		return parseProxyKeyURL(raw)
	}
	return lookupSourceProxyURL(ctx, raw, sourceServerDetails)
}

func parseProxyKeyURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errorutils.CheckErrorf("--proxy-key value %q is not a valid URL: %s. %s", redactProxyKey(raw), err.Error(), proxyKeyUsageHint)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" || parsed.Host == "" {
		return nil, errorutils.CheckErrorf("--proxy-key value %q must be an HTTP or HTTPS proxy URL (for example http://proxy:3128). To proxy CLI traffic, set HTTPS_PROXY on the CLI host instead", redactProxyKey(raw))
	}
	return parsed, nil
}

func lookupSourceProxyURL(ctx context.Context, proxyKey string, sourceServerDetails *config.ServerDetails) (*url.URL, error) {
	if sourceServerDetails == nil {
		return nil, namedProxyLookupError(proxyKey, "source Artifactory is not configured")
	}
	serviceManager, err := createTransferServiceManager(ctx, sourceServerDetails, nil)
	if err != nil {
		return nil, namedProxyLookupError(proxyKey, err.Error())
	}
	artDetails := serviceManager.GetConfig().GetServiceDetails()
	proxiesURL, err := clientutils.BuildUrl(artDetails.GetUrl(),
		path.Join("api/system/configuration/platform/proxies"), map[string]string{})
	if err != nil {
		return nil, namedProxyLookupError(proxyKey, err.Error())
	}
	httpDetails := artDetails.CreateHttpClientDetails()
	resp, body, _, err := serviceManager.Client().SendGet(proxiesURL, true, &httpDetails)
	if err != nil {
		return nil, namedProxyLookupError(proxyKey, err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, namedProxyLookupError(proxyKey, "source Artifactory returned "+resp.Status)
	}

	var proxies sourceProxiesResponse
	if err = json.Unmarshal(body, &proxies); err != nil {
		return nil, namedProxyLookupError(proxyKey, "source Artifactory returned an invalid proxy response")
	}
	for _, proxy := range proxies.Proxies {
		if strings.EqualFold(proxy.Key, proxyKey) {
			proxyURL, buildErr := proxyDescriptorURL(proxy)
			if buildErr != nil {
				return nil, namedProxyLookupError(proxyKey, buildErr.Error())
			}
			return proxyURL, nil
		}
	}
	return nil, namedProxyLookupError(proxyKey, "the key was not found")
}

func proxyDescriptorURL(proxy sourceProxyDescriptor) (*url.URL, error) {
	host := strings.TrimSpace(proxy.Host)
	if host == "" {
		return nil, fmt.Errorf("the source proxy descriptor has no host")
	}
	scheme := "http"
	if proxy.Https {
		scheme = "https"
	}
	if strings.Contains(host, "://") {
		parsedHost, err := url.Parse(host)
		if err != nil || parsedHost.Host == "" || parsedHost.Path != "" && parsedHost.Path != "/" {
			return nil, fmt.Errorf("the source proxy descriptor has an invalid host")
		}
		if parsedHost.Scheme != "http" && parsedHost.Scheme != "https" {
			return nil, fmt.Errorf("the source proxy descriptor uses unsupported scheme %q", parsedHost.Scheme)
		}
		scheme = parsedHost.Scheme
		host = parsedHost.Host
	}
	if proxy.Port > 0 {
		hostname := host
		if parsedHost, err := url.Parse(scheme + "://" + host); err == nil && parsedHost.Hostname() != "" {
			hostname = parsedHost.Hostname()
		}
		host = net.JoinHostPort(hostname, strconv.Itoa(proxy.Port))
	}
	proxyURL := &url.URL{Scheme: scheme, Host: host}
	if isEncryptedProxyPassword(proxy.Password) {
		return nil, fmt.Errorf("the source API returned an encrypted proxy password that the CLI cannot use")
	}
	if proxy.Username != "" && strings.TrimSpace(proxy.Password) == "" {
		return nil, fmt.Errorf("the source API omitted the proxy password; authenticate to source with an access token so the password can be returned")
	}
	if proxy.Username == "" && proxy.Password != "" {
		return nil, fmt.Errorf("the source proxy descriptor has a password but no username")
	}
	if proxy.Username != "" {
		proxyURL.User = url.UserPassword(proxy.Username, proxy.Password)
	}
	return parseProxyKeyURL(proxyURL.String())
}

func isEncryptedProxyPassword(password string) bool {
	upper := strings.ToUpper(strings.TrimSpace(password))
	return strings.HasPrefix(upper, "**ENC**") ||
		strings.HasPrefix(upper, "ENC(") ||
		strings.HasPrefix(upper, "{AES}")
}

func namedProxyLookupError(proxyKey, reason string) error {
	return errorutils.CheckErrorf("could not resolve --proxy-key %q: it was looked up on the source Artifactory, but %s. %s",
		proxyKey, reason, proxyKeyUsageHint)
}

func redactProxyKey(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	return parsed.Redacted()
}

func newTargetProxyTransport(proxyURL *url.URL, serverDetails *config.ServerDetails) (http.RoundTripper, error) {
	insecureTls := false
	if serverDetails != nil {
		insecureTls = serverDetails.InsecureTls
	}
	transport := cloneDefaultTransport()
	transport.Proxy = http.ProxyURL(proxyURL)

	certsPath, err := coreutils.GetJfrogCertsDir()
	if err != nil {
		return nil, fmt.Errorf("failed resolving JFrog certs dir for --proxy-key: %w", err)
	}
	transport, err = cert.GetTransportWithLoadedCert(certsPath, insecureTls, transport)
	if err != nil {
		return nil, fmt.Errorf("failed loading TLS certs for --proxy-key: %w", err)
	}
	return transport, nil
}

func cloneDefaultTransport() *http.Transport {
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		return defaultTransport.Clone()
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
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
