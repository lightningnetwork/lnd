//go:build neutrinorpc
// +build neutrinorpc

package neutrinorpc

import (
	"github.com/lightninglabs/neutrino"
)

// Config is the primary configuration struct for the neutrino RPC server. It
// contains all the items required for the rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	// ChainService is required to handle neutrino chain service requests.
	NeutrinoCS *neutrino.ChainService
}
