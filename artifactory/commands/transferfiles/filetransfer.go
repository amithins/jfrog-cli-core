package transferfiles

import (
	"context"
	"io"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
)

type fileTransferSource interface {
	GetFileMetadata(ctx context.Context, file api.FileRepresentation) (*SourceFileMetadata, error)
	GetFileReader(ctx context.Context, file api.FileRepresentation) (io.ReadCloser, error)
}

type fileTransferTarget interface {
	TryChecksumDeploy(ctx context.Context, metadata *SourceFileMetadata, options TargetDeployOptions) (ChecksumDeployOutcome, error)
	Put(ctx context.Context, metadata *SourceFileMetadata, reader io.Reader, options TargetDeployOptions) error
	CreateFolder(ctx context.Context, metadata *SourceFileMetadata, options TargetDeployOptions) error
	ApplyProperties(ctx context.Context, metadata *SourceFileMetadata, options TargetDeployOptions) (skippedLargeProps bool, err error)
	ApplyStatistics(ctx context.Context, metadata *SourceFileMetadata) error
}

type FileTransferOptions struct {
	TargetDeployOptions TargetDeployOptions
	BuildInfoRepo       bool
	StreamBufferSize    int
}

type TransferResult struct {
	Candidate         api.FileRepresentation
	Status            api.ChunkFileStatusType
	Err               error
	ChecksumDeployed  bool
	BytesTransferred  int64
	FileSize          int64
	DurationMillis    int64
	SkippedLargeProps bool
	SourceItemGone    bool
}

func (r TransferResult) ToFileUploadStatus() api.FileUploadStatusResponse {
	status := api.FileUploadStatusResponse{
		FileRepresentation: r.Candidate,
		SizeBytes:          r.FileSize,
		ChecksumDeployed:   r.ChecksumDeployed,
		Status:             r.Status,
	}
	if r.Err != nil {
		status.Reason = r.Err.Error()
	}
	return status
}

type FileTransfer struct {
	source  fileTransferSource
	target  fileTransferTarget
	options FileTransferOptions
}

func NewFileTransfer(source fileTransferSource, target fileTransferTarget, options FileTransferOptions) *FileTransfer {
	return &FileTransfer{
		source:  source,
		target:  target,
		options: options,
	}
}

// ConfigureOptions updates deploy options for the next repository phase.
// Phases run sequentially, so no synchronization is required.
func (ft *FileTransfer) ConfigureOptions(options FileTransferOptions) {
	ft.options = options
}

func (ft *FileTransfer) TransferFile(ctx context.Context, candidate api.FileRepresentation) TransferResult {
	startTime := time.Now()
	result := TransferResult{Candidate: candidate, Status: api.Success}

	metadata, err := ft.source.GetFileMetadata(ctx, candidate)
	if err != nil {
		if IsSourceItemGone(err) {
			result.SourceItemGone = true
			return ft.finalizeResult(result, startTime, nil, candidate)
		}
		return ft.finalizeResult(ft.failResult(result, err), startTime, nil, candidate)
	}

	if metadata.Name == "" {
		return ft.transferFolder(ctx, result, metadata, startTime, candidate)
	}

	outcome, err := ft.target.TryChecksumDeploy(ctx, metadata, ft.options.TargetDeployOptions)
	if err != nil {
		return ft.finalizeResult(ft.failResult(result, err), startTime, metadata, candidate)
	}
	if outcome == ChecksumDeployHit {
		result.ChecksumDeployed = true
		return ft.finalizeResult(ft.applyPropertiesAndStats(ctx, result, metadata), startTime, metadata, candidate)
	}

	reader, err := ft.source.GetFileReader(ctx, candidate)
	if err != nil {
		if IsSourceItemGone(err) {
			result.SourceItemGone = true
			return ft.finalizeResult(result, startTime, metadata, candidate)
		}
		return ft.finalizeResult(ft.failResult(result, err), startTime, metadata, candidate)
	}

	bufferSize := ft.options.StreamBufferSize
	if bufferSize <= 0 {
		bufferSize = defaultStreamBufferSize
	}
	bytesTransferred, err := StreamGetToPut(ctx, metadata.Size, reader, func(streamCtx context.Context, streamReader io.Reader) error {
		return ft.target.Put(streamCtx, metadata, streamReader, ft.options.TargetDeployOptions)
	}, bufferSize)
	result.BytesTransferred = bytesTransferred
	if err != nil {
		return ft.finalizeResult(ft.failResult(result, err), startTime, metadata, candidate)
	}

	return ft.finalizeResult(ft.applyPropertiesAndStats(ctx, result, metadata), startTime, metadata, candidate)
}

func (ft *FileTransfer) transferFolder(ctx context.Context, result TransferResult, metadata *SourceFileMetadata, startTime time.Time, candidate api.FileRepresentation) TransferResult {
	if candidate.NonEmptyDir && !hasEligibleProperties(metadata.Properties, ft.options.TargetDeployOptions) {
		result.Status = api.SkippedNonEmptyDir
		return ft.finalizeResult(result, startTime, metadata, candidate)
	}
	if err := ft.target.CreateFolder(ctx, metadata, ft.options.TargetDeployOptions); err != nil {
		return ft.finalizeResult(ft.failResult(result, err), startTime, metadata, candidate)
	}
	return ft.finalizeResult(ft.applyPropertiesAndStats(ctx, result, metadata), startTime, metadata, candidate)
}

func (ft *FileTransfer) applyPropertiesAndStats(ctx context.Context, result TransferResult, metadata *SourceFileMetadata) TransferResult {
	skippedLargeProps, err := ft.target.ApplyProperties(ctx, metadata, ft.options.TargetDeployOptions)
	if err != nil {
		return ft.failResult(result, err)
	}
	result.SkippedLargeProps = skippedLargeProps
	if skippedLargeProps {
		result.Status = api.SkippedLargeProps
	}
	if err = ft.target.ApplyStatistics(ctx, metadata); err != nil {
		return ft.failResult(result, err)
	}
	return result
}

func (ft *FileTransfer) failResult(result TransferResult, err error) TransferResult {
	result.Status = api.Fail
	result.Err = err
	return result
}

func (ft *FileTransfer) finalizeResult(result TransferResult, startTime time.Time, metadata *SourceFileMetadata, candidate api.FileRepresentation) TransferResult {
	result.DurationMillis = time.Since(startTime).Milliseconds()
	if metadata != nil {
		result.FileSize = metadata.Size
	} else {
		result.FileSize = candidate.Size
	}
	return result
}

func hasEligibleProperties(properties map[string][]string, options TargetDeployOptions) bool {
	eligibleProps, _ := filterEligibleProperties(properties, options)
	return eligibleProps.KeysLen() > 0
}

var (
	_ fileTransferSource   = (*SourceClient)(nil)
	_ fileTransferTarget   = (*TargetClient)(nil)
	_ fileTransferExecutor = (*FileTransfer)(nil)
)
