package onion_message

import (
	"context"
	"encoding/hex"
	"log/slog"

	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/subscribe"
)

// OnionMessageUpdate is onion message update dispatched to any potential
// subscriber.
type OnionMessageUpdate struct {
	// Peer is the peer pubkey
	Peer [33]byte

	// BlindingPoint is the route blinding ephemeral pubkey to be used for
	// the onion message.
	BlindingPoint [33]byte

	// OnionBlob is the raw serialized mix header used to relay messages in
	// a privacy-preserving manner. This blob should be handled in the same
	// manner as onions used to route HTLCs, with the exception that it uses
	// blinded routes by default.
	OnionBlob []byte
}

// OnionEndpoint handles incoming onion messages.
type OnionEndpoint struct {
	// subscribe.Server is used for subscriptions to onion messages.
	onionMessageServer *subscribe.Server
}

// A compile-time check to ensure OnionEndpoint implements the Endpoint
// interface.
var _ msgmux.Endpoint = (*OnionEndpoint)(nil)

// NewOnionEndpoint creates a new OnionEndpoint.
func NewOnionEndpoint(messageServer *subscribe.Server) *OnionEndpoint {
	return &OnionEndpoint{
		onionMessageServer: messageServer,
	}
}

// Name returns the unique name of the endpoint.
func (o *OnionEndpoint) Name() string {
	return "OnionMessageHandler"
}

// CanHandle checks if the endpoint can handle the incoming message.
// It returns true if the message is an lnwire.OnionMessage.
func (o *OnionEndpoint) CanHandle(msg msgmux.PeerMsg) bool {
	_, ok := msg.Message.(*lnwire.OnionMessage)
	return ok
}

// SendMessage processes the incoming onion message.
// It returns true if the message was successfully processed.
func (o *OnionEndpoint) SendMessage(ctx context.Context,
	msg msgmux.PeerMsg) bool {

	onionMsg, ok := msg.Message.(*lnwire.OnionMessage)
	if !ok {
		return false
	}

	peer := msg.PeerPub.SerializeCompressed()
	log.DebugS(ctx, "OnionEndpoint received OnionMessage",
		slog.String("peer", hex.EncodeToString(peer)),
		lnutils.LogPubKey("path_key", onionMsg.PathKey),
		lnutils.LogBytesPreview("onion_blob", onionMsg.OnionBlob),
		slog.Int("blob length", len(onionMsg.OnionBlob)))

	var peerArr [33]byte
	copy(peerArr[:], peer)

	// Convert blinding point []byte to [33]byte.
	blinding := onionMsg.PathKey.SerializeCompressed()
	var blindingArr [33]byte
	copy(blindingArr[:], blinding)

	err := o.onionMessageServer.SendUpdate(&OnionMessageUpdate{
		Peer:          peerArr,
		BlindingPoint: blindingArr,
		OnionBlob:     onionMsg.OnionBlob,
	})
	if err != nil {
		log.ErrorS(ctx, "Failed to send onion message update", err)
		return false
	}

	return true
}
