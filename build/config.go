package build

import "github.com/btcsuite/btclog/v2"

// LogConfig holds logging configuration options.
//
//nolint:lll
type LogConfig struct {
	Console *LoggerConfig `group:"console" namespace:"console" description:"The logger writing to stdout and stderr."`
	File    *LoggerConfig `group:"file" namespace:"file" description:"The logger writing to LND's standard log file."`
}

// DefaultLogConfig returns the default logging config options.
func DefaultLogConfig() *LogConfig {
	return &LogConfig{
		Console: &LoggerConfig{},
		File:    &LoggerConfig{},
	}
}

// LoggerConfig holds options for a particular logger.
//
//nolint:lll
type LoggerConfig struct {
	Disable      bool `long:"disable" description:"Disable this logger."`
	NoTimestamps bool `long:"no-timestamps" description:"Omit timestamps from log lines."`
}

// HandlerOptions returns the set of btclog.HandlerOptions that the state of the
// config struct translates to.
func (cfg *LoggerConfig) HandlerOptions() []btclog.HandlerOption {
	var opts []btclog.HandlerOption
	if cfg.NoTimestamps {
		opts = append(opts, btclog.WithNoTimestamp())
	}

	return opts
}
