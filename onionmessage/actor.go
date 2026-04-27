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
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/record"
)

const (
	// DefaultOnionMailboxSize is the buffer capacity for per-peer onion
	// message actor mailboxes.
	DefaultOnionMailboxSize = 50

	// DefaultMinREDThreshold is the queue depth at which Random Early
	// Detection begins probabilistically dropping onion messages. Below
	// this threshold no drops occur; above DefaultOnionMailboxSize all
	// messages are dropped. Must be strictly less than
	// DefaultOnionMailboxSize.
	DefaultMinREDThreshold = 40

	// DefaultPeerOnionMsgKbps is the default sustained per-peer onion
	// message ingress rate, in decimal kilobits per second (1 Kbps =
	// 1000 bits/s). Sizing is expressed against a 32 KiB onion_message
	// packet (the BOLT 4 spec cap on the sphinx-level payload inside
	// onion_message), not the 65 KiB lnwire envelope cap — at ~32 KiB
	// per packet this is roughly two such messages per second per peer.
	// A value of zero disables the per-peer limiter entirely.
	DefaultPeerOnionMsgKbps = 512

	// DefaultPeerOnionMsgBurstBytes is the default per-peer token bucket
	// depth, in bytes. Sized to hold approximately eight 32 KiB onion
	// message packets (see DefaultPeerOnionMsgKbps for why we measure
	// against 32 KiB rather than the 65 KiB lnwire envelope cap) so a
	// peer can briefly burst above the sustained rate without drops.
	DefaultPeerOnionMsgBurstBytes = 8 * 32 * 1024

	// DefaultGlobalOnionMsgKbps is the default sustained aggregate onion
	// message ingress rate across all peers, in decimal kilobits per
	// second. Targets ~5 Mbps worst-case ingress so that onion message
	// bandwidth cannot dwarf a typical routing node's payment traffic.
	// A value of zero disables the global limiter entirely.
	DefaultGlobalOnionMsgKbps = 5120

	// DefaultGlobalOnionMsgBurstBytes is the default global token bucket
	// depth, in bytes. Sized to hold approximately fifty 32 KiB onion
	// message packets, measured against the BOLT 4 onion_message_packet
	// cap rather than the 65 KiB lnwire envelope cap (see
	// DefaultPeerOnionMsgKbps).
	DefaultGlobalOnionMsgBurstBytes = 50 * 32 * 1024
)

// Compile-time assertion: DefaultMinREDThreshold must be strictly less than
// DefaultOnionMailboxSize. If this overflows, the constants are misconfigured.
const _ = uint(DefaultOnionMailboxSize - DefaultMinREDThreshold - 1)

// Compile-time assertions: the default burst sizes must be able to hold at
// least one maximum-sized wire message, otherwise every AllowN call on a
// freshly constructed limiter would fail and the limiter would silently
// drop all onion traffic.
const _ = uint(DefaultPeerOnionMsgBurstBytes - lnwire.MaxMsgBody)
const _ = uint(DefaultGlobalOnionMsgBurstBytes - lnwire.MaxMsgBody)

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

// NewRequest creates a new Request from an onion message.
func NewRequest(msg lnwire.OnionMessage) *Request {
	return &Request{msg: msg}
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
// at spawn time. Callers may pass ActorOptions to customise the mailbox (size,
// drop predicate, etc.) on a per-peer basis.
type OnionActorFactory func(system *actor.ActorSystem,
	peerPubKey [33]byte,
	opts ...actor.ActorOption[*Request, *Response]) (OnionPeerActorRef,
	error)

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
		ctx, a.router, a.resolver, &req.msg,
	)

	routingAction, err := routingActionResult.Unpack()
	if err != nil {
		log.ErrorS(logCtx, "Failed to handle onion message", err)

		return fn.Err[*Response](err)
	}

	// Block same-peer cycles: do not forward a message back to
	// the peer that sent it.
	routingAction.WhenLeft(func(fwdAction forwardAction) {
		var nextNodeIDBytes [33]byte
		copy(
			nextNodeIDBytes[:],
			fwdAction.nextNodeID.SerializeCompressed(),
		)

		if nextNodeIDBytes == a.peerPubKey {
			log.WarnS(logCtx,
				"Dropping cyclic onion message",
				ErrSamePeerCycle,
				lnutils.LogPubKey(
					"next_node_id",
					fwdAction.nextNodeID,
				),
			)

			err = ErrSamePeerCycle
		}
	})
	if err != nil {
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
// system, peer public key, and optional per-peer ActorOptions at spawn time.
//
// Callers supply ActorOptions (mailbox factory, size overrides, etc.) via the
// opts variadic so that backpressure policy can be customised per peer.
func NewOnionActorFactory(router OnionRouter, resolver NodeIDResolver,
	peerSender PeerMessageSender,
	dispatcher OnionMessageUpdateDispatcher) OnionActorFactory {

	return func(system *actor.ActorSystem, peerPubKey [33]byte,
		opts ...actor.ActorOption[*Request, *Response],
	) (OnionPeerActorRef, error) {

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
			opts...,
		)
		if err != nil {
			return nil, err
		}

		log.Debugf("Spawned onion peer actor for peer %s",
			pubKeyHex)

		return actorRef, nil
	}
}

// DefaultOnionActorOpts returns ActorOptions that configure a
// BackpressureMailbox with a RED drop predicate and the default onion mailbox
// size. The RED thresholds are derived from the mailbox capacity so that all
// parameters are centralised and self-consistent.
func DefaultOnionActorOpts() []actor.ActorOption[*Request, *Response] {
	factory := func(ctx context.Context,
		capacity int) actor.Mailbox[*Request, *Response] {

		// Dynamically calculate the min threshold to be
		// the same proportion (40/50 = 80%) of the actual
		// capacity.
		minThreshold := (capacity * DefaultMinREDThreshold) /
			DefaultOnionMailboxSize

		// Ensure minThreshold is strictly less than
		// capacity for RED to work.
		if minThreshold >= capacity {
			minThreshold = capacity - 1
		}
		if minThreshold < 0 {
			minThreshold = 0
		}

		shouldDrop, err := queue.RandomEarlyDrop(
			minThreshold, capacity,
		)
		if err != nil {
			// This should never happen given the
			// threshold clamping above, but fall back to
			// dropping all messages rather than risking
			// a blocked readHandler.
			shouldDrop = func(int) bool {
				return true
			}
		}

		return actor.NewBackpressureMailbox[*Request, *Response](
			ctx, capacity, shouldDrop,
		)
	}

	return []actor.ActorOption[*Request, *Response]{
		actor.WithMailboxFactory(factory),
		actor.WithMailboxSize[*Request, *Response](
			DefaultOnionMailboxSize,
		),
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
