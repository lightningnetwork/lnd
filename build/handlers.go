package build

import (
	"os"

	"github.com/btcsuite/btclog/v2"
)

// NewDefaultLoggers returns the standard console logger and rotating log
// writer loggers that we generally want to use. It also applies the various
// config options to the loggers.
func NewDefaultLoggers(cfg *LogConfig, rotator *RotatingLogWriter) (
	btclog.Handler, btclog.Handler) {

	consoleLogHandler := btclog.NewDefaultHandler(
		os.Stdout, cfg.Console.HandlerOptions()...,
	)
	logFileHandler := btclog.NewDefaultHandler(
		rotator, cfg.File.HandlerOptions()...,
	)

	return consoleLogHandler, logFileHandler
}
