package timeout

import (
	"github.com/lightningnetwork/lnd/actor"
)

// MapTimeoutExpired creates a transformed TellOnlyRef that converts timeout
// expiry messages into a target actor's message type. This helper simplifies
// integrating the timeout actor with other actors by automatically
// transforming ExpiredMsg into the appropriate message type.
//
// The returned ref forwards both Tell and TryTell to targetRef, so the timeout
// actor's non-blocking delivery (and its retry on ErrMailboxFull) sees the
// target's real mailbox state.
//
// Example usage:
//
//	// Transform timeout expiry into a session actor message.
//	callbackRef := timeout.MapTimeoutExpired(
//		sessionActorRef,
//		func(expired timeout.ExpiredMsg) session.Msg {
//			return &session.TimeoutExpired{
//				ID: expired.ID,
//			}
//		},
//	)
//
//	// Send schedule request to timeout actor.
//	timeoutActorRef.Tell(ctx, &timeout.ScheduleTimeoutRequest{
//		ID:       timeout.ID(sessionID),
//		Duration: sessionTimeout,
//		Callback: callbackRef,
//	})
func MapTimeoutExpired[Out actor.Message](targetRef actor.TellOnlyRef[Out],
	mapFn func(ExpiredMsg) Out) actor.TellOnlyRef[*ExpiredMsg] {

	// Use the MapInputRef utility from the actor library to handle the
	// transformation from *ExpiredMsg to the caller's output type.
	return actor.NewMapInputRef(
		targetRef,
		func(expired *ExpiredMsg) Out {
			return mapFn(*expired)
		},
	)
}

// MapTickFired creates a transformed TellOnlyRef that converts recurring-tick
// fire messages into a target actor's message type. It mirrors
// MapTimeoutExpired for the recurring-tick scheduler:
//
//	tickRef := timeout.MapTickFired(
//		sessionActorRef,
//		func(fired timeout.TickFiredMsg) session.Msg {
//			return &session.TickFired{ID: fired.ID}
//		},
//	)
//
//	timeoutActorRef.Tell(ctx, &timeout.ScheduleRecurringTickRequest{
//		ID:       tickID,
//		Interval: pollInterval,
//		Callback: tickRef,
//	})
func MapTickFired[Out actor.Message](targetRef actor.TellOnlyRef[Out],
	mapFn func(TickFiredMsg) Out) actor.TellOnlyRef[*TickFiredMsg] {

	return actor.NewMapInputRef(
		targetRef,
		func(fired *TickFiredMsg) Out {
			return mapFn(*fired)
		},
	)
}
