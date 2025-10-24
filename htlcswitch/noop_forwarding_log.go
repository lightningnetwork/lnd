package htlcswitch

import (
	"github.com/lightningnetwork/lnd/channeldb"
)

// NoOpForwardingLog is an implementation of the ForwardingLog interface that
// discards all forwarding events. This is used when the --no-forwarding-history
// flag is enabled to prevent persisting payment forwarding history to the
// database for privacy reasons.
//
// While this prevents forensic reconstruction of forwarding activity, it also
// means that the ForwardingHistory RPC endpoint will return no data. All other
// LND functionality (payment routing, settling HTLCs, etc.) works normally.
type NoOpForwardingLog struct{}

// NewNoOpForwardingLog creates a new instance of the no-op forwarding log.
func NewNoOpForwardingLog() *NoOpForwardingLog {
	return &NoOpForwardingLog{}
}

// AddForwardingEvents does nothing and returns nil. All forwarding events are
// discarded rather than being persisted to the database.
func (n *NoOpForwardingLog) AddForwardingEvents(
	events []channeldb.ForwardingEvent) error {

	// Silently discard all events for privacy
	return nil
}

// Compile-time check to ensure NoOpForwardingLog implements ForwardingLog.
var _ ForwardingLog = (*NoOpForwardingLog)(nil)

