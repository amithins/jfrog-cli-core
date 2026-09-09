package transferfiles

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturingFileTransferExecutor struct {
	mu         sync.Mutex
	candidates []api.FileRepresentation
}

func (c *capturingFileTransferExecutor) TransferFile(_ context.Context, candidate api.FileRepresentation) TransferResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.candidates = append(c.candidates, candidate)
	return TransferResult{Candidate: candidate, Status: api.Success}
}

func TestTransferFiles_invokesExecutorPerCandidateAndReportsFailure(t *testing.T) {
	executor := &fakeFileTransferExecutor{}
	pcWrapper := newProducerConsumerWrapper()
	base := phaseBase{
		context:      context.Background(),
		fileTransfer: executor,
		pcDetails:    &pcWrapper,
	}
	delayHelper := delayUploadHelper{}
	errorsChannelMng := createErrorsChannelMng()

	var collected []ExtendedFileUploadStatusResponse
	var readWg sync.WaitGroup
	readWg.Add(1)
	go func() {
		defer readWg.Done()
		for e := range errorsChannelMng.channel {
			collected = append(collected, e)
		}
	}()

	files := []api.FileRepresentation{
		{Repo: "repo", Path: "a", Name: "ok.jar", Size: 1},
		{Repo: "repo", Path: "b", Name: "fail.jar", Size: 2},
	}
	shouldStop, err := transferFiles(files, base, delayHelper, &errorsChannelMng, &pcWrapper)
	assert.NoError(t, err)
	assert.False(t, shouldStop)

	assert.NoError(t, runProducerConsumers(&pcWrapper))
	errorsChannelMng.close()
	readWg.Wait()

	assert.Equal(t, 2, executor.callCount)
	assert.Len(t, collected, 1)
	assert.Equal(t, api.Fail, collected[0].Status)
	assert.Equal(t, "fail.jar", collected[0].Name)
	assert.Equal(t, "transfer failed", collected[0].Reason)
}

func TestTransferFiles_preservesNonEmptyDirFlag(t *testing.T) {
	executor := &capturingFileTransferExecutor{}
	pcWrapper := newProducerConsumerWrapper()
	base := phaseBase{
		context:      context.Background(),
		fileTransfer: executor,
		pcDetails:    &pcWrapper,
	}
	delayHelper := delayUploadHelper{}
	errorsChannelMng := createErrorsChannelMng()

	files := []api.FileRepresentation{
		{Repo: "repo", Path: "folder", NonEmptyDir: true},
	}
	shouldStop, err := transferFiles(files, base, delayHelper, &errorsChannelMng, &pcWrapper)
	assert.NoError(t, err)
	assert.False(t, shouldStop)
	assert.NoError(t, runProducerConsumers(&pcWrapper))

	require.Len(t, executor.candidates, 1)
	assert.True(t, executor.candidates[0].NonEmptyDir)
	assert.Equal(t, "folder", executor.candidates[0].Path)
}

func TestHandleTransferFileResult_successUpdatesStateAndTimeEstimation(t *testing.T) {
	stateManager, cleanUp := state.InitStateTest(t)
	defer cleanUp()

	const fileSize = int64(1024)
	assert.NoError(t, stateManager.SetRepoState("test-repo", 0, 0, false, true))
	assert.NoError(t, stateManager.SetWorkingThreads(1))

	phaseBase := &phaseBase{stateManager: stateManager}
	result := TransferResult{
		Candidate:      api.FileRepresentation{Repo: "test-repo", Path: "a", Name: "file.jar", Size: fileSize},
		Status:         api.Success,
		FileSize:       fileSize,
		DurationMillis: 100,
	}
	errorsChannelMng := createErrorsChannelMng()

	assert.NoError(t, handleTransferFileResult(phaseBase, result, &errorsChannelMng))
	assert.Equal(t, fileSize, stateManager.CurrentRepo.Phase1Info.TransferredSizeBytes)
	assert.Equal(t, int64(1), stateManager.CurrentRepo.Phase1Info.TransferredUnits)
	assert.Equal(t, uint64(fileSize), stateManager.TimeEstimationManager.CurrentTotalTransferredBytes)
	assert.NotEmpty(t, stateManager.TimeEstimationManager.LastSpeeds)
}

func TestHandleTransferFileResult_failureReportsToErrorsChannel(t *testing.T) {
	stateManager, cleanUp := state.InitStateTest(t)
	defer cleanUp()
	assert.NoError(t, stateManager.SetRepoState("test-repo", 0, 0, false, true))

	phaseBase := &phaseBase{stateManager: stateManager}
	result := TransferResult{
		Candidate: api.FileRepresentation{Repo: "test-repo", Path: "a", Name: "fail.jar", Size: 1},
		Status:    api.Fail,
		Err:       errors.New("transfer failed"),
		FileSize:  1,
	}
	errorsChannelMng := createErrorsChannelMng()

	var collected []ExtendedFileUploadStatusResponse
	var readWg sync.WaitGroup
	readWg.Add(1)
	go func() {
		defer readWg.Done()
		for e := range errorsChannelMng.channel {
			collected = append(collected, e)
		}
	}()

	assert.NoError(t, handleTransferFileResult(phaseBase, result, &errorsChannelMng))
	errorsChannelMng.close()
	readWg.Wait()

	require.Len(t, collected, 1)
	assert.Equal(t, api.Fail, collected[0].Status)
	assert.Equal(t, "fail.jar", collected[0].Name)
	assert.Equal(t, "transfer failed", collected[0].Reason)
	assert.Zero(t, stateManager.CurrentRepo.Phase1Info.TransferredSizeBytes)
}

func TestHandleTransferFileResult_skippedLargePropsUpdatesProgressAndErrors(t *testing.T) {
	stateManager, cleanUp := state.InitStateTest(t)
	defer cleanUp()

	const fileSize = int64(2048)
	assert.NoError(t, stateManager.SetRepoState("test-repo", 0, 0, false, true))
	assert.NoError(t, stateManager.SetWorkingThreads(1))

	dirNode, err := stateManager.LookUpNode("pkg")
	require.NoError(t, err)
	require.NoError(t, dirNode.IncrementFilesCount(uint64(fileSize)))
	require.NoError(t, dirNode.MarkDoneExploring())

	phaseBase := &phaseBase{stateManager: stateManager}
	result := TransferResult{
		Candidate:      api.FileRepresentation{Repo: "test-repo", Path: "pkg", Name: "artifact.jar", Size: fileSize},
		Status:         api.SkippedLargeProps,
		FileSize:       fileSize,
		DurationMillis: 50,
	}
	errorsChannelMng := createErrorsChannelMng()

	var collected []ExtendedFileUploadStatusResponse
	var readWg sync.WaitGroup
	readWg.Add(1)
	go func() {
		defer readWg.Done()
		for e := range errorsChannelMng.channel {
			collected = append(collected, e)
		}
	}()

	assert.NoError(t, handleTransferFileResult(phaseBase, result, &errorsChannelMng))
	errorsChannelMng.close()
	readWg.Wait()

	require.Len(t, collected, 1)
	assert.Equal(t, api.SkippedLargeProps, collected[0].Status)
	assert.Equal(t, "artifact.jar", collected[0].Name)
	assert.Equal(t, fileSize, stateManager.CurrentRepo.Phase1Info.TransferredSizeBytes)
	assert.Equal(t, int64(1), stateManager.CurrentRepo.Phase1Info.TransferredUnits)
	assert.Equal(t, uint64(fileSize), stateManager.TimeEstimationManager.CurrentTotalTransferredBytes)
	assert.NotEmpty(t, stateManager.TimeEstimationManager.LastSpeeds)

	transferredCount, _, err := dirNode.CalculateTransferredFilesAndSize()
	require.NoError(t, err)
	assert.EqualValues(t, 1, transferredCount)
}
