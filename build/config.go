//go:build !dev
// +build !dev

package build

import "github.com/btcsuite/btclog/v2"

const (
	callSiteOff   = "off"
	callSiteShort = "short"
	callSiteLong  = "long"
)

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
		Console: &LoggerConfig{
			CallSite: callSiteOff,
		},
		File: &LoggerConfig{
			CallSite: callSiteOff,
		},
	}
}

// LoggerConfig holds options for a particular logger.
//
//nolint:lll
type LoggerConfig struct {
	Disable      bool   `long:"disable" description:"Disable this logger."`
	NoTimestamps bool   `long:"no-timestamps" description:"Omit timestamps from log lines."`
	CallSite     string `long:"call-site" description:"Include the call-site of each log line." choice:"off" choice:"short" choice:"long"`
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
