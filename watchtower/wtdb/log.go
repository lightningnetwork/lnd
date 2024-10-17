package wtdb

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/watchtower/wtdb/migration1"
	"github.com/lightningnetwork/lnd/watchtower/wtdb/migration2"
	"github.com/lightningnetwork/lnd/watchtower/wtdb/migration3"
	"github.com/lightningnetwork/lnd/watchtower/wtdb/migration4"
	"github.com/lightningnetwork/lnd/watchtower/wtdb/migration5"
	"github.com/lightningnetwork/lnd/watchtower/wtdb/migration6"
	"github.com/lightningnetwork/lnd/watchtower/wtdb/migration7"
	"github.com/lightningnetwork/lnd/watchtower/wtdb/migration8"
)

// log is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var log btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("WTDB", nil))
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
	log = logger
	migration1.UseLogger(logger)
	migration2.UseLogger(logger)
	migration3.UseLogger(logger)
	migration4.UseLogger(logger)
	migration5.UseLogger(logger)
	migration6.UseLogger(logger)
	migration7.UseLogger(logger)
	migration8.UseLogger(logger)
}
