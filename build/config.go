package build

import (
	"fmt"

	"github.com/btcsuite/btclog/v2"
)

const (
	callSiteOff   = "off"
	callSiteShort = "short"
	callSiteLong  = "long"

	defaultLogCompressor = Gzip

	// DefaultMaxLogFiles is the default maximum number of log files to
	// keep.
	DefaultMaxLogFiles = 10

	// DefaultMaxLogFileSize is the default maximum log file size in MB.
	DefaultMaxLogFileSize = 20
)

// LogConfig holds logging configuration options.
//
//nolint:lll
type LogConfig struct {
	Console *consoleLoggerCfg `group:"console" namespace:"console" description:"The logger writing to stdout and stderr."`
	File    *FileLoggerConfig `group:"file" namespace:"file" description:"The logger writing to LND's standard log file."`
}

// Validate validates the LogConfig struct values.
func (c *LogConfig) Validate() error {
	if !SupportedLogCompressor(c.File.Compressor) {
		return fmt.Errorf("invalid log compressor: %v",
			c.File.Compressor)
	}

	return nil
}

// LoggerConfig holds options for a particular logger.
//
//nolint:lll
type LoggerConfig struct {
	Disable      bool   `long:"disable" description:"Disable this logger."`
	NoTimestamps bool   `long:"no-timestamps" description:"Omit timestamps from log lines."`
	CallSite     string `long:"call-site" description:"Include the call-site of each log line." choice:"off" choice:"short" choice:"long"`
}

// DefaultLogConfig returns the default logging config options.
func DefaultLogConfig() *LogConfig {
	return &LogConfig{
		Console: defaultConsoleLoggerCfg(),
		File: &FileLoggerConfig{
			Compressor:     defaultLogCompressor,
			MaxLogFiles:    DefaultMaxLogFiles,
			MaxLogFileSize: DefaultMaxLogFileSize,
			LoggerConfig: LoggerConfig{
				CallSite: callSiteOff,
			},
		},
	}
}

// HandlerOptions returns the set of btclog.HandlerOptions that the state of the
// config struct translates to.
func (cfg *LoggerConfig) HandlerOptions() []btclog.HandlerOption {
	opts := []btclog.HandlerOption{
		// The default skip depth used by the logging library is 6 but
		// since we wrap the logging handlers with another level of
		// abstraction with the handlerSet, we increase the skip depth
		// to 7 here.
		btclog.WithCallSiteSkipDepth(7),
	}

	if cfg.NoTimestamps {
		opts = append(opts, btclog.WithNoTimestamp())
	}

	switch cfg.CallSite {
	case callSiteShort:
		opts = append(opts, btclog.WithCallerFlags(btclog.Lshortfile))
	case callSiteLong:
		opts = append(opts, btclog.WithCallerFlags(btclog.Llongfile))
	}

	return opts
}

// FileLoggerConfig extends LoggerConfig with specific log file options.
//
//nolint:lll
type FileLoggerConfig struct {
	LoggerConfig
	Compressor     string `long:"compressor" description:"Compression algorithm to use when rotating logs." choice:"gzip" choice:"zstd"`
	MaxLogFiles    int    `long:"max-files" description:"Maximum logfiles to keep (0 for no rotation)"`
	MaxLogFileSize int    `long:"max-file-size" description:"Maximum logfile size in MB"`
}
