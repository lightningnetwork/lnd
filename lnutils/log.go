package lnutils

import (
	"log/slog"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/davecgh/go-spew/spew"
)

// LogClosure is used to provide a closure over expensive logging operations so
// don't have to be performed when the logging level doesn't warrant it.
type LogClosure func() string

// String invokes the underlying function and returns the result.
func (c LogClosure) String() string {
	return c()
}

// NewLogClosure returns a new closure over a function that returns a string
// which itself provides a Stringer interface so that it can be used with the
// logging system.
func NewLogClosure(c func() string) LogClosure {
	return LogClosure(c)
}

// SpewLogClosure takes an interface and returns the string of it created from
// `spew.Sdump` in a LogClosure.
func SpewLogClosure(a any) LogClosure {
	return func() string {
		return spew.Sdump(a)
	}
}

// NewSeparatorClosure returns a new closure that logs a separator line.
func NewSeparatorClosure() LogClosure {
	return func() string {
		return strings.Repeat("=", 80)
	}
}

// LogPubKey returns a slog attribute for logging a public key in hex format.
func LogPubKey(key string, pubKey *btcec.PublicKey) slog.Attr {
	// Handle nil pubkey gracefully, although callers should ideally prevent
	// this.
	if pubKey == nil {
		return btclog.Fmt(key, "<nil>")
	}

	return btclog.Hex6(key, pubKey.SerializeCompressed())
}
