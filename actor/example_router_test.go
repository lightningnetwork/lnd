package actor_test

import (
	"context"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// RouterGreetingMsg is a message type for the router example.
type RouterGreetingMsg struct {
	actor.BaseMessage
	Name string
}

// MessageType implements actor.Message.
func (m RouterGreetingMsg) MessageType() string { return "RouterGreetingMsg" }

// RouterGreetingResponse is a response type for the router example.
type RouterGreetingResponse struct {
	Greeting  string
	HandlerID string
}

// ExampleRouter demonstrates creating multiple actors under the same service
// key and using a router to dispatch messages to them.
func ExampleRouter() {
	system := actor.NewActorSystem()
	defer system.Shutdown()

	//nolint:ll
	routerGreeterKey := actor.NewServiceKey[RouterGreetingMsg, RouterGreetingResponse](
		"router-greeter-service",
	)

	// Behavior for the first greeter actor.
	actorID1 := "router-greeter-1"
	greeterBehavior1 := actor.NewFunctionBehavior(
		func(ctx context.Context,
			msg RouterGreetingMsg) fn.Result[RouterGreetingResponse] {

			return fn.Ok(RouterGreetingResponse{
				Greeting:  "Greetings, " + msg.Name + "!",
				HandlerID: actorID1,
			})
		},
	)
	routerGreeterKey.Spawn(system, actorID1, greeterBehavior1)
	fmt.Printf("Actor %s spawned.\n", actorID1)

	// Behavior for the second greeter actor.
	actorID2 := "router-greeter-2"
	greeterBehavior2 := actor.NewFunctionBehavior(
		func(ctx context.Context,
			msg RouterGreetingMsg) fn.Result[RouterGreetingResponse] {

			return fn.Ok(RouterGreetingResponse{
				Greeting:  "Salutations, " + msg.Name + "!",
				HandlerID: actorID2,
			})
		},
	)
	routerGreeterKey.Spawn(system, actorID2, greeterBehavior2)
	fmt.Printf("Actor %s spawned.\n", actorID2)

	// Create a router for the "router-greeter-service".
	greeterRouter := actor.NewRouter(
		system.Receptionist(), routerGreeterKey,
		actor.NewRoundRobinStrategy[RouterGreetingMsg,
			RouterGreetingResponse](),
		system.DeadLetters(),
	)
	fmt.Printf("Router %s created for service key '%s'.\n",
		greeterRouter.ID(), "router-greeter-service")

	// Send messages through the router.
	names := []string{"Alice", "Bob", "Charlie", "David"}
	for _, name := range names {
		askCtx, askCancel := context.WithTimeout(
			context.Background(), 1*time.Second,
		)
		futureResponse := greeterRouter.Ask(
			askCtx, RouterGreetingMsg{Name: name},
		)

		awaitCtx, awaitCancel := context.WithTimeout(
			context.Background(), 1*time.Second,
		)
		result := futureResponse.Await(awaitCtx)

		result.WhenErr(func(err error) {
			fmt.Printf("For %s: Error - %v\n", name, err)
		})
		result.WhenOk(func(response RouterGreetingResponse) {
			fmt.Printf("For %s: Received '%s' from %s\n",
				name, response.Greeting, response.HandlerID)
		})
		awaitCancel()
		askCancel()
	}

	// Output:
	// Actor router-greeter-1 spawned.
	// Actor router-greeter-2 spawned.
	// Router router(router-greeter-service) created for service key 'router-greeter-service'.
	// For Alice: Received 'Greetings, Alice!' from router-greeter-1
	// For Bob: Received 'Salutations, Bob!' from router-greeter-2
	// For Charlie: Received 'Greetings, Charlie!' from router-greeter-1
	// For David: Received 'Salutations, David!' from router-greeter-2
}
