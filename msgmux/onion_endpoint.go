package msgmux

import (
	"context"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/subscribe"
)

// OnionEndpoint handles incoming onion messages.
type OnionEndpoint struct {
	// subscribe.Server is used for subscriptions to onion messages.
	onionMessageServer *subscribe.Server
}

// OnionMessageUpdate is onion message update dispatched to any potential
// subscriber.
type OnionMessageUpdate struct {
	// Peer is the peer pubkey
	Peer [33]byte

	// BlindingPoint is the route blinding ephemeral pubkey to be used for
	// the onion message.
	BlindingPoint []byte

	// OnionBlob is the raw serialized mix header used to relay messages in
	// a privacy-preserving manner. This blob should be handled in the same
	// manner as onions used to route HTLCs, with the exception that it uses
	// blinded routes by default.
	OnionBlob []byte
}

// NewOnionEndpoint creates a new OnionEndpoint.
func NewOnionEndpoint(messageServer *subscribe.Server) *OnionEndpoint {
	return &OnionEndpoint{
		onionMessageServer: messageServer,
	}
}

// Name returns the unique name of the endpoint.
func (o *OnionEndpoint) Name() EndpointName {
	return "OnionMessageHandler"
}

// CanHandle checks if the endpoint can handle the incoming message.
// It returns true if the message is an lnwire.OnionMessage.
func (o *OnionEndpoint) CanHandle(msg PeerMsg) bool {
	_, ok := msg.Message.(*lnwire.OnionMessage)
	return ok
}

// SendMessage processes the incoming onion message.
// It returns true if the message was successfully processed.
func (o *OnionEndpoint) SendMessage(ctx context.Context, msg PeerMsg) bool {
	onionMsg, ok := msg.Message.(*lnwire.OnionMessage)
	if !ok {
		// This should ideally not happen if CanHandle is implemented
		// correctly. log.Warnf("OnionEndpoint received
		// non-OnionMessage: %T", msg.Message)
		return false
	}

	peer := msg.PeerPub.SerializeCompressed()
	// TODO: Implement actual onion message processing logic here. For
	// example, you might decrypt the onion packet, forward it, or handle
	// the payload.
	log.Debugf("OnionEndpoint received OnionMessage from peer %s: "+
		"BlindingPoint=%v, OnionPacket[:10]=%x...", peer,
		onionMsg.BlindingPoint, onionMsg.OnionBlob)

	var peerArr [33]byte
	copy(peerArr[:], peer)
	err := o.onionMessageServer.SendUpdate(&OnionMessageUpdate{
		Peer:          peerArr,
		BlindingPoint: onionMsg.BlindingPoint.SerializeCompressed(),
		OnionBlob:     onionMsg.OnionBlob,
	})
	if err != nil {
		log.Errorf("Failed to send onion message update: %v", err)
		return false
	}

	// For now, we'll just acknowledge that we "handled" it.
	// Replace with actual processing success/failure.
	_ = onionMsg // Silencing unused variable for now

	return true
}

// A compile-time check to ensure OnionEndpoint implements the Endpoint
// interface.
var _ Endpoint = (*OnionEndpoint)(nil)
