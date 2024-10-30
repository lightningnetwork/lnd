package build

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jrick/logrotate/rotator"
	"github.com/klauspost/compress/zstd"
)

// RotatingLogWriter is a wrapper around the LogWriter that supports log file
// rotation.
type RotatingLogWriter struct {
	// pipe is the write-end pipe for writing to the log rotator.
	pipe *io.PipeWriter

	rotator *rotator.Rotator
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

	logDir, _ := filepath.Split(logFile)
	err := os.MkdirAll(logDir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	r.rotator, err = rotator.New(
		logFile, int64(cfg.MaxLogFileSize*1024), false, cfg.MaxLogFiles,
	)
	if err != nil {
		return fmt.Errorf("failed to create file rotator: %w", err)
	}

	// Reject unknown compressors.
	if !SupportedLogCompressor(cfg.Compressor) {
		return fmt.Errorf("unknown log compressor: %v", cfg.Compressor)
	}

	var c rotator.Compressor
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

	// Apply the compressor and its file suffix to the log rotator.
	r.rotator.SetCompressor(c, logCompressors[cfg.Compressor])

	// Run rotator as a goroutine now but make sure we catch any errors
	// that happen in case something with the rotation goes wrong during
	// runtime (like running out of disk space or not being allowed to
	// create a new logfile for whatever reason).
	pr, pw := io.Pipe()
	go func() {
		err := r.rotator.Run(pr)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"failed to run file rotator: %v\n", err)
		}
	}()

	r.pipe = pw

	return nil
}

// Write writes the byte slice to the log rotator, if present.
func (r *RotatingLogWriter) Write(b []byte) (int, error) {
	if r.rotator != nil {
		return r.rotator.Write(b)
	}

	return len(b), nil
}

// Close closes the underlying log rotator if it has already been created.
func (r *RotatingLogWriter) Close() error {
	if r.rotator != nil {
		return r.rotator.Close()
	}

	return nil
}
