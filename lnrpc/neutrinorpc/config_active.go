// +build neutrinorpc

package neutrinorpc

import (
	sync "sync"

	"github.com/lightninglabs/neutrino"
)

// Config is the primary configuration struct for the neutrino RPC server. It
// contains all the items required for the rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	// curFilteredBlockHeight lets the rescan goroutine track where it
	// thinks the chain is based on OnFilteredBlockConnected and
	// OnFilteredBlockDisconnected.
	CurFilteredBlockHeight int32

	// ChainService is required to handle neutrino chain service requests.
	NeutrinoCS *neutrino.ChainService

	rescanRunning bool

	// rescanMtx locks all the variables to which the rescan goroutine's
	// notifications write.
	rescanMtx sync.RWMutex

	// Rescan quit channel.
	RescanQuit chan struct{}
}
