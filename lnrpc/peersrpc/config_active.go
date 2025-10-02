//go:build peersrpc
// +build peersrpc

package peersrpc

import (
	"context"
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
	GetNodeAnnouncement func() lnwire.NodeAnnouncement1

	// ParseAddr parses an address from its string format to a net.Addr.
	ParseAddr func(addr string) (net.Addr, error)

	// UpdateNodeAnnouncement updates and broadcasts our node announcement,
	// setting the feature vector provided and applying the
	// NodeAnnModifiers. If no feature updates are required, a nil feature
	// vector should be provided.
	UpdateNodeAnnouncement func(ctx context.Context,
		features *lnwire.RawFeatureVector,
		mods ...netann.NodeAnnModifier) error
}
