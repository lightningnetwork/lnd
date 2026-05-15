package channelnotifier

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/stretchr/testify/require"
)

// TestChannelUpdateEvent tests that channel update events are properly
// notified to subscribers.
func TestChannelUpdateEvent(t *testing.T) {
	// Initialize the notification server.
	ntfnServer := New(nil)
	require.NoError(t, ntfnServer.Start())

	defer func() {
		require.NoError(t, ntfnServer.Stop())
	}()

	// Subscribe to channel events.
	sub, err := ntfnServer.SubscribeChannelEvents()
	require.NoError(t, err)
	defer sub.Cancel()

	// Create a mock channel state.
	channel := &chanstate.OpenChannel{}

	// Notify the server of a channel update event.
	ntfnServer.NotifyChannelUpdateEvent(channel)

	// Consume the event.
	select {
	case event := <-sub.Updates():
		updateEvent, ok := event.(ChannelUpdateEvent)
		require.True(t, ok)
		require.Equal(t, channel, updateEvent.Channel)

	case <-time.After(time.Second):
		t.Fatalf("expected to receive channel update event")
	}
}

// TestNotifyEarlyClosedChannelEvent verifies that the early-dispatch path
// delivers exactly the supplied close summary to subscribers without
// consulting the channel database. This is the path used by the chain watcher
// at first conf to insta-dispatch CLOSED_CHANNEL events for cooperative
// closes, before the close summary is persisted.
func TestNotifyEarlyClosedChannelEvent(t *testing.T) {
	t.Parallel()

	// Pass nil for chanDB; the early-dispatch path must not touch it.
	ntfnServer := New(nil)
	require.NoError(t, ntfnServer.Start())
	t.Cleanup(func() {
		require.NoError(t, ntfnServer.Stop())
	})

	sub, err := ntfnServer.SubscribeChannelEvents()
	require.NoError(t, err)
	t.Cleanup(sub.Cancel)

	// Build a close summary with IsPending=true to mirror what the chain
	// watcher will hand in at first-conf detection.
	chanPoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x01, 0x02, 0x03},
		Index: 4,
	}
	summary := &chanstate.ChannelCloseSummary{
		ChanPoint: chanPoint,
		CloseType: chanstate.CooperativeClose,
		IsPending: true,
	}

	ntfnServer.NotifyEarlyClosedChannelEvent(summary)

	select {
	case event := <-sub.Updates():
		closedEvent, ok := event.(ClosedChannelEvent)
		require.True(
			t, ok, "expected ClosedChannelEvent, got %T", event,
		)
		require.NotNil(t, closedEvent.CloseSummary)
		require.True(t, closedEvent.CloseSummary.IsPending,
			"early dispatched summary must carry IsPending=true")
		require.Equal(t, summary, closedEvent.CloseSummary,
			"early dispatched summary must reach subscriber "+
				"verbatim")

	case <-time.After(time.Second):
		t.Fatal("expected to receive early closed channel event")
	}
}

// TestNotifyEarlyClosedChannelEventSingleEvent guards against accidental
// re-dispatch: a single early-notify call must produce exactly one event,
// not two (e.g. a fan-out bug between the early and the legacy paths).
func TestNotifyEarlyClosedChannelEventSingleEvent(t *testing.T) {
	t.Parallel()

	ntfnServer := New(nil)
	require.NoError(t, ntfnServer.Start())
	t.Cleanup(func() {
		require.NoError(t, ntfnServer.Stop())
	})

	sub, err := ntfnServer.SubscribeChannelEvents()
	require.NoError(t, err)
	t.Cleanup(sub.Cancel)

	summary := &chanstate.ChannelCloseSummary{
		ChanPoint: wire.OutPoint{Index: 7},
		CloseType: chanstate.CooperativeClose,
		IsPending: true,
	}
	ntfnServer.NotifyEarlyClosedChannelEvent(summary)

	// Drain the single expected event.
	select {
	case <-sub.Updates():
	case <-time.After(time.Second):
		t.Fatal("expected to receive early closed channel event")
	}

	// Any further read should not produce another event.
	select {
	case extra := <-sub.Updates():
		t.Fatalf("unexpected second event: %T", extra)
	case <-time.After(50 * time.Millisecond):
	}
}
