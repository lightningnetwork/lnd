package lnwallet

import (
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// AuxChannelNegotiator is an interface that allows aux channel implementations
// to inject or handle custom records in the init message that is used when
// establishing a connection with a peer. It may also notify the aux channel
// implementation for the channel ready or channel reestablish events, which
// mark the channel as ready to use.
type AuxChannelNegotiator interface {
	// GetInitRecords is called when sending an init message to a peer.
	// It returns custom records to include in the init message TLVs. The
	// implementation can decide which records to include based on the peer
	// identity.
	GetInitRecords(peer route.Vertex) (lnwire.CustomRecords, error)

	// ProcessInitRecords handles received init records from a peer. The
	// implementation can store state internally to affect future
	// channel operations with this peer.
	ProcessInitRecords(peer route.Vertex,
		customRecords lnwire.CustomRecords) error

	// ProcessChannelReady handles the event of marking a channel identified
	// by its channel ID as ready to use. We also provide the peer the
	// channel was established with.
	ProcessChannelReady(cid lnwire.ChannelID, peer route.Vertex)

	// ProcessReestablish handles the received channel_reestablish message
	// which marks a channel identified by its cid as ready to use again.
	ProcessReestablish(cid lnwire.ChannelID, peer route.Vertex)
}
