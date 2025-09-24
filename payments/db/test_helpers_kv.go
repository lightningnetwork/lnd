//go:build !test_db_sqlite && !test_db_postgres

package paymentsdb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// assertNoIndex checks that an index for the sequence number provided does not
// exist.
func assertNoIndex(t *testing.T, p DB, seqNr uint64) {
	t.Helper()

	// This is only used for the kv implementation.
	kvPaymentDB, ok := p.(*KVStore)
	if !ok {
		return
	}

	_, err := fetchPaymentIndexEntry(t, kvPaymentDB, seqNr)
	require.Equal(t, ErrNoSequenceNrIndex, err)
}

// assertPaymentIndex looks up the index for a payment in the db and checks
// that its payment hash matches the expected hash passed in.
func assertPaymentIndex(t *testing.T, p DB, expectedHash lntypes.Hash) {
	t.Helper()

	// This is only used for the kv implementation.
	kvPaymentDB, ok := p.(*KVStore)
	if !ok {
		return
	}

	// Lookup the payment so that we have its sequence number and check
	// that is has correctly been indexed in the payment indexes bucket.
	pmt, err := kvPaymentDB.FetchPayment(expectedHash)
	require.NoError(t, err)

	hash, err := fetchPaymentIndexEntry(t, kvPaymentDB, pmt.SequenceNum)
	require.NoError(t, err)
	require.Equal(t, expectedHash, *hash)
}

// fetchPaymentIndexEntry gets the payment hash for the sequence number provided
// from our payment indexes bucket.
func fetchPaymentIndexEntry(t *testing.T, p *KVStore,
	sequenceNumber uint64) (*lntypes.Hash, error) {

	t.Helper()

	var hash lntypes.Hash

	if err := kvdb.View(p.db, func(tx walletdb.ReadTx) error {
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
