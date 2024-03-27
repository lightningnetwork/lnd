package sweep

import (
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

	err = store.StoreTx(tx1.TxHash())
	if err != nil {
		t.Fatal(err)
	}

	// Notify publication of tx2
	tx2 := wire.MsgTx{}
	tx2.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 2,
		},
	})

	err = store.StoreTx(tx2.TxHash())
	if err != nil {
		t.Fatal(err)
	}

	// Recreate the sweeper store
	store, err = createStore()
	if err != nil {
		t.Fatal(err)
	}

	// Assert that both txes are recognized as our own.
	ours, err := store.IsOurTx(tx1.TxHash())
	if err != nil {
		t.Fatal(err)
	}
	if !ours {
		t.Fatal("expected tx to be ours")
	}

	ours, err = store.IsOurTx(tx2.TxHash())
	if err != nil {
		t.Fatal(err)
	}
	if !ours {
		t.Fatal("expected tx to be ours")
	}

	// An different hash should be reported as not being ours.
	var unknownHash chainhash.Hash
	ours, err = store.IsOurTx(unknownHash)
	if err != nil {
		t.Fatal(err)
	}
	if ours {
		t.Fatal("expected tx to be not ours")
	}

	txns, err := store.ListSweeps()
	require.NoError(t, err, "unexpected error")

	// Create a map containing the sweeps we expect to be returned by list
	// sweeps.
	expected := map[chainhash.Hash]bool{
		tx1.TxHash(): true,
		tx2.TxHash(): true,
	}

	if len(txns) != len(expected) {
		t.Fatalf("expected: %v sweeps, got: %v", len(expected),
			len(txns))
	}

	for _, tx := range txns {
		_, ok := expected[tx]
		if !ok {
			t.Fatalf("unexpected tx: %v", tx)
		}
	}
}
