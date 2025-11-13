package actor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// testMessageA is a test message type for transformation testing.
type testMessageA struct {
	BaseMessage
	Value int
	Text  string
}

// MessageType returns the message type identifier.
func (m testMessageA) MessageType() string {
	return "testMessageA"
}

// testMessageB is another test message type for transformation testing.
type testMessageB struct {
	BaseMessage
	DoubledValue int
	UpperText    string
}

// MessageType returns the message type identifier.
func (m testMessageB) MessageType() string {
	return "testMessageB"
}

// mockTellOnlyRef is a mock implementation of TellOnlyRef for testing.
type mockTellOnlyRef[M Message] struct {
	id       string
	received []M
}

func (m *mockTellOnlyRef[M]) Tell(ctx context.Context, msg M) {
	m.received = append(m.received, msg)
}

func (m *mockTellOnlyRef[M]) ID() string {
	return m.id
}

// TestMapInputRefBasicTransformation tests that messages are correctly
// transformed when sent through a MapInputRef.
func TestMapInputRefBasicTransformation(t *testing.T) {
	t.Parallel()

	// Create a mock target ref that expects testMessageB.
	targetRef := &mockTellOnlyRef[testMessageB]{
		id: "test-target",
	}

	// Create a transformation function from A to B.
	transformFn := func(a testMessageA) testMessageB {
		return testMessageB{
			DoubledValue: a.Value * 2,
			UpperText:    a.Text + "-TRANSFORMED",
		}
	}

	// Create the MapInputRef that accepts testMessageA.
	adaptedRef := NewMapInputRef(targetRef, transformFn)

	// Send a message of type A.
	ctx := context.Background()
	inputMsg := testMessageA{
		Value: 42,
		Text:  "hello",
	}
	adaptedRef.Tell(ctx, inputMsg)

	// Verify the target received the transformed message.
	require.Len(t, targetRef.received, 1)
	received := targetRef.received[0]
	require.Equal(t, 84, received.DoubledValue)
	require.Equal(t, "hello-TRANSFORMED", received.UpperText)
}

// TestMapInputRefMultipleMessages tests that multiple messages are all
// correctly transformed.
func TestMapInputRefMultipleMessages(t *testing.T) {
	t.Parallel()

	targetRef := &mockTellOnlyRef[testMessageB]{
		id: "test-target",
	}

	transformFn := func(a testMessageA) testMessageB {
		return testMessageB{
			DoubledValue: a.Value * 2,
			UpperText:    a.Text,
		}
	}

	adaptedRef := NewMapInputRef(targetRef, transformFn)
	ctx := context.Background()

	// Send multiple messages.
	messages := []testMessageA{
		{Value: 1, Text: "one"},
		{Value: 2, Text: "two"},
		{Value: 3, Text: "three"},
	}

	for _, msg := range messages {
		adaptedRef.Tell(ctx, msg)
	}

	// Verify all messages were transformed and received.
	require.Len(t, targetRef.received, 3)
	require.Equal(t, 2, targetRef.received[0].DoubledValue)
	require.Equal(t, "one", targetRef.received[0].UpperText)
	require.Equal(t, 4, targetRef.received[1].DoubledValue)
	require.Equal(t, "two", targetRef.received[1].UpperText)
	require.Equal(t, 6, targetRef.received[2].DoubledValue)
	require.Equal(t, "three", targetRef.received[2].UpperText)
}

// TestMapInputRefID tests that the ID method returns a prefixed version of
// the target ref's ID.
func TestMapInputRefID(t *testing.T) {
	t.Parallel()

	targetRef := &mockTellOnlyRef[testMessageB]{
		id: "my-target-actor",
	}

	transformFn := func(a testMessageA) testMessageB {
		return testMessageB{}
	}

	adaptedRef := NewMapInputRef(targetRef, transformFn)

	// Verify the ID includes the target ID with a prefix.
	expectedID := "map-input-my-target-actor"
	require.Equal(t, expectedID, adaptedRef.ID())
}

// TestMapInputRefTypeSafety tests that the generic type constraints ensure
// compile-time type safety.
func TestMapInputRefTypeSafety(t *testing.T) {
	t.Parallel()

	// This test verifies that the type system works correctly. If this
	// compiles, it proves type safety is maintained.
	targetRef := &mockTellOnlyRef[testMessageB]{
		id: "test-target",
	}

	// Create a MapInputRef[A, B].
	var adaptedRef TellOnlyRef[testMessageA] = NewMapInputRef(
		targetRef,
		func(a testMessageA) testMessageB {
			return testMessageB{
				DoubledValue: a.Value,
			}
		},
	)

	// Verify we can use it as a TellOnlyRef[testMessageA].
	ctx := context.Background()
	adaptedRef.Tell(ctx, testMessageA{Value: 10})

	// The fact that this compiles and runs proves type safety.
	require.Len(t, targetRef.received, 1)
}

// TestMapInputRefIdentityTransform tests that MapInputRef works when the
// input and output types are the same (identity transformation).
func TestMapInputRefIdentityTransform(t *testing.T) {
	t.Parallel()

	targetRef := &mockTellOnlyRef[testMessageA]{
		id: "test-target",
	}

	// Identity transformation: A -> A with modified value.
	transformFn := func(a testMessageA) testMessageA {
		a.Value = a.Value + 100
		return a
	}

	adaptedRef := NewMapInputRef(targetRef, transformFn)

	ctx := context.Background()
	inputMsg := testMessageA{
		Value: 5,
		Text:  "test",
	}
	adaptedRef.Tell(ctx, inputMsg)

	// Verify the transformation was applied.
	require.Len(t, targetRef.received, 1)
	require.Equal(t, 105, targetRef.received[0].Value)
	require.Equal(t, "test", targetRef.received[0].Text)
}
