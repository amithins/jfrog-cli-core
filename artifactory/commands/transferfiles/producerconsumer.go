package transferfiles

import (
	"sync"

	"github.com/jfrog/gofrog/parallel"
	clientUtils "github.com/jfrog/jfrog-client-go/utils"
)

type producerConsumerWrapper struct {
	chunkUploaderProducerConsumer parallel.Runner
	chunkBuilderProducerConsumer  parallel.Runner
	errorsQueue                   *clientUtils.ErrorsQueue
	totalProcessedUploadChunks    int
	processedUploadChunksMutex    sync.Mutex
}

func (pcw *producerConsumerWrapper) incProcessedChunksWhenPossible() bool {
	pcw.processedUploadChunksMutex.Lock()
	defer pcw.processedUploadChunksMutex.Unlock()
	if pcw.totalProcessedUploadChunks < GetChunkUploaderThreads() {
		pcw.totalProcessedUploadChunks++
		return true
	}
	return false
}

func (pcw *producerConsumerWrapper) decProcessedChunks() {
	pcw.processedUploadChunksMutex.Lock()
	defer pcw.processedUploadChunksMutex.Unlock()
	pcw.totalProcessedUploadChunks--
}
