package actor_test

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// SingletonGreetingMsg is a message type for the singleton example.
type SingletonGreetingMsg struct {
	actor.BaseMessage
	Name string
}

// MessageType implements actor.Message.
func (m SingletonGreetingMsg) MessageType() string {
	return "SingletonGreetingMsg"
}

// ExampleServiceKey_SpawnSingleton demonstrates the singleton pattern: a
// single actor is registered under a service key, and consumers reach it
// through a SingletonRef that performs receptionist lookups on each call.
// This avoids the overhead of a Router+RoutingStrategy when there is never
// more than one actor per key.
func ExampleServiceKey_SpawnSingleton() {
	system := actor.NewActorSystem()
	defer func() { _ = system.Shutdown() }()

	greeterKey := actor.NewServiceKey[SingletonGreetingMsg, string](
		"singleton-greeter",
	)

	// The spawner registers exactly one actor for the key. Calling
	// SpawnSingleton again would replace any existing actor atomically
	// from the caller's perspective (stop-then-register).
	behavior := actor.NewFunctionBehavior(
		func(_ context.Context,
			msg SingletonGreetingMsg) fn.Result[string] {

			return fn.Ok("Hello, " + msg.Name + "!")
		},
	)
	if _, err := greeterKey.SpawnSingleton(
		system, "greeter-actor", behavior,
	); err != nil {
		fmt.Printf("spawn failed: %v\n", err)
		return
	}

	// Consumers get a SingletonRef. They don't need to know the actor
	// ID or hold a direct reference — the ref resolves the actor via
	// the receptionist on each Tell/Ask.
	ref := greeterKey.Singleton(system)

	for _, name := range []string{"Alice", "Bob"} {
		result := ref.Ask(
			context.Background(), SingletonGreetingMsg{Name: name},
		).Await(context.Background())
		result.WhenOk(func(s string) {
			fmt.Println(s)
		})
	}

	// Output:
	// Hello, Alice!
	// Hello, Bob!
}
