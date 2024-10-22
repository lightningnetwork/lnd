package channeldb

import (
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
)

// AddrSource is an interface that allow us to get the addresses for a target
// node. It may combine the results of multiple address sources.
type AddrSource interface {
	// AddrsForNode returns all known addresses for the target node public
	// key.
	AddrsForNode(nodePub *btcec.PublicKey) ([]net.Addr, error)
}
