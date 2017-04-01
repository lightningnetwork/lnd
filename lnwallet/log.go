package lnwallet

import (
	"errors"
	"io"

	"github.com/btcsuite/btclog"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcwallet/chain"
	btcwallet "github.com/roasbeef/btcwallet/wallet"
	"github.com/roasbeef/btcwallet/wtxmgr"
)

// walletLog is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var walletLog btclog.Logger

// The default amount of logging is none.
func init() {
	DisableLog()
}

// DisableLog disables all library log output.  Logging output is disabled
// by default until either UseLogger or SetLogWriter are called.
func DisableLog() {
	walletLog = btclog.Disabled
}

// UseLogger uses a specified Logger to output package logging info.
// This should be used in preference to SetLogWriter if the caller is also
// using btclog.
func UseLogger(logger btclog.Logger) {
	walletLog = logger

	btcwallet.UseLogger(logger)
	wtxmgr.UseLogger(logger)
	btcrpcclient.UseLogger(logger)
	chain.UseLogger(logger)
}

// SetLogWriter uses a specified io.Writer to output package logging info.
// This allows a caller to direct package logging output without needing a
// dependency on seelog.  If the caller is also using btclog, UseLogger should
// be used instead.
func SetLogWriter(w io.Writer, level string) error {
	if w == nil {
		return errors.New("nil writer")
	}

	lvl, ok := btclog.LogLevelFromString(level)
	if !ok {
		return errors.New("invalid log level")
	}

	l, err := btclog.NewLoggerFromWriter(w, lvl)
	if err != nil {
		return err
	}

	UseLogger(l)
	return nil
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
