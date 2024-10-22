package build

import (
	"context"

	"github.com/btcsuite/btclog/v2"
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

// CriticalS writes a structured log with the given message and key-value pair
// attributes with LevelCritical to the log. It will then call the shutdown
// logger's shutdown function to prompt safe shutdown.
//
// Note: it is part of the btclog.Logger interface.
func (s *ShutdownLogger) CriticalS(ctx context.Context, msg string, err error,
	attr ...interface{}) {

	s.Logger.CriticalS(ctx, msg, err, attr...)
	s.Logger.Info("Sending request for shutdown")
	s.shutdown()
}
