package timeout

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/stretchr/testify/require"
)

// targetMsg stands in for a requester's own message type.
type targetMsg struct {
	actor.BaseMessage

	id      ID
	firedAt time.Time
}

// MessageType returns the type of this message.
func (m *targetMsg) MessageType() string { return "targetMsg" }

// TestMapTimeoutExpired verifies the adapter converts an expiry into the
// target's message type on both send paths, and that TryTell surfaces the
// target's full mailbox so the timeout actor's retry logic can see it.
func TestMapTimeoutExpired(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	target := newChanRef[*targetMsg]("target", 1)

	ref := MapTimeoutExpired(target, func(e ExpiredMsg) *targetMsg {
		return &targetMsg{id: e.ID}
	})
	require.Contains(t, ref.ID(), "target")

	ref.Tell(ctx, &ExpiredMsg{ID: "told"})
	got := <-target.Messages()
	require.Equal(t, ID("told"), got.id)

	require.NoError(t, ref.TryTell(ctx, &ExpiredMsg{ID: "tried"}))
	require.ErrorIs(
		t, ref.TryTell(ctx, &ExpiredMsg{ID: "overflow"}),
		actor.ErrMailboxFull,
	)

	got = <-target.Messages()
	require.Equal(t, ID("tried"), got.id)
}

// TestMapTickFired verifies the recurring adapter carries both the ID and the
// fire instant through to the target's message type.
func TestMapTickFired(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	target := newChanRef[*targetMsg]("target", 1)

	ref := MapTickFired(target, func(f TickFiredMsg) *targetMsg {
		return &targetMsg{id: f.ID, firedAt: f.FiredAt}
	})

	require.NoError(t, ref.TryTell(ctx, &TickFiredMsg{
		ID:      "tick",
		FiredAt: startEpoch,
	}))

	got := <-target.Messages()
	require.Equal(t, ID("tick"), got.id)
	require.Equal(t, startEpoch, got.firedAt)
}
