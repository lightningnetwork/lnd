package onionmessage

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
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
	ReplyPath *lnwire.ReplyPath

	// EncryptedRecipientData contains the encrypted recipient data for the
	// onion message, created by the creator of the blinded route. This is
	// the receiver for the last leg of the route, and the sender for the
	// first leg uyp to the introduction point.
	EncryptedRecipientData []byte
}

// OnionEndpointOption defines a function that can be used to configure
// an OnionEndpoint. This allows for flexible configuration of the endpoint
// when creating a new instance.
type OnionEndpointOption func(*OnionEndpoint)

// WithMessageServer sets the subscribe.Server for the OnionEndpoint.
func WithMessageServer(server *subscribe.Server) OnionEndpointOption {
	return func(o *OnionEndpoint) {
		o.onionMessageServer = server
	}
}

// OnionEndpoint handles incoming onion messages.
type OnionEndpoint struct {
	// subscribe.Server is used for subscriptions to onion messages.
	onionMessageServer *subscribe.Server

	// router is the sphinx router used to process onion_message_packet
	router *sphinx.Router

	// resolver is used to resolve node public keys from short channel IDs.
	resolver NodeIDResolver

	// receptionist is the actor system receptionist used to look up peer
	// onion actors.
	receptionist *actor.Receptionist
}

// A compile-time check to ensure OnionEndpoint implements the Endpoint
// interface.
var _ msgmux.Endpoint = (*OnionEndpoint)(nil)

// NewOnionEndpoint creates a new OnionEndpoint with the given options.
func NewOnionEndpoint(receptionist *actor.Receptionist, router *sphinx.Router,
	resolver NodeIDResolver, opts ...OnionEndpointOption) (*OnionEndpoint,
	error) {

	if receptionist == nil {
		return nil, ErrNilReceptionist
	}

	if router == nil {
		return nil, ErrNilRouter
	}

	if resolver == nil {
		return nil, ErrNilResolver
	}

	o := &OnionEndpoint{
		receptionist: receptionist,
		router:       router,
		resolver:     resolver,
	}
	for _, opt := range opts {
		opt(o)
	}

	return o, nil
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

	routingActionResult := processOnionMessage(
		o.router, o.resolver, onionMsg,
	)

	routingAction, err := routingActionResult.Unpack()
	if err != nil {
		log.ErrorS(logCtx, "Failed to handle onion message", err)
		return false
	}

	var isProcessedSuccessful bool = false

	// Handle the routing action.
	payload := fn.ElimEither(routingAction,
		func(forwardAction forwardAction) *lnwire.OnionMessagePayload {
			log.DebugS(logCtx, "Forwarding onion message",
				lnutils.LogPubKey("next_node_id",
					forwardAction.nextNodeID),
			)

			err := o.forwardMessage(
				ctx, forwardAction.nextNodeID,
				forwardAction.nextPathKey,
				forwardAction.nextPacket,
			)

			if err != nil {
				log.ErrorS(logCtx, "Failed to forward onion "+
					"message", err)
				isProcessedSuccessful = false
			}

			isProcessedSuccessful = true

			return forwardAction.payload
		},
		func(deliverAction deliverAction) *lnwire.OnionMessagePayload {
			log.DebugS(logCtx, "Delivering onion message to self")
			isProcessedSuccessful = true
			return deliverAction.payload
		})

	// Convert peer []byte to [33]byte.
	var peerArr [33]byte
	copy(peerArr[:], peer)

	// Convert path key []byte to [33]byte.
	var pathKeyArr [33]byte
	copy(pathKeyArr[:], onionMsg.PathKey.SerializeCompressed())

	// Create the onion message update to send to subscribers.
	update := &OnionMessageUpdate{
		Peer:      peerArr,
		PathKey:   pathKeyArr,
		OnionBlob: onionMsg.OnionBlob,
	}

	// If we have a payload, add its contents to our update.
	if payload != nil {
		customRecords := make(record.CustomSet)
		for _, v := range payload.FinalHopPayloads {
			customRecords[uint64(v.TLVType)] = v.Value
		}
		update.CustomRecords = customRecords
		update.ReplyPath = payload.ReplyPath
		update.EncryptedRecipientData = payload.EncryptedData
	}

	// Send the update to any subscribers.
	if sendErr := o.onionMessageServer.SendUpdate(update); sendErr != nil {
		log.ErrorS(logCtx, "Failed to send onion message update",
			sendErr)

		return false
	}

	return isProcessedSuccessful
}

// forwardMessage forwards the onion message to the next node using the peer
// actor that was spawned for that node.
func (o *OnionEndpoint) forwardMessage(ctx context.Context,
	nextNodeID *btcec.PublicKey, nextBlindingPoint *btcec.PublicKey,
	nextPacket []byte) error {

	var nextNodeIDBytes [33]byte
	copy(nextNodeIDBytes[:], nextNodeID.SerializeCompressed())

	// Find the onion peer actor for the next node ID.
	actorRefOpt := findPeerActor(o.receptionist, nextNodeIDBytes)

	// If no actor found, return an error.
	if actorRefOpt.IsNone() {
		return fmt.Errorf("failed to forward onion message: no peer "+
			"actor found for node %v (peer may be disconnected)",
			nextNodeID)
	}

	// Send the onion message to the peer actor.
	actorRefOpt.WhenSome(func(actorRef actor.ActorRef[*OMRequest,
		*OMResponse]) {

		onionMsg := lnwire.NewOnionMessage(
			nextBlindingPoint, nextPacket,
		)
		req := &OMRequest{msg: *onionMsg}
		actorRef.Tell(ctx, req)
	})

	// Successfully forwarded the message.
	return nil
}
