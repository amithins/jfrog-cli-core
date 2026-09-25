package transferfiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	coreutils "github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/artifactory"
	artifactoryutils "github.com/jfrog/jfrog-client-go/artifactory/services/utils"
	"github.com/jfrog/jfrog-client-go/http/httpclient"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

var ErrSourceItemGone = errors.New("source item no longer exists")

func IsSourceItemGone(err error) bool {
	return errors.Is(err, ErrSourceItemGone)
}

type SourceFileMetadata struct {
	Repo             string
	Path             string
	Name             string
	Size             int64
	Sha1             string
	Sha256           string
	Md5              string
	Created          string
	CreatedBy        string
	LastModified     string
	ModifiedBy       string
	Properties       map[string][]string
	DownloadCount    int64
	LastDownloaded   int64
	LastDownloadedBy string
}

type itemStatistics struct {
	DownloadCount    int64  `json:"downloadCount"`
	LastDownloaded   int64  `json:"lastDownloaded"`
	LastDownloadedBy string `json:"lastDownloadedBy"`
}

type SourceClient struct {
	serverDetails          *config.ServerDetails
	metadataServiceManager artifactory.ArtifactoryServicesManager
	streamServiceManager   artifactory.ArtifactoryServicesManager
}

func createStreamingTransferServiceManager(ctx context.Context, serverDetails *config.ServerDetails, httpClient *http.Client) (artifactory.ArtifactoryServicesManager, error) {
	return coreutils.CreateServiceManagerWithContextAndHttpClient(ctx, serverDetails, false, 0, 0, 0, 0, httpClient)
}

func NewSourceClient(ctx context.Context, serverDetails *config.ServerDetails) (*SourceClient, error) {
	metadataServiceManager, err := createTransferServiceManager(ctx, serverDetails, nil)
	if err != nil {
		return nil, err
	}
	streamTransport, err := newDefaultStreamingTransport(serverDetails)
	if err != nil {
		return nil, err
	}
	streamServiceManager, err := createStreamingTransferServiceManager(ctx, serverDetails, httpClientWithTransport(streamTransport, 0))
	if err != nil {
		return nil, err
	}
	return &SourceClient{
		serverDetails:          serverDetails,
		metadataServiceManager: metadataServiceManager,
		streamServiceManager:   streamServiceManager,
	}, nil
}

func (sc *SourceClient) GetFileMetadata(ctx context.Context, file api.FileRepresentation) (*SourceFileMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	relativePath := fileRelativePath(file)
	fileInfo, err := sc.fetchFileInfo(ctx, sc.metadataServiceManager, relativePath)
	if err != nil {
		return nil, err
	}

	// Folder-info responses carry no "size" field; treat that as size 0 rather than a parse failure.
	// Fall back to the AQL-reported size when it's available.
	var size int64
	if fileInfo.Size == "" {
		if file.Size > 0 {
			size = file.Size
		}
	} else {
		var parseErr error
		size, parseErr = strconv.ParseInt(fileInfo.Size, 10, 64)
		if parseErr != nil {
			if file.Size > 0 {
				size = file.Size
			} else {
				return nil, fmt.Errorf("failed to parse file size %q for %s: %w", fileInfo.Size, relativePath, parseErr)
			}
		}
	}

	itemProps, err := sc.fetchItemProperties(ctx, sc.metadataServiceManager, relativePath)
	if err != nil {
		return nil, err
	}

	metadata := &SourceFileMetadata{
		Repo:         file.Repo,
		Path:         file.Path,
		Name:         file.Name,
		Size:         size,
		Sha1:         fileInfo.Checksums.Sha1,
		Sha256:       fileInfo.Checksums.Sha256,
		Md5:          fileInfo.Checksums.Md5,
		Created:      fileInfo.Created,
		CreatedBy:    fileInfo.CreatedBy,
		LastModified: fileInfo.LastModified,
		ModifiedBy:   fileInfo.ModifiedBy,
	}
	if itemProps != nil {
		metadata.Properties = itemProps.Properties
	}
	if file.Name != "" {
		stats, statsErr := sc.fetchItemStatistics(ctx, sc.metadataServiceManager, relativePath)
		if statsErr != nil {
			return nil, statsErr
		}
		if stats != nil {
			metadata.DownloadCount = stats.DownloadCount
			metadata.LastDownloaded = stats.LastDownloaded
			metadata.LastDownloadedBy = stats.LastDownloadedBy
		}
	}
	return metadata, nil
}

func (sc *SourceClient) fetchFileInfo(ctx context.Context, manager artifactory.ArtifactoryServicesManager, relativePath string) (*artifactoryutils.FileInfo, error) {
	resp, body, err := sendStorageGet(ctx, manager, relativePath, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		gone, confirmErr := sc.confirmItemGone(ctx, manager, relativePath)
		if confirmErr != nil {
			return nil, confirmErr
		}
		if gone {
			return nil, ErrSourceItemGone
		}
		// The initial 404 didn't hold up on a corroborating re-check: the item exists
		// after all (a transient proxy hiccup, restart window, etc). Surface this as a
		// plain, retryable error instead of silently treating the file as deleted.
		return nil, fmt.Errorf("received a 404 from the source storage-info endpoint for %q, but a corroborating existence check found the item still exists; treating this as a transient error, not a deletion", relativePath)
	}
	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
		return nil, err
	}

	result := &artifactoryutils.FileInfo{}
	if err = json.Unmarshal(body, result); err != nil {
		return nil, errorutils.CheckError(err)
	}
	return result, nil
}

func (sc *SourceClient) fetchItemProperties(ctx context.Context, manager artifactory.ArtifactoryServicesManager, relativePath string) (*artifactoryutils.ItemProperties, error) {
	resp, body, err := sendStorageGet(ctx, manager, relativePath, "properties")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		// Pinned to Artifactory 7 English error bodies: a properties 404 whose message
		// contains "No properties could be found" means the item exists with an empty set.
		if strings.Contains(string(body), "No properties could be found") {
			return nil, nil
		}
		// The body didn't match the pinned string (different locale/version, a proxy error
		// page, ...). Don't trust that alone to mean the item was deleted: corroborate with
		// a direct existence check before concluding "gone".
		gone, confirmErr := sc.confirmItemGone(ctx, manager, relativePath)
		if confirmErr != nil {
			return nil, confirmErr
		}
		if gone {
			return nil, ErrSourceItemGone
		}
		// Item still exists per the corroborating check: this was just an unrecognized
		// "no properties" response, not a deletion.
		return nil, nil
	}
	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
		return nil, err
	}

	result := &artifactoryutils.ItemProperties{}
	if err = json.Unmarshal(body, result); err != nil {
		return nil, errorutils.CheckError(err)
	}
	return result, nil
}

// confirmItemGone issues a fresh storage-info request to corroborate an ambiguous 404
// seen on another endpoint before the caller finalizes it as a deletion. This guards
// against a single 404 (from a transient proxy hiccup, a restart window, or an
// unrecognized error body) being silently mistaken for "item deleted".
func (sc *SourceClient) confirmItemGone(ctx context.Context, manager artifactory.ArtifactoryServicesManager, relativePath string) (bool, error) {
	resp, body, err := sendStorageGet(ctx, manager, relativePath, "")
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return true, nil
	}
	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
		return false, err
	}
	return false, nil
}

func (sc *SourceClient) fetchItemStatistics(ctx context.Context, manager artifactory.ArtifactoryServicesManager, relativePath string) (*itemStatistics, error) {
	resp, body, err := sendStorageGet(ctx, manager, relativePath, "stats")
	if err != nil {
		if isContextDoneError(ctx, err) {
			return nil, err
		}
		log.Warn("Couldn't get stats for", relativePath+". Reason:", err.Error())
		return nil, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK); err != nil {
		log.Warn("Couldn't get stats for", relativePath+". Reason:", err.Error())
		return nil, nil
	}

	result := &itemStatistics{}
	if err = json.Unmarshal(body, result); err != nil {
		log.Warn("Couldn't get stats for", relativePath+". Reason:", err.Error())
		return nil, nil
	}
	return result, nil
}

func sendStorageGet(ctx context.Context, manager artifactory.ArtifactoryServicesManager, relativePath, query string) (*http.Response, []byte, error) {
	artDetails := manager.GetConfig().GetServiceDetails()
	restAPI := path.Join("api/storage", path.Clean(relativePath))
	fullURL, err := clientutils.BuildUrl(artDetails.GetUrl(), restAPI, map[string]string{})
	if err != nil {
		return nil, nil, err
	}
	if query != "" {
		fullURL += "?" + query
	}
	return doManagerGet(ctx, manager, fullURL, true)
}

func doManagerGet(ctx context.Context, manager artifactory.ArtifactoryServicesManager, fullURL string, closeBody bool) (*http.Response, []byte, error) {
	httpClientsDetails := manager.GetConfig().GetServiceDetails().CreateHttpClientDetails()
	httpCli := manager.Client().GetHttpClient()
	cfg := manager.GetConfig()

	var resp *http.Response
	var body []byte
	// RetryExecutor sleeps between attempts with a plain time.Sleep and only checks ctx
	// between attempts, so cancellation can lag by up to one retry interval. Same behavior
	// as other client-go callers of RetryExecutor.
	retryExecutor := clientutils.RetryExecutor{
		Context:                  ctx,
		MaxRetries:               cfg.GetHttpRetries(),
		RetriesIntervalMilliSecs: cfg.GetHttpRetryWaitMilliSecs(),
		ErrorMessage:             fmt.Sprintf("Failure occurred while sending GET request to %s", fullURL),
		ExecutionHandler: func() (bool, error) {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
			if reqErr != nil {
				return !isContextDoneError(ctx, reqErr), reqErr
			}
			applyHttpClientDetails(req, httpClientsDetails)
			var doErr error
			resp, doErr = httpCli.GetClient().Do(req)
			if doErr != nil {
				if isContextDoneError(ctx, doErr) {
					return false, doErr
				}
				return true, doErr
			}
			if resp == nil {
				return false, errorutils.CheckErrorf("received empty response from server")
			}
			if shouldRetryHTTPStatus(resp.StatusCode) {
				drainAndClose(resp.Body)
				return true, nil
			}
			if closeBody {
				defer drainAndClose(resp.Body)
				var readErr error
				body, readErr = io.ReadAll(resp.Body)
				if readErr != nil {
					return false, readErr
				}
			}
			return false, nil
		},
	}
	err := retryExecutor.Execute()
	return resp, body, err
}

// applyHttpClientDetails mirrors the authentication, User-Agent and close-connection behavior
// that jfrog-client-go's http/httpclient.HttpClient.doRequest applies via its own
// setAuthentication/addUserAgentHeader helpers, for the raw *http.Client requests this package
// sends outside the service manager (so a per-call ctx can actually abort them). Keep both in
// sync if client-go adds an auth mode or changes those defaults.
func applyHttpClientDetails(req *http.Request, details httputils.HttpClientDetails) {
	// Match client-go's doRequest: don't reuse a persistent connection across requests.
	req.Close = true
	req.Header.Set("User-Agent", clientutils.GetUserAgent())
	if details.ApiKey != "" {
		if details.User != "" {
			req.SetBasicAuth(details.User, details.ApiKey)
		} else {
			req.Header.Set("X-JFrog-Art-Api", details.ApiKey)
		}
	} else if details.AccessToken != "" {
		if httpclient.IsApiKey(details.AccessToken) {
			req.SetBasicAuth(details.User, details.AccessToken)
		} else {
			req.Header.Set("Authorization", "Bearer "+details.AccessToken)
		}
	} else if details.Password != "" {
		req.SetBasicAuth(details.User, details.Password)
	}
	for name, value := range details.Headers {
		req.Header.Set(name, value)
	}
}

func shouldRetryHTTPStatus(statusCode int) bool {
	return statusCode >= 500 || statusCode == http.StatusTooManyRequests
}

func isContextDoneError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

func (sc *SourceClient) GetFileReader(ctx context.Context, file api.FileRepresentation) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	artDetails := sc.streamServiceManager.GetConfig().GetServiceDetails()
	readPath, err := clientutils.BuildUrl(artDetails.GetUrl(), fileRelativePath(file), map[string]string{})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readPath, nil)
	if err != nil {
		return nil, err
	}
	applyHttpClientDetails(req, artDetails.CreateHttpClientDetails())
	resp, err := sc.streamServiceManager.Client().GetHttpClient().GetClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errorutils.CheckErrorf("received empty response from source download")
	}
	if resp.StatusCode == http.StatusNotFound {
		drainAndClose(resp.Body)
		return nil, ErrSourceItemGone
	}
	if resp.StatusCode != http.StatusOK {
		drainAndClose(resp.Body)
		return nil, errorutils.CheckResponseStatus(resp, http.StatusOK)
	}
	return &contextReadCloser{ctx: ctx, rc: resp.Body}, nil
}

func fileRelativePath(file api.FileRepresentation) string {
	return path.Join(file.Repo, file.Path, file.Name)
}

func closeReadCloser(rc io.ReadCloser) {
	if rc != nil {
		_ = rc.Close()
	}
}

type contextReadCloser struct {
	ctx       context.Context
	rc        io.ReadCloser
	closeOnce sync.Once
	closeErr  error
}

func (c *contextReadCloser) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		_ = c.Close()
		return 0, err
	}
	return c.rc.Read(p)
}

// Close is idempotent: Read may already have closed rc on ctx cancellation, and callers
// close the reader again on their own error paths, which would otherwise double-close a
// non-idempotent underlying ReadCloser.
func (c *contextReadCloser) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.rc.Close()
	})
	return c.closeErr
}
