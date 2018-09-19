package chainntnfs

import (
	"bytes"
	"io/ioutil"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
)

func initHintCache(t *testing.T, disable bool) *HeightHintCache {
	t.Helper()

	tempDir, err := ioutil.TempDir("", "kek")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}
	db, err := channeldb.Open(tempDir)
	if err != nil {
		t.Fatalf("unable to create db: %v", err)
	}
	hintCache, err := NewHeightHintCache(db, disable)
	if err != nil {
		t.Fatalf("unable to create hint cache: %v", err)
	}

	return hintCache
}

// TestHeightHintCacheConfirms ensures that the height hint cache properly
// caches confirm hints for transactions.
func TestHeightHintCacheConfirms(t *testing.T) {
	t.Parallel()

	hintCache := initHintCache(t, false)

	// Querying for a transaction hash not found within the cache should
	// return an error indication so.
	var unknownHash chainhash.Hash
	_, err := hintCache.QueryConfirmHint(unknownHash)
	if err != ErrConfirmHintNotFound {
		t.Fatalf("expected ErrConfirmHintNotFound, got: %v", err)
	}

	// Now, we'll create some transaction hashes and commit them to the
	// cache with the same confirm hint.
	const height = 100
	const numHashes = 5
	txHashes := make([]chainhash.Hash, numHashes)
	for i := 0; i < numHashes; i++ {
		var txHash chainhash.Hash
		copy(txHash[:], bytes.Repeat([]byte{byte(i)}, 32))
		txHashes[i] = txHash
	}

	if err := hintCache.CommitConfirmHint(height, txHashes...); err != nil {
		t.Fatalf("unable to add entries to cache: %v", err)
	}

	// With the hashes committed, we'll now query the cache to ensure that
	// we're able to properly retrieve the confirm hints.
	for _, txHash := range txHashes {
		confirmHint, err := hintCache.QueryConfirmHint(txHash)
		if err != nil {
			t.Fatalf("unable to query for hint: %v", err)
		}
		if confirmHint != height {
			t.Fatalf("expected confirm hint %d, got %d", height,
				confirmHint)
		}
	}

	// We'll also attempt to purge all of them in a single database
	// transaction.
	if err := hintCache.PurgeConfirmHint(txHashes...); err != nil {
		t.Fatalf("unable to remove confirm hints: %v", err)
	}

	// Finally, we'll attempt to query for each hash. We should expect not
	// to find a hint for any of them.
	for _, txHash := range txHashes {
		_, err := hintCache.QueryConfirmHint(txHash)
		if err != ErrConfirmHintNotFound {
			t.Fatalf("expected ErrConfirmHintNotFound, got :%v", err)
		}
	}
}

// TestHeightHintCacheSpends ensures that the height hint cache properly caches
// spend hints for outpoints.
func TestHeightHintCacheSpends(t *testing.T) {
	t.Parallel()

	hintCache := initHintCache(t, false)

	// Querying for an outpoint not found within the cache should return an
	// error indication so.
	var unknownOutPoint wire.OutPoint
	_, err := hintCache.QuerySpendHint(unknownOutPoint)
	if err != ErrSpendHintNotFound {
		t.Fatalf("expected ErrSpendHintNotFound, got: %v", err)
	}

	// Now, we'll create some outpoints and commit them to the cache with
	// the same spend hint.
	const height = 100
	const numOutpoints = 5
	var txHash chainhash.Hash
	copy(txHash[:], bytes.Repeat([]byte{0xFF}, 32))
	outpoints := make([]wire.OutPoint, numOutpoints)
	for i := uint32(0); i < numOutpoints; i++ {
		outpoints[i] = wire.OutPoint{Hash: txHash, Index: i}
	}

	if err := hintCache.CommitSpendHint(height, outpoints...); err != nil {
		t.Fatalf("unable to add entry to cache: %v", err)
	}

	// With the outpoints committed, we'll now query the cache to ensure
	// that we're able to properly retrieve the confirm hints.
	for _, op := range outpoints {
		spendHint, err := hintCache.QuerySpendHint(op)
		if err != nil {
			t.Fatalf("unable to query for hint: %v", err)
		}
		if spendHint != height {
			t.Fatalf("expected spend hint %d, got %d", height,
				spendHint)
		}
	}

	// We'll also attempt to purge all of them in a single database
	// transaction.
	if err := hintCache.PurgeSpendHint(outpoints...); err != nil {
		t.Fatalf("unable to remove spend hint: %v", err)
	}

	// Finally, we'll attempt to query for each outpoint. We should expect
	// not to find a hint for any of them.
	for _, op := range outpoints {
		_, err = hintCache.QuerySpendHint(op)
		if err != ErrSpendHintNotFound {
			t.Fatalf("expected ErrSpendHintNotFound, got: %v", err)
		}
	}
}

// TestHeightHintCacheDisabled asserts that a disabled height hint cache never
// returns spend or confirm hints that are committed.
func TestHeightHintCacheDisabled(t *testing.T) {
	t.Parallel()

	const height uint32 = 100

	// Create a disabled height hint cache.
	hintCache := initHintCache(t, true)

	// Querying a disabled cache w/ no spend hint should return not found.
	var outpoint wire.OutPoint
	_, err := hintCache.QuerySpendHint(outpoint)
	if err != ErrSpendHintNotFound {
		t.Fatalf("expected ErrSpendHintNotFound, got: %v", err)
	}

	// Commit a spend hint to the disabled cache, which should be a noop.
	if err := hintCache.CommitSpendHint(height, outpoint); err != nil {
		t.Fatalf("unable to commit spend hint: %v", err)
	}

	// Querying a disabled cache after commit noop should return not found.
	_, err = hintCache.QuerySpendHint(outpoint)
	if err != ErrSpendHintNotFound {
		t.Fatalf("expected ErrSpendHintNotFound, got: %v", err)
	}

	// Reenable the cache, this time actually committing a spend hint.
	hintCache.disabled = false
	if err := hintCache.CommitSpendHint(height, outpoint); err != nil {
		t.Fatalf("unable to commit spend hint: %v", err)
	}

	// Disable the cache again, spend hint should not be found.
	hintCache.disabled = true
	_, err = hintCache.QuerySpendHint(outpoint)
	if err != ErrSpendHintNotFound {
		t.Fatalf("expected ErrSpendHintNotFound, got: %v", err)
	}

	// Querying a disabled cache w/ no conf hint should return not found.
	var txid chainhash.Hash
	_, err = hintCache.QueryConfirmHint(txid)
	if err != ErrConfirmHintNotFound {
		t.Fatalf("expected ErrConfirmHintNotFound, got: %v", err)
	}

	// Commit a conf hint to the disabled cache, which should be a noop.
	if err := hintCache.CommitConfirmHint(height, txid); err != nil {
		t.Fatalf("unable to commit spend hint: %v", err)
	}

	// Querying a disabled cache after commit noop should return not found.
	_, err = hintCache.QueryConfirmHint(txid)
	if err != ErrConfirmHintNotFound {
		t.Fatalf("expected ErrConfirmHintNotFound, got: %v", err)
	}

	// Reenable the cache, this time actually committing a conf hint.
	hintCache.disabled = false
	if err := hintCache.CommitConfirmHint(height, txid); err != nil {
		t.Fatalf("unable to commit spend hint: %v", err)
	}

	// Disable the cache again, conf hint should not be found.
	hintCache.disabled = true
	_, err = hintCache.QueryConfirmHint(txid)
	if err != ErrConfirmHintNotFound {
		t.Fatalf("expected ErrConfirmHintNotFound, got: %v", err)
	}
}
