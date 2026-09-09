package transferfiles

import (
	"sync"

	"github.com/jfrog/gofrog/parallel"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/state"
	clientUtils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

const (
	totalNumberPollingGoRoutines = 1
	tasksMaxCapacity             = 5000000
)

type transferManager struct {
	phaseBase
	delayUploadComparisonFunctions []shouldDelayUpload
}

func newTransferManager(base phaseBase, delayUploadComparisonFunctions []shouldDelayUpload) *transferManager {
	return &transferManager{phaseBase: base, delayUploadComparisonFunctions: delayUploadComparisonFunctions}
}

type transferActionWithProducerConsumerType func(
	pcWrapper *producerConsumerWrapper,
	delayHelper delayUploadHelper,
	errorsChannelMng *ErrorsChannelMng) error

type transferDelayAction func(phase phaseBase, addedDelayFiles []string) error

func (ftm *transferManager) doTransferWithProducerConsumer(transferAction transferActionWithProducerConsumerType, delayAction transferDelayAction) error {
	*ftm.pcDetails = newProducerConsumerWrapper()
	return ftm.doTransfer(ftm.pcDetails, transferAction, delayAction)
}

func (ftm *transferManager) doTransfer(pcWrapper *producerConsumerWrapper, transferAction transferActionWithProducerConsumerType, delayAction transferDelayAction) error {
	var runWaitGroup sync.WaitGroup
	var writersWaitGroup sync.WaitGroup

	errorsChannelMng := createErrorsChannelMng()
	transferErrorsMng, err := newTransferErrorsToFile(ftm.repoKey, ftm.phaseId, state.ConvertTimeToEpochMilliseconds(ftm.startTime), &errorsChannelMng, ftm.progressBar, ftm.stateManager)
	if err != nil {
		return err
	}
	writersWaitGroup.Add(1)
	go func() {
		defer writersWaitGroup.Done()
		errorsChannelMng.err = transferErrorsMng.start()
	}()

	delayedArtifactsChannelMng := createdDelayedArtifactsChannelMng()
	delayedArtifactsMng, err := newTransferDelayedArtifactsManager(&delayedArtifactsChannelMng, ftm.repoKey, state.ConvertTimeToEpochMilliseconds(ftm.startTime))
	if err != nil {
		return err
	}
	if len(ftm.delayUploadComparisonFunctions) > 0 {
		writersWaitGroup.Add(1)
		go func() {
			defer writersWaitGroup.Done()
			delayedArtifactsChannelMng.err = delayedArtifactsMng.start()
		}()
	}

	pollingTasksManager := newPollingTasksManager(totalNumberPollingGoRoutines)
	err = pollingTasksManager.start(&ftm.phaseBase, &runWaitGroup, pcWrapper)
	if err != nil {
		pollingTasksManager.stop()
		return err
	}
	runWaitGroup.Add(1)
	var actionErr error
	var delayUploadHelper = delayUploadHelper{
		ftm.delayUploadComparisonFunctions,
		&delayedArtifactsChannelMng,
	}
	go func() {
		defer runWaitGroup.Done()
		actionErr = transferAction(pcWrapper, delayUploadHelper, &errorsChannelMng)
		if pcWrapper == nil {
			pollingTasksManager.stop()
		}
	}()

	executionErr := runProducerConsumers(pcWrapper)
	pollingTasksManager.stop()
	runWaitGroup.Wait()
	errorsChannelMng.close()
	delayedArtifactsChannelMng.close()
	writersWaitGroup.Wait()

	var returnedError error
	for _, err := range []error{actionErr, errorsChannelMng.err, delayedArtifactsChannelMng.err, executionErr, ftm.getInterruptionErr()} {
		if err != nil {
			log.Error(err)
			returnedError = err
		}
	}

	if returnedError == nil && delayAction != nil {
		var addedDelayFiles []string
		if delayedArtifactsMng.delayedWriter != nil {
			addedDelayFiles = delayedArtifactsMng.delayedWriter.contentFiles
		}
		returnedError = delayAction(ftm.phaseBase, addedDelayFiles)
	}
	return returnedError
}

type PollingTasksManager struct {
	doneChannel            chan bool
	totalGoRoutines        int
	totalRunningGoRoutines int
}

func newPollingTasksManager(totalGoRoutines int) PollingTasksManager {
	return PollingTasksManager{doneChannel: make(chan bool, totalGoRoutines), totalGoRoutines: totalGoRoutines}
}

func (ptm *PollingTasksManager) start(phaseBase *phaseBase, runWaitGroup *sync.WaitGroup, pcWrapper *producerConsumerWrapper) error {
	runWaitGroup.Add(1)
	err := ptm.addGoRoutine()
	if err != nil {
		return err
	}
	go func() {
		defer runWaitGroup.Done()
		periodicallyUpdateThreadsAndStopStatus(pcWrapper, ptm.doneChannel, phaseBase.buildInfoRepo, phaseBase.stopSignal)
	}()

	if phaseBase.stateManager != nil {
		if err := phaseBase.stateManager.SetWorkingThreads(GetChunkUploaderThreads()); err != nil {
			log.Error("Couldn't set the current number of working threads:", err.Error())
		}
	}
	return nil
}

func (ptm *PollingTasksManager) addGoRoutine() error {
	if ptm.totalGoRoutines < ptm.totalRunningGoRoutines+1 {
		return errorutils.CheckErrorf("can't create another polling go routine. maximum number of go routines is: %d", ptm.totalGoRoutines)
	}
	ptm.totalRunningGoRoutines++
	return nil
}

func (ptm *PollingTasksManager) stop() {
	for i := 0; i < ptm.totalRunningGoRoutines; i++ {
		ptm.doneChannel <- true
	}
}

func newProducerConsumerWrapper() producerConsumerWrapper {
	chunkUploaderProducerConsumer := parallel.NewRunner(GetChunkUploaderThreads(), tasksMaxCapacity, false)
	chunkBuilderProducerConsumer := parallel.NewRunner(GetChunkBuilderThreads(), tasksMaxCapacity, false)
	chunkUploaderProducerConsumer.SetFinishedNotification(true)
	chunkBuilderProducerConsumer.SetFinishedNotification(true)
	errorsQueue := clientUtils.NewErrorsQueue(1)

	return producerConsumerWrapper{
		chunkUploaderProducerConsumer: chunkUploaderProducerConsumer,
		chunkBuilderProducerConsumer:  chunkBuilderProducerConsumer,
		errorsQueue:                   errorsQueue,
	}
}

func runProducerConsumers(pcWrapper *producerConsumerWrapper) (executionErr error) {
	go func() {
		pcWrapper.chunkUploaderProducerConsumer.Run()
	}()
	go func() {
		<-pcWrapper.chunkBuilderProducerConsumer.GetFinishedNotification()
		log.Debug("Chunk builder producer consumer has completed all tasks. " +
			"All files relevant to this phase were found and added to chunks that are being uploaded...")
		pcWrapper.chunkBuilderProducerConsumer.Done()
	}()

	pcWrapper.chunkBuilderProducerConsumer.Run()
	if pcWrapper.chunkUploaderProducerConsumer.IsStarted() {
		pcWrapper.chunkUploaderProducerConsumer.ResetFinishNotificationIfActive()
		<-pcWrapper.chunkUploaderProducerConsumer.GetFinishedNotification()
		log.Debug("Chunk uploaded producer consumer has completed all tasks. All files relevant to this phase have all been uploaded.")
	}
	pcWrapper.chunkUploaderProducerConsumer.Done()
	executionErr = pcWrapper.errorsQueue.GetError()
	return
}
