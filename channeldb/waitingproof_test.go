package channeldb

import (
	"errors"
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestWaitingProofStore tests add/get/remove functions of the waiting proof
// storage.
func TestWaitingProofStore(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "failed to make test database")

	proof1 := NewWaitingProof(true, &lnwire.AnnounceSignatures1{
		NodeSignature:    wireSig,
		BitcoinSignature: wireSig,
		ExtraOpaqueData:  make([]byte, 0),
	})

	store, err := NewWaitingProofStore(db)
	if err != nil {
		t.Fatalf("unable to create the waiting proofs storage: %v",
			err)
	}

	if err := store.Add(proof1); err != nil {
		t.Fatalf("unable add proof to storage: %v", err)
	}

	proof2, err := store.Get(proof1.Key())
	require.NoError(t, err, "unable retrieve proof from storage")
	if !reflect.DeepEqual(proof1, proof2) {
		t.Fatalf("wrong proof retrieved: expected %v, got %v",
			spew.Sdump(proof1), spew.Sdump(proof2))
	}

	if _, err := store.Get(proof1.OppositeKey()); err != ErrWaitingProofNotFound {
		t.Fatalf("proof shouldn't be found: %v", err)
	}

	if err := store.Remove(proof1.Key()); err != nil {
		t.Fatalf("unable remove proof from storage: %v", err)
	}

	if err := store.ForAll(func(proof *WaitingProof) error {
		return errors.New("storage should be empty")
	}, func() {}); err != nil && err != ErrWaitingProofNotFound {
		t.Fatal(err)
	}
}
