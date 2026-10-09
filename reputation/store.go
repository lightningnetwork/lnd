package reputation

import (
	"context"
	"time"
)

// ChannelState is the persisted reputation state of a single channel. It holds
// the two decaying averages exactly as they are kept in memory, the running
// value and the time it was last updated at, so that decay over a node's
// downtime is applied lazily on the first read after a restart.
type ChannelState struct {
	// SCID is the short channel id of the channel.
	SCID uint64

	// OutgoingReputation is the reputation the channel has accrued as an
	// outgoing link, in millisatoshis.
	OutgoingReputation int64

	// OutgoingReputationUpdatedAt is the time the outgoing reputation
	// average was last updated.
	OutgoingReputationUpdatedAt time.Time

	// IncomingRevenue is the revenue the channel has earned as an incoming
	// link, in millisatoshis, aggregated over several windows.
	IncomingRevenue int64

	// IncomingRevenueUpdatedAt is the time the incoming revenue average
	// was last updated.
	IncomingRevenueUpdatedAt time.Time

	// IncomingRevenueStartedAt is the time the incoming revenue average
	// started tracking, which drives its warm-up factor.
	IncomingRevenueStartedAt time.Time
}

// Store persists channel reputation state across restarts. Pending HTLCs are
// deliberately not part of it: they are rebuilt from the switch's in-flight
// circuits on startup.
type Store interface {
	// FetchChannels returns the persisted state of every channel.
	FetchChannels(ctx context.Context) ([]ChannelState, error)

	// UpsertChannels writes the given channel states, replacing any
	// existing state for the same channels.
	UpsertChannels(ctx context.Context, channels []ChannelState) error

	// DeleteChannel removes the persisted state of a channel. Deleting a
	// channel that has no persisted state is not an error.
	DeleteChannel(ctx context.Context, scid uint64) error
}

// noopStore is the Store used when no persistence backend is configured. It
// holds nothing, so reputation lives in memory only and is re-accrued from
// live traffic after a restart.
type noopStore struct{}

// NewNoopStore returns a Store that persists nothing.
func NewNoopStore() Store {
	return noopStore{}
}

// FetchChannels returns no channels.
func (noopStore) FetchChannels(context.Context) ([]ChannelState, error) {
	return nil, nil
}

// UpsertChannels discards the given channels.
func (noopStore) UpsertChannels(context.Context, []ChannelState) error {
	return nil
}

// DeleteChannel does nothing.
func (noopStore) DeleteChannel(context.Context, uint64) error {
	return nil
}
