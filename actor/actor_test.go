package actor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// testMsg is a simple message type for testing. It embeds BaseMessage to
// satisfy the actor.Message interface.
type testMsg struct {
	BaseMessage
	data string

	replyChan chan string
}

// MessageType returns the type name of the message.
func (m *testMsg) MessageType() string {
	return "testMsg"
}

// newTestMsg creates a new test message.
func newTestMsg(data string) *testMsg {
	return &testMsg{data: data}
}

// newTestMsgWithReply creates a new test message that includes a reply channel.
// This can be used by test behaviors to send data back to the test
// synchronously, especially for Tell operations.
func newTestMsgWithReply(data string, replyChan chan string) *testMsg {
	return &testMsg{data: data, replyChan: replyChan}
}

// echoBehavior is a simple actor behavior that processes *testMsg messages. It
// stores the last message's data and, for Ask, echoes it back. For Tell, if
// replyChan is set in testMsg, it sends data back on it.
type echoBehavior struct {
	lastMsgData     atomic.Value
	processingDelay time.Duration
	t               *testing.T
}

// newEchoBehavior creates a new echoBehavior.
func newEchoBehavior(t *testing.T, delay time.Duration) *echoBehavior {
	return &echoBehavior{t: t, processingDelay: delay}
}

// Receive handles incoming messages. It simulates work if processingDelay is
// set, stores the message data, and responds for Ask operations or via
// replyChan for Tell.
func (b *echoBehavior) Receive(_ context.Context,
	msg *testMsg) fn.Result[string] {

	if b.processingDelay > 0 {
		time.Sleep(b.processingDelay)
	}

	b.lastMsgData.Store(msg.data)

	if msg.replyChan != nil {
		// Attempt to send the data on the reply channel, but quit if
		// it takes longer than 1 second (e.g., channel unbuffered
		// and no receiver).
		select {
		case msg.replyChan <- msg.data:
		case <-time.After(time.Second):
			b.t.Logf("warning: replyChan send timed out")
		}
	}

	return fn.Ok(fmt.Sprintf("echo: %s", msg.data))
}

// GetLastMsgData retrieves the data from the last message processed.
func (b *echoBehavior) GetLastMsgData() (string, bool) {
	val := b.lastMsgData.Load()
	if val == nil {
		return "", false
	}
	data, ok := val.(string)
	return data, ok
}

// errorBehavior is an actor behavior that always returns a predefined error
// upon receiving a message.
type errorBehavior struct {
	err error
}

// newErrorBehavior creates a new errorBehavior.
func newErrorBehavior(err error) *errorBehavior {
	return &errorBehavior{err: err}
}

// Receive always returns the configured error.
func (b *errorBehavior) Receive(_ context.Context,
	_ *testMsg) fn.Result[string] {

	return fn.Err[string](b.err)
}

// blockingBehavior is an actor behavior that blocks until its actorCtx is done.
type blockingBehavior struct{}

// Receive blocks until the actor's context is cancelled, then returns the
// context's error.
func (b *blockingBehavior) Receive(actorCtx context.Context,
	_ *testMsg) fn.Result[string] {

	<-actorCtx.Done()
	return fn.Err[string](actorCtx.Err())
}

// deadLetterTestMsg is a distinct message type used for testing DLO
// interactions.
type deadLetterTestMsg struct {
	BaseMessage
	id string
}

// MessageType returns the type name of the message.
func (m *deadLetterTestMsg) MessageType() string {
	return "deadLetterTestMsg"
}

// deadLetterObserverBehavior is a behavior for a test Dead Letter Office actor.
// It records all messages sent to it, allowing tests to verify DLO
// interactions.
type deadLetterObserverBehavior struct {
	mu           sync.Mutex
	receivedMsgs []Message
}

// newDeadLetterObserverBehavior creates a new deadLetterObserverBehavior.
func newDeadLetterObserverBehavior() *deadLetterObserverBehavior {
	return &deadLetterObserverBehavior{
		receivedMsgs: make([]Message, 0),
	}
}

// Receive records the incoming message and returns a successful result.
func (b *deadLetterObserverBehavior) Receive(_ context.Context,
	msg Message) fn.Result[any] {

	b.mu.Lock()
	b.receivedMsgs = append(b.receivedMsgs, msg)
	b.mu.Unlock()

	return fn.Ok[any](nil)
}

// GetReceivedMsgs returns a copy of all messages received by this DLO.
func (b *deadLetterObserverBehavior) GetReceivedMsgs() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	msgs := make([]Message, len(b.receivedMsgs))
	copy(msgs, b.receivedMsgs)

	return msgs
}

// actorTestHarness provides helper methods for setting up actors in tests. It
// manages a dedicated DLO for actors created through it.
type actorTestHarness struct {
	t      *testing.T
	dlo    *Actor[Message, any]
	dloBeh *deadLetterObserverBehavior
}

// newActorTestHarness sets up a test harness with a dedicated DLO. The DLO is
// automatically stopped when the test cleans up.
func newActorTestHarness(t *testing.T) *actorTestHarness {
	t.Helper()

	dloBeh := newDeadLetterObserverBehavior()
	dloCfg := ActorConfig[Message, any]{
		ID:          "test-dlo-" + t.Name(),
		Behavior:    dloBeh,
		DLO:         nil,
		MailboxSize: 10,
	}
	dloActor := NewActor[Message, any](dloCfg)
	dloActor.Start()

	t.Cleanup(dloActor.Stop)

	return &actorTestHarness{
		t:      t,
		dlo:    dloActor,
		dloBeh: dloBeh,
	}
}

// newActor creates, starts, and registers a new actor for cleanup. The actor
// will use the harness's DLO.
func (h *actorTestHarness) newActor(id string,
	beh ActorBehavior[*testMsg, string],
	mailboxSize int) *Actor[*testMsg, string] {

	h.t.Helper()

	cfg := ActorConfig[*testMsg, string]{
		ID:          id,
		Behavior:    beh,
		DLO:         h.dlo.Ref(),
		MailboxSize: mailboxSize,
	}
	actor := NewActor(cfg)
	actor.Start()

	h.t.Cleanup(actor.Stop)

	return actor
}

// assertDLOMessage checks that the DLO eventually receives a specific message.
func (h *actorTestHarness) assertDLOMessage(expectedMsg Message) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		msgs := h.dloBeh.GetReceivedMsgs()
		for _, m := range msgs {
			if reflect.DeepEqual(m, expectedMsg) {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond,
		"dLO did not receive expected message: %v", expectedMsg,
	)
}

// assertNoDLOMessages checks that the DLO has not received any messages.
func (h *actorTestHarness) assertNoDLOMessages() {
	h.t.Helper()

	// Allow a very brief moment for any async DLO sends to occur.
	time.Sleep(20 * time.Millisecond)

	msgs := h.dloBeh.GetReceivedMsgs()

	require.Empty(h.t, msgs, "dLO received unexpected messages")
}

// TestActorNewActorIDAndRefs verifies that NewActor correctly initializes an
// actor's ID and provides functional ActorRef and TellOnlyRef instances.
func TestActorNewActorIDAndRefs(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	actorID := "test-actor-1"
	beh := newEchoBehavior(t, 0)
	actor := h.newActor(actorID, beh, 1)

	require.Equal(t, actorID, actor.Ref().ID(), "actorRef ID mismatch")
	require.Equal(
		t, actorID, actor.TellRef().ID(), "tellOnlyRef ID mismatch",
	)
	require.NotNil(t, actor.Ref(), "actorRef should not be nil")
	require.NotNil(t, actor.TellRef(), "tellOnlyRef should not be nil")
}

// TestActorStartStop verifies the basic lifecycle of an actor: starting,
// processing messages, and stopping.
func TestActorStartStop(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	beh := newEchoBehavior(t, 0)
	actor := h.newActor("test-actor-lifecycle", beh, 1)

	// Actor should be running and process a message.
	msgData := "hello"
	replyChan := make(chan string, 1)
	actor.Ref().Tell(
		context.Background(), newTestMsgWithReply(msgData, replyChan),
	)

	received, err := fn.RecvOrTimeout(replyChan, 100*time.Millisecond)
	if err != nil {
		t.Fatal("timed out waiting for actor to process message")
	}
	require.Equal(
		t, msgData, received, "actor did not process message before stop",
	)

	actor.Stop()
	time.Sleep(50 * time.Millisecond)

	// Try sending another message; it should ideally not be processed or go
	// to DLO.
	msgDataAfterStop := "message-after-stop"
	replyChanAfterStop := make(chan string, 1)
	actor.Ref().Tell(
		context.Background(),
		newTestMsgWithReply(msgDataAfterStop, replyChanAfterStop),
	)

	// We expect a timeout here, meaning the message was not processed by
	// the echoBehavior's replyChan.
	_, err = fn.RecvOrTimeout(replyChanAfterStop, 100*time.Millisecond)
	if err == nil { // err == nil means a message was received
		t.Fatal("actor processed message after Stop()")
	}
	require.ErrorContains(t, err, "timeout hit")

	h.assertDLOMessage(
		&testMsg{data: msgDataAfterStop, replyChan: replyChanAfterStop},
	)
}

// TestActorTellBasic verifies that a message sent via Tell is processed by the
// actor's behavior.
func TestActorTellBasic(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	beh := newEchoBehavior(t, 0)
	actor := h.newActor("test-actor-tell", beh, 1)

	msgData := "tell-message"
	replyChan := make(chan string, 1)
	actor.Ref().Tell(
		context.Background(), newTestMsgWithReply(msgData, replyChan),
	)

	receivedTell, errTell := fn.RecvOrTimeout(replyChan, 100*time.Millisecond)
	if errTell != nil {
		t.Fatal("timed out waiting for Tell message processing")
	}
	require.Equal(
		t, msgData, receivedTell, "behavior did not receive Tell message data",
	)

	lastData, ok := beh.GetLastMsgData()
	require.True(t, ok, "last message data not set in behavior")
	require.Equal(t, msgData, lastData, "last message data mismatch")
	h.assertNoDLOMessages()
}

// TestActorAskSuccess verifies that a message sent via Ask is processed, and
// the returned Future is completed with the behavior's successful result.
func TestActorAskSuccess(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	beh := newEchoBehavior(t, 0)
	actor := h.newActor("test-actor-ask-success", beh, 1)

	msgData := "ask-message"
	future := actor.Ref().Ask(context.Background(), newTestMsg(msgData))

	result := future.Await(context.Background())
	require.False(t, result.IsErr(), "ask returned an error: %v", result.Err())

	result.WhenOk(func(val string) {
		expectedReply := fmt.Sprintf("echo: %s", msgData)
		require.Equal(t, expectedReply, val, "ask response mismatch")
	})

	lastData, ok := beh.GetLastMsgData()
	require.True(t, ok, "last message data not set in behavior")
	require.Equal(t, msgData, lastData, "last message data mismatch")
	h.assertNoDLOMessages()
}

// TestActorAskErrorBehavior verifies that if an actor's behavior returns an
// error, the Future from an Ask call is completed with that error.
func TestActorAskErrorBehavior(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	expectedErr := errors.New("behavior error")
	beh := newErrorBehavior(expectedErr)
	actor := h.newActor("test-actor-ask-error", beh, 1)

	future := actor.Ref().Ask(
		context.Background(), newTestMsg("ask-error-test"),
	)

	result := future.Await(context.Background())
	require.True(t, result.IsErr(), "ask should have returned an error")
	require.ErrorIs(t, result.Err(), expectedErr, "ask error mismatch")

	h.assertNoDLOMessages()
}
