package lnwallet

import (
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

// AuxChannelNegotiator is an interface that allows aux channel implementations
// to inject and process feature bits during the init and channel_reestablish
// messages. This enables dynamic channel behavior modification based on
// negotiated features.
type AuxChannelNegotiator interface {
	// GetInitFeatures is called when sending an init message to a peer.
	// It returns custom feature bits to include in the init message TLVs.
	// The implementation can decide which features to advertise based on
	// the peer's identity.
	GetInitFeatures(peer route.Vertex) (tlv.Blob, error)

	// ProcessInitFeatures handles received init feature TLVs from a peer.
	// The implementation can store state internally to affect future
	// channel operations with this peer.
	ProcessInitFeatures(peer route.Vertex, features tlv.Blob) error

	// ProcessChannelReady handles the event of marking a channel identified
	// by its channel ID as ready to use. We also provide the peer the
	// channel was established with.
	ProcessChannelReady(cid lnwire.ChannelID, peer route.Vertex)

	// GetReestablishFeatures is called when sending a channel_reestablish
	// message. It returns feature bits based on the specific channel
	// identified by its funding outpoint and aux channel blob.
	GetReestablishFeatures(cid lnwire.ChannelID,
		auxChanBlob tlv.Blob) (tlv.Blob, error)

	// ProcessReestablishFeatures handles received channel_reestablish
	// feature TLVs. This is a blocking call - the channel link will wait
	// for this method to complete before continuing channel operations.
	// The implementation can modify aux channel behavior based on the
	// negotiated features.
	ProcessReestablishFeatures(cid lnwire.ChannelID,
		features tlv.Blob, auxChanBlob tlv.Blob) error
}
