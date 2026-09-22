package actor

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

// wrappedMsg is a second message type used to exercise MapInputRef.
type wrappedMsg struct {
	BaseMessage
	inner string
}

// MessageType returns the type name of the message.
func (m *wrappedMsg) MessageType() string {
	return "wrappedMsg"
}

// TestMapInputRef asserts that MapInputRef transforms messages on both Tell
// and TryTell, and that FilterMapInputRef drops the messages its transform
// rejects.
func TestMapInputRef(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		b := newGatedBehavior(true)
		a := newGatedActor(t, b, 4)
		defer a.Stop()

		ctx := t.Context()
		mapped := NewMapInputRef(
			a.TellRef(), func(m *wrappedMsg) *testMsg {
				return newTestMsg("mapped:" + m.inner)
			},
		)
		require.Equal(t, "map-input->gated", mapped.ID())

		mapped.Tell(ctx, &wrappedMsg{inner: "a"})
		require.NoError(t, mapped.TryTell(ctx, &wrappedMsg{inner: "b"}))

		synctest.Wait()
		require.Equal(t, "mapped:a", <-b.seen)
		require.Equal(t, "mapped:b", <-b.seen)

		filtered := NewFilterMapInputRef(
			a.TellRef(), func(m *wrappedMsg) (*testMsg, bool) {
				if m.inner == "drop" {
					return nil, false
				}

				return newTestMsg("kept:" + m.inner), true
			},
		)

		filtered.Tell(ctx, &wrappedMsg{inner: "drop"})
		require.NoError(
			t, filtered.TryTell(ctx, &wrappedMsg{inner: "drop"}),
		)
		filtered.Tell(ctx, &wrappedMsg{inner: "c"})

		synctest.Wait()
		require.Equal(t, "kept:c", <-b.seen)
		require.Empty(t, b.seen)
	})
}
