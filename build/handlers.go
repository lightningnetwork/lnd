package build

import (
	"os"

	"github.com/btcsuite/btclog/v2"
)

// NewDefaultLogHandlers returns the standard console logger and rotating log
// writer handlers that we generally want to use. It also applies the various
// config options to the loggers.
func NewDefaultLogHandlers(cfg *LogConfig,
	rotator *RotatingLogWriter) []btclog.Handler {

	var handlers []btclog.Handler

	consoleLogHandler := btclog.NewDefaultHandler(
		os.Stdout, cfg.Console.HandlerOptions()...,
	)
	logFileHandler := btclog.NewDefaultHandler(
		rotator, cfg.File.HandlerOptions()...,
	)

	maybeAddLogger := func(cmdOptionDisable bool, handler btclog.Handler) {
		if !cmdOptionDisable {
			handlers = append(handlers, handler)
		}
	}
	switch LoggingType {
	case LogTypeStdOut:
		maybeAddLogger(cfg.Console.Disable, consoleLogHandler)
	case LogTypeDefault:
		maybeAddLogger(cfg.Console.Disable, consoleLogHandler)
		maybeAddLogger(cfg.File.Disable, logFileHandler)
	}

	return handlers
}
