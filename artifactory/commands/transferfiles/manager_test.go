package transferfiles

import (
	"sync"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunProducerConsumers(t *testing.T) {
	// Create the producer-consumers
	producerConsumerWrapper := newProducerConsumerWrapper()

	// Add 10 tasks for the chunkBuilderProducerConsumer. Each task provides a task to the chunkUploaderProducerConsumer.
	for i := 0; i < 10; i++ {
		_, err := producerConsumerWrapper.chunkBuilderProducerConsumer.AddTask(func(int) error {
			time.Sleep(time.Millisecond * 100)
			_, err := producerConsumerWrapper.chunkUploaderProducerConsumer.AddTask(
				func(int) error {
					time.Sleep(time.Millisecond)
					return nil
				},
			)
			assert.NoError(t, err)
			return nil
		})
		assert.NoError(t, err)
	}

	// Run the producer-consumers
	err := runProducerConsumers(&producerConsumerWrapper)
	assert.NoError(t, err)

	// Assert no active treads left in the producer-consumers
	assert.Zero(t, producerConsumerWrapper.chunkBuilderProducerConsumer.ActiveThreads())
	assert.Zero(t, producerConsumerWrapper.chunkUploaderProducerConsumer.ActiveThreads())
}

func TestPollingTasksManager_startFileTransferPathSetsWorkingThreads(t *testing.T) {
	stateManager, cleanUp := state.InitStateTest(t)
	defer cleanUp()

	const expectedThreads = 7
	prevUploaderThreads := curChunkUploaderThreads
	defer func() { curChunkUploaderThreads = prevUploaderThreads }()
	curChunkUploaderThreads = expectedThreads

	phaseBase := &phaseBase{
		stateManager: stateManager,
		fileTransfer: &fakeFileTransferExecutor{},
	}
	ptm := newPollingTasksManager(totalNumberPollingGoRoutines)
	var runWaitGroup sync.WaitGroup
	require.NoError(t, ptm.start(phaseBase, &runWaitGroup, nil))

	workingThreads, err := stateManager.GetWorkingThreads()
	require.NoError(t, err)
	assert.Equal(t, expectedThreads, workingThreads)

	ptm.stop()
	runWaitGroup.Wait()
}
