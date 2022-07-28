//go:build neutrinorpc
// +build neutrinorpc

package neutrinorpc

import (
	"github.com/lightninglabs/neutrino"
	"sync"
)

// Config is the primary configuration struct for the neutrino RPC server. It
// contains all the items required for the rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	// ChainService is required to handle neutrino chain service requests.
	NeutrinoCS *neutrino.ChainService

	/*
		FIX: Given this is just a config, I don't think the fields below are relevant, as they can live closer to the actual sub-server implementation itself.
	*/

	// curFilteredBlockHeight lets the rescan goroutine track where it
	// thinks the chain is based on OnFilteredBlockConnected and
	// OnFilteredBlockDisconnected.
	CurFilteredBlockHeight int32

	rescanRunning bool

	// rescanMtx locks all the variables to which the rescan goroutine's
	// notifications write.
	rescanMtx sync.RWMutex

	// Rescan quit channel.
	RescanQuit chan struct{}
}
