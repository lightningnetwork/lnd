//go:build peersrpc
// +build peersrpc

package peersrpc

import (
	"net"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
)

// Config is the primary configuration struct for the peers RPC subserver.
// It contains all the items required for the server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
type Config struct {
	// GetNodeAnnouncement is used to send our retrieve the current
	// node announcement information.
	GetNodeAnnouncement func() (lnwire.NodeAnnouncement, error)

	// ParseAddr parses an address from its string format to a net.Addr.
	ParseAddr func(addr string) (net.Addr, error)

	// UpdateNodeAnnouncement updates our node announcement applying the
	// given NodeAnnModifiers and broadcasts the new version to the network.
	UpdateNodeAnnouncement func(...netann.NodeAnnModifier) error
}
