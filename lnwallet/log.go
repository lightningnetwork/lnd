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
