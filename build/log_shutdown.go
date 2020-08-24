package build

import (
	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/signal"
)

// ShutdownLogger wraps an existing logger with a shutdown function which will
// be called on Critical/Criticalf to prompt shutdown.
type ShutdownLogger struct {
	btclog.Logger
}

// NewShutdownLogger creates a shutdown logger for the log provided which will
// use the signal package to request shutdown on critical errors.
func NewShutdownLogger(logger btclog.Logger) *ShutdownLogger {
	return &ShutdownLogger{
		Logger: logger,
	}
}

// Criticalf formats message according to format specifier and writes to
// log with LevelCritical. It will then call the shutdown logger's shutdown
// function to prompt safe shutdown.
//
// Note: it is part of the btclog.Logger interface.
func (s *ShutdownLogger) Criticalf(format string, params ...interface{}) {
	s.Logger.Criticalf(format, params...)
	s.shutdown()
}

// Critical formats message using the default formats for its operands
// and writes to log with LevelCritical. It will then call the shutdown
// logger's shutdown function to prompt safe shutdown.
//
// Note: it is part of the btclog.Logger interface.
func (s *ShutdownLogger) Critical(v ...interface{}) {
	s.Logger.Critical(v)
	s.shutdown()
}

// shutdown checks whether we are listening for interrupts, since a shutdown
// request to the signal package will block if it is not running, and requests
// shutdown if possible.
func (s *ShutdownLogger) shutdown() {
	if !signal.Listening() {
		s.Logger.Info("Request for shutdown ignored")
		return
	}

	s.Logger.Info("Sending request for shutdown")
	signal.RequestShutdown()
}
