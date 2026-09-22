package transferfiles

import (
	"context"
	"errors"
	"fmt"
	"io"
)

const defaultStreamBufferSize = 32 * 1024

var ErrStreamSizeMismatch = errors.New("stream body size mismatch")

func IsStreamSizeMismatch(err error) bool {
	return errors.Is(err, ErrStreamSizeMismatch)
}

type putFunc func(ctx context.Context, reader io.Reader) error

// StreamGetToPut pipes source GET bytes into target PUT using a bounded buffer.
// expectedSize must match the exact number of bytes read from source.
func StreamGetToPut(ctx context.Context, expectedSize int64, source io.ReadCloser, put putFunc, bufferSize int) (int64, error) {
	if bufferSize <= 0 {
		bufferSize = defaultStreamBufferSize
	}
	if source == nil {
		return 0, fmt.Errorf("source reader is nil")
	}

	pr, pw := io.Pipe()
	copyDone := make(chan streamCopyResult, 1)
	buf := make([]byte, bufferSize)

	go func() {
		var result streamCopyResult
		defer func() {
			copyDone <- result
		}()
		defer closePipeWriter(pw, &result.err)

		result.bytesCopied, result.err = copyWithExactSize(ctx, pw, source, expectedSize, buf)
	}()

	// put(ctx, pr) can block on pr.Read(), which only unblocks once the copy goroutine above
	// writes to (or closes) pw. If source.Read() is itself blocked and ctx-blind, that copy
	// goroutine never reaches pw, so a ctx cancellation would otherwise hang here forever:
	// nothing closes source or pw until after put returns, and put can't return until one of
	// them is closed. This watcher breaks that deadlock by closing both as soon as ctx is done,
	// without waiting for put to return first.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeReadCloser(source)
			_ = pw.CloseWithError(ctx.Err())
		case <-watchDone:
		}
	}()

	putErr := func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic in put: %v", recovered)
			}
		}()
		return put(ctx, pr)
	}()
	close(watchDone)
	closeReadCloser(pr)

	sourceClosed := false
	if putErr != nil {
		// Closing the pipe reader only unblocks writes to the pipe. Close the
		// source too so an in-flight source Read cannot keep this call hung.
		closeReadCloser(source)
		sourceClosed = true
	}
	copyResult := <-copyDone
	if !sourceClosed {
		closeReadCloser(source)
	}
	if putErr != nil {
		return copyResult.bytesCopied, errors.Join(putErr, copyResult.err)
	}
	if copyResult.err != nil {
		return copyResult.bytesCopied, copyResult.err
	}
	return copyResult.bytesCopied, nil
}

type streamCopyResult struct {
	bytesCopied int64
	err         error
}

func copyWithExactSize(ctx context.Context, dst io.Writer, src io.Reader, expectedSize int64, buf []byte) (int64, error) {
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		n, readErr := src.Read(buf)
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if n > 0 {
			if copied+int64(n) > expectedSize {
				return copied, fmt.Errorf("%w: expected %d bytes, read at least %d", ErrStreamSizeMismatch, expectedSize, copied+int64(n))
			}
			written, writeErr := dst.Write(buf[:n])
			if err := ctx.Err(); err != nil {
				return copied + int64(written), errors.Join(writeErr, err)
			}
			if writeErr != nil {
				return copied + int64(written), writeErr
			}
			if written != n {
				return copied + int64(written), io.ErrShortWrite
			}
			copied += int64(n)
		}
		if readErr == io.EOF {
			if copied != expectedSize {
				return copied, fmt.Errorf("%w: expected %d bytes, got %d", ErrStreamSizeMismatch, expectedSize, copied)
			}
			return copied, nil
		}
		if readErr != nil {
			return copied, readErr
		}
	}
}

func closePipeWriter(pw *io.PipeWriter, copyErr *error) {
	if pw == nil {
		return
	}
	if *copyErr != nil {
		_ = pw.CloseWithError(*copyErr)
		return
	}
	_ = pw.Close()
}
