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
//nolint:ll
type LogConfig struct {
	Console      *consoleLoggerCfg `group:"console" namespace:"console" description:"The logger writing to stdout and stderr."`
	File         *FileLoggerConfig `group:"file" namespace:"file" description:"The logger writing to LND's standard log file."`
	NoCommitHash bool              `long:"no-commit-hash" description:"If set, the commit-hash of the current build will not be included in log lines by default."`
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
//nolint:ll
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
			LoggerConfig: &LoggerConfig{
				CallSite: callSiteOff,
			},
		},
	}
}

// HandlerOptions returns the set of btclog.HandlerOptions that the state of the
// config struct translates to.
func (cfg *LoggerConfig) HandlerOptions() []btclog.HandlerOption {
	opts := []btclog.HandlerOption{
		// We wrap the logger provided by the logging library with
		// another layer of abstraction with the handlerSet, and so we
		// need to increase the default skip depth by 1.
		btclog.WithCallSiteSkipDepth(btclog.DefaultSkipDepth + 1),
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
//nolint:ll
type FileLoggerConfig struct {
	*LoggerConfig  `yaml:",inline"`
	Compressor     string `long:"compressor" description:"Compression algorithm to use when rotating logs." choice:"gzip" choice:"zstd"`
	MaxLogFiles    int    `long:"max-files" description:"Maximum logfiles to keep (0 for no rotation)"`
	MaxLogFileSize int    `long:"max-file-size" description:"Maximum logfile size in MB"`
}
