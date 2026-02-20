package channelnotifier

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
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
	channel := &channeldb.OpenChannel{}

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
