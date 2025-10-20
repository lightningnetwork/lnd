//go:build !test_db_sqlite && !test_db_postgres

package paymentsdb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates an BBolt database for testing.
func NewTestDB(t *testing.T, opts ...OptionModifier) (DB, TestHarness) {
	backend, backendCleanup, err := kvdb.GetTestBackend(
		t.TempDir(), "paymentsDB",
	)
	require.NoError(t, err)

	t.Cleanup(backendCleanup)

	paymentDB, err := NewKVStore(backend, opts...)
	require.NoError(t, err)

	return paymentDB, &kvTestHarness{db: paymentDB}
}

// NewKVTestDB is a helper function that creates an BBolt database for testing
// and there is no need to convert the interface to the KVStore because for
// some unit tests we still need access to the kvdb interface.
func NewKVTestDB(t *testing.T, opts ...OptionModifier) *KVStore {
	backend, backendCleanup, err := kvdb.GetTestBackend(
		t.TempDir(), "kvPaymentDB",
	)
	require.NoError(t, err)

	t.Cleanup(backendCleanup)

	paymentDB, err := NewKVStore(backend, opts...)
	require.NoError(t, err)

	return paymentDB
}

// kvTestHarness is the KV-specific test harness implementation.
type kvTestHarness struct {
	db *KVStore
}

// AssertPaymentIndex looks up the index for a payment in the db and checks
// that its payment hash matches the expected hash passed in.
func (h *kvTestHarness) AssertPaymentIndex(t *testing.T,
	expectedHash lntypes.Hash) {

	t.Helper()

	ctx := t.Context()

	// Lookup the payment so that we have its sequence number and check
	// that it has correctly been indexed in the payment indexes bucket.
	pmt, err := h.db.FetchPayment(ctx, expectedHash)
	require.NoError(t, err)

	hash, err := h.fetchPaymentIndexEntry(t, pmt.SequenceNum)
	require.NoError(t, err)
	assert.Equal(t, expectedHash, *hash)
}

// AssertNoIndex checks that an index for the sequence number provided does not
// exist.
func (h *kvTestHarness) AssertNoIndex(t *testing.T, seqNr uint64) {
	t.Helper()

	_, err := h.fetchPaymentIndexEntry(t, seqNr)
	require.Equal(t, ErrNoSequenceNrIndex, err)
}

// fetchPaymentIndexEntry gets the payment hash for the sequence number
// provided from the payment indexes bucket.
func (h *kvTestHarness) fetchPaymentIndexEntry(t *testing.T,
	sequenceNumber uint64) (*lntypes.Hash, error) {

	t.Helper()

	var hash lntypes.Hash

	if err := kvdb.View(h.db.db, func(tx walletdb.ReadTx) error {
		indexBucket := tx.ReadBucket(paymentsIndexBucket)
		key := make([]byte, 8)
		byteOrder.PutUint64(key, sequenceNumber)

		indexValue := indexBucket.Get(key)
		if indexValue == nil {
			return ErrNoSequenceNrIndex
		}

		r := bytes.NewReader(indexValue)

		var err error
		hash, err = deserializePaymentIndex(r)

		return err
	}, func() {
		hash = lntypes.Hash{}
	}); err != nil {
		return nil, err
	}

	return &hash, nil
}
