package build

import (
	"fmt"
	"io"
	"strings"

	"github.com/btcsuite/btclog"
)

// LogType is an indicating the type of logging specified by the build flag.
type LogType byte

const (
	// LogTypeNone indicates no logging.
	LogTypeNone LogType = iota

	// LogTypeStdOut all logging is written directly to stdout.
	LogTypeStdOut

	// LogTypeDefault logs to both stdout and a given io.PipeWriter.
	LogTypeDefault
)

// String returns a human readable identifier for the logging type.
func (t LogType) String() string {
	switch t {
	case LogTypeNone:
		return "none"
	case LogTypeStdOut:
		return "stdout"
	case LogTypeDefault:
		return "default"
	default:
		return "unknown"
	}
}

// LogWriter is a stub type whose behavior can be changed using the build flags
// "stdlog" and "nolog". The default behavior is to write to both stdout and the
// RotatorPipe. Passing "stdlog" will cause it only to write to stdout, and
// "nolog" implements Write as a no-op.
type LogWriter struct {
	// RotatorPipe is the write-end pipe for writing to the log rotator.  It
	// is written to by the Write method of the LogWriter type. This only
	// needs to be set if neither the stdlog or nolog builds are set.
	RotatorPipe *io.PipeWriter
}

// NewSubLogger constructs a new subsystem log from the current LogWriter
// implementation. This is primarily intended for use with stdlog, as the actual
// writer is shared amongst all instantiations.
func NewSubLogger(subsystem string,
	genSubLogger func(string) btclog.Logger) btclog.Logger {

	switch Deployment {

	// For production builds, generate a new subsystem logger from the
	// primary log backend. If no function is provided, logging will be
	// disabled.
	case Production:
		if genSubLogger != nil {
			return genSubLogger(subsystem)
		}

	// For development builds, we must handle two distinct types of logging:
	// unit tests and running the live daemon, e.g. for integration testing.
	case Development:
		switch LoggingType {

		// Default logging is used when running the standalone daemon.
		// We'll use the optional sublogger constructor to mimic the
		// production behavior.
		case LogTypeDefault:
			if genSubLogger != nil {
				return genSubLogger(subsystem)
			}

		// Logging to stdout is used in unit tests. It is not important
		// that they share the same backend, since all output is written
		// to std out.
		case LogTypeStdOut:
			backend := btclog.NewBackend(&LogWriter{})
			logger := backend.Logger(subsystem)

			// Set the logging level of the stdout logger to use the
			// configured logging level specified by build flags.
			level, _ := btclog.LevelFromString(LogLevel)
			logger.SetLevel(level)

			return logger
		}
	}

	// For any other configurations, we'll disable logging.
	return btclog.Disabled
}

// SubLoggers is a type that holds a map of subsystem loggers keyed by their
// subsystem name.
type SubLoggers map[string]btclog.Logger

// LeveledSubLogger provides the ability to retrieve the subsystem loggers of
// a logger and set their log levels individually or all at once.
type LeveledSubLogger interface {
	// SubLoggers returns the map of all registered subsystem loggers.
	SubLoggers() SubLoggers

	// SupportedSubsystems returns a slice of strings containing the names
	// of the supported subsystems. Should ideally correspond to the keys
	// of the subsystem logger map and be sorted.
	SupportedSubsystems() []string

	// SetLogLevel assigns an individual subsystem logger a new log level.
	SetLogLevel(subsystemID string, logLevel string)

	// SetLogLevels assigns all subsystem loggers the same new log level.
	SetLogLevels(logLevel string)
}

// ParseAndSetDebugLevels attempts to parse the specified debug level and set
// the levels accordingly on the given logger. An appropriate error is returned
// if anything is invalid.
func ParseAndSetDebugLevels(level string, logger LeveledSubLogger) error {
	// Split at the delimiter.
	levels := strings.Split(level, ",")
	if len(levels) == 0 {
		return fmt.Errorf("invalid log level: %v", level)
	}

	// If the first entry has no =, treat is as the log level for all
	// subsystems.
	globalLevel := levels[0]
	if !strings.Contains(globalLevel, "=") {
		// Validate debug log level.
		if !validLogLevel(globalLevel) {
			str := "the specified debug level [%v] is invalid"
			return fmt.Errorf(str, globalLevel)
		}

		// Change the logging level for all subsystems.
		logger.SetLogLevels(globalLevel)

		// The rest will target specific subsystems.
		levels = levels[1:]
	}

	// Go through the subsystem/level pairs while detecting issues and
	// update the log levels accordingly.
	for _, logLevelPair := range levels {
		if !strings.Contains(logLevelPair, "=") {
			str := "the specified debug level contains an " +
				"invalid subsystem/level pair [%v]"
			return fmt.Errorf(str, logLevelPair)
		}

		// Extract the specified subsystem and log level.
		fields := strings.Split(logLevelPair, "=")
		if len(fields) != 2 {
			str := "the specified debug level has an invalid " +
				"format [%v] -- use format subsystem1=level1," +
				"subsystem2=level2"
			return fmt.Errorf(str, logLevelPair)
		}
		subsysID, logLevel := fields[0], fields[1]
		subLoggers := logger.SubLoggers()

		// Validate subsystem.
		if _, exists := subLoggers[subsysID]; !exists {
			str := "the specified subsystem [%v] is invalid -- " +
				"supported subsystems are %v"
			return fmt.Errorf(
				str, subsysID, logger.SupportedSubsystems(),
			)
		}

		// Validate log level.
		if !validLogLevel(logLevel) {
			str := "the specified debug level [%v] is invalid"
			return fmt.Errorf(str, logLevel)
		}

		logger.SetLogLevel(subsysID, logLevel)
	}

	return nil
}

// validLogLevel returns whether or not logLevel is a valid debug log level.
func validLogLevel(logLevel string) bool {
	switch logLevel {
	case "trace":
		fallthrough
	case "debug":
		fallthrough
	case "info":
		fallthrough
	case "warn":
		fallthrough
	case "error":
		fallthrough
	case "critical":
		fallthrough
	case "off":
		return true
	}
	return false
}
