package build

import (
	"github.com/btcsuite/btclog"
)

// ShutdownLogger wraps an existing logger with a shutdown function which will
// be called on Critical/Criticalf to prompt shutdown.
type ShutdownLogger struct {
	btclog.Logger
	shutdown func()
}

// NewShutdownLogger creates a shutdown logger for the log provided which will
// use the signal package to request shutdown on critical errors.
func NewShutdownLogger(logger btclog.Logger, shutdown func()) *ShutdownLogger {
	return &ShutdownLogger{
		Logger:   logger,
		shutdown: shutdown,
	}
}

// Criticalf formats message according to format specifier and writes to
// log with LevelCritical. It will then call the shutdown logger's shutdown
// function to prompt safe shutdown.
//
// Note: it is part of the btclog.Logger interface.
func (s *ShutdownLogger) Criticalf(format string, params ...interface{}) {
	s.Logger.Criticalf(format, params...)
	s.Logger.Info("Sending request for shutdown")
	s.shutdown()
}

// Critical formats message using the default formats for its operands
// and writes to log with LevelCritical. It will then call the shutdown
// logger's shutdown function to prompt safe shutdown.
//
// Note: it is part of the btclog.Logger interface.
func (s *ShutdownLogger) Critical(v ...interface{}) {
	s.Logger.Critical(v)
	s.Logger.Info("Sending request for shutdown")
	s.shutdown()
}
