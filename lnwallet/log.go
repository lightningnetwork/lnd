package lnwallet

import (
	"errors"
	"io"

	"github.com/btcsuite/btclog"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
)

// log is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var log btclog.Logger

// The default amount of logging is none.
func init() {
	DisableLog()
}

// DisableLog disables all library log output.  Logging output is disabled
// by default until either UseLogger or SetLogWriter are called.
func DisableLog() {
	log = btclog.Disabled
}

// UseLogger uses a specified Logger to output package logging info.
// This should be used in preference to SetLogWriter if the caller is also
// using btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
	btcwallet.UseLogger(logger)
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
