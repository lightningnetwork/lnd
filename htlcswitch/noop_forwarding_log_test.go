package htlcswitch

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// TestNoOpForwardingLog verifies that the no-op forwarding log correctly
// implements the ForwardingLog interface and discards all events.
func TestNoOpForwardingLog(t *testing.T) {
	t.Parallel()

	// Create a new no-op forwarding log.
	log := NewNoOpForwardingLog()

	// Create some test forwarding events.
	events := []channeldb.ForwardingEvent{
		{
			Timestamp:      time.Now(),
			IncomingChanID: lnwire.NewShortChanIDFromInt(1),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(2),
			AmtIn:          1000,
			AmtOut:         990,
			IncomingHtlcID: fn.Some(uint64(1)),
			OutgoingHtlcID: fn.Some(uint64(2)),
		},
		{
			Timestamp:      time.Now().Add(time.Minute),
			IncomingChanID: lnwire.NewShortChanIDFromInt(3),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(4),
			AmtIn:          2000,
			AmtOut:         1980,
			IncomingHtlcID: fn.Some(uint64(3)),
			OutgoingHtlcID: fn.Some(uint64(4)),
		},
	}

	// Adding events should not return an error, even though they are
	// discarded.
	err := log.AddForwardingEvents(events)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Adding an empty slice should also work.
	err = log.AddForwardingEvents([]channeldb.ForwardingEvent{})
	if err != nil {
		t.Fatalf("expected no error for empty events, got: %v", err)
	}

	// Adding nil should also work (defensive programming).
	err = log.AddForwardingEvents(nil)
	if err != nil {
		t.Fatalf("expected no error for nil events, got: %v", err)
	}
}

// TestNoOpForwardingLogInterface verifies that NoOpForwardingLog implements
// the ForwardingLog interface at compile time.
func TestNoOpForwardingLogInterface(t *testing.T) {
	var _ ForwardingLog = (*NoOpForwardingLog)(nil)
}

