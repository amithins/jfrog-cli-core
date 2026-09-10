package transferfiles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	coreutils "github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/artifactory"
	artifactoryutils "github.com/jfrog/jfrog-client-go/artifactory/services/utils"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
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
	streamServiceManager, err := createStreamingTransferServiceManager(ctx, serverDetails, nil)
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
	fileInfo, err := sc.fetchFileInfo(relativePath)
	if err != nil {
		return nil, err
	}

	itemProps, err := sc.fetchItemProperties(relativePath)
	if err != nil {
		return nil, err
	}

	size, parseErr := strconv.ParseInt(fileInfo.Size, 10, 64)
	if parseErr != nil && file.Size > 0 {
		size = file.Size
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
		stats, statsErr := sc.fetchItemStatistics(relativePath)
		if statsErr != nil {
			log.Warn("Couldn't get stats for", relativePath+". Reason:", statsErr.Error())
		} else if stats != nil {
			metadata.DownloadCount = stats.DownloadCount
			metadata.LastDownloaded = stats.LastDownloaded
			metadata.LastDownloadedBy = stats.LastDownloadedBy
		}
	}
	return metadata, nil
}

func (sc *SourceClient) fetchFileInfo(relativePath string) (*artifactoryutils.FileInfo, error) {
	client := sc.metadataServiceManager.Client()
	artDetails := sc.metadataServiceManager.GetConfig().GetServiceDetails()
	restAPI := path.Join("api/storage", path.Clean(relativePath))
	fullURL, err := clientutils.BuildUrl(artDetails.GetUrl(), restAPI, map[string]string{})
	if err != nil {
		return nil, err
	}

	httpClientsDetails := artDetails.CreateHttpClientDetails()
	resp, body, _, err := client.SendGet(fullURL, true, &httpClientsDetails)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrSourceItemGone
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

func (sc *SourceClient) fetchItemProperties(relativePath string) (*artifactoryutils.ItemProperties, error) {
	client := sc.metadataServiceManager.Client()
	artDetails := sc.metadataServiceManager.GetConfig().GetServiceDetails()
	restAPI := path.Join("api/storage", path.Clean(relativePath))
	propertiesURL, err := clientutils.BuildUrl(artDetails.GetUrl(), restAPI, map[string]string{})
	if err != nil {
		return nil, err
	}
	propertiesURL += "?properties"

	httpClientsDetails := artDetails.CreateHttpClientDetails()
	resp, body, _, err := client.SendGet(propertiesURL, true, &httpClientsDetails)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		if strings.Contains(string(body), "No properties could be found") {
			return nil, nil
		}
		return nil, ErrSourceItemGone
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

func (sc *SourceClient) fetchItemStatistics(relativePath string) (*itemStatistics, error) {
	client := sc.metadataServiceManager.Client()
	artDetails := sc.metadataServiceManager.GetConfig().GetServiceDetails()
	restAPI := path.Join("api/storage", path.Clean(relativePath))
	statsURL, err := clientutils.BuildUrl(artDetails.GetUrl(), restAPI, map[string]string{})
	if err != nil {
		return nil, err
	}
	statsURL += "?stats"

	httpClientsDetails := artDetails.CreateHttpClientDetails()
	resp, body, _, err := client.SendGet(statsURL, true, &httpClientsDetails)
	if err != nil {
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

func (sc *SourceClient) GetFileReader(ctx context.Context, file api.FileRepresentation) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	artDetails := sc.streamServiceManager.GetConfig().GetServiceDetails()
	readPath, err := clientutils.BuildUrl(artDetails.GetUrl(), fileRelativePath(file), map[string]string{})
	if err != nil {
		return nil, err
	}

	httpClientsDetails := artDetails.CreateHttpClientDetails()
	reader, resp, err := sc.streamServiceManager.Client().ReadRemoteFile(readPath, &httpClientsDetails)
	if err != nil {
		closeReadCloser(reader)
		return nil, err
	}
	if resp == nil {
		return nil, errorutils.CheckErrorf("received empty response from source download")
	}
	if resp.StatusCode == http.StatusNotFound {
		closeReadCloser(reader)
		return nil, ErrSourceItemGone
	}
	if resp.StatusCode != http.StatusOK {
		closeReadCloser(reader)
		return nil, errorutils.CheckResponseStatus(resp, http.StatusOK)
	}
	return &contextReadCloser{ctx: ctx, rc: reader}, nil
}

func fileRelativePath(file api.FileRepresentation) string {
	if file.Path == "." {
		return path.Join(file.Repo, file.Name)
	}
	return path.Join(file.Repo, file.Path, file.Name)
}

func closeReadCloser(rc io.ReadCloser) {
	if rc != nil {
		_ = rc.Close()
	}
}

type contextReadCloser struct {
	ctx context.Context
	rc  io.ReadCloser
}

func (c *contextReadCloser) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		_ = c.rc.Close()
		return 0, err
	}
	return c.rc.Read(p)
}

func (c *contextReadCloser) Close() error {
	return c.rc.Close()
}
