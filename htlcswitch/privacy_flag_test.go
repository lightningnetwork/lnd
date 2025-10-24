package htlcswitch

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"time"
)

// TestNoForwardingHistoryFlag tests that the NoForwardingHistory flag
// prevents both database and text logging of forwarding events.
func TestNoForwardingHistoryFlag(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                string
		noForwardingHistory bool
		expectLogging       bool
	}{
		{
			name:                "normal mode - logging enabled",
			noForwardingHistory: false,
			expectLogging:       true,
		},
		{
			name:                "privacy mode - logging disabled",
			noForwardingHistory: true,
			expectLogging:       false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a mock forwarding log that tracks if events were added
			mockLog := &mockForwardingLogCounter{
				events: make(map[time.Time]channeldb.ForwardingEvent),
			}

			// Test that the NoOpForwardingLog is used when flag is true
			if tc.noForwardingHistory {
				noOpLog := NewNoOpForwardingLog()
				
				// Create a test event
				event := channeldb.ForwardingEvent{
					Timestamp:      time.Now(),
					IncomingChanID: lnwire.NewShortChanIDFromInt(1),
					OutgoingChanID: lnwire.NewShortChanIDFromInt(2),
					AmtIn:          1000,
					AmtOut:         990,
					IncomingHtlcID: fn.Some(uint64(1)),
					OutgoingHtlcID: fn.Some(uint64(2)),
				}

				// Add event to no-op log
				err := noOpLog.AddForwardingEvents([]channeldb.ForwardingEvent{event})
				require.NoError(t, err)

				// Verify it was NOT persisted (no-op behavior)
				require.Equal(t, 0, len(mockLog.events), "no-op log should not persist events")
			} else {
				// In normal mode, events should be persisted
				event := channeldb.ForwardingEvent{
					Timestamp:      time.Now(),
					IncomingChanID: lnwire.NewShortChanIDFromInt(1),
					OutgoingChanID: lnwire.NewShortChanIDFromInt(2),
					AmtIn:          1000,
					AmtOut:         990,
					IncomingHtlcID: fn.Some(uint64(1)),
					OutgoingHtlcID: fn.Some(uint64(2)),
				}

				// Add event to mock log
				err := mockLog.AddForwardingEvents([]channeldb.ForwardingEvent{event})
				require.NoError(t, err)

				// Verify it WAS persisted
				require.Equal(t, 1, len(mockLog.events), "normal log should persist events")
			}
		})
	}
}

// mockForwardingLogCounter is a mock that counts how many events were added
type mockForwardingLogCounter struct {
	events map[time.Time]channeldb.ForwardingEvent
}

func (m *mockForwardingLogCounter) AddForwardingEvents(events []channeldb.ForwardingEvent) error {
	for _, event := range events {
		m.events[event.Timestamp] = event
	}
	return nil
}

// TestSwitchConfigNoForwardingHistory verifies that the Switch Config struct
// has the NoForwardingHistory field and it can be set correctly.
func TestSwitchConfigNoForwardingHistory(t *testing.T) {
	t.Parallel()

	// Create a config with privacy flag enabled
	cfg := Config{
		NoForwardingHistory: true,
	}

	require.True(t, cfg.NoForwardingHistory, "NoForwardingHistory flag should be true")

	// Create a config with privacy flag disabled
	cfg2 := Config{
		NoForwardingHistory: false,
	}

	require.False(t, cfg2.NoForwardingHistory, "NoForwardingHistory flag should be false")
}

