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

	putErr := put(ctx, pr)
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
