package channeldb

import (
	"testing"

	"reflect"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
)

// TestWaitingProofStore tests add/get/remove functions of the waiting proof
// storage.
func TestWaitingProofStore(t *testing.T) {
	t.Parallel()

	db, cleanup, err := makeTestDB()
	if err != nil {

	}
	defer cleanup()

	proof1 := NewWaitingProof(true, &lnwire.AnnounceSignatures{
		NodeSignature:    wireSig,
		BitcoinSignature: wireSig,
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
	if err != nil {
		t.Fatalf("unable retrieve proof from storage: %v", err)
	}
	if !reflect.DeepEqual(proof1, proof2) {
		t.Fatal("wrong proof retrieved")
	}

	if _, err := store.Get(proof1.OppositeKey()); err != ErrWaitingProofNotFound {
		t.Fatalf("proof shouldn't be found: %v", err)
	}

	if err := store.Remove(proof1.Key()); err != nil {
		t.Fatalf("unable remove proof from storage: %v", err)
	}

	if err := store.ForAll(func(proof *WaitingProof) error {
		return errors.New("storage should be empty")
	}); err != nil && err != ErrWaitingProofNotFound {
		t.Fatal(err)
	}
}
