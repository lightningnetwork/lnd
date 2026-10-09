package reputation

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// scidLen is the length of a big endian encoded short channel id.
const scidLen = 8

// SQLQueries is the set of queries the SQL store runs against the reputation
// tables.
type SQLQueries interface {
	UpsertReputationChannel(ctx context.Context,
		arg sqlc.UpsertReputationChannelParams) error

	FetchReputationChannels(ctx context.Context) ([]sqlc.ReputationChannel,
		error)

	DeleteReputationChannel(ctx context.Context, scid []byte) (int64, error)
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
				return fmt.Errorf("channel %d: %w", c.SCID, err)
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
func (s *SQLStore) DeleteChannel(ctx context.Context, scid uint64) error {
	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		_, err := db.DeleteReputationChannel(ctx, encodeSCID(scid))

		return err
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("unable to delete reputation channel %d: %w",
			scid, err)
	}

	return nil
}

// upsertParams converts a ChannelState into the parameters of the upsert
// query. Timestamps are stored in UTC.
func upsertParams(c ChannelState) sqlc.UpsertReputationChannelParams {
	return sqlc.UpsertReputationChannelParams{
		Scid:                     encodeSCID(c.SCID),
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
	scid, err := decodeSCID(row.Scid)
	if err != nil {
		return ChannelState{}, err
	}

	return ChannelState{
		SCID:                     scid,
		OutgoingReputation:       row.OutgoingReputation,
		IncomingRevenue:          row.IncomingRevenue,
		IncomingRevenueUpdatedAt: row.IncomingRevenueUpdatedAt.UTC(),
		IncomingRevenueStartedAt: row.IncomingRevenueStartedAt.UTC(),
		OutgoingReputationUpdatedAt: row.OutgoingReputationUpdatedAt.
			UTC(),
	}, nil
}

// encodeSCID encodes a short channel id as big endian bytes, so that rows
// order by channel age.
func encodeSCID(scid uint64) []byte {
	var b [scidLen]byte
	binary.BigEndian.PutUint64(b[:], scid)

	return b[:]
}

// decodeSCID decodes a big endian encoded short channel id.
func decodeSCID(b []byte) (uint64, error) {
	if len(b) != scidLen {
		return 0, fmt.Errorf("invalid scid length %d, want %d", len(b),
			scidLen)
	}

	return binary.BigEndian.Uint64(b), nil
}
