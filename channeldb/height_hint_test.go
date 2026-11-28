package channeldb

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/mock"
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
	_, err := hintCache.QueryConfirmHintWithOrigin(unknownConfRequest)
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
		confirmHint, err := hintCache.QueryConfirmHintWithOrigin(
			confRequest,
		)
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
		_, err := hintCache.QueryConfirmHintWithOrigin(confRequest)
		require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
	}
}

// TestHeightHintCacheConfirmsPerScript ensures that a request for an output
// script that never confirms cannot advance another script's cached hint.
func TestHeightHintCacheConfirmsPerScript(t *testing.T) {
	t.Parallel()

	hintCache := initHintCache(t)
	txid := chainhash.Hash{1}
	script := func(value byte) []byte {
		return append([]byte{0x00, 0x20}, bytes.Repeat(
			[]byte{value}, 32,
		)...)
	}

	confirmed, err := chainntnfs.NewConfRequest(&txid, script(2))
	require.NoError(t, err)
	unconfirmed, err := chainntnfs.NewConfRequest(&txid, script(3))
	require.NoError(t, err)
	unknown, err := chainntnfs.NewConfRequest(&txid, script(4))
	require.NoError(t, err)

	require.NoError(t, hintCache.CommitConfirmHint(100, confirmed))
	require.NoError(t, hintCache.CommitConfirmHint(105, unconfirmed))

	hint, err := hintCache.QueryConfirmHint(confirmed)
	require.NoError(t, err)
	require.EqualValues(t, 100, hint)

	hint, err = hintCache.QueryConfirmHint(unconfirmed)
	require.NoError(t, err)
	require.EqualValues(t, 105, hint)

	_, err = hintCache.QueryConfirmHint(unknown)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)

	require.NoError(t, hintCache.PurgeConfirmHint(unconfirmed))
	hint, err = hintCache.QueryConfirmHint(confirmed)
	require.NoError(t, err)
	require.EqualValues(t, 100, hint)
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
	_, err := hintCache.QuerySpendHintWithOrigin(unknownSpendRequest)
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
		spendHint, err := hintCache.QuerySpendHintWithOrigin(
			spendRequest,
		)
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
		_, err = hintCache.QuerySpendHintWithOrigin(spendRequest)
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
	cachedConfHint, err := hintCache.QueryConfirmHintWithOrigin(confRequest)
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
	cachedSpendHint, err := hintCache.QuerySpendHintWithOrigin(spendRequest)
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

	hint, err := hintCache.QueryConfirmHintWithOrigin(confRequest)
	require.NoError(t, err)
	require.Equal(t, chainntnfs.HeightHint{Height: 123}, hint)
	legacyHeight, err := hintCache.QueryConfirmHint(confRequest)
	require.NoError(t, err)
	require.Equal(t, uint32(123), legacyHeight)

	// The original height-only write method remains available to callers
	// implementing the legacy interface and records an unknown origin.
	require.NoError(t, hintCache.CommitConfirmHint(234, confRequest))
	hint, err = hintCache.QueryConfirmHintWithOrigin(confRequest)
	require.NoError(t, err)
	require.Equal(t, chainntnfs.HeightHint{Height: 234}, hint)

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

// TestHeightHintLegacyPrefixRestart checks that an interrupted prefix is
// dispatched again even when the first registration used a four-byte hint.
func TestHeightHintLegacyPrefixRestart(t *testing.T) {
	// Arrange real on-disk records from before origins were encoded. Both
	// notification types should still resume their initial scan at 180.
	cache := initHintCache(t)
	txid := chainhash.Hash{1}
	op := wire.OutPoint{Hash: txid}
	script := chainntnfs.ZeroTaprootPkScript.Script()
	confReq, err := chainntnfs.NewConfRequest(&txid, script)
	require.NoError(t, err)
	spendReq, err := chainntnfs.NewSpendRequest(&op, script)
	require.NoError(t, err)
	confKey, err := confHintKey(&confReq)
	require.NoError(t, err)
	spendKey, err := spendHintKey(&spendReq)
	require.NoError(t, err)

	var legacy bytes.Buffer
	require.NoError(t, WriteElement(&legacy, uint32(180)))
	err = kvdb.Update(cache.db, func(tx kvdb.RwTx) error {
		// Seed the buckets directly so the current encoder cannot
		// silently add an origin to either legacy value.
		err := tx.ReadWriteBucket(confirmHintBucket).Put(
			confKey, legacy.Bytes(),
		)
		if err != nil {
			return err
		}

		return tx.ReadWriteBucket(spendHintBucket).Put(
			spendKey, legacy.Bytes(),
		)
	}, func() {})
	require.NoError(t, err)

	n := chainntnfs.NewTxNotifier(
		200, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	conf, err := n.RegisterConf(&txid, script, 1, 150)
	require.NoError(t, err)
	require.NotNil(t, conf.HistoricalDispatch)
	require.EqualValues(t, 180, conf.HistoricalDispatch.StartHeight)
	spend, err := n.RegisterSpend(&op, script, 150)
	require.NoError(t, err)
	require.NotNil(t, spend.HistoricalDispatch)
	require.EqualValues(t, 180, spend.HistoricalDispatch.StartHeight)

	// Act by accepting an earlier subscription for each request. Restart
	// before either backend can report scan progress.
	conf, err = n.RegisterConf(&txid, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, conf.HistoricalDispatch)
	require.EqualValues(t, 50, conf.HistoricalDispatch.StartHeight)
	require.EqualValues(t, 149, conf.HistoricalDispatch.EndHeight)
	spend, err = n.RegisterSpend(&op, script, 50)
	require.NoError(t, err)
	require.NotNil(t, spend.HistoricalDispatch)
	require.EqualValues(t, 50, spend.HistoricalDispatch.StartHeight)
	require.EqualValues(t, 149, spend.HistoricalDispatch.EndHeight)
	n.TearDown()

	// Assert that the replacement notifier returns actual dispatches at
	// the earlier hint with only the shared database surviving the restart.
	n = chainntnfs.NewTxNotifier(
		200, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)
	conf, err = n.RegisterConf(&txid, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, conf.HistoricalDispatch)
	spend, err = n.RegisterSpend(&op, script, 50)
	require.NoError(t, err)
	require.NotNil(t, spend.HistoricalDispatch)
	require.Equal(t, []uint32{50, 50}, []uint32{
		conf.HistoricalDispatch.StartHeight,
		spend.HistoricalDispatch.StartHeight,
	})
}

// confirmPurgeMock delegates normal hint operations to the database and
// substitutes a write failure only for the specified confirmation purge.
type confirmPurgeMock struct {
	*HeightHintCache
	mock.Mock
}

// Verify that substituting the purge still exposes origin-aware lookups.
var _ chainntnfs.ConfirmHintCacheWithOrigin = (*confirmPurgeMock)(nil)

// PurgeConfirmHint returns the arranged error for the exact purge request.
func (m *confirmPurgeMock) PurgeConfirmHint(
	reqs ...chainntnfs.ConfRequest) error {

	args := m.Called(reqs)
	return args.Error(0)
}

// TestHeightHintLegacyPrefixWriteFailure checks that a cache error cannot
// drop the live subscriber that needs the missing confirmation range.
func TestHeightHintLegacyPrefixWriteFailure(t *testing.T) {
	// Arrange an unknown-origin hint and fail only the purge of this key.
	// All lookups still use the same real cache as production.
	cache := initHintCache(t)
	txid := chainhash.Hash{1}
	script := chainntnfs.ZeroTaprootPkScript.Script()
	req, err := chainntnfs.NewConfRequest(&txid, script)
	require.NoError(t, err)
	require.NoError(t, cache.CommitConfirmHint(180, req))
	confCache := &confirmPurgeMock{HeightHintCache: cache}
	confCache.On("PurgeConfirmHint", []chainntnfs.ConfRequest{req}).
		Return(errors.New("purge failed")).Once()
	n := chainntnfs.NewTxNotifier(
		200, chainntnfs.ReorgSafetyLimit, confCache, cache,
	)
	t.Cleanup(n.TearDown)
	_, err = n.RegisterConf(&txid, script, 1, 150)
	require.NoError(t, err)

	// Act by accepting the earlier subscriber while invalidation fails.
	early, err := n.RegisterConf(&txid, script, 1, 50)

	// Assert the caller still receives the missing range in this process.
	require.NoError(t, err)
	require.NotNil(t, early.HistoricalDispatch)
	require.EqualValues(t, 50, early.HistoricalDispatch.StartHeight)
	require.EqualValues(t, 149, early.HistoricalDispatch.EndHeight)
	confCache.AssertExpectations(t)
}

// TestHeightHintPrefixRetainsCachedHints checks that admitting a prefix leaves
// stored hints intact when the lookup cannot skip any part of that prefix.
func TestHeightHintPrefixRetainsCachedHints(t *testing.T) {
	for _, tc := range []struct {
		name         string
		queryDisable bool
		hint         chainntnfs.HeightHint
	}{
		{
			name:         "disabled lookup",
			queryDisable: true,
			hint: chainntnfs.HeightHint{
				Height: 180,
				Origin: 100,
			},
		},
		{
			name: "legacy hint at prefix start",
			hint: chainntnfs.HeightHint{Height: 50},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange stored hints and start both scans at 150. The
			// earlier subscriber will extend each scan to 50.
			cache := initHintCacheWithConfig(t, CacheConfig{
				QueryDisable: tc.queryDisable,
			})
			stored, err := NewHeightHintCache(
				CacheConfig{}, cache.db,
			)
			require.NoError(t, err)
			n := chainntnfs.NewTxNotifier(
				200, chainntnfs.ReorgSafetyLimit, cache, cache,
			)
			t.Cleanup(n.TearDown)
			txid := chainhash.Hash{1}
			op := wire.OutPoint{Hash: txid}
			script := chainntnfs.ZeroTaprootPkScript.Script()
			cr, err := chainntnfs.NewConfRequest(&txid, script)
			require.NoError(t, err)
			sr, err := chainntnfs.NewSpendRequest(&op, script)
			require.NoError(t, err)
			require.NoError(t, stored.CommitConfirmHints(
				chainntnfs.ConfirmHints{cr: tc.hint},
			))
			require.NoError(t, stored.CommitSpendHints(
				chainntnfs.SpendHints{sr: tc.hint},
			))
			_, err = n.RegisterConf(&txid, script, 1, 150)
			require.NoError(t, err)
			_, err = n.RegisterSpend(&op, script, 150)
			require.NoError(t, err)

			// Act by admitting an earlier prefix for each request.
			conf, err := n.RegisterConf(&txid, script, 1, 50)
			require.NoError(t, err)
			require.NotNil(t, conf.HistoricalDispatch)
			spend, err := n.RegisterSpend(&op, script, 50)
			require.NoError(t, err)
			require.NotNil(t, spend.HistoricalDispatch)

			// Assert through an enabled view so a disabled lookup
			// cannot mask deletion of either database entry.
			ch, err := stored.QueryConfirmHintWithOrigin(cr)
			require.NoError(t, err)
			require.Equal(t, tc.hint, ch)
			sh, err := stored.QuerySpendHintWithOrigin(sr)
			require.NoError(t, err)
			require.Equal(t, tc.hint, sh)
		})
	}
}
