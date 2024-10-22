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

// multiAddrSource is an implementation of AddrSource which gathers all the
// known addresses for a given node from multiple backends and de-duplicates the
// results.
type multiAddrSource struct {
	sources []AddrSource
}

// NewMultiAddrSource constructs a new AddrSource which will query all the
// provided sources for a node's addresses and will then de-duplicate the
// results.
func NewMultiAddrSource(sources ...AddrSource) AddrSource {
	return &multiAddrSource{
		sources: sources,
	}
}

// AddrsForNode returns all known addresses for the target node public key.
//
// NOTE: this implements the AddrSource interface.
func (c *multiAddrSource) AddrsForNode(nodePub *btcec.PublicKey) ([]net.Addr,
	error) {

	// The multiple address sources will likely contain duplicate addresses,
	// so we use a map here to de-dup them.
	dedupedAddrs := make(map[string]net.Addr)

	// Iterate over all the address sources and query each one for the
	// addresses it has for the node in question.
	for _, src := range c.sources {
		addrs, err := src.AddrsForNode(nodePub)
		if err != nil {
			return nil, err
		}

		for _, addr := range addrs {
			dedupedAddrs[addr.String()] = addr
		}
	}

	// Convert the map into a list we can return.
	addrs := make([]net.Addr, 0, len(dedupedAddrs))
	for _, addr := range dedupedAddrs {
		addrs = append(addrs, addr)
	}

	return addrs, nil
}

// A compile-time check to ensure that multiAddrSource implements the AddrSource
// interface.
var _ AddrSource = (*multiAddrSource)(nil)
