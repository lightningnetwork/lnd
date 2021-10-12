//go:build autopilotrpc
// +build autopilotrpc

package autopilotrpc

import (
	"github.com/lightningnetwork/lnd/autopilot"
)

// Config is the primary configuration struct for the autopilot RPC server. It
// contains all the items required for the rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	// Manager is the running autopilot manager.
	Manager *autopilot.Manager
}
