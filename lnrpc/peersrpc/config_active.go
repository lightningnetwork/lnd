//go:build peersrpc
// +build peersrpc

package peersrpc

import (
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
)

type Config struct {
	// GetNodeAnnouncement is used to send our retrieve the current
	// node announcement information.
	GetNodeAnnouncement func() (lnwire.NodeAnnouncement, error)

	// UpdateNodeAnnouncement updates our node announcement applying the
	// given NodeAnnModifiers and broadcasts the new version to the network.
	UpdateNodeAnnouncement func(...netann.NodeAnnModifier) error
}
