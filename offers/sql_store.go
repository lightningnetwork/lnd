package offers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// SQLOfferQueries is the interface that defines the set of operations that can
// be executed against the offers SQL database.
type SQLOfferQueries interface {
	// InsertOffer inserts an offer row and returns its primary key. It
	// fails with a unique constraint violation when the offer hash is
	// already stored.
	InsertOffer(ctx context.Context,
		arg sqlc.InsertOfferParams) (int64, error)

	// GetOfferByHash returns the offer row with the given offer hash, or
	// sql.ErrNoRows when no row has it.
	GetOfferByHash(ctx context.Context,
		offerHash []byte) (sqlc.Offer, error)
}

// BatchedSQLOfferQueries combines the offer queries interface with batched
// transaction execution.
type BatchedSQLOfferQueries interface {
	SQLOfferQueries

	sqldb.BatchedTx[SQLOfferQueries]
}

// SQLStore is the SQL-backed implementation of the Store interface.
type SQLStore struct {
	db    BatchedSQLOfferQueries
	clock clock.Clock
}

// A compile-time check that SQLStore implements Store.
var _ Store = (*SQLStore)(nil)

// NewSQLStore creates a new SQL-backed offer store.
func NewSQLStore(db BatchedSQLOfferQueries, clock clock.Clock) *SQLStore {
	return &SQLStore{
		db:    db,
		clock: clock,
	}
}

// InsertOffer persists a new offer and returns its database ID. The store sets
// the creation time from its clock, and a new offer is always enabled. On
// success it also sets ID and CreatedAt on offer to the stored values. It
// returns ErrOfferExists if an offer with the same offer hash is already
// stored.
func (s *SQLStore) InsertOffer(ctx context.Context, offer *Offer) (int64,
	error) {

	// Postgres keeps microseconds, so drop the rest. The time on offer
	// then equals the stored time on every backend.
	createdAt := s.clock.Now().UTC().Truncate(time.Microsecond)

	var id int64

	err := s.db.ExecTx(
		ctx, sqldb.WriteTxOpt(),
		func(q SQLOfferQueries) error {
			var err error
			id, err = q.InsertOffer(ctx, sqlc.InsertOfferParams{
				Hash:      offer.Hash[:],
				Encoded:   offer.Encoded,
				CreatedAt: createdAt,
			})

			return err
		},
		sqldb.NoOpReset,
	)
	if err != nil {
		var uniqueErr *sqldb.ErrSQLUniqueConstraintViolation
		if errors.As(sqldb.MapSQLError(err), &uniqueErr) {
			return 0, ErrOfferExists
		}

		return 0, fmt.Errorf("insert offer: %w", err)
	}

	offer.ID = id
	offer.CreatedAt = createdAt

	return id, nil
}

// GetOfferByHash retrieves an offer by its 32-byte offer hash. It returns
// ErrOfferNotFound if no offer has that hash.
func (s *SQLStore) GetOfferByHash(ctx context.Context, offerHash [32]byte) (
	*Offer, error) {

	var offer *Offer

	err := s.db.ExecTx(
		ctx, sqldb.ReadTxOpt(),
		func(q SQLOfferQueries) error {
			row, err := q.GetOfferByHash(ctx, offerHash[:])
			if err != nil {
				return err
			}

			offer, err = marshalOffer(row)

			return err
		},
		sqldb.NoOpReset,
	)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrOfferNotFound

	case err != nil:
		return nil, fmt.Errorf("get offer by offer hash: %w", err)
	}

	return offer, nil
}

// marshalOffer converts a sqlc.Offer row to our domain Offer type. It returns
// an error if the stored hash is not 32 bytes long, which means the row is
// corrupt.
func marshalOffer(row sqlc.Offer) (*Offer, error) {
	hash, err := lntypes.MakeHash(row.Hash)
	if err != nil {
		return nil, fmt.Errorf("offer %d: %w", row.ID, err)
	}

	return &Offer{
		ID:         row.ID,
		Hash:       hash,
		Encoded:    row.Encoded,
		IsDisabled: row.IsDisabled,
		CreatedAt:  row.CreatedAt,
	}, nil
}
