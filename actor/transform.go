package actor

import "context"

// MapInputRef wraps a TellOnlyRef and transforms incoming messages before
// forwarding them to the target ref. This allows adapting a ref that expects
// message type Out to accept message type In, eliminating the need for
// intermediate adapter actors.
//
// This is particularly useful for notification patterns where a source actor
// sends events of a specific type, but consumers want to receive events in
// their own domain-specific type.
//
// Example usage:
//
//	// roundActorRef accepts round.ConfirmationEvent
//	// chainsource sends chainsource.ConfirmationEvent
//	adaptedRef := actor.NewMapInputRef(
//	    roundActorRef,
//	    func(cs chainsource.ConfirmationEvent) round.ConfirmationEvent {
//	        return round.ConfirmationEvent{
//	            TxID:        cs.Txid,
//	            BlockHeight: cs.BlockHeight,
//	            // ... transform fields
//	        }
//	    },
//	)
//	// Now adaptedRef can be used as TellOnlyRef[chainsource.ConfirmationEvent]
type MapInputRef[In Message, Out Message] struct {
	targetRef TellOnlyRef[Out]
	mapFn     func(In) Out
}

// NewMapInputRef creates a new message-transforming wrapper around a
// TellOnlyRef. The mapFn function is called for each message to transform it
// from type In to type Out before forwarding to targetRef.
func NewMapInputRef[In Message, Out Message](
	targetRef TellOnlyRef[Out], mapFn func(In) Out) *MapInputRef[In, Out] {

	return &MapInputRef[In, Out]{
		targetRef: targetRef,
		mapFn:     mapFn,
	}
}

// Tell transforms the incoming message using the map function and forwards it
// to the target ref. If the context is cancelled before the message can be sent
// to the target actor's mailbox, the message may be dropped.
func (m *MapInputRef[In, Out]) Tell(ctx context.Context, msg In) {
	transformed := m.mapFn(msg)
	m.targetRef.Tell(ctx, transformed)
}

// ID returns a unique identifier for this actor. The ID includes the
// "map-input-" prefix to indicate this is a transformation wrapper.
func (m *MapInputRef[In, Out]) ID() string {
	return "map-input-" + m.targetRef.ID()
}
