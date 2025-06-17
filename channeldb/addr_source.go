package channeldb

import (
	"context"
	"errors"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
)

// AddrSource is an interface that allow us to get the addresses for a target
// node. It may combine the results of multiple address sources.
type AddrSource interface {
	// AddrsForNode returns all known addresses for the target node public
	// key. The returned boolean must indicate if the given node is unknown
	// to the backing source.
	AddrsForNode(ctx context.Context,
		nodePub *btcec.PublicKey) (bool, []net.Addr, error)
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

// AddrsForNode returns all known addresses for the target node public key. It
// queries all the address sources provided and de-duplicates the results. The
// returned boolean is false only if none of the backing sources know of the
// node.
//
// NOTE: this implements the AddrSource interface.
func (c *multiAddrSource) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	if len(c.sources) == 0 {
		return false, nil, errors.New("no address sources")
	}

	// The multiple address sources will likely contain duplicate addresses,
	// so we use a map here to de-dup them.
	dedupedAddrs := make(map[string]net.Addr)

	// known will be set to true if any backing source is aware of the node.
	var known bool

	// Iterate over all the address sources and query each one for the
	// addresses it has for the node in question.
	for _, src := range c.sources {
		isKnown, addrs, err := src.AddrsForNode(ctx, nodePub)
		if err != nil {
			return false, nil, err
		}

		if isKnown {
			known = true
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

	return known, addrs, nil
}

// A compile-time check to ensure that multiAddrSource implements the AddrSource
// interface.
var _ AddrSource = (*multiAddrSource)(nil)
