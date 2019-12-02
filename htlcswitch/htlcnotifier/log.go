package htlcnotifier

import (
	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
)

// Subsystem defines the logging code for this subsystem.
const Subsystem = "HTNF"

// log is a logger that is initialized with no output filters.  This means the
// package will not perform any logging by default until the caller requests
// it.
var log btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger(Subsystem, nil))
}

// DisableLog disables all library log output.  Logging output is disabled by
// by default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.  This
// should be used in preference to SetLogWriter if the caller is also using
// btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
}

// logClosure is used to provide a closure over expensive logging operations so
// don't have to be performed when the logging level doesn't warrant it.
type logClosure func() string // nolint:unused

// String invokes the underlying function and returns the result.
func (c logClosure) String() string {
	return c()
}
