package onionmessage

import (
	"context"
	"encoding/hex"
	"log/slog"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

// Request is a message sent to an OnionPeerActor when an onion message is
// received from the peer. The actor processes the message through the full
// onion message pipeline: decode, decrypt, route, and forward/deliver.
type Request struct {
	// Embed BaseMessage to satisfy the actor package Message interface.
	actor.BaseMessage

	// msg is the onion message to process. This field is unexported as
	// it's an implementation detail of the actor system and should not be
	// accessed directly by external code.
	msg lnwire.OnionMessage
}

// MessageType returns a string identifier for the Request message type.
func (m *Request) MessageType() string {
	return "OnionMessageRequest"
}

// Response is the response message sent back from an OnionPeerActor after
// processing an incoming onion message.
type Response struct {
	actor.BaseMessage
	Success bool
}

// MessageType returns a string identifier for the Response message type.
func (m *Response) MessageType() string {
	return "OnionMessageResponse"
}

// OnionPeerActorRef is a reference to an OnionPeerActor.
type OnionPeerActorRef actor.ActorRef[*Request, *Response]

// NewOnionMessageServiceKey creates a service key for registering and looking
// up onion peer actors. The service key uses the peer's compressed public key
// (hex-encoded) as the identifier. It returns both the service key and the
// hex-encoded public key string for use in actor naming and logging.
func NewOnionMessageServiceKey(
	pubKey [33]byte) (actor.ServiceKey[*Request, *Response], string) {

	pubKeyHex := hex.EncodeToString(pubKey[:])

	return actor.NewServiceKey[*Request, *Response](pubKeyHex), pubKeyHex
}

// OnionActorFactory is a function that spawns a new OnionPeerActor for a
// given peer within the actor system. The factory captures shared dependencies
// (router, resolver, sender, dispatcher) and only requires per-peer parameters
// at spawn time.
type OnionActorFactory func(system *actor.ActorSystem,
	peerPubKey [33]byte) (OnionPeerActorRef, error)

// OnionPeerActor handles the full onion message processing pipeline for a
// specific peer connection. It decodes incoming onion messages, determines
// the routing action (forward or deliver), executes the action, and dispatches
// updates to subscribers.
type OnionPeerActor struct {
	// peerPubKey is the compressed public key of the peer this actor
	// handles messages for.
	peerPubKey [33]byte

	// peerSender is used to forward onion messages to other peers.
	peerSender PeerMessageSender

	// router is the onion router used to process onion message packets.
	router OnionRouter

	// resolver resolves node public keys from short channel IDs.
	resolver NodeIDResolver

	// updateDispatcher dispatches onion message updates to subscribers.
	updateDispatcher OnionMessageUpdateDispatcher
}

// Receive processes an incoming onion message from the peer. It decodes the
// onion packet, determines whether to forward or deliver the message, executes
// the routing action, and dispatches an update to subscribers.
//
// This method implements the actor.ActorBehavior interface.
func (a *OnionPeerActor) Receive(ctx context.Context,
	req *Request) fn.Result[*Response] {

	select {
	case <-ctx.Done():
		log.DebugS(ctx, "OnionPeerActor context canceled, "+
			"not processing")

		return fn.Err[*Response](ErrActorShuttingDown)
	default:
	}

	logCtx := btclog.WithCtx(ctx,
		slog.String("peer",
			hex.EncodeToString(a.peerPubKey[:])),
		lnutils.LogPubKey("path_key", req.msg.PathKey),
	)

	log.DebugS(logCtx, "OnionPeerActor received OnionMessage",
		btclog.HexN("onion_blob", req.msg.OnionBlob, 10),
		slog.Int("blob_length", len(req.msg.OnionBlob)))

	routingActionResult := processOnionMessage(
		a.router, a.resolver, &req.msg,
	)

	routingAction, err := routingActionResult.Unpack()
	if err != nil {
		log.ErrorS(logCtx, "Failed to handle onion message", err)

		return fn.Err[*Response](err)
	}

	// Handle the routing action.
	payload := fn.ElimEither(routingAction,
		func(fwdAction forwardAction) *lnwire.OnionMessagePayload {
			log.DebugS(logCtx, "Forwarding onion message",
				lnutils.LogPubKey("next_node_id",
					fwdAction.nextNodeID),
			)

			nextMsg := lnwire.NewOnionMessage(
				fwdAction.nextPathKey,
				fwdAction.nextPacket,
			)

			var nextNodeIDBytes [33]byte
			copy(
				nextNodeIDBytes[:],
				fwdAction.nextNodeID.SerializeCompressed(),
			)

			sendErr := a.peerSender.SendToPeer(
				nextNodeIDBytes, nextMsg,
			)
			if sendErr != nil {
				log.ErrorS(logCtx, "Failed to forward "+
					"onion message", sendErr)
			}

			return fwdAction.payload
		},
		func(dlvrAction deliverAction) *lnwire.OnionMessagePayload {
			log.DebugS(logCtx, "Delivering onion message "+
				"to self")

			return dlvrAction.payload
		})

	// Convert path key to [33]byte.
	var pathKeyArr [33]byte
	copy(pathKeyArr[:], req.msg.PathKey.SerializeCompressed())

	// Create the onion message update to send to subscribers.
	update := &OnionMessageUpdate{
		Peer:      a.peerPubKey,
		PathKey:   pathKeyArr,
		OnionBlob: req.msg.OnionBlob,
	}

	// If we have a payload, add its contents to our update.
	if payload != nil {
		customRecords := make(record.CustomSet)
		for _, v := range payload.FinalHopTLVs {
			customRecords[uint64(v.TLVType)] = v.Value
		}
		update.CustomRecords = customRecords
		update.ReplyPath = payload.ReplyPath
		update.EncryptedRecipientData = payload.EncryptedData
	}

	// Send the update to any subscribers.
	if sendErr := a.updateDispatcher.SendUpdate(update); sendErr != nil {
		log.ErrorS(logCtx, "Failed to send onion message update",
			sendErr)

		return fn.Err[*Response](sendErr)
	}

	return fn.Ok(&Response{Success: true})
}

// NewOnionActorFactory creates a factory function that spawns OnionPeerActors
// with shared dependencies. The returned factory captures the router,
// resolver, peer sender, and update dispatcher, requiring only the actor
// system and peer public key at spawn time.
func NewOnionActorFactory(router OnionRouter, resolver NodeIDResolver,
	peerSender PeerMessageSender,
	dispatcher OnionMessageUpdateDispatcher) OnionActorFactory {

	return func(system *actor.ActorSystem,
		peerPubKey [33]byte) (OnionPeerActorRef, error) {

		peerActor := &OnionPeerActor{
			peerPubKey:       peerPubKey,
			peerSender:       peerSender,
			router:           router,
			resolver:         resolver,
			updateDispatcher: dispatcher,
		}

		serviceKey, pubKeyHex := NewOnionMessageServiceKey(
			peerPubKey,
		)
		actorRef, err := serviceKey.Spawn(
			system, "onion-peer-actor-"+pubKeyHex, peerActor,
		)
		if err != nil {
			return nil, err
		}

		log.Debugf("Spawned onion peer actor for peer %s",
			pubKeyHex)

		return actorRef, nil
	}
}

// StopOnionActor stops the onion peer actor for the given public key using the
// provided actor reference. This should be called when a peer disconnects to
// clean up the actor.
func StopOnionActor(system *actor.ActorSystem, pubKey [33]byte,
	ref OnionPeerActorRef) {

	serviceKey, pubKeyHex := NewOnionMessageServiceKey(pubKey)

	log.Debugf("Stopping onion peer actor for peer %s", pubKeyHex)

	serviceKey.Unregister(
		system, actor.ActorRef[*Request, *Response](ref),
	)
}
