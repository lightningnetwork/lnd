package offers

import (
	"crypto/sha256"
	"database/sql"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// testTime is the time the test clock reports. It has no sub-second part, so
// it survives the database round trip unchanged.
var testTime = time.Unix(1735689600, 0).UTC()

// testClock is the clock the tests give CreateOffer. It stands at testTime,
// like the clock of the test store.
var testClock = clock.NewTestClock(testTime)

// newTestSQLStore creates an offer store on the database that the build tags
// select.
func newTestSQLStore(t *testing.T) *SQLStore {
	t.Helper()

	db := newTestBaseDB(t)

	executor := sqldb.NewTransactionExecutor(
		db,
		func(tx *sql.Tx) SQLOfferQueries {
			return db.WithTx(tx)
		},
	)

	return NewSQLStore(executor, clock.NewTestClock(testTime))
}

// testOffer returns an offer with a synthetic encoding and offer hash.
func testOffer() *Offer {
	encoded := "lno1qgsqvgnwgcg35z6ee2h3yczraddm72xrfua" +
		"9uve2rlrm9deu7xyfzrcgqyqs"

	return &Offer{
		Hash:    sha256.Sum256([]byte(encoded)),
		Encoded: encoded,
	}
}

// TestInsertAndGetOffer verifies that an offer round-trips through insert and
// lookup by offer hash, and that the store sets the creation time.
func TestInsertAndGetOffer(t *testing.T) {
	t.Parallel()

	store := newTestSQLStore(t)
	ctx := t.Context()
	offer := testOffer()

	id, err := store.InsertOffer(ctx, offer)
	require.NoError(t, err)

	got, err := store.GetOfferByHash(ctx, offer.Hash)
	require.NoError(t, err)
	require.Equal(t, &Offer{
		ID:        id,
		Hash:      offer.Hash,
		Encoded:   offer.Encoded,
		CreatedAt: testTime,
	}, got)
}

// TestInsertDuplicateOffer verifies that a second offer with the same offer
// hash is rejected with ErrOfferExists.
func TestInsertDuplicateOffer(t *testing.T) {
	t.Parallel()

	store := newTestSQLStore(t)
	ctx := t.Context()
	offer := testOffer()

	_, err := store.InsertOffer(ctx, offer)
	require.NoError(t, err)

	_, err = store.InsertOffer(ctx, offer)
	require.ErrorIs(t, err, ErrOfferExists)
}

// TestGetUnknownOffer verifies that a lookup of an unknown offer hash returns
// ErrOfferNotFound.
func TestGetUnknownOffer(t *testing.T) {
	t.Parallel()

	store := newTestSQLStore(t)

	_, err := store.GetOfferByHash(t.Context(), [32]byte{1})
	require.ErrorIs(t, err, ErrOfferNotFound)
}
