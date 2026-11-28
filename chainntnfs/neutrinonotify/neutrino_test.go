package neutrinonotify

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/cache/lru"
	"github.com/lightninglabs/neutrino/filterdb"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// TestExtendHistoricalDispatch asserts that an initial scan catches up to the
// backend tip while a supplemental prefix retains its fixed boundary.
func TestExtendHistoricalDispatch(t *testing.T) {
	endHeight := uint32(100)
	extendHistoricalDispatch(&endHeight, false, 120)
	require.Equal(t, uint32(120), endHeight)

	endHeight = 100
	extendHistoricalDispatch(&endHeight, true, 120)
	require.Equal(t, uint32(100), endHeight)
}

// TestHistoricalConfDetailsAfterRollback checks that a supplemental scan still
// reads the surviving block when the tip retreats during filter setup.
func TestHistoricalConfDetailsAfterRollback(t *testing.T) {
	// Arrange a two-header chain so rollback removes the original scan end
	// from the same store that historicalConfDetails will actually read.
	dir := t.TempDir()
	db, cleanup, err := kvdb.GetTestBackend(dir, "headers.db")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	params := &chaincfg.RegressionNetParams
	headers, err := headerfs.NewBlockHeaderStore(dir, db, params)
	require.NoError(t, err)
	first := wire.BlockHeader{PrevBlock: *params.GenesisHash}
	second := wire.BlockHeader{PrevBlock: first.BlockHash()}
	err = headers.WriteHeaders(
		headerfs.BlockHeader{BlockHeader: &first, Height: 1},
		headerfs.BlockHeader{BlockHeader: &second, Height: 2},
	)
	require.NoError(t, err)
	tip, err := headers.RollbackLastBlock()
	require.NoError(t, err)

	// An empty basic filter lets the surviving height return a genuine
	// negative result without fetching a block or contacting a peer.
	filter, err := builder.BuildBasicFilter(&wire.MsgBlock{
		Header: first,
	}, nil)
	require.NoError(t, err)
	filters := lru.NewCache[
		neutrino.FilterCacheKey, *neutrino.CacheableFilter,
	](1024)
	_, err = filters.Put(neutrino.FilterCacheKey{
		BlockHash:  first.BlockHash(),
		FilterType: filterdb.RegularFilter,
	}, &neutrino.CacheableFilter{Filter: filter})
	require.NoError(t, err)
	n := &NeutrinoNotifier{p2pNode: &neutrino.ChainService{
		BlockHeaders: headers,
		FilterCache:  filters,
	}}
	request := chainntnfs.ConfRequest{
		PkScript: chainntnfs.ZeroTaprootPkScript,
	}

	// Act on the new tip exactly as registration does after filter setup,
	// then run the lookup against the rolled-back store.
	end := uint32(2)
	extendHistoricalDispatch(&end, true, uint32(tip.Height))
	details, err := n.historicalConfDetails(request, 1, end)

	// Assert that the range includes block 1 and yields an empty result.
	// A zero end would silently skip that block and also yield nil details.
	require.NoError(t, err)
	require.Equal(t, uint32(1), end)
	require.Nil(t, details)
}
