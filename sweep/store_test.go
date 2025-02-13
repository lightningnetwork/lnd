package sweep

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// TestStore asserts that the store persists the presented data to disk and is
// able to retrieve it again.
func TestStore(t *testing.T) {
	// Create new store.
	cdb, err := channeldb.MakeTestDB(t)
	require.NoError(t, err)

	var chain chainhash.Hash
	store, err := NewSweeperStore(cdb, &chain)
	require.NoError(t, err)

	// Notify publication of tx1
	tx1 := wire.MsgTx{}
	tx1.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 1,
		},
	})

	tr1 := &TxRecord{
		Txid: tx1.TxHash(),
	}

	err = store.StoreTx(tr1)
	require.NoError(t, err)

	// Notify publication of tx2
	tx2 := wire.MsgTx{}
	tx2.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 2,
		},
	})

	tr2 := &TxRecord{
		Txid: tx2.TxHash(),
	}

	err = store.StoreTx(tr2)
	require.NoError(t, err)

	// Recreate the sweeper store
	store, err = NewSweeperStore(cdb, &chain)
	require.NoError(t, err)

	// Assert that both txes are recognized as our own.
	ours := store.IsOurTx(tx1.TxHash())
	require.True(t, ours, "expected tx to be ours")

	ours = store.IsOurTx(tx2.TxHash())
	require.True(t, ours, "expected tx to be ours")

	// An different hash should be reported as not being ours.
	var unknownHash chainhash.Hash
	ours = store.IsOurTx(unknownHash)
	require.False(t, ours, "expected tx to not be ours")

	txns, err := store.ListSweeps()
	require.NoError(t, err, "unexpected error")

	// Create a map containing the sweeps we expect to be returned by list
	// sweeps.
	expected := map[chainhash.Hash]bool{
		tx1.TxHash(): true,
		tx2.TxHash(): true,
	}
	require.Len(t, txns, len(expected))

	for _, tx := range txns {
		_, ok := expected[tx]
		require.Truef(t, ok, "unexpected txid returned: %v", tx)
	}
}

// TestTxRecord asserts that the serializeTxRecord and deserializeTxRecord
// behave as expected.
func TestTxRecord(t *testing.T) {
	t.Parallel()

	// Create a testing record.
	//
	// NOTE: Txid is omitted because it is not serialized.
	tr := &TxRecord{
		FeeRate:   1000,
		Fee:       10000,
		Published: true,
	}

	var b bytes.Buffer

	// Assert we can serialize the record.
	err := serializeTxRecord(&b, tr)
	require.NoError(t, err)

	// Assert we can deserialize the record.
	result, err := deserializeTxRecord(&b)
	require.NoError(t, err)

	// Assert the deserialized record is equal to the original.
	require.Equal(t, tr, result)
}

// TestGetTx asserts that the GetTx method behaves as expected.
func TestGetTx(t *testing.T) {
	t.Parallel()

	cdb, err := channeldb.MakeTestDB(t)
	require.NoError(t, err)

	// Create a testing store.
	chain := chainhash.Hash{}
	store, err := NewSweeperStore(cdb, &chain)
	require.NoError(t, err)

	// Create a testing record.
	txid := chainhash.Hash{1, 2, 3}
	tr := &TxRecord{
		Txid:      txid,
		FeeRate:   1000,
		Fee:       10000,
		Published: true,
	}

	// Assert we can store this tx record.
	err = store.StoreTx(tr)
	require.NoError(t, err)

	// Assert we can query the tx record.
	result, err := store.GetTx(txid)
	require.NoError(t, err)
	require.Equal(t, tr, result)

	// Assert we get an error when querying a non-existing tx.
	_, err = store.GetTx(chainhash.Hash{4, 5, 6})
	require.ErrorIs(t, ErrTxNotFound, err)
}

// TestGetTxCompatible asserts that when there's old tx record data in the
// database it can be successfully queried.
func TestGetTxCompatible(t *testing.T) {
	t.Parallel()

	cdb, err := channeldb.MakeTestDB(t)
	require.NoError(t, err)

	// Create a testing store.
	chain := chainhash.Hash{}
	store, err := NewSweeperStore(cdb, &chain)
	require.NoError(t, err)

	// Create a testing txid.
	txid := chainhash.Hash{0, 1, 2, 3}

	// Create a record using the old format "hash -> empty byte slice".
	err = kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		txHashesBucket := tx.ReadWriteBucket(txHashesBucketKey)
		return txHashesBucket.Put(txid[:], []byte{})
	}, func() {})
	require.NoError(t, err)

	// Assert we can query the tx record.
	result, err := store.GetTx(txid)
	require.NoError(t, err)
	require.Equal(t, txid, result.Txid)

	// Assert the Published field is true.
	require.True(t, result.Published)
}

// TestDeleteTx asserts that the DeleteTx method behaves as expected.
func TestDeleteTx(t *testing.T) {
	t.Parallel()

	cdb, err := channeldb.MakeTestDB(t)
	require.NoError(t, err)

	// Create a testing store.
	chain := chainhash.Hash{}
	store, err := NewSweeperStore(cdb, &chain)
	require.NoError(t, err)

	// Create a testing record.
	txid := chainhash.Hash{1, 2, 3}
	tr := &TxRecord{
		Txid:      txid,
		FeeRate:   1000,
		Fee:       10000,
		Published: true,
	}

	// Assert we can store this tx record.
	err = store.StoreTx(tr)
	require.NoError(t, err)

	// Assert we can delete the tx record.
	err = store.DeleteTx(txid)
	require.NoError(t, err)

	// Query it again should give us an error.
	_, err = store.GetTx(txid)
	require.ErrorIs(t, ErrTxNotFound, err)

	// Assert deleting a non-existing tx doesn't return an error.
	err = store.DeleteTx(chainhash.Hash{4, 5, 6})
	require.NoError(t, err)
}
