package signal

import (
	"sync"

	"github.com/btcsuite/btclog/v2"
)

// log is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var log btclog.Logger

// logMtx protects the log variable from concurrent access. This mutex is
// necessary because the mainInterruptHandler goroutine reads the logger while
// UseLogger may be called from the main goroutine during startup to replace
// the logger with a properly configured one. Without this protection, a data
// race would occur as reported in issue #6137.
var logMtx sync.RWMutex

// The default amount of logging is none.
func init() {
	DisableLog()
}

// DisableLog disables all library log output.  Logging output is disabled
// by default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.
// This should be used in preference to SetLogWriter if the caller is also
// using btclog.
func UseLogger(logger btclog.Logger) {
	logMtx.Lock()
	defer logMtx.Unlock()
	log = logger
}

// getLogger returns the current logger in a thread-safe manner. This function
// must be used instead of directly accessing the log variable to prevent data
// races between the mainInterruptHandler goroutine and the main goroutine
// during logger initialization.
func getLogger() btclog.Logger {
	logMtx.RLock()
	defer logMtx.RUnlock()
	if log == nil {
		return btclog.Disabled
	}

	return log
}
