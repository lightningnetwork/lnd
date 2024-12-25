package build

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/btcsuite/btclog/v2"
)

// SubLogCreator can be used to create a new logger for a particular subsystem.
type SubLogCreator interface {
	// Logger returns a new logger for a particular subsystem.
	Logger(subsystemTag string) btclog.Logger
}

// SubLoggerManager manages a set of subsystem loggers. Level updates will be
// applied to all the loggers managed by the manager.
type SubLoggerManager struct {
	genLogger SubLogCreator

	loggers SubLoggers
	mu      sync.Mutex
}

// A compile time check to ensure SubLoggerManager implements the
// LeveledSubLogger interface.
var _ LeveledSubLogger = (*SubLoggerManager)(nil)

// NewSubLoggerManager constructs a new SubLoggerManager.
func NewSubLoggerManager(handlers ...btclog.Handler) *SubLoggerManager {
	return &SubLoggerManager{
		loggers: make(SubLoggers),
		genLogger: newSubLogGenerator(
			newHandlerSet(btclog.LevelInfo, handlers...),
		),
	}
}

// GenSubLogger creates a new sub-logger and adds it to the set managed by the
// SubLoggerManager. A shutdown callback function is provided to be able to shut
// down in case of a critical error.
func (r *SubLoggerManager) GenSubLogger(subsystem string,
	shutdown func()) btclog.Logger {

	// Create a new logger with the given subsystem tag.
	logger := r.genLogger.Logger(subsystem)

	// Wrap the new logger in a Shutdown logger so that the shutdown
	// call back is called if a critical log is ever written via this new
	// logger.
	l := NewShutdownLogger(logger, shutdown)

	r.RegisterSubLogger(subsystem, l)

	return l
}

// RegisterSubLogger registers the given logger under the given subsystem name.
func (r *SubLoggerManager) RegisterSubLogger(subsystem string,
	logger btclog.Logger) {

	// Add the new logger to the set of loggers managed by the manager.
	r.mu.Lock()
	r.loggers[subsystem] = logger
	r.mu.Unlock()
}

// SubLoggers returns all currently registered subsystem loggers for this log
// writer.
//
// NOTE: This is part of the LeveledSubLogger interface.
func (r *SubLoggerManager) SubLoggers() SubLoggers {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.loggers
}

// SupportedSubsystems returns a sorted string slice of all keys in the
// subsystems map, corresponding to the names of the subsystems.
//
// NOTE: This is part of the LeveledSubLogger interface.
func (r *SubLoggerManager) SupportedSubsystems() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Convert the subsystemLoggers map keys to a string slice.
	subsystems := make([]string, 0, len(r.loggers))
	for subsysID := range r.loggers {
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
func (r *SubLoggerManager) SetLogLevel(subsystemID string, logLevel string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.setLogLevelUnsafe(subsystemID, logLevel)
}

// setLogLevelUnsafe sets the logging level for provided subsystem. Invalid
// subsystems are ignored. Uninitialized subsystems are dynamically created as
// needed.
//
// NOTE: the SubLoggerManager mutex must be held before calling this method.
func (r *SubLoggerManager) setLogLevelUnsafe(subsystemID string,
	logLevel string) {

	// Ignore invalid subsystems.
	logger, ok := r.loggers[subsystemID]
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
func (r *SubLoggerManager) SetLogLevels(logLevel string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Configure all sub-systems with the new logging level. Dynamically
	// create loggers as needed.
	for subsystemID := range r.loggers {
		r.setLogLevelUnsafe(subsystemID, logLevel)
	}
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
