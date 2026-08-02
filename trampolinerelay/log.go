package trampolinerelay

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
)

// Subsystem is the logging subsystem used by the trampoline relay.
const Subsystem = "TRLY"

var log btclog.Logger

func init() {
	UseLogger(build.NewSubLogger(Subsystem, nil))
}

// DisableLog disables trampoline relay logging.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger sets the trampoline relay logger.
func UseLogger(logger btclog.Logger) {
	log = logger
}
