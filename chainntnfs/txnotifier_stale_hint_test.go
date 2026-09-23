package chainntnfs_test

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// TestTxNotifierStaleScanResultAfterMaturity asserts that a request set is
// removed once its request matures, even while its historical scan is still
// outstanding, and that the scan's late result is then rejected rather than
// credited to a newer set for the same request. Backends like btcd never
// report some scans at all, so keeping the set alive for its scan would leak
// it, while crediting the result to the newer set would advance that set's
// hint past a range its own scan has not covered.
func TestTxNotifierStaleScanResultAfterMaturity(t *testing.T) {
	const (
		startHeight      = uint32(100)
		reorgSafetyLimit = uint32(4)
	)

	outpoint, script, tx := staleHintSpend("stale-scan-after-maturity")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, reorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// The spend is found at tip while the first scan is outstanding, and
	// then matures.
	first, err := n.RegisterSpend(&outpoint, script, 95)
	require.NoError(t, err)
	stale := first.HistoricalDispatch
	require.NotNil(t, stale)
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	advanceEmptyChain(
		t, n, startHeight+2, startHeight+1+reorgSafetyLimit,
	)
	select {
	case <-first.Event.Done:
	default:
		t.Fatal("matured spend not retired")
	}

	// The matured set is gone, so a new registration dispatches its own
	// scan.
	second, err := n.RegisterSpend(&outpoint, script, 95)
	require.NoError(t, err)
	fresh := second.HistoricalDispatch
	require.NotNil(t, fresh)
	before, err := cache.QuerySpendHint(fresh.SpendRequest)
	require.NoError(t, err)

	// The first scan's result belongs to the removed set.
	err = n.UpdateSpendDetails(stale, nil)
	require.ErrorIs(t, err, chainntnfs.ErrStaleScanResult)
	after, err := cache.QuerySpendHint(fresh.SpendRequest)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// The new set's own scan still completes normally.
	spenderHash := tx.TxHash()
	require.NoError(t, n.UpdateSpendDetails(fresh, &chainntnfs.SpendDetail{
		SpentOutPoint:  &outpoint,
		SpenderTxHash:  &spenderHash,
		SpendingTx:     tx,
		SpendingHeight: int32(startHeight + 1),
	}))
	select {
	case details := <-second.Event.Spend:
		require.EqualValues(t, startHeight+1, details.SpendingHeight)
	default:
		t.Fatal("new subscriber missed spend")
	}
}
