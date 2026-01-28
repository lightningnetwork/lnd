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
	pubKey [33]byte) OnionPeerActorRef {

	// The actor logic creates a function behavior that sends onion messages
	// using the provided sender function.
	actorLogic := func(ctx context.Context,
		req *Request) fn.Result[*Response] {

		select {
		case <-ctx.Done():

			return fn.Err[*Response](
				ErrActorShuttingDown,
			)
		default:
		}

		sender(&req.msg)
		response := &Response{Success: true}

		return fn.Ok(response)
	}

	// Create a behavior from the function.
	behavior := actor.NewFunctionBehavior(actorLogic)

	pubKeyHex := hex.EncodeToString(pubKey[:])
	serviceKey := actor.NewServiceKey[*Request, *Response](pubKeyHex)
	actorRef := serviceKey.Spawn(
		system, "onion-peer-actor-"+pubKeyHex, behavior,
	)

	return actorRef
}

func findPeerActor(receptionist *actor.Receptionist, pubKey [33]byte,
) fn.Option[actor.ActorRef[*Request, *Response]] {

	pubKeyHex := hex.EncodeToString(pubKey[:])
	serviceKey := actor.NewServiceKey[*Request, *Response](pubKeyHex)
	refs := actor.FindInReceptionist(receptionist, serviceKey)

	return fn.Head(refs)
}
