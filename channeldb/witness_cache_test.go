package channeldb

import (
	"crypto/sha256"
	"reflect"
	"testing"
)

// TestWitnessCacheRetrieval tests that we're able to add and lookup new
// witnesses to the witness cache.
func TestWitnessCacheRetrieval(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	wCache := cdb.NewWitnessCache()

	// We'll be attempting to add then lookup a d simple hash witness
	// within this test.
	witness := rev[:]
	witnessKey := sha256.Sum256(witness)

	// First, we'll attempt to add the witness to the database.
	err = wCache.AddWitness(Sha256HashWitness, witness, chanID)
	if err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	// With the witness stored, we'll now attempt to look it up. We should
	// get back the *exact* same witness as we originally stored.
	dbWitness, err := wCache.LookupWitness(Sha256HashWitness, witnessKey[:], chanID)
	if err != nil {
		t.Fatalf("unable to look up witness: %v", err)
	}

	if !reflect.DeepEqual(witness, dbWitness[:]) {
		t.Fatalf("witnesses don't match: expected %x, got %x",
			witness[:], dbWitness[:])
	}
}

// TestWitnessCacheDeletion tests that we're able to delete a witnesses for a single
// channel and also a class of witnesses from the cache.
func TestWitnessCacheDeletion(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	wCache := cdb.NewWitnessCache()

	// We'll start by adding two witnesses to the cache.
	witness1 := rev[:]
	witness1Key := sha256.Sum256(witness1)
	if err := wCache.AddWitness(Sha256HashWitness, witness1, chanID); err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	witness2 := key[:]
	witness2Key := sha256.Sum256(witness2)
	if err := wCache.AddWitness(Sha256HashWitness, witness2, chanID2); err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	// We'll now call DeleteChannel to delete all witnesses for chanID and
	// try to lookup the witness again.
	if err := wCache.DeleteChannel(Sha256HashWitness, chanID); err != nil {
		t.Fatalf("unable to delete channel's witnesses: %v", err)
	}

	// Lookup the deleted witness
	_, err = wCache.LookupWitness(Sha256HashWitness, witness1Key[:], chanID)
	if err != ErrNoWitnesses {
		t.Fatalf("expected ErrNoWitnesses instead got: %v", err)
	}

	// Next, we'll attempt to delete the entire witness class itself. When
	// we try to lookup the second witness, we should again get
	// ErrNoWitnesses.
	if err := wCache.DeleteWitnessClass(Sha256HashWitness); err != nil {
		t.Fatalf("unable to delete witness class: %v", err)
	}
	_, err = wCache.LookupWitness(Sha256HashWitness, witness2Key[:], chanID2)
	if err != ErrNoWitnesses {
		t.Fatalf("expected ErrNoWitnesses instead got: %v", err)
	}
}

// TestFinalizeChannelState tests that we're able to change a channel's ChannelState
// to FINALIZED.
func TestFinalizeChannelState(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	wCache := cdb.NewWitnessCache()

	witness1 := rev[:]

	// Add the witness to the cache (this inits the channel with a WAITING
	// ChannelState).
	if err := wCache.AddWitness(Sha256HashWitness, witness1, chanID); err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	// Update chanID's ChannelState.
	if err := wCache.FinalizeChannelState(Sha256HashWitness, chanID); err != nil {
		t.Fatalf("unable to finalize channel: %v", err)
	}

	// Call FetchAllChannelStates and find our match.
	chanIDs, chanStates, err := wCache.FetchAllChannelStates(Sha256HashWitness)
	if err != nil {
		t.Fatalf("unable to fetch channels: %v", err)
	}

	var found bool
	for i := 0; i < len(chanIDs); i++ {
		if chanIDs[i].ToUint64() == chanID.ToUint64() {
			if chanStates[i] == FINALIZED {
				found = true
				break;
			}
		}
	}

	if !found {
		t.Fatalf("unable to update channel's state to finalized")
	}
}

// TestWitnessChannelRetrieval tests that the WitnessCache's FetchAllChannelStates
// function works as expected.
func TestWitnessChannelRetrieval(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	wCache := cdb.NewWitnessCache()

	// We'll start by adding two witnesses to the cache.
	witness1 := rev[:]
	if err := wCache.AddWitness(Sha256HashWitness, witness1, chanID); err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	witness2 := key[:]
	if err := wCache.AddWitness(Sha256HashWitness, witness2, chanID2); err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	// Update chanID's ChannelState.
	if err := wCache.FinalizeChannelState(Sha256HashWitness, chanID); err != nil {
		t.Fatalf("unable to finalize channel: %v", err)
	}

	// Call FetchAllChannelStates and find our match.
	chanIDs, chanStates, err := wCache.FetchAllChannelStates(Sha256HashWitness)
	if err != nil {
		t.Fatalf("unable to fetch channels: %v", err)
	}

	success := 0
	for i := 0; i < len(chanIDs); i++ {
		chanInt := chanIDs[i].ToUint64()
		if chanInt == chanID.ToUint64() {
			if chanStates[i] == WAITING {
				success++
			}
		} else if chanInt == chanID2.ToUint64() {
			if chanStates[i] == FINALIZED {
				success++
			}
		}
	}

	if success != 2 {
		t.Fatalf("unable to fetch channel states correctly")
	}
}

// TestWitnessCacheUnknownWitness tests that we get an error if we attempt to
// query/add/delete an unknown witness.
func TestWitnessCacheUnknownWitness(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	wCache := cdb.NewWitnessCache()

	// We'll attempt to add a new, undefined witness type to the database.
	// We should get an error.
	err = wCache.AddWitness(234, key[:], chanID)
	if err != ErrUnknownWitnessType {
		t.Fatalf("expected ErrUnknownWitnessType, got %v", err)
	}
}
