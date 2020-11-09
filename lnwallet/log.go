package lnwallet

import (
	"github.com/btcsuite/btclog"
	"github.com/btcsuite/btcwallet/chain"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"

	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// walletLog is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var walletLog btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("LNWL", nil))
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
	walletLog = logger

	btcwallet.UseLogger(logger)
	wtxmgr.UseLogger(logger)
	chain.UseLogger(logger)
	chainfee.UseLogger(logger)
}

// logClosure is used to provide a closure over expensive logging operations
// so don't have to be performed when the logging level doesn't warrant it.
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
