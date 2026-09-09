package transferfiles

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamGetToPut_success_exactSize(t *testing.T) {
	payload := strings.Repeat("a", 100)
	source := io.NopCloser(strings.NewReader(payload))
	var received []byte

	n, err := StreamGetToPut(context.Background(), int64(len(payload)), source, func(_ context.Context, reader io.Reader) error {
		var readErr error
		received, readErr = readAllWithBuffer(reader, defaultStreamBufferSize)
		return readErr
	}, defaultStreamBufferSize)
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), n)
	assert.Equal(t, payload, string(received))
}

func TestStreamGetToPut_shortBody_fails(t *testing.T) {
	source := io.NopCloser(strings.NewReader("short"))
	_, err := StreamGetToPut(context.Background(), 100, source, func(_ context.Context, reader io.Reader) error {
		_, copyErr := io.Copy(io.Discard, reader)
		return copyErr
	}, defaultStreamBufferSize)
	require.Error(t, err)
	assert.True(t, IsStreamSizeMismatch(err))
}

func TestStreamGetToPut_longBody_fails(t *testing.T) {
	source := io.NopCloser(strings.NewReader(strings.Repeat("x", 101)))
	_, err := StreamGetToPut(context.Background(), 100, source, func(_ context.Context, reader io.Reader) error {
		_, copyErr := io.Copy(io.Discard, reader)
		return copyErr
	}, defaultStreamBufferSize)
	require.Error(t, err)
	assert.True(t, IsStreamSizeMismatch(err))
}

func TestStreamGetToPut_putError_closesSource(t *testing.T) {
	sourceClosed := false
	source := &closeTrackingReadCloser{
		ReadCloser: io.NopCloser(strings.NewReader("payload")),
		onClose: func() {
			sourceClosed = true
		},
	}

	putErr := errors.New("put failed")
	_, err := StreamGetToPut(context.Background(), 7, source, func(_ context.Context, _ io.Reader) error {
		return putErr
	}, defaultStreamBufferSize)
	require.ErrorIs(t, err, putErr)
	assert.True(t, sourceClosed)
}

func TestStreamGetToPut_cancel_closesBothLegs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blockRead := make(chan struct{})

	source := &blockingReadCloser{
		data:  strings.Repeat("z", 1024),
		block: blockRead,
	}
	var putReader io.Reader
	putStarted := make(chan struct{})

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
		close(blockRead)
	}()

	_, err := StreamGetToPut(ctx, 1024, source, func(_ context.Context, reader io.Reader) error {
		putReader = reader
		close(putStarted)
		_, readErr := reader.Read(make([]byte, 1))
		return readErr
	}, defaultStreamBufferSize)
	require.Error(t, err)
	assert.True(t, source.closed)

	select {
	case <-putStarted:
		require.NotNil(t, putReader)
	case <-time.After(time.Second):
		t.Fatal("put did not start")
	}
}

func TestStreamGetToPut_usesBoundedBuffer_notReadAll(t *testing.T) {
	payload := strings.Repeat("b", defaultStreamBufferSize*3+17)
	source := io.NopCloser(strings.NewReader(payload))
	var maxRead int

	n, err := StreamGetToPut(context.Background(), int64(len(payload)), source, func(_ context.Context, reader io.Reader) error {
		buf := make([]byte, defaultStreamBufferSize)
		for {
			readN, readErr := reader.Read(buf)
			if readN > maxRead {
				maxRead = readN
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}, defaultStreamBufferSize)
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), n)
	assert.LessOrEqual(t, maxRead, defaultStreamBufferSize)
}

func TestStreamGetToPut_retryIssuesFreshReader(t *testing.T) {
	var getCount atomic.Int32
	payload := "retry-me"

	first := func() io.ReadCloser {
		getCount.Add(1)
		return io.NopCloser(strings.NewReader(payload))
	}

	putFn := func(_ context.Context, reader io.Reader) error {
		_, err := io.Copy(io.Discard, reader)
		return err
	}

	_, err := StreamGetToPut(context.Background(), int64(len(payload)), first(), putFn, defaultStreamBufferSize)
	require.NoError(t, err)
	_, err = StreamGetToPut(context.Background(), int64(len(payload)), first(), putFn, defaultStreamBufferSize)
	require.NoError(t, err)
	assert.Equal(t, int32(2), getCount.Load())
}

type closeTrackingReadCloser struct {
	io.ReadCloser
	onClose func()
}

func (c *closeTrackingReadCloser) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	if c.ReadCloser != nil {
		return c.ReadCloser.Close()
	}
	return nil
}

type blockingReadCloser struct {
	data   string
	offset int
	block  chan struct{}
	closed bool
}

func (b *blockingReadCloser) Read(p []byte) (int, error) {
	select {
	case <-b.block:
	case <-time.After(2 * time.Second):
	}
	if b.offset >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.offset:])
	b.offset += n
	return n, nil
}

func (b *blockingReadCloser) Close() error {
	b.closed = true
	return nil
}

func readAllWithBuffer(r io.Reader, bufSize int) ([]byte, error) {
	buf := make([]byte, bufSize)
	var out []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}
