package build

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/btcsuite/btclog"
	"github.com/jrick/logrotate/rotator"
)

// RotatingLogWriter is a wrapper around the LogWriter that supports log file
// rotation.
type RotatingLogWriter struct {
	logWriter *LogWriter

	backendLog *btclog.Backend

	logRotator *rotator.Rotator

	subsystemLoggers SubLoggers
}

// A compile time check to ensure RotatingLogWriter implements the
// LeveledSubLogger interface.
var _ LeveledSubLogger = (*RotatingLogWriter)(nil)

// NewRotatingLogWriter creates a new file rotating log writer.
//
// NOTE: `InitLogRotator` must be called to set up log rotation after creating
// the writer.
func NewRotatingLogWriter() *RotatingLogWriter {
	logWriter := &LogWriter{}
	backendLog := btclog.NewBackend(logWriter)
	return &RotatingLogWriter{
		logWriter:        logWriter,
		backendLog:       backendLog,
		subsystemLoggers: SubLoggers{},
	}
}

// GenSubLogger creates a new sublogger. A shutdown callback function
// is provided to be able to shutdown in case of a critical error.
func (r *RotatingLogWriter) GenSubLogger(tag string, shutdown func()) btclog.Logger {
	logger := r.backendLog.Logger(tag)
	return NewShutdownLogger(logger, shutdown)
}

// RegisterSubLogger registers a new subsystem logger.
func (r *RotatingLogWriter) RegisterSubLogger(subsystem string,
	logger btclog.Logger) {

	r.subsystemLoggers[subsystem] = logger
}

// InitLogRotator initializes the log file rotator to write logs to logFile and
// create roll files in the same directory. It should be called as early on
// startup and possible and must be closed on shutdown by calling `Close`.
func (r *RotatingLogWriter) InitLogRotator(logFile string, maxLogFileSize int,
	maxLogFiles int) error {

	logDir, _ := filepath.Split(logFile)
	err := os.MkdirAll(logDir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create log directory: %v", err)
	}
	r.logRotator, err = rotator.New(
		logFile, int64(maxLogFileSize*1024), false, maxLogFiles,
	)
	if err != nil {
		return fmt.Errorf("failed to create file rotator: %v", err)
	}

	// Run rotator as a goroutine now but make sure we catch any errors
	// that happen in case something with the rotation goes wrong during
	// runtime (like running out of disk space or not being allowed to
	// create a new logfile for whatever reason).
	pr, pw := io.Pipe()
	go func() {
		err := r.logRotator.Run(pr)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"failed to run file rotator: %v\n", err)
		}
	}()

	r.logWriter.RotatorPipe = pw
	return nil
}

// Close closes the underlying log rotator if it has already been created.
func (r *RotatingLogWriter) Close() error {
	if r.logRotator != nil {
		return r.logRotator.Close()
	}
	return nil
}

// SubLoggers returns all currently registered subsystem loggers for this log
// writer.
//
// NOTE: This is part of the LeveledSubLogger interface.
func (r *RotatingLogWriter) SubLoggers() SubLoggers {
	return r.subsystemLoggers
}

// SupportedSubsystems returns a sorted string slice of all keys in the
// subsystems map, corresponding to the names of the subsystems.
//
// NOTE: This is part of the LeveledSubLogger interface.
func (r *RotatingLogWriter) SupportedSubsystems() []string {
	// Convert the subsystemLoggers map keys to a string slice.
	subsystems := make([]string, 0, len(r.subsystemLoggers))
	for subsysID := range r.subsystemLoggers {
		subsystems = append(subsystems, subsysID)
	}

	// Sort the subsystems for stable display.
	sort.Strings(subsystems)
	return subsystems
}

// SetLogLevel sets the logging level for provided subsystem. Invalid
// subsystems are ignored. Uninitialized subsystems are dynamically created as
// needed.
//
// NOTE: This is part of the LeveledSubLogger interface.
func (r *RotatingLogWriter) SetLogLevel(subsystemID string, logLevel string) {
	// Ignore invalid subsystems.
	logger, ok := r.subsystemLoggers[subsystemID]
	if !ok {
		return
	}

	// Defaults to info if the log level is invalid.
	level, _ := btclog.LevelFromString(logLevel)
	logger.SetLevel(level)
}

// SetLogLevels sets the log level for all subsystem loggers to the passed
// level. It also dynamically creates the subsystem loggers as needed, so it
// can be used to initialize the logging system.
//
// NOTE: This is part of the LeveledSubLogger interface.
func (r *RotatingLogWriter) SetLogLevels(logLevel string) {
	// Configure all sub-systems with the new logging level. Dynamically
	// create loggers as needed.
	for subsystemID := range r.subsystemLoggers {
		r.SetLogLevel(subsystemID, logLevel)
	}
}
