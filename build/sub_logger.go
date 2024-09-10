package build

import (
	"fmt"
	"strings"

	"github.com/btcsuite/btclog"
)

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
