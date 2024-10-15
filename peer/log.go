package peer

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
)

// peerLog is a logger that is initialized with the btclog.Disabled logger.
var peerLog btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("PEER", nil))
}

// DisableLog disables all logging output.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.
func UseLogger(logger btclog.Logger) {
	peerLog = logger
}
