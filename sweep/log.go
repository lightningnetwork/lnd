package sweep

import (
	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
)

var (
	// log is a logger that is initialized with no output filters. This
	// means the package will not perform any logging by default until the
	// caller requests it.
	log btclog.Logger

	// feeLog is used by the fee bumper.
	feeLog btclog.Logger
)

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("SWPR", nil))
	UseBumperLogger(build.NewSubLogger("FBMP", nil))
}

// DisableLog disables all library log output.  Logging output is disabled by
// default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.  This
// should be used in preference to SetLogWriter if the caller is also using
// btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
}

// UseBumperLogger uses a specified Logger to output package logging info.
// This should be used in preference to SetLogWriter if the caller is also
// using btclog.
func UseBumperLogger(logger btclog.Logger) {
	feeLog = logger
}

// logClosure is used to provide a closure over expensive logging operations so
// don't have to be performed when the logging level doesn't warrant it.
type logClosure func() string

// String invokes the underlying function and returns the result.
func (c logClosure) String() string {
	return c()
}

// newLogClosure returns a new closure over a function that returns a string
// which itself provides a Stringer interface so that it can be used with the
// logging system.
func newLogClosure(c func() string) logClosure {
	return logClosure(c)
}
