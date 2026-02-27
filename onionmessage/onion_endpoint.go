package onionmessage

import (
	"context"
	"encoding/hex"
	"log/slog"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/subscribe"
)

// OnionMessageUpdate is onion message update dispatched to any potential
// subscriber.
type OnionMessageUpdate struct {
	// Peer is the peer pubkey
	Peer [33]byte

	// PathKey is the route blinding ephemeral pubkey to be used for
	// the onion message.
	PathKey [33]byte

	// OnionBlob is the raw serialized mix header used to relay messages in
	// a privacy-preserving manner. This blob should be handled in the same
	// manner as onions used to route HTLCs, with the exception that it uses
	// blinded routes by default.
	OnionBlob []byte

	// CustomRecords contains any custom TLV records included in the
	// payload.
	CustomRecords record.CustomSet

	// ReplyPath contains the reply path information for the onion message.
	ReplyPath *sphinx.BlindedPath

	// EncryptedRecipientData contains the encrypted recipient data for the
	// onion message, created by the creator of the blinded route. This is
	// the receiver for the last leg of the route, and the sender for the
	// first leg up to the introduction point.
	EncryptedRecipientData []byte
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

	logCtx := btclog.WithCtx(ctx,
		slog.String("peer", hex.EncodeToString(peer)),
		lnutils.LogPubKey("path_key", onionMsg.PathKey),
	)

	log.DebugS(logCtx, "OnionEndpoint received OnionMessage",
		btclog.HexN("onion_blob", onionMsg.OnionBlob, 10),
		slog.Int("blob_length", len(onionMsg.OnionBlob)))

	var peerArr [33]byte
	copy(peerArr[:], peer)

	// Convert path key []byte to [33]byte.
	pathKey := onionMsg.PathKey.SerializeCompressed()
	var pathKeyArr [33]byte
	copy(pathKeyArr[:], pathKey)

	err := o.onionMessageServer.SendUpdate(&OnionMessageUpdate{
		Peer:      peerArr,
		PathKey:   pathKeyArr,
		OnionBlob: onionMsg.OnionBlob,
	})
	if err != nil {
		log.ErrorS(logCtx, "Failed to send onion message update", err)
		return false
	}

	return true
}
