package onionmessage

import (
	"context"
	"encoding/hex"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Request is a message sent to an Onion Peer Actor to request sending an
// onion message to a specific peer. Each actor is responsible for a single
// peer connection, and this message is used to queue onion messages for
// delivery to that peer.
type Request struct {
	// Embed BaseMessage to satisfy the actor package Message interface.
	actor.BaseMessage

	// msg is the onion message to send. This field is unexported as it's an
	// implementation detail of the actor system and should not be accessed
	// directly by external code.
	msg lnwire.OnionMessage
}

// MessageType returns a string identifier for the Request message type.
func (m *Request) MessageType() string {
	return "OnionMessageRequest"
}

// Response is the response message sent back from an Onion Peer Actor.
type Response struct {
	actor.BaseMessage
	Success bool
}

// MessageType returns a string identifier for the Response message type.
func (m *Response) MessageType() string {
	return "OnionMessageResponse"
}

// OnionPeerActorRef is a reference to an Onion Peer Actor.
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

// OnionPeerActor handles onion message delivery to a specific peer. It
// implements the actor.ActorBehavior interface and can be tested directly
// without the actor system.
type OnionPeerActor struct {
	sender func(msg *lnwire.OnionMessage)
}

// NewOnionPeerActor creates a new OnionPeerActor with the given sender
// function.
func NewOnionPeerActor(sender func(msg *lnwire.OnionMessage)) *OnionPeerActor {
	return &OnionPeerActor{sender: sender}
}

// Receive processes an incoming onion message request and sends it to the peer.
// This method implements the actor.ActorBehavior interface.
func (a *OnionPeerActor) Receive(ctx context.Context,
	req *Request) fn.Result[*Response] {

	select {
	case <-ctx.Done():
		log.DebugS(ctx, "OnionPeerActor context canceled, not sending")

		return fn.Err[*Response](ErrActorShuttingDown)
	default:
	}

	log.TraceS(ctx, "OnionPeerActor sending onion message")

	a.sender(&req.msg)

	return fn.Ok(&Response{Success: true})
}

// SpawnOnionPeerActor spawns a new Onion Peer Actor responsible for sending
// onion messages to a single peer identified by pubKey.
//
// Actor Lifecycle:
// - One actor per connected peer
// - Registered with receptionist using peer's public key as service key
// - Automatically cleaned up when peer disconnects
//
// The sender function is used to send the actual onion messages to the peer.
// This function should be provided by the brontide peer connection logic.
//
// Returns a reference to the spawned actor that can be used to send messages
// via Tell() or Ask() operations.
func SpawnOnionPeerActor(system *actor.ActorSystem,
	sender func(msg *lnwire.OnionMessage),
	pubKey [33]byte) (OnionPeerActorRef, error) {

	peerActor := NewOnionPeerActor(sender)

	serviceKey, pubKeyHex := NewOnionMessageServiceKey(pubKey)
	actorRef, err := serviceKey.Spawn(
		system, "onion-peer-actor-"+pubKeyHex, peerActor,
	)
	if err != nil {
		return nil, err
	}

	log.Debugf("Spawned onion peer actor for peer %s", pubKeyHex)

	return actorRef, nil
}

func findPeerActor(receptionist *actor.Receptionist, pubKey [33]byte,
) fn.Option[actor.ActorRef[*Request, *Response]] {

	serviceKey, _ := NewOnionMessageServiceKey(pubKey)
	refs := actor.FindInReceptionist(receptionist, serviceKey)

	return fn.Head(refs)
}

// StopPeerActor looks up the onion peer actor for the given public key and
// stops it. This should be called when a peer disconnects to clean up the
// actor. If no actor exists for the given public key, this is a no-op.
func StopPeerActor(system *actor.ActorSystem, pubKey [33]byte) {
	serviceKey, pubKeyHex := NewOnionMessageServiceKey(pubKey)

	actorOpt := findPeerActor(system.Receptionist(), pubKey)
	actorOpt.WhenSome(func(ref actor.ActorRef[*Request, *Response]) {
		log.Debugf("Stopping onion peer actor for peer %s", pubKeyHex)

		serviceKey.Unregister(system, ref)
	})
}
