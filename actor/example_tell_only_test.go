package actor_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// LogMsg is a message type for the TellOnly example.
type LogMsg struct {
	actor.BaseMessage
	Text string
}

// MessageType implements actor.Message.
func (m LogMsg) MessageType() string { return "LogMsg" }

// LoggerActorBehavior is a simple actor behavior that logs messages. It doesn't
// produce a meaningful response for Ask, so it's a good candidate for TellOnly
// interactions.
type LoggerActorBehavior struct {
	mu      sync.Mutex
	logs    []string
	actorID string
}

func NewLoggerActorBehavior(id string) *LoggerActorBehavior {
	return &LoggerActorBehavior{actorID: id}
}

// Receive processes LogMsg messages by appending them to an internal log. The
// response type is 'any' as it's not typically used with Ask.
func (l *LoggerActorBehavior) Receive(ctx context.Context,
	msg actor.Message) fn.Result[any] {

	logMessage, ok := msg.(LogMsg)
	if !ok {
		return fn.Err[any](fmt.Errorf("unexpected message "+
			"type: %s", msg.MessageType()))
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	entry := fmt.Sprintf("[%s from %s]: %s", time.Now().Format("15:04:05"),
		l.actorID, logMessage.Text)
	l.logs = append(l.logs, entry)

	// For Tell, the result is often ignored, but we must return something.
	return fn.Ok[any](nil)
}

func (l *LoggerActorBehavior) GetLogs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	copiedLogs := make([]string, len(l.logs))
	copy(copiedLogs, l.logs)

	return copiedLogs
}

// ExampleTellOnlyRef demonstrates using a TellOnlyRef for fire-and-forget
// messaging with an actor.
func ExampleTellOnlyRef() {
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// The logger actor doesn't really have a response type for Ask, so we
	// use 'any'.
	loggerServiceKey := actor.NewServiceKey[actor.Message, any](
		"tell-only-logger-service",
	)

	actorID := "my-logger"
	loggerLogic := NewLoggerActorBehavior(actorID)

	// Spawn the actor.
	fullRef := loggerServiceKey.Spawn(system, actorID, loggerLogic)
	fmt.Printf("Actor %s spawned.\n", fullRef.ID())

	// Get a TellOnlyRef for the actor. We can get this from the Actor
	// instance itself if we had it, or by type assertion if we know the
	// underlying ref supports it. Since fullRef is ActorRef[actor.Message,
	// any], it already satisfies TellOnlyRef[actor.Message].
	//
	// Or, if we had the *Actor instance: tellOnlyLogger =
	// actorInstance.TellRef()
	var tellOnlyLogger actor.TellOnlyRef[actor.Message] = fullRef

	fmt.Printf("Obtained TellOnlyRef for %s.\n", tellOnlyLogger.ID())

	// Send messages using Tell.
	tellOnlyLogger.Tell(
		context.Background(), LogMsg{Text: "First log entry."},
	)
	tellOnlyLogger.Tell(
		context.Background(), LogMsg{Text: "Second log entry."},
	)

	// Allow some time for messages to be processed.
	time.Sleep(10 * time.Millisecond)

	// Retrieve logs directly from the behavior for verification in this
	// example. In a real scenario, this might not be possible or desired.
	logs := loggerLogic.GetLogs()
	fmt.Println("Logged entries:")
	for _, entry := range logs {
		// Strip the timestamp and actor ID for consistent example
		// output. Example entry: "[15:04:05 from my-logger]: Actual log
		// text"
		parts := strings.SplitN(entry, "]: ", 2)
		if len(parts) == 2 {
			fmt.Println(parts[1])
		}
	}

	// Attempting to Ask using tellOnlyLogger would be a compile-time error:
	// tellOnlyLogger.Ask(context.Background(), LogMsg{Text: "This would
	// fail"})

	// Output:
	// Actor my-logger spawned.
	// Obtained TellOnlyRef for my-logger.
	// Logged entries:
	// First log entry.
	// Second log entry.
}
