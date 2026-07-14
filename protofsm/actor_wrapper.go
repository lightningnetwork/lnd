package protofsm

import (
	"fmt"

	"github.com/lightningnetwork/lnd/actor"
)

// ActorMessage wraps an Event, in order to create a new message that can be
// used with the actor package.
type ActorMessage[Event any] struct {
	actor.BaseMessage

	// Event is the event that is being sent to the actor.
	Event Event
}

// MessageType returns the type of the message.
//
// NOTE: This implements the actor.Message interface.
func (a ActorMessage[Event]) MessageType() string {
	return fmt.Sprintf("ActorMessage(%T)", a.Event)
}
