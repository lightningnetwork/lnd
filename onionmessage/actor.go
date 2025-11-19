package onionmessage

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// OMRequest is a message sent to an Onion Peer Actor to send an onion message
// to the peer for which the actor is responsible.
type OMRequest struct {
	// Embed BaseMessage to satisfy the actor package Message interface.
	actor.BaseMessage
	msg lnwire.OnionMessage
}

// MessageType returns a string identifier for the OMRequest message type.
func (m *OMRequest) MessageType() string {
	return "OnionMessageRequest"
}

// OMResponse is the response message sent back from an Onion Peer Actor
type OMResponse struct {
	actor.BaseMessage
	Success bool
}

// MessageType returns a string identifier for the OMResponse message type.
func (m *OMResponse) MessageType() string {
	return "OnionMessageResponse"
}

// OnionPeerActorRef is a reference to an Onion Peer Actor.
type OnionPeerActorRef actor.ActorRef[*OMRequest, *OMResponse]

// SpawnOnionPeerActor spawns a new Onion Peer Actor responsible for sending
// onion messages to a single peer identified by pubKey. The sender function
// is used to send the actual onion messages. The function returns a reference
// to the spawned actor.
func SpawnOnionPeerActor(system *actor.ActorSystem,
	sender func(msg *lnwire.OnionMessage),
	pubKey [33]byte) OnionPeerActorRef {

	// The actor logic creates a function behavior that sends onion messages
	// using the provided sender function.
	actorLogic := func(ctx context.Context,
		req *OMRequest) fn.Result[*OMResponse] {

		select {
		case <-ctx.Done():
			return fn.Err[*OMResponse](
				errors.New("actor shutting down"),
			)
		default:
		}

		sender(&req.msg)
		response := &OMResponse{Success: true}
		return fn.Ok(response)
	}

	// Create a behavior from the function.
	behavior := actor.NewFunctionBehavior(actorLogic)

	pubKeyHex := hex.EncodeToString(pubKey[:])
	serviceKey := actor.NewServiceKey[*OMRequest, *OMResponse](pubKeyHex)
	actorRef := serviceKey.Spawn(
		system, "onion-peer-actor-"+pubKeyHex, behavior,
	)

	return actorRef
}
