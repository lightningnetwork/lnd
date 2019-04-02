package channeldb

import (
	"crypto/sha256"
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
)

// TestWitnessCacheSha256Retrieval tests that we're able to add and lookup new
// sha256 preimages to the witness cache.
func TestWitnessCacheSha256Retrieval(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	wCache := cdb.NewWitnessCache()

	// We'll be attempting to add then lookup two simple sha256 preimages
	// within this test.
	preimage1 := lntypes.Preimage(rev)
	preimage2 := lntypes.Preimage(key)

	preimages := []lntypes.Preimage{preimage1, preimage2}
	hashes := []lntypes.Hash{preimage1.Hash(), preimage2.Hash()}

	// First, we'll attempt to add the preimages to the database.
	err = wCache.AddSha256Witnesses(preimages...)
	if err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	// With the preimages stored, we'll now attempt to look them up.
	for i, hash := range hashes {
		preimage := preimages[i]

		// We should get back the *exact* same preimage as we originally
		// stored.
		dbPreimage, err := wCache.LookupSha256Witness(hash)
		if err != nil {
			t.Fatalf("unable to look up witness: %v", err)
		}

		if preimage != dbPreimage {
			t.Fatalf("witnesses don't match: expected %x, got %x",
				preimage[:], dbPreimage[:])
		}
	}
}

// TestWitnessCacheSha256Deletion tests that we're able to delete a single
// sha256 preimage, and also a class of witnesses from the cache.
func TestWitnessCacheSha256Deletion(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	wCache := cdb.NewWitnessCache()

	// We'll start by adding two preimages to the cache.
	preimage1 := lntypes.Preimage(key)
	hash1 := preimage1.Hash()

	preimage2 := lntypes.Preimage(rev)
	hash2 := preimage2.Hash()

	if err := wCache.AddSha256Witnesses(preimage1); err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	if err := wCache.AddSha256Witnesses(preimage2); err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	// We'll now delete the first preimage. If we attempt to look it up, we
	// should get ErrNoWitnesses.
	err = wCache.DeleteSha256Witness(hash1)
	if err != nil {
		t.Fatalf("unable to delete witness: %v", err)
	}
	_, err = wCache.LookupSha256Witness(hash1)
	if err != ErrNoWitnesses {
		t.Fatalf("expected ErrNoWitnesses instead got: %v", err)
	}

	// Next, we'll attempt to delete the entire witness class itself. When
	// we try to lookup the second preimage, we should again get
	// ErrNoWitnesses.
	if err := wCache.DeleteWitnessClass(Sha256HashWitness); err != nil {
		t.Fatalf("unable to delete witness class: %v", err)
	}
	_, err = wCache.LookupSha256Witness(hash2)
	if err != ErrNoWitnesses {
		t.Fatalf("expected ErrNoWitnesses instead got: %v", err)
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
	err = wCache.legacyAddWitnesses(234, key[:])
	if err != ErrUnknownWitnessType {
		t.Fatalf("expected ErrUnknownWitnessType, got %v", err)
	}
}

// TestAddSha256Witnesses tests that insertion using AddSha256Witnesses behaves
// identically to the insertion via the generalized interface.
func TestAddSha256Witnesses(t *testing.T) {
	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	wCache := cdb.NewWitnessCache()

	// We'll start by adding a witnesses to the cache using the generic
	// AddWitnesses method.
	witness1 := rev[:]
	preimage1 := lntypes.Preimage(rev)
	hash1 := preimage1.Hash()

	witness2 := key[:]
	preimage2 := lntypes.Preimage(key)
	hash2 := preimage2.Hash()

	var (
		witnesses = [][]byte{witness1, witness2}
		preimages = []lntypes.Preimage{preimage1, preimage2}
		hashes    = []lntypes.Hash{hash1, hash2}
	)

	err = wCache.legacyAddWitnesses(Sha256HashWitness, witnesses...)
	if err != nil {
		t.Fatalf("unable to add witness: %v", err)
	}

	for i, hash := range hashes {
		preimage := preimages[i]

		dbPreimage, err := wCache.LookupSha256Witness(hash)
		if err != nil {
			t.Fatalf("unable to lookup witness: %v", err)
		}

		// Assert that the retrieved witness matches the original.
		if dbPreimage != preimage {
			t.Fatalf("retrieved witness mismatch, want: %x, "+
				"got: %x", preimage, dbPreimage)
		}

		// We'll now delete the witness, as we'll be reinserting it
		// using the specialized AddSha256Witnesses method.
		err = wCache.DeleteSha256Witness(hash)
		if err != nil {
			t.Fatalf("unable to delete witness: %v", err)
		}
	}

	// Now, add the same witnesses using the type-safe interface for
	// lntypes.Preimages..
	err = wCache.AddSha256Witnesses(preimages...)
	if err != nil {
		t.Fatalf("unable to add sha256 preimage: %v", err)
	}

	// Finally, iterate over the keys and assert that the returned witnesses
	// match the original witnesses. This asserts that the specialized
	// insertion method behaves identically to the generalized interface.
	for i, hash := range hashes {
		preimage := preimages[i]

		dbPreimage, err := wCache.LookupSha256Witness(hash)
		if err != nil {
			t.Fatalf("unable to lookup witness: %v", err)
		}

		// Assert that the retrieved witness matches the original.
		if dbPreimage != preimage {
			t.Fatalf("retrieved witness mismatch, want: %x, "+
				"got: %x", preimage, dbPreimage)
		}
	}
}

// legacyAddWitnesses adds a batch of new witnesses of wType to the witness
// cache. The type of the witness will be used to map each witness to the key
// that will be used to look it up. All witnesses should be of the same
// WitnessType.
//
// NOTE: Previously this method exposed a generic interface for adding
// witnesses, which has since been deprecated in favor of a strongly typed
// interface for each witness class. We keep this method around to assert the
// correctness of specialized witness adding methods.
func (w *WitnessCache) legacyAddWitnesses(wType WitnessType,
	witnesses ...[]byte) error {

	// Optimistically compute the witness keys before attempting to start
	// the db transaction.
	entries := make([]witnessEntry, 0, len(witnesses))
	for _, witness := range witnesses {
		// Map each witness to its key by applying the appropriate
		// transformation for the given witness type.
		switch wType {
		case Sha256HashWitness:
			key := sha256.Sum256(witness)
			entries = append(entries, witnessEntry{
				key:     key[:],
				witness: witness,
			})
		default:
			return ErrUnknownWitnessType
		}
	}

	return w.addWitnessEntries(wType, entries)
}
