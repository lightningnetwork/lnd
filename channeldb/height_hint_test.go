package channeldb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

func initHintCache(t *testing.T) *HeightHintCache {
	t.Helper()

	defaultCfg := CacheConfig{
		QueryDisable: false,
	}

	return initHintCacheWithConfig(t, defaultCfg)
}

func initHintCacheWithConfig(t *testing.T, cfg CacheConfig) *HeightHintCache {
	t.Helper()

	db := OpenForTesting(t, t.TempDir())

	hintCache, err := NewHeightHintCache(cfg, db.Backend)
	require.NoError(t, err, "unable to create hint cache")

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
	unknownConfRequest := chainntnfs.ConfRequest{TxID: unknownHash}
	_, err := hintCache.QueryConfirmHint(unknownConfRequest)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)

	// Now, we'll create some transaction hashes and commit them to the
	// cache in one transaction, each with its own origin.
	const height = 100
	const numHashes = 5
	confRequests := make([]chainntnfs.ConfRequest, numHashes)
	hints := make(chainntnfs.ConfirmHints)
	for i := 0; i < numHashes; i++ {
		var txHash chainhash.Hash
		copy(txHash[:], bytes.Repeat([]byte{byte(i + 1)}, 32))
		confRequests[i] = chainntnfs.ConfRequest{TxID: txHash}
		hints[confRequests[i]] = chainntnfs.HeightHint{
			Height: height,
			Origin: uint32(i + 1),
		}
	}

	err = hintCache.CommitConfirmHints(hints)
	require.NoError(t, err, "unable to add entries to cache")

	// With the hashes committed, we'll now query the cache to ensure that
	// we're able to properly retrieve the confirm hints.
	for _, confRequest := range confRequests {
		confirmHint, err := hintCache.QueryConfirmHint(confRequest)
		require.NoError(t, err)
		require.Equal(t, hints[confRequest], confirmHint)
	}

	// We'll also attempt to purge all of them in a single database
	// transaction.
	err = hintCache.PurgeConfirmHint(confRequests...)
	require.NoError(t, err)

	// Finally, we'll attempt to query for each hash. We should expect not
	// to find a hint for any of them.
	for _, confRequest := range confRequests {
		_, err := hintCache.QueryConfirmHint(confRequest)
		require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
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
	unknownSpendRequest := chainntnfs.SpendRequest{
		OutPoint: unknownOutPoint,
	}
	_, err := hintCache.QuerySpendHint(unknownSpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)

	// Now, we'll create some outpoints and commit them to the cache in one
	// transaction, each with its own origin.
	const height = 100
	const numOutpoints = 5
	spendRequests := make([]chainntnfs.SpendRequest, numOutpoints)
	hints := make(chainntnfs.SpendHints)
	for i := uint32(0); i < numOutpoints; i++ {
		spendRequests[i] = chainntnfs.SpendRequest{
			OutPoint: wire.OutPoint{Index: i + 1},
		}
		hints[spendRequests[i]] = chainntnfs.HeightHint{
			Height: height,
			Origin: i + 1,
		}
	}

	err = hintCache.CommitSpendHints(hints)
	require.NoError(t, err, "unable to add entries to cache")

	// With the outpoints committed, we'll now query the cache to ensure
	// that we're able to properly retrieve the spend hints.
	for _, spendRequest := range spendRequests {
		spendHint, err := hintCache.QuerySpendHint(spendRequest)
		require.NoError(t, err)
		require.Equal(t, hints[spendRequest], spendHint)
	}

	// We'll also attempt to purge all of them in a single database
	// transaction.
	err = hintCache.PurgeSpendHint(spendRequests...)
	require.NoError(t, err)

	// Finally, we'll attempt to query for each outpoint. We should expect
	// not to find a hint for any of them.
	for _, spendRequest := range spendRequests {
		_, err = hintCache.QuerySpendHint(spendRequest)
		require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)
	}
}

// TestQueryDisable asserts querying for confirmation or spend hints always
// return height zero when QueryDisabled is set to true in the CacheConfig.
func TestQueryDisable(t *testing.T) {
	cfg := CacheConfig{
		QueryDisable: true,
	}

	hintCache := initHintCacheWithConfig(t, cfg)

	// Insert a new confirmation hint with a non-zero height.
	const confHeight = 100
	confRequest := chainntnfs.ConfRequest{
		TxID: chainhash.Hash{0x01, 0x02, 0x03},
	}
	err := hintCache.CommitConfirmHints(
		chainntnfs.ConfirmHints{
			confRequest: {Height: confHeight, Origin: 1},
		},
	)
	require.Nil(t, err)

	// Query for the confirmation hint, which should return zero.
	cachedConfHint, err := hintCache.QueryConfirmHint(confRequest)
	require.Nil(t, err)
	require.Equal(t, chainntnfs.HeightHint{}, cachedConfHint)

	// Insert a new spend hint with a non-zero height.
	const spendHeight = 200
	spendRequest := chainntnfs.SpendRequest{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x4, 0x05, 0x06},
			Index: 42,
		},
	}
	err = hintCache.CommitSpendHints(
		chainntnfs.SpendHints{
			spendRequest: {Height: spendHeight, Origin: 1},
		},
	)
	require.Nil(t, err)

	// Query for the spend hint, which should return zero.
	cachedSpendHint, err := hintCache.QuerySpendHint(spendRequest)
	require.Nil(t, err)
	require.Equal(t, chainntnfs.HeightHint{}, cachedSpendHint)
}

// TestHeightHintEncoding asserts that a hint written before origins were
// persisted decodes with an unknown origin, and that a new hint still begins
// with its height so that an older binary reading only four bytes recovers
// it.
func TestHeightHintEncoding(t *testing.T) {
	t.Parallel()

	hintCache := initHintCache(t)
	confRequest := chainntnfs.ConfRequest{
		TxID: chainhash.Hash{0x07},
	}
	key, err := confHintKey(&confRequest)
	require.NoError(t, err)

	// Write a legacy value holding only the height.
	var legacy bytes.Buffer
	require.NoError(t, WriteElement(&legacy, uint32(123)))
	err = kvdb.Update(hintCache.db, func(tx kvdb.RwTx) error {
		return tx.ReadWriteBucket(confirmHintBucket).Put(
			key, legacy.Bytes(),
		)
	}, func() {})
	require.NoError(t, err)

	hint, err := hintCache.QueryConfirmHint(confRequest)
	require.NoError(t, err)
	require.Equal(t, chainntnfs.HeightHint{Height: 123}, hint)

	// A new value is the height followed by the origin.
	value, err := encodeHeightHint(chainntnfs.HeightHint{
		Height: 456,
		Origin: 400,
	})
	require.NoError(t, err)
	var height uint32
	require.NoError(t, ReadElement(bytes.NewReader(value), &height))
	require.Equal(t, uint32(456), height)

	decoded, err := decodeHeightHint(value)
	require.NoError(t, err)
	require.Equal(
		t, chainntnfs.HeightHint{Height: 456, Origin: 400}, decoded,
	)
}
