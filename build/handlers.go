package build

import (
	"os"

	"github.com/btcsuite/btclog"
)

// NewDefaultLoggers returns the standard console logger and rotating log
// writer loggers that we generally want to use. It also applies the various
// config options to the loggers.
func NewDefaultLoggers(cfg *LogConfig, rotator *RotatingLogWriter) (
	btclog.Handler, btclog.Handler) {

	var (
		consoleOpts []btclog.HandlerOption
		fileOpts    []btclog.HandlerOption
	)
	if cfg.Console.NoTimestamps {
		consoleOpts = append(consoleOpts, btclog.WithNoTimestamp())
	}
	if cfg.Console.Style {
		consoleOpts = append(consoleOpts, btclog.WithStyledOutput())
	}
	if cfg.File.NoTimestamps {
		fileOpts = append(fileOpts, btclog.WithNoTimestamp())
	}

	consoleLogHandler := btclog.NewDefaultHandler(os.Stdout, consoleOpts...)
	logFileHandler := btclog.NewDefaultHandler(rotator, fileOpts...)

	return consoleLogHandler, logFileHandler
}
