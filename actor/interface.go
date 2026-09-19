package actor

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ErrActorTerminated indicates that an operation failed because the target
// actor was terminated or in the process of shutting down.
var ErrActorTerminated = fmt.Errorf("actor terminated")

// ErrMessageDropped indicates that a message was dropped by the mailbox's
// backpressure mechanism (e.g., RED-style load shedding).
var ErrMessageDropped = errors.New("message dropped by backpressure")

// ErrEmptyActorID is returned when an actor is created with an empty ID.
var ErrEmptyActorID = fmt.Errorf("actor ID must not be empty")

// ErrNilBehavior is returned when an actor is created with a nil behavior.
var ErrNilBehavior = fmt.Errorf("actor behavior must not be nil")

// ErrDuplicateActorID is returned when attempting to register an actor with an
// ID that is already in use within the actor system.
var ErrDuplicateActorID = fmt.Errorf("actor ID already registered")

// BaseMessage is a helper struct that can be embedded in message types defined
// outside the actor package to satisfy the Message interface's unexported
// messageMarker method.
type BaseMessage struct{}

// messageMarker implements the unexported method for the Message interface,
// allowing types that embed BaseMessage to satisfy the Message interface.
func (BaseMessage) messageMarker() {}

// Message is a sealed interface for actor messages. Actors will receive
// messages conforming to this interface. The interface is "sealed" by the
// unexported messageMarker method, meaning only types that can satisfy it
// (e.g., by embedding BaseMessage or being in the same package) can be
// Messages.
type Message interface {
	// messageMarker is a private method that makes this a sealed interface
	// (see BaseMessage for embedding).
	messageMarker()

	// MessageType returns the type name of the message for
	// routing/filtering.
	MessageType() string
}

// TellOnlyRef is a reference to an actor that only supports "tell" operations.
// This is useful for scenarios where only fire-and-forget message passing is
// needed, or to restrict capabilities.
type TellOnlyRef[M Message] interface {
	// Tell sends a message without waiting for a response. If the
	// context is cancelled before the message can be sent to the actor's
	// mailbox, the message may be dropped.
	Tell(ctx context.Context, msg M)

	// ID returns the unique identifier for this actor.
	ID() string
}

// ActorRef is a reference to an actor that supports both "tell" and "ask"
// operations. It embeds TellOnlyRef and adds the Ask method for
// request-response interactions.
type ActorRef[M Message, R any] interface {
	TellOnlyRef[M]

	// Ask sends a message and returns a Future for the response.
	// The Future will be completed with the actor's reply or an error
	// if the operation fails (e.g., context cancellation before send).
	Ask(ctx context.Context, msg M) fn.Future[R]
}

// ActorBehavior defines the logic for how an actor processes incoming messages.
// It is a strategy interface that encapsulates the actor's reaction to
// messages.
type ActorBehavior[M Message, R any] interface {
	// Receive processes a message and returns a Result. The provided
	// context is the actor's internal context, which can be used to
	// detect actor shutdown requests.
	Receive(actorCtx context.Context, msg M) fn.Result[R]
}
