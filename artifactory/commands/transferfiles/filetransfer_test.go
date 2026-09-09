package transferfiles

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockTransferSource struct {
	metadata       *SourceFileMetadata
	metadataErr    error
	reader         io.ReadCloser
	readerErr      error
	getReaderCalls int
}

func (m *mockTransferSource) GetFileMetadata(_ context.Context, _ api.FileRepresentation) (*SourceFileMetadata, error) {
	if m.metadataErr != nil {
		return nil, m.metadataErr
	}
	return m.metadata, nil
}

func (m *mockTransferSource) GetFileReader(_ context.Context, _ api.FileRepresentation) (io.ReadCloser, error) {
	m.getReaderCalls++
	if m.readerErr != nil {
		return nil, m.readerErr
	}
	return m.reader, nil
}

type mockTransferTarget struct {
	checksumOutcome ChecksumDeployOutcome
	checksumErr     error
	checksumCalls   int
	putErr          error
	putCalls        int
	propsSkipped    bool
	propsErr        error
	propsCalls      int
	statsErr        error
	statsCalls      int
	folderErr       error
	folderCalls     int
}

func (m *mockTransferTarget) TryChecksumDeploy(_ context.Context, _ *SourceFileMetadata, _ TargetDeployOptions) (ChecksumDeployOutcome, error) {
	m.checksumCalls++
	if m.checksumErr != nil {
		return ChecksumDeployMiss, m.checksumErr
	}
	return m.checksumOutcome, nil
}

func (m *mockTransferTarget) Put(_ context.Context, _ *SourceFileMetadata, reader io.Reader, _ TargetDeployOptions) error {
	m.putCalls++
	if m.putErr != nil {
		_, _ = io.Copy(io.Discard, reader)
		return m.putErr
	}
	_, err := io.Copy(io.Discard, reader)
	return err
}

func (m *mockTransferTarget) CreateFolder(_ context.Context, _ *SourceFileMetadata, _ TargetDeployOptions) error {
	m.folderCalls++
	return m.folderErr
}

func (m *mockTransferTarget) ApplyProperties(_ context.Context, _ *SourceFileMetadata, _ TargetDeployOptions) (bool, error) {
	m.propsCalls++
	return m.propsSkipped, m.propsErr
}

func (m *mockTransferTarget) ApplyStatistics(_ context.Context, _ *SourceFileMetadata) error {
	m.statsCalls++
	return m.statsErr
}

func testFileCandidate() api.FileRepresentation {
	return api.FileRepresentation{
		Repo: testSourceRepo,
		Path: testSourcePath,
		Name: testSourceName,
		Size: 11,
	}
}

func testFileMetadata() *SourceFileMetadata {
	return &SourceFileMetadata{
		Repo:         testSourceRepo,
		Path:         testSourcePath,
		Name:         testSourceName,
		Size:         11,
		Sha1:         "sha1-value",
		Sha256:       "sha256-value",
		Md5:          "md5-value",
		Created:      "2020-01-01T00:00:00.000Z",
		CreatedBy:    "admin",
		LastModified: "2020-01-02T00:00:00.000Z",
		ModifiedBy:   "deployer",
		Properties: map[string][]string{
			"build.name": {"app"},
		},
	}
}

func TestFileTransfer_checksumHit_skipsGet(t *testing.T) {
	source := &mockTransferSource{metadata: testFileMetadata()}
	target := &mockTransferTarget{checksumOutcome: ChecksumDeployHit}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), testFileCandidate())

	require.NoError(t, result.Err)
	assert.Equal(t, api.Success, result.Status)
	assert.True(t, result.ChecksumDeployed)
	assert.Zero(t, result.BytesTransferred)
	assert.Equal(t, int64(11), result.FileSize)
	assert.GreaterOrEqual(t, result.DurationMillis, int64(0))
	assert.Equal(t, 1, target.checksumCalls)
	assert.Zero(t, source.getReaderCalls)
	assert.Equal(t, 1, target.propsCalls)
	assert.Equal(t, 1, target.statsCalls)

	status := result.ToFileUploadStatus()
	assert.Equal(t, int64(11), result.FileSize)
	assert.Equal(t, int64(11), status.SizeBytes)
}

func TestFileTransfer_checksumMiss_streamsThenPropertiesAndStats(t *testing.T) {
	payload := "hello world"
	source := &mockTransferSource{
		metadata: testFileMetadata(),
		reader:   io.NopCloser(strings.NewReader(payload)),
	}
	target := &mockTransferTarget{checksumOutcome: ChecksumDeployMiss}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), testFileCandidate())

	require.NoError(t, result.Err)
	assert.Equal(t, api.Success, result.Status)
	assert.False(t, result.ChecksumDeployed)
	assert.Equal(t, int64(len(payload)), result.BytesTransferred)
	assert.Equal(t, 1, target.checksumCalls)
	assert.Equal(t, 1, source.getReaderCalls)
	assert.Equal(t, 1, target.putCalls)
	assert.Equal(t, 1, target.propsCalls)
	assert.Equal(t, 1, target.statsCalls)
}

func TestFileTransfer_checksumBeforeGet(t *testing.T) {
	order := make([]string, 0, 2)
	source := &orderTrackingSource{
		mockTransferSource: mockTransferSource{
			metadata: testFileMetadata(),
			reader:   io.NopCloser(strings.NewReader("hello world")),
		},
		onGetReader: func() { order = append(order, "get") },
	}
	target := &orderTrackingTarget{
		mockTransferTarget: mockTransferTarget{checksumOutcome: ChecksumDeployMiss},
		onChecksum:         func() { order = append(order, "checksum") },
	}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	_ = ft.TransferFile(context.Background(), testFileCandidate())
	assert.Equal(t, []string{"checksum", "get"}, order)
}

func TestFileTransfer_source404_isSuccessfulNoOp(t *testing.T) {
	source := &mockTransferSource{metadataErr: ErrSourceItemGone}
	target := &mockTransferTarget{}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), testFileCandidate())

	require.NoError(t, result.Err)
	assert.Equal(t, api.Success, result.Status)
	assert.True(t, result.SourceItemGone)
	assert.Zero(t, target.checksumCalls)
	assert.Zero(t, source.getReaderCalls)
}

func TestFileTransfer_putFailure_isRetryable(t *testing.T) {
	putErr := errors.New("target put failed")
	source := &mockTransferSource{
		metadata: testFileMetadata(),
		reader:   io.NopCloser(strings.NewReader("hello world")),
	}
	target := &mockTransferTarget{checksumOutcome: ChecksumDeployMiss, putErr: putErr}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), testFileCandidate())

	require.ErrorIs(t, result.Err, putErr)
	assert.Equal(t, api.Fail, result.Status)
}

func TestFileTransfer_retryOpensFreshGet(t *testing.T) {
	payload := "hello world"
	source := &mockTransferSource{
		metadata: testFileMetadata(),
		reader:   io.NopCloser(strings.NewReader(payload)),
	}
	target := &mockTransferTarget{checksumOutcome: ChecksumDeployMiss}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})

	result1 := ft.TransferFile(context.Background(), testFileCandidate())
	require.NoError(t, result1.Err)
	source.reader = io.NopCloser(strings.NewReader(payload))
	result2 := ft.TransferFile(context.Background(), testFileCandidate())
	require.NoError(t, result2.Err)
	assert.Equal(t, 2, source.getReaderCalls)
}

func TestFileTransfer_toFileUploadStatus_success(t *testing.T) {
	result := TransferResult{
		Candidate:        testFileCandidate(),
		Status:           api.Success,
		ChecksumDeployed: true,
		BytesTransferred: 0,
		FileSize:         testFileCandidate().Size,
	}
	status := result.ToFileUploadStatus()
	assert.Equal(t, api.Success, status.Status)
	assert.True(t, status.ChecksumDeployed)
	assert.Equal(t, testFileCandidate().Repo, status.Repo)
	assert.Equal(t, testFileCandidate().Size, status.SizeBytes)
}

func TestFileTransfer_checksumHit_toFileUploadStatus_usesLogicalFileSize(t *testing.T) {
	candidate := api.FileRepresentation{
		Repo: testSourceRepo,
		Path: testSourcePath,
		Name: testSourceName,
		Size: 42,
	}
	source := &mockTransferSource{metadata: &SourceFileMetadata{
		Repo: testSourceRepo,
		Path: testSourcePath,
		Name: testSourceName,
		Size: 42,
	}}
	target := &mockTransferTarget{checksumOutcome: ChecksumDeployHit}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), candidate)

	require.NoError(t, result.Err)
	assert.Zero(t, result.BytesTransferred)
	assert.Equal(t, int64(42), result.FileSize)
	assert.Equal(t, int64(42), result.ToFileUploadStatus().SizeBytes)
}

func testFolderCandidate(nonEmpty bool) api.FileRepresentation {
	return api.FileRepresentation{
		Repo:        testSourceRepo,
		Path:        "empty-dir",
		NonEmptyDir: nonEmpty,
	}
}

func testFolderMetadata() *SourceFileMetadata {
	return &SourceFileMetadata{
		Repo: testSourceRepo,
		Path: "empty-dir",
		Name: "",
		Size: 0,
	}
}

func TestFileTransfer_folderNonEmptyDirNoProperties_skipsCreateFolder(t *testing.T) {
	source := &mockTransferSource{metadata: testFolderMetadata()}
	target := &mockTransferTarget{}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), testFolderCandidate(true))

	require.NoError(t, result.Err)
	assert.Equal(t, api.SkippedNonEmptyDir, result.Status)
	assert.Zero(t, target.folderCalls)
	assert.Zero(t, target.propsCalls)
	assert.Zero(t, target.statsCalls)
	assert.GreaterOrEqual(t, result.DurationMillis, int64(0))
}

func TestFileTransfer_folderNonEmptyDirWithProperties_createsFolder(t *testing.T) {
	metadata := testFolderMetadata()
	metadata.Properties = map[string][]string{
		"build.name": {"app"},
	}
	source := &mockTransferSource{metadata: metadata}
	target := &mockTransferTarget{}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), testFolderCandidate(true))

	require.NoError(t, result.Err)
	assert.Equal(t, api.Success, result.Status)
	assert.Equal(t, 1, target.folderCalls)
	assert.Equal(t, 1, target.propsCalls)
	assert.Equal(t, 1, target.statsCalls)
}

func TestFileTransfer_emptyFolder_createsFolder(t *testing.T) {
	source := &mockTransferSource{metadata: testFolderMetadata()}
	target := &mockTransferTarget{}

	ft := NewFileTransfer(source, target, FileTransferOptions{TargetDeployOptions: defaultTargetDeployOptions()})
	result := ft.TransferFile(context.Background(), testFolderCandidate(false))

	require.NoError(t, result.Err)
	assert.Equal(t, api.Success, result.Status)
	assert.Equal(t, 1, target.folderCalls)
	assert.Equal(t, 1, target.propsCalls)
	assert.Equal(t, 1, target.statsCalls)
}

type orderTrackingSource struct {
	mockTransferSource
	onGetReader func()
}

func (s *orderTrackingSource) GetFileReader(ctx context.Context, file api.FileRepresentation) (io.ReadCloser, error) {
	if s.onGetReader != nil {
		s.onGetReader()
	}
	return s.mockTransferSource.GetFileReader(ctx, file)
}

type orderTrackingTarget struct {
	mockTransferTarget
	onChecksum func()
}

func (t *orderTrackingTarget) TryChecksumDeploy(ctx context.Context, metadata *SourceFileMetadata, options TargetDeployOptions) (ChecksumDeployOutcome, error) {
	if t.onChecksum != nil {
		t.onChecksum()
	}
	return t.mockTransferTarget.TryChecksumDeploy(ctx, metadata, options)
}
