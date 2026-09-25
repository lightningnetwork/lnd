package timeout

import "github.com/btcsuite/btclog/v2"

// Subsystem defines the logging code for this subsystem.
const Subsystem = "TMOT"

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests
// it.
var log = btclog.Disabled

// UseLogger uses a specified Logger to output package logging info.
func UseLogger(logger btclog.Logger) {
	log = logger
}

// logger returns the logger the actor was configured with via Config.Log,
// falling back to the package-level logger set by UseLogger. The fallback is
// resolved on every call rather than captured at construction, so a logger
// installed after the actor was created still takes effect.
func (a *Actor) logger() btclog.Logger {
	return a.log.UnwrapOr(log)
}
