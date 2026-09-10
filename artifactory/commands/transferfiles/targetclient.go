package transferfiles

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jfrog/build-info-go/entities"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/artifactory"
	artifactoryutils "github.com/jfrog/jfrog-client-go/artifactory/services/utils"
	"github.com/jfrog/jfrog-client-go/auth"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

const (
	maxPropertyValueLength         = 2400
	maxPropertyEncodedStringLength = 4000
	itemStatisticsSuffix           = ":statistics"
)

type artifactoryStatsXML struct {
	XMLName          xml.Name `xml:"artifactory.stats"`
	DownloadCount    int64    `xml:"downloadCount"`
	LastDownloaded   int64    `xml:"lastDownloaded"`
	LastDownloadedBy string   `xml:"lastDownloadedBy"`
}

type updateItemPropertiesBody struct {
	Props map[string][]string `json:"props"`
}

type TargetDeployOptions struct {
	BuildInfoRepo             bool
	MinChecksumDeploySize     int64
	CheckExistenceInFilestore bool
	PackageType               string
}

type ChecksumDeployOutcome int

const (
	ChecksumDeployMiss ChecksumDeployOutcome = iota
	ChecksumDeployHit
)

type TargetClient struct {
	serverDetails          *config.ServerDetails
	metadataServiceManager artifactory.ArtifactoryServicesManager
	streamServiceManager   artifactory.ArtifactoryServicesManager
}

func NewTargetClient(ctx context.Context, serverDetails *config.ServerDetails, proxyTransport http.RoundTripper) (*TargetClient, error) {
	metadataServiceManager, err := createTransferServiceManager(ctx, serverDetails, httpClientWithTransport(proxyTransport, time.Minute))
	if err != nil {
		return nil, err
	}
	streamServiceManager, err := createStreamingTransferServiceManager(ctx, serverDetails, httpClientWithTransport(proxyTransport, 0))
	if err != nil {
		return nil, err
	}
	return &TargetClient{
		serverDetails:          serverDetails,
		metadataServiceManager: metadataServiceManager,
		streamServiceManager:   streamServiceManager,
	}, nil
}

func (tc *TargetClient) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := tc.metadataServiceManager.Ping()
	return err
}

func (tc *TargetClient) TryChecksumDeploy(ctx context.Context, metadata *SourceFileMetadata, options TargetDeployOptions) (ChecksumDeployOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ChecksumDeployMiss, err
	}
	if !shouldTryChecksumDeploy(metadata, options) {
		return ChecksumDeployMiss, nil
	}
	deployURL, err := tc.buildDeployURL(targetRelativePath(metadata))
	if err != nil {
		return ChecksumDeployMiss, err
	}
	deployURL += propertyMatrixSuffix(metadata, options)
	httpClientsDetails := tc.metadataServiceManager.GetConfig().GetServiceDetails().CreateHttpClientDetails()
	addDeployHeaders(&httpClientsDetails, tc.metadataServiceManager.GetConfig().GetServiceDetails(), metadata, options, true)
	resp, body, err := tc.metadataServiceManager.Client().SendPut(deployURL, nil, &httpClientsDetails)
	if err != nil {
		return ChecksumDeployMiss, err
	}
	if isChecksumDeployHit(resp.StatusCode) {
		return ChecksumDeployHit, nil
	}
	if isChecksumDeployMiss(resp.StatusCode) {
		return ChecksumDeployMiss, nil
	}
	return ChecksumDeployMiss, errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK, http.StatusCreated, http.StatusAccepted)
}

func (tc *TargetClient) Put(ctx context.Context, metadata *SourceFileMetadata, reader io.Reader, options TargetDeployOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deployURL, err := tc.buildDeployURL(targetRelativePath(metadata))
	if err != nil {
		return err
	}
	deployURL += propertyMatrixSuffix(metadata, options)
	details := fileDetailsFromMetadata(metadata)
	httpClientsDetails := tc.streamServiceManager.GetConfig().GetServiceDetails().CreateHttpClientDetails()
	addDeployHeaders(&httpClientsDetails, tc.streamServiceManager.GetConfig().GetServiceDetails(), metadata, options, false)
	artDetails := tc.streamServiceManager.GetConfig().GetServiceDetails()
	resp, body, err := artifactoryutils.UploadFileFromReader(reader, deployURL, &artDetails, details, httpClientsDetails, tc.streamServiceManager.Client())
	if err != nil {
		return err
	}
	if resp == nil {
		return errorutils.CheckErrorf("received empty response from target deploy")
	}
	if isSuccessfulDeployStatusCode(resp.StatusCode) {
		return nil
	}
	return errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK, http.StatusCreated, http.StatusAccepted)
}

func (tc *TargetClient) CreateFolder(ctx context.Context, metadata *SourceFileMetadata, options TargetDeployOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deployURL, err := tc.buildDeployURL(targetRelativePath(metadata))
	if err != nil {
		return err
	}
	deployURL += propertyMatrixSuffix(metadata, options)
	httpClientsDetails := tc.metadataServiceManager.GetConfig().GetServiceDetails().CreateHttpClientDetails()
	addIdentityHeaders(&httpClientsDetails, metadata)
	resp, body, err := tc.metadataServiceManager.Client().SendPut(deployURL, nil, &httpClientsDetails)
	if err != nil {
		return err
	}
	if isSuccessfulDeployStatusCode(resp.StatusCode) {
		return nil
	}
	return errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK, http.StatusCreated, http.StatusAccepted)
}

func (tc *TargetClient) ApplyProperties(ctx context.Context, metadata *SourceFileMetadata, options TargetDeployOptions) (skippedLargeProps bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	eligibleProps, skippedLargeProps := filterEligibleProperties(metadata.Properties, options)
	if eligibleProps.KeysLen() == 0 {
		return skippedLargeProps, nil
	}
	if len(eligibleProps.ToEncodedString(false)) <= maxPropertyEncodedStringLength {
		// Short property maps are applied as matrix params on checksum/full PUT.
		return skippedLargeProps, nil
	}
	relativePath := strings.TrimSuffix(targetRelativePath(metadata), "/")
	return skippedLargeProps, tc.applyPropertiesViaPatch(relativePath, eligibleProps)
}

func (tc *TargetClient) applyPropertiesViaPatch(relativePath string, eligibleProps *artifactoryutils.Properties) error {
	setPropertiesURL, err := clientutils.BuildUrl(tc.metadataServiceManager.GetConfig().GetServiceDetails().GetUrl(), path.Join("api", "metadata", relativePath), map[string]string{})
	if err != nil {
		return err
	}
	requestBody, err := json.Marshal(updateItemPropertiesBody{Props: eligibleProps.ToMap()})
	if err != nil {
		return errorutils.CheckError(err)
	}
	httpClientsDetails := tc.metadataServiceManager.GetConfig().GetServiceDetails().CreateHttpClientDetails()
	httpClientsDetails.SetContentTypeApplicationJson()
	resp, body, err := tc.metadataServiceManager.Client().SendPatch(setPropertiesURL, requestBody, &httpClientsDetails)
	if err != nil {
		return err
	}
	return errorutils.CheckResponseStatusWithBody(resp, body, http.StatusNoContent)
}

func (tc *TargetClient) ApplyStatistics(ctx context.Context, metadata *SourceFileMetadata) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if metadata.Name == "" || !hasDownloadStatistics(metadata) {
		return nil
	}
	relativePath := strings.TrimSuffix(targetRelativePath(metadata), "/")
	deployURL, err := tc.buildDeployURL(relativePath)
	if err != nil {
		log.Warn("Couldn't set stats for", relativePath+". Reason:", err.Error())
		return nil
	}
	statsXML, err := xml.Marshal(artifactoryStatsXML{
		DownloadCount:    metadata.DownloadCount,
		LastDownloaded:   metadata.LastDownloaded,
		LastDownloadedBy: metadata.LastDownloadedBy,
	})
	if err != nil {
		log.Warn("Couldn't set stats for", relativePath+". Reason:", err.Error())
		return nil
	}
	httpClientsDetails := tc.metadataServiceManager.GetConfig().GetServiceDetails().CreateHttpClientDetails()
	httpClientsDetails.AddHeader("Content-Type", "application/xml")
	resp, body, err := tc.metadataServiceManager.Client().SendPut(deployURL+itemStatisticsSuffix, statsXML, &httpClientsDetails)
	if err != nil {
		log.Warn("Couldn't set stats for", relativePath+". Reason:", err.Error())
		return nil
	}
	if err = errorutils.CheckResponseStatusWithBody(resp, body, http.StatusOK, http.StatusCreated, http.StatusNoContent); err != nil {
		log.Warn("Couldn't set stats for", relativePath+". Reason:", err.Error())
		return nil
	}
	log.Debug("Applied download statistics for", targetRelativePath(metadata))
	return nil
}

func hasDownloadStatistics(metadata *SourceFileMetadata) bool {
	return metadata.DownloadCount > 0 || metadata.LastDownloaded > 0 || metadata.LastDownloadedBy != ""
}

func (tc *TargetClient) buildDeployURL(relativePath string) (string, error) {
	return clientutils.BuildUrl(tc.metadataServiceManager.GetConfig().GetServiceDetails().GetUrl(), relativePath, map[string]string{})
}

func targetRelativePath(metadata *SourceFileMetadata) string {
	if metadata.Name == "" {
		folderPath := path.Join(metadata.Repo, metadata.Path)
		if !strings.HasSuffix(folderPath, "/") {
			folderPath += "/"
		}
		return folderPath
	}
	if metadata.Path == "." {
		return path.Join(metadata.Repo, metadata.Name)
	}
	return path.Join(metadata.Repo, metadata.Path, metadata.Name)
}

func shouldTryChecksumDeploy(metadata *SourceFileMetadata, options TargetDeployOptions) bool {
	if options.BuildInfoRepo || metadata.Name == "" {
		return false
	}
	if metadata.Sha1 == "" {
		return false
	}
	return metadata.Size >= options.MinChecksumDeploySize
}

func fileDetailsFromMetadata(metadata *SourceFileMetadata) *fileutils.FileDetails {
	return &fileutils.FileDetails{
		Size: metadata.Size,
		Checksum: entities.Checksum{
			Sha1: metadata.Sha1, Sha256: metadata.Sha256, Md5: metadata.Md5,
		},
	}
}

func addDeployHeaders(httpClientsDetails *httputils.HttpClientDetails, serviceDetails auth.ServiceDetails, metadata *SourceFileMetadata, options TargetDeployOptions, checksumDeploy bool) {
	addIdentityHeaders(httpClientsDetails, metadata)
	artifactoryutils.AddChecksumHeaders(httpClientsDetails.Headers, fileDetailsFromMetadata(metadata))
	artifactoryutils.AddAuthHeaders(httpClientsDetails.Headers, serviceDetails)
	if checksumDeploy {
		httpClientsDetails.AddHeader("X-Checksum-Deploy", "true")
		if options.CheckExistenceInFilestore {
			httpClientsDetails.AddHeader("X-Check-Binary-Existence-In-Filestore", "true")
			httpClientsDetails.AddHeader("X-Checksum-Md5", metadata.Md5)
			httpClientsDetails.AddHeader("X-Binary-Size", fmt.Sprintf("%d", metadata.Size))
		}
	}
}

func addIdentityHeaders(httpClientsDetails *httputils.HttpClientDetails, metadata *SourceFileMetadata) {
	if created, ok := artifactoryTimestampHeaderValue(metadata.Created); ok {
		httpClientsDetails.AddHeader("X-Artifactory-Created", created)
	}
	if metadata.CreatedBy != "" {
		httpClientsDetails.AddHeader("X-Artifactory-Created-By", metadata.CreatedBy)
	}
	if lastModified, ok := artifactoryTimestampHeaderValue(metadata.LastModified); ok {
		httpClientsDetails.AddHeader("X-Artifactory-Last-Modified", lastModified)
	}
	if metadata.ModifiedBy != "" {
		httpClientsDetails.AddHeader("X-Artifactory-Modified-By", metadata.ModifiedBy)
	}
}

func artifactoryTimestampHeaderValue(timestamp string) (string, bool) {
	if timestamp == "" {
		return "", false
	}
	if _, err := strconv.ParseInt(timestamp, 10, 64); err == nil {
		return timestamp, true
	}
	parsed, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(parsed.UnixMilli(), 10), true
}

func filterEligibleProperties(properties map[string][]string, options TargetDeployOptions) (*artifactoryutils.Properties, bool) {
	eligibleProps := artifactoryutils.NewProperties()
	skippedLargeProps := false
	for key, values := range properties {
		if isGeneratedPropertyKey(key, options.PackageType) {
			continue
		}
		for _, value := range values {
			if len(value) > maxPropertyValueLength {
				skippedLargeProps = true
				continue
			}
			eligibleProps.AddProperty(key, value)
		}
	}
	return eligibleProps, skippedLargeProps
}

func propertyMatrixSuffix(metadata *SourceFileMetadata, options TargetDeployOptions) string {
	if metadata == nil {
		return ""
	}
	eligibleProps, _ := filterEligibleProperties(metadata.Properties, options)
	if eligibleProps.KeysLen() == 0 {
		return ""
	}
	encoded := eligibleProps.ToEncodedString(false)
	if encoded == "" || len(encoded) > maxPropertyEncodedStringLength {
		return ""
	}
	return ";" + encoded
}

func isChecksumDeployHit(statusCode int) bool {
	return statusCode == http.StatusOK || statusCode == http.StatusCreated || statusCode == http.StatusAccepted
}

// Only a missing blob (404) falls through to a full-body PUT.
// 409 means the target path exists with a different checksum; overwrite is not attempted.
func isChecksumDeployMiss(statusCode int) bool {
	return statusCode == http.StatusNotFound
}

func isSuccessfulDeployStatusCode(statusCode int) bool {
	return statusCode == http.StatusOK || statusCode == http.StatusCreated || statusCode == http.StatusAccepted
}
