package reputation

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// SQLQueries is the set of queries the SQL store runs against the reputation
// tables.
type SQLQueries interface {
	UpsertReputationChannel(ctx context.Context,
		arg sqlc.UpsertReputationChannelParams) error

	FetchReputationChannels(ctx context.Context) ([]sqlc.ReputationChannel,
		error)

	DeleteReputationChannel(ctx context.Context, channelID []byte) (int64,
		error)
}

// BatchedSQLQueries combines the reputation queries with the ability to run
// them in a single transaction.
type BatchedSQLQueries interface {
	SQLQueries

	sqldb.BatchedTx[SQLQueries]
}

// SQLStore is the native SQL implementation of Store.
type SQLStore struct {
	db BatchedSQLQueries
}

// A compile time check that SQLStore implements Store.
var _ Store = (*SQLStore)(nil)

// NewSQLStore creates a Store backed by the given SQL queries.
func NewSQLStore(db BatchedSQLQueries) *SQLStore {
	return &SQLStore{db: db}
}

// FetchChannels returns the persisted state of every channel.
func (s *SQLStore) FetchChannels(ctx context.Context) ([]ChannelState, error) {
	var channels []ChannelState

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		rows, err := db.FetchReputationChannels(ctx)
		if err != nil {
			return err
		}

		channels = make([]ChannelState, 0, len(rows))
		for _, row := range rows {
			state, err := channelStateFromRow(row)
			if err != nil {
				return err
			}

			channels = append(channels, state)
		}

		return nil
	}, func() {
		channels = nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to fetch reputation channels: "+
			"%w", err)
	}

	return channels, nil
}

// UpsertChannels writes the given channel states in a single transaction,
// replacing any existing state for the same channels.
func (s *SQLStore) UpsertChannels(ctx context.Context,
	channels []ChannelState) error {

	if len(channels) == 0 {
		return nil
	}

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		for _, c := range channels {
			err := db.UpsertReputationChannel(ctx, upsertParams(c))
			if err != nil {
				return fmt.Errorf("channel %v: %w", c.ChanID,
					err)
			}
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("unable to upsert reputation channels: %w",
			err)
	}

	return nil
}

// DeleteChannel removes the persisted state of a channel. Deleting a channel
// that has no persisted state is not an error.
func (s *SQLStore) DeleteChannel(ctx context.Context,
	chanID lnwire.ChannelID) error {

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		_, err := db.DeleteReputationChannel(ctx, chanID[:])

		return err
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("unable to delete reputation channel %v: %w",
			chanID, err)
	}

	return nil
}

// upsertParams converts a ChannelState into the parameters of the upsert
// query. Timestamps are stored in UTC.
func upsertParams(c ChannelState) sqlc.UpsertReputationChannelParams {
	return sqlc.UpsertReputationChannelParams{
		ChannelID:                c.ChanID[:],
		OutgoingReputation:       c.OutgoingReputation,
		IncomingRevenue:          c.IncomingRevenue,
		IncomingRevenueUpdatedAt: c.IncomingRevenueUpdatedAt.UTC(),
		IncomingRevenueStartedAt: c.IncomingRevenueStartedAt.UTC(),
		OutgoingReputationUpdatedAt: c.OutgoingReputationUpdatedAt.
			UTC(),
	}
}

// channelStateFromRow converts a database row into a ChannelState.
func channelStateFromRow(row sqlc.ReputationChannel) (ChannelState, error) {
	chanID, err := decodeChannelID(row.ChannelID)
	if err != nil {
		return ChannelState{}, err
	}

	return ChannelState{
		ChanID:                   chanID,
		OutgoingReputation:       row.OutgoingReputation,
		IncomingRevenue:          row.IncomingRevenue,
		IncomingRevenueUpdatedAt: row.IncomingRevenueUpdatedAt.UTC(),
		IncomingRevenueStartedAt: row.IncomingRevenueStartedAt.UTC(),
		OutgoingReputationUpdatedAt: row.OutgoingReputationUpdatedAt.
			UTC(),
	}, nil
}

// decodeChannelID decodes a stored channel_id, which must be exactly 32 bytes.
func decodeChannelID(b []byte) (lnwire.ChannelID, error) {
	var chanID lnwire.ChannelID
	if len(b) != len(chanID) {
		return chanID, fmt.Errorf("invalid channel_id length %d, "+
			"want %d", len(b), len(chanID))
	}

	copy(chanID[:], b)

	return chanID, nil
}
