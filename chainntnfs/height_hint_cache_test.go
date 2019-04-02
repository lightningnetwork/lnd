package chainntnfs

import (
	"bytes"
	"io/ioutil"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
)

func initHintCache(t *testing.T) *HeightHintCache {
	t.Helper()

	tempDir, err := ioutil.TempDir("", "kek")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}
	db, err := channeldb.Open(tempDir)
	if err != nil {
		t.Fatalf("unable to create db: %v", err)
	}
	hintCache, err := NewHeightHintCache(db)
	if err != nil {
		t.Fatalf("unable to create hint cache: %v", err)
	}

	return hintCache
}

// TestHeightHintCacheConfirms ensures that the height hint cache properly
// caches confirm hints for transactions.
func TestHeightHintCacheConfirms(t *testing.T) {
	t.Parallel()

	hintCache := initHintCache(t)

	// Querying for a transaction hash not found within the cache should
	// return an error indication so.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x01}, 32))
	unknownConfRequest := ConfRequest{TxID: unknownHash}
	_, err := hintCache.QueryConfirmHint(unknownConfRequest)
	if err != ErrConfirmHintNotFound {
		t.Fatalf("expected ErrConfirmHintNotFound, got: %v", err)
	}

	// Now, we'll create some transaction hashes and commit them to the
	// cache with the same confirm hint.
	const height = 100
	const numHashes = 5
	confRequests := make([]ConfRequest, numHashes)
	for i := 0; i < numHashes; i++ {
		var txHash chainhash.Hash
		copy(txHash[:], bytes.Repeat([]byte{byte(i + 1)}, 32))
		confRequests[i] = ConfRequest{TxID: txHash}
	}

	err = hintCache.CommitConfirmHint(height, confRequests...)
	if err != nil {
		t.Fatalf("unable to add entries to cache: %v", err)
	}

	// With the hashes committed, we'll now query the cache to ensure that
	// we're able to properly retrieve the confirm hints.
	for _, confRequest := range confRequests {
		confirmHint, err := hintCache.QueryConfirmHint(confRequest)
		if err != nil {
			t.Fatalf("unable to query for hint of %v: %v", confRequest, err)
		}
		if confirmHint != height {
			t.Fatalf("expected confirm hint %d, got %d", height,
				confirmHint)
		}
	}

	// We'll also attempt to purge all of them in a single database
	// transaction.
	if err := hintCache.PurgeConfirmHint(confRequests...); err != nil {
		t.Fatalf("unable to remove confirm hints: %v", err)
	}

	// Finally, we'll attempt to query for each hash. We should expect not
	// to find a hint for any of them.
	for _, confRequest := range confRequests {
		_, err := hintCache.QueryConfirmHint(confRequest)
		if err != ErrConfirmHintNotFound {
			t.Fatalf("expected ErrConfirmHintNotFound, got :%v", err)
		}
	}
}

// TestHeightHintCacheSpends ensures that the height hint cache properly caches
// spend hints for outpoints.
func TestHeightHintCacheSpends(t *testing.T) {
	t.Parallel()

	hintCache := initHintCache(t)

	// Querying for an outpoint not found within the cache should return an
	// error indication so.
	unknownOutPoint := wire.OutPoint{Index: 1}
	unknownSpendRequest := SpendRequest{OutPoint: unknownOutPoint}
	_, err := hintCache.QuerySpendHint(unknownSpendRequest)
	if err != ErrSpendHintNotFound {
		t.Fatalf("expected ErrSpendHintNotFound, got: %v", err)
	}

	// Now, we'll create some outpoints and commit them to the cache with
	// the same spend hint.
	const height = 100
	const numOutpoints = 5
	spendRequests := make([]SpendRequest, numOutpoints)
	for i := uint32(0); i < numOutpoints; i++ {
		spendRequests[i] = SpendRequest{
			OutPoint: wire.OutPoint{Index: i + 1},
		}
	}

	err = hintCache.CommitSpendHint(height, spendRequests...)
	if err != nil {
		t.Fatalf("unable to add entries to cache: %v", err)
	}

	// With the outpoints committed, we'll now query the cache to ensure
	// that we're able to properly retrieve the confirm hints.
	for _, spendRequest := range spendRequests {
		spendHint, err := hintCache.QuerySpendHint(spendRequest)
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
	if err := hintCache.PurgeSpendHint(spendRequests...); err != nil {
		t.Fatalf("unable to remove spend hint: %v", err)
	}

	// Finally, we'll attempt to query for each outpoint. We should expect
	// not to find a hint for any of them.
	for _, spendRequest := range spendRequests {
		_, err = hintCache.QuerySpendHint(spendRequest)
		if err != ErrSpendHintNotFound {
			t.Fatalf("expected ErrSpendHintNotFound, got: %v", err)
		}
	}
}
