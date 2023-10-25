package sweep

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

// TestStore asserts that the store persists the presented data to disk and is
// able to retrieve it again.
func TestStore(t *testing.T) {
	t.Run("bolt", func(t *testing.T) {

		// Create new store.
		cdb, err := channeldb.MakeTestDB(t)
		if err != nil {
			t.Fatalf("unable to open channel db: %v", err)
		}

		testStore(t, func() (SweeperStore, error) {
			var chain chainhash.Hash
			return NewSweeperStore(cdb, &chain)
		})
	})
	t.Run("mock", func(t *testing.T) {
		store := NewMockSweeperStore()

		testStore(t, func() (SweeperStore, error) {
			// Return same store, because the mock has no real
			// persistence.
			return store, nil
		})
	})
}

func testStore(t *testing.T, createStore func() (SweeperStore, error)) {
	store, err := createStore()
	if err != nil {
		t.Fatal(err)
	}

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
	store, err = createStore()
	if err != nil {
		t.Fatal(err)
	}

	// Assert that both txes are recognized as our own.
	ours, err := store.IsOurTx(tx1.TxHash())
	require.NoError(t, err)
	require.True(t, ours, "expected tx to be ours")

	ours, err = store.IsOurTx(tx2.TxHash())
	require.NoError(t, err)
	require.True(t, ours, "expected tx to be ours")

	// An different hash should be reported as not being ours.
	var unknownHash chainhash.Hash
	ours, err = store.IsOurTx(unknownHash)
	require.NoError(t, err)
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
