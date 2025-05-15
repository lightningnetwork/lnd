package actor_test

import (
	"context"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// CounterMsg is a message type for the stateful counter actor.
// It can be used to increment the counter or get its current value.
type CounterMsg struct {
	actor.BaseMessage
	Increment int
	GetValue  bool
	Who       string
}

// MessageType implements actor.Message.
func (m CounterMsg) MessageType() string { return "CounterMsg" }

// CounterResponse is a response type for the counter actor.
type CounterResponse struct {
	Value     int
	Responder string
}

// StatefulCounterActor demonstrates an actor that maintains internal state (a
// counter) and processes messages to modify or query that state.
type StatefulCounterActor struct {
	counter int
	actorID string
}

// NewStatefulCounterActor creates a new counter actor.
func NewStatefulCounterActor(id string) *StatefulCounterActor {
	return &StatefulCounterActor{
		actorID: id,
	}
}

// Receive is the message handler for the StatefulCounterActor.
// It implements the actor.ActorBehavior interface implicitly when wrapped.
func (s *StatefulCounterActor) Receive(ctx context.Context,
	msg CounterMsg) fn.Result[CounterResponse] {

	if msg.Increment > 0 {
		// For increment, we can just acknowledge or return the new
		// value. Messages are sent serially, so we don't need to worry
		// about a mutex here.
		s.counter += msg.Increment

		return fn.Ok(CounterResponse{
			Value:     s.counter,
			Responder: s.actorID,
		})
	}

	if msg.GetValue {
		return fn.Ok(CounterResponse{
			Value:     s.counter,
			Responder: s.actorID,
		})
	}

	return fn.Err[CounterResponse](fmt.Errorf("invalid CounterMsg"))
}

// ExampleActor_stateful demonstrates creating an actor whose behavior is defined
// by a struct with methods, allowing it to maintain internal state.
func ExampleActor_stateful() {
	system := actor.NewActorSystem()
	defer system.Shutdown()

	counterServiceKey := actor.NewServiceKey[CounterMsg, CounterResponse](
		"struct-counter-service",
	)

	// Create an instance of our stateful actor logic.
	actorID := "counter-actor-1"
	counterLogic := NewStatefulCounterActor(actorID)

	// Spawn the actor.
	// The counterLogic instance itself satisfies the ActorBehavior
	// interface because its Receive method matches the required signature.
	counterRef := counterServiceKey.Spawn(system, actorID, counterLogic)
	fmt.Printf("Actor %s spawned.\n", counterRef.ID())

	// Send messages to increment the counter.
	for i := 1; i <= 3; i++ {
		askCtx, askCancel := context.WithTimeout(
			context.Background(), 1*time.Second,
		)
		futureResp := counterRef.Ask(askCtx,
			CounterMsg{
				Increment: i,
				Who:       fmt.Sprintf("Incrementer-%d", i),
			},
		)
		awaitCtx, awaitCancel := context.WithTimeout(
			context.Background(), 1*time.Second,
		)
		resp := futureResp.Await(awaitCtx)

		resp.WhenOk(func(r CounterResponse) {
			fmt.Printf("Incremented by %d, new value: %d "+
				"(from %s)\n", i, r.Value, r.Responder)
		})
		resp.WhenErr(func(e error) {
			fmt.Printf("Error incrementing: %v\n", e)
		})
		awaitCancel()
		askCancel()
	}

	// Send a message to get the current value.
	askCtx, askCancel := context.WithTimeout(
		context.Background(), 1*time.Second,
	)
	futureResp := counterRef.Ask(
		askCtx, CounterMsg{GetValue: true, Who: "Getter"},
	)

	awaitCtx, awaitCancel := context.WithTimeout(
		context.Background(), 1*time.Second,
	)

	finalValueResp := futureResp.Await(awaitCtx)
	finalValueResp.WhenOk(func(r CounterResponse) {
		fmt.Printf("Final counter value: %d (from %s)\n",
			r.Value, r.Responder)
	})
	finalValueResp.WhenErr(func(e error) {
		fmt.Printf("Error getting value: %v\n", e)
	})
	awaitCancel()
	askCancel()

	// Output:
	// Actor counter-actor-1 spawned.
	// Incremented by 1, new value: 1 (from counter-actor-1)
	// Incremented by 2, new value: 3 (from counter-actor-1)
	// Incremented by 3, new value: 6 (from counter-actor-1)
	// Final counter value: 6 (from counter-actor-1)
}
