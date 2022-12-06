package peerconn

import (
	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
)

// connLog is a logger that is initialized with the btclog.Disabled logger.
var connLog btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("PCMN", nil))
}

// DisableLog disables all logging output.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.
func UseLogger(logger btclog.Logger) {
	connLog = logger
}
