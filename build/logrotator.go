package build

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/jrick/logrotate/rotator"
	"github.com/klauspost/compress/zstd"
)

// RotatingLogWriter is a wrapper around the LogWriter that supports log file
// rotation.
type RotatingLogWriter struct {
	// pipe is the write-end pipe for writing to the log rotator.
	pipe *io.PipeWriter

	rotator *rotator.Rotator
	runErr  chan error

	closeOnce sync.Once
	closeErr  error
}

// NewRotatingLogWriter creates a new file rotating log writer.
//
// NOTE: `InitLogRotator` must be called to set up log rotation after creating
// the writer.
func NewRotatingLogWriter() *RotatingLogWriter {
	return &RotatingLogWriter{}
}

// InitLogRotator initializes the log file rotator to write logs to logFile and
// create roll files in the same directory. It should be called as early on
// startup and possible and must be closed on shutdown by calling `Close`.
func (r *RotatingLogWriter) InitLogRotator(cfg *FileLoggerConfig,
	logFile string) error {

	// Reject unknown compressors before opening the log file.
	if !SupportedLogCompressor(cfg.Compressor) {
		return fmt.Errorf("unknown log compressor: %v", cfg.Compressor)
	}

	var c rotator.Compressor
	var err error
	switch cfg.Compressor {
	case Gzip:
		c = gzip.NewWriter(nil)

	case Zstd:
		c, err = zstd.NewWriter(nil)
		if err != nil {
			return fmt.Errorf("failed to create zstd compressor: "+
				"%w", err)
		}
	}

	logDir, _ := filepath.Split(logFile)
	err = os.MkdirAll(logDir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	r.rotator, err = rotator.New(
		logFile, int64(cfg.MaxLogFileSize*1024), false, cfg.MaxLogFiles,
	)
	if err != nil {
		return fmt.Errorf("failed to create file rotator: %w", err)
	}

	// Apply the compressor and its file suffix to the log rotator.
	r.rotator.SetCompressor(c, logCompressors[cfg.Compressor])

	// Run the rotator from a single goroutine so parallel callers are
	// serialized by the pipe before they access the rotator.
	pr, pw := io.Pipe()
	r.runErr = make(chan error, 1)
	go func() {
		err := r.rotator.Run(pr)
		if errors.Is(err, io.EOF) {
			err = nil
		}

		// Propagate runtime rotation errors to blocked and future writers.
		_ = pr.CloseWithError(err)
		r.runErr <- err
	}()

	r.pipe = pw

	return nil
}

// Write writes the byte slice to the log rotator, if present.
func (r *RotatingLogWriter) Write(b []byte) (int, error) {
	if r.pipe != nil {
		return r.pipe.Write(b)
	}

	return len(b), nil
}

// Close closes the underlying log rotator if it has already been created.
func (r *RotatingLogWriter) Close() error {
	r.closeOnce.Do(func() {
		if r.pipe == nil {
			if r.rotator != nil {
				r.closeErr = r.rotator.Close()
			}

			return
		}

		pipeErr := r.pipe.Close()
		runErr := <-r.runErr
		rotatorErr := r.rotator.Close()

		r.closeErr = errors.Join(pipeErr, runErr, rotatorErr)
	})

	return r.closeErr
}
