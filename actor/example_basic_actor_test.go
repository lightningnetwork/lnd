package actor_test

import (
	"context"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// BasicGreetingMsg is a simple message type for the basic actor example.
type BasicGreetingMsg struct {
	actor.BaseMessage
	Name string
}

// MessageType implements actor.Message.
func (m BasicGreetingMsg) MessageType() string { return "BasicGreetingMsg" }

// BasicGreetingResponse is a simple response type.
type BasicGreetingResponse struct {
	Greeting string
}

// ExampleActor demonstrates creating a single actor, sending it a message
// directly using Ask, and then unregistering and stopping it.
func ExampleActor() {
	system := actor.NewActorSystem()
	defer system.Shutdown()

	//nolint:ll
	greeterKey := actor.NewServiceKey[BasicGreetingMsg, BasicGreetingResponse](
		"basic-greeter",
	)

	actorID := "my-greeter"
	greeterBehavior := actor.NewFunctionBehavior(
		func(ctx context.Context,
			msg BasicGreetingMsg) fn.Result[BasicGreetingResponse] {

			return fn.Ok(BasicGreetingResponse{
				Greeting: "Hello, " + msg.Name + " from " +
					actorID,
			})
		},
	)

	// Spawn the actor. This registers it with the system and receptionist,
	// and starts it. It returns an ActorRef.
	greeterRef := greeterKey.Spawn(system, actorID, greeterBehavior)
	fmt.Printf("Actor %s spawned.\n", greeterRef.ID())

	// Send a message directly to the actor's reference.
	askCtx, askCancel := context.WithTimeout(
		context.Background(), 1*time.Second,
	)
	defer askCancel()
	futureResponse := greeterRef.Ask(
		askCtx, BasicGreetingMsg{Name: "World"},
	)

	awaitCtx, awaitCancel := context.WithTimeout(
		context.Background(), 1*time.Second,
	)
	defer awaitCancel()
	result := futureResponse.Await(awaitCtx)

	result.WhenErr(func(err error) {
		fmt.Printf("Error awaiting response: %v\n", err)
	})
	result.WhenOk(func(response BasicGreetingResponse) {
		fmt.Printf("Received: %s\n", response.Greeting)
	})

	// Unregister the actor. This also stops the actor.
	unregistered := greeterKey.Unregister(system, greeterRef)
	if unregistered {
		fmt.Printf("Actor %s unregistered and stopped.\n",
			greeterRef.ID())
	} else {
		fmt.Printf("Failed to unregister actor %s.\n", greeterRef.ID())
	}

	// Verify it's no longer in the receptionist.
	refsAfterUnregister := actor.FindInReceptionist(
		system.Receptionist(), greeterKey,
	)
	fmt.Printf("Actors for key '%s' after unregister: %d\n",
		"basic-greeter", len(refsAfterUnregister))

	// Output:
	// Actor my-greeter spawned.
	// Received: Hello, World from my-greeter
	// Actor my-greeter unregistered and stopped.
	// Actors for key 'basic-greeter' after unregister: 0
}
