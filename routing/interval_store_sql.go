package routing

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// intervalConfidenceScale is the denominator used to store a confidence, which
// ranges from zero to one, as an integer number of parts per million.
const intervalConfidenceScale = 1_000_000

// SQLIntervalQueries is the subset of the generated query interface that the
// interval store needs. Keeping it narrow means the store depends only on the
// queries it actually issues.
type SQLIntervalQueries interface {
	UpsertLiquidityInterval(ctx context.Context,
		arg sqlc.UpsertLiquidityIntervalParams) error

	ListLiquidityIntervals(ctx context.Context, limit int32) (
		[]sqlc.LiquidityInterval, error)

	PruneLiquidityIntervals(ctx context.Context, limit int32) error

	DeleteLiquidityIntervals(ctx context.Context) error
}

// BatchedIntervalQueries is a version of SQLIntervalQueries capable of batched
// database operations.
type BatchedIntervalQueries interface {
	SQLIntervalQueries

	sqldb.BatchedTx[SQLIntervalQueries]
}

// SQLIntervalStore persists liquidity interval beliefs to a SQL database. It
// is the durable half of the interval router's memory; the in-memory
// IntervalStore is what the router actually reads while it routes.
type SQLIntervalStore struct {
	db BatchedIntervalQueries

	clock clock.Clock
}

// A compile time assertion to ensure the SQL store satisfies the persister
// contract, and that the generated queries satisfy the narrow interface above.
var _ IntervalPersister = (*SQLIntervalStore)(nil)
var _ SQLIntervalQueries = (*sqlc.Queries)(nil)

// NewSQLIntervalStore builds a store backed by the given database.
func NewSQLIntervalStore(db *sqldb.BaseDB) *SQLIntervalStore {
	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLIntervalQueries {
			return db.WithTx(tx)
		},
	)

	return &SQLIntervalStore{
		db:    executor,
		clock: clock.NewDefaultClock(),
	}
}

// FetchIntervals returns at most limit of the most recently written beliefs.
//
// NOTE: Part of the IntervalPersister interface.
func (s *SQLIntervalStore) FetchIntervals(ctx context.Context, limit int) (
	[]PersistedInterval, error) {

	var intervals []PersistedInterval

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(),
		func(tx SQLIntervalQueries) error {
			rows, err := tx.ListLiquidityIntervals(
				ctx, boundedLimit(limit),
			)
			if err != nil {
				return err
			}

			intervals = make([]PersistedInterval, 0, len(rows))
			for _, row := range rows {
				entry, err := unmarshalInterval(row)
				if err != nil {
					// A row we cannot read is a belief we
					// no longer hold, which costs an
					// attempt to rediscover and nothing
					// more. Say so and carry on rather than
					// refusing to start.
					log.Warnf("Skipping unreadable "+
						"liquidity interval: %v", err)

					continue
				}

				intervals = append(intervals, entry)
			}

			return nil
		}, func() {
			intervals = nil
		},
	)
	if err != nil {
		return nil, err
	}

	return intervals, nil
}

// StoreIntervals writes the given beliefs, replacing any already held for the
// same directed channels.
//
// NOTE: Part of the IntervalPersister interface.
func (s *SQLIntervalStore) StoreIntervals(ctx context.Context,
	intervals []PersistedInterval) error {

	if len(intervals) == 0 {
		return nil
	}

	now := s.clock.Now().UTC()

	return s.db.ExecTx(ctx, sqldb.WriteTxOpt(),
		func(tx SQLIntervalQueries) error {
			for _, entry := range intervals {
				err := tx.UpsertLiquidityInterval(
					ctx, marshalInterval(entry, now),
				)
				if err != nil {
					return fmt.Errorf("unable to store "+
						"liquidity interval: %w", err)
				}
			}

			return nil
		}, sqldb.NoOpReset,
	)
}

// PruneIntervals drops all but the given number of most recently written
// beliefs.
//
// NOTE: Part of the IntervalPersister interface.
func (s *SQLIntervalStore) PruneIntervals(ctx context.Context,
	keep int) error {

	return s.db.ExecTx(ctx, sqldb.WriteTxOpt(),
		func(tx SQLIntervalQueries) error {
			return tx.PruneLiquidityIntervals(
				ctx, boundedLimit(keep),
			)
		}, sqldb.NoOpReset,
	)
}

// PurgeIntervals drops every stored belief.
//
// NOTE: Part of the IntervalPersister interface.
func (s *SQLIntervalStore) PurgeIntervals(ctx context.Context) error {
	return s.db.ExecTx(ctx, sqldb.WriteTxOpt(),
		func(tx SQLIntervalQueries) error {
			return tx.DeleteLiquidityIntervals(ctx)
		}, sqldb.NoOpReset,
	)
}

// boundedLimit converts a row limit into the type the generated queries take,
// clamping it into range.
func boundedLimit(limit int) int32 {
	if limit <= 0 {
		return DefaultMaxIntervalHistory
	}
	if limit > math.MaxInt32 {
		return math.MaxInt32
	}

	return int32(limit)
}

// marshalInterval turns a belief into the row that represents it.
func marshalInterval(entry PersistedInterval,
	now time.Time) sqlc.UpsertLiquidityIntervalParams {

	var scid [8]byte
	binary.BigEndian.PutUint64(scid[:], entry.Key.ChanID)

	interval := entry.Interval

	confidence := int64(
		math.Round(interval.Confidence * intervalConfidenceScale),
	)
	confidence = min(max(confidence, 0), intervalConfidenceScale)

	return sqlc.UpsertLiquidityIntervalParams{
		Scid:          scid[:],
		FromNode:      entry.Key.From[:],
		ToNode:        entry.Key.To[:],
		LowerOkMsat:   int64(interval.LowerOK),
		UpperFailMsat: int64(interval.UpperFail),
		EstimateMsat:  int64(interval.Estimate),
		ConfidencePpm: confidence,
		Successes:     int64(interval.Successes),
		Failures:      int64(interval.Failures),
		LiquidityMode: int32(interval.Mode),
		UpdatedAt:     now,
	}
}

// unmarshalInterval turns a stored row back into a belief.
//
// NOTE: the Restored flag is deliberately not set here. It is the store that
// decides a belief is restored, when it seeds one in, so that this function
// stays a plain reading of what is on disk.
func unmarshalInterval(row sqlc.LiquidityInterval) (PersistedInterval, error) {
	var entry PersistedInterval

	if len(row.Scid) != 8 {
		return entry, fmt.Errorf("expected an 8 byte channel id, got "+
			"%d bytes", len(row.Scid))
	}
	if len(row.FromNode) != route.VertexSize ||
		len(row.ToNode) != route.VertexSize {

		return entry, fmt.Errorf("expected %d byte node keys, got %d "+
			"and %d bytes", route.VertexSize, len(row.FromNode),
			len(row.ToNode))
	}

	// A negative amount cannot have been written by this store, so a row
	// carrying one has been tampered with or corrupted.
	if row.LowerOkMsat < 0 || row.UpperFailMsat < 0 ||
		row.EstimateMsat < 0 {

		return entry, fmt.Errorf("liquidity interval holds a negative " +
			"amount")
	}

	key := IntervalKey{
		ChanID: binary.BigEndian.Uint64(row.Scid),
	}
	copy(key.From[:], row.FromNode)
	copy(key.To[:], row.ToNode)

	mode := int8(intervalModeUnknown)
	switch {
	case row.LiquidityMode < 0:
		mode = intervalModeDepleted

	case row.LiquidityMode > 0:
		mode = intervalModeRich
	}

	confidence := float64(row.ConfidencePpm) / intervalConfidenceScale

	return PersistedInterval{
		Key: key,
		Interval: LiquidityInterval{
			LowerOK:    lnwire.MilliSatoshi(row.LowerOkMsat),
			UpperFail:  lnwire.MilliSatoshi(row.UpperFailMsat),
			Estimate:   lnwire.MilliSatoshi(row.EstimateMsat),
			Confidence: min(max(confidence, 0), 1),
			Successes:  boundedCount(row.Successes),
			Failures:   boundedCount(row.Failures),
			Mode:       mode,
			Known:      true,
		},
	}, nil
}

// boundedCount converts a stored observation count back into the counter type,
// clamping it into range.
func boundedCount(count int64) uint32 {
	if count <= 0 {
		return 0
	}
	if count > math.MaxUint32 {
		return math.MaxUint32
	}

	return uint32(count)
}
