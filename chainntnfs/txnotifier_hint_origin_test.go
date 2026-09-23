package chainntnfs_test

import (
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// TestTxNotifierHintOriginGuardsEarlierHint asserts that a hint persisted while
// only a late-hint subscriber watched a request cannot hide an earlier
// confirmation or spend from the next subscriber after a restart. The first
// subscriber's hint is later than the real event, so its scans never covered
// that block. A subscriber whose hint precedes the cached hint's origin must
// therefore scan from its own hint, even when it registers first.
func TestTxNotifierHintOriginGuardsEarlierHint(t *testing.T) {
	const (
		startHeight = uint32(1000)
		tipHeight   = uint32(1010)
		lateHint    = uint32(990)
		earlyHint   = uint32(850)
	)

	t.Run("confirmation", func(t *testing.T) {
		script, tx := staleHintTx("hint-origin-conf")
		txid := tx.TxHash()
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)

		// The late subscriber's scan and tip tracking leave a hint at
		// the tip with the late hint as its origin.
		late, err := n.RegisterConf(&txid, script, 1, lateHint)
		require.NoError(t, err)
		request := late.HistoricalDispatch.ConfRequest
		require.NoError(t, n.UpdateConfDetails(request, nil))
		advanceEmptyChain(t, n, startHeight+1, tipHeight)
		hint, err := cache.QueryConfirmHint(request)
		require.NoError(t, err)
		require.Equal(t, chainntnfs.HeightHint{
			Height: tipHeight, Origin: lateHint,
		}, hint)

		// After a restart, the earlier subscriber registers first and
		// cannot use a hint whose origin is above its own hint.
		n.TearDown()
		n = chainntnfs.NewTxNotifier(
			tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		t.Cleanup(n.TearDown)
		early, err := n.RegisterConf(&txid, script, 1, earlyHint)
		require.NoError(t, err)
		require.NotNil(t, early.HistoricalDispatch)
		require.Equal(
			t, earlyHint, early.HistoricalDispatch.StartHeight,
		)
	})

	t.Run("spend", func(t *testing.T) {
		outpoint, script, _ := staleHintSpend("hint-origin-spend")
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)

		late, err := n.RegisterSpend(&outpoint, script, lateHint)
		require.NoError(t, err)
		request := late.HistoricalDispatch.SpendRequest
		require.NoError(t, n.UpdateSpendDetails(request, nil))
		advanceEmptyChain(t, n, startHeight+1, tipHeight)
		hint, err := cache.QuerySpendHint(request)
		require.NoError(t, err)
		require.Equal(t, chainntnfs.HeightHint{
			Height: tipHeight, Origin: lateHint,
		}, hint)

		n.TearDown()
		n = chainntnfs.NewTxNotifier(
			tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		t.Cleanup(n.TearDown)
		early, err := n.RegisterSpend(&outpoint, script, earlyHint)
		require.NoError(t, err)
		require.NotNil(t, early.HistoricalDispatch)
		require.Equal(
			t, earlyHint, early.HistoricalDispatch.StartHeight,
		)
	})
	// Progress persisted by a backend while the late scan is outstanding
	// carries the scan's origin too.
	t.Run("spend progress", func(t *testing.T) {
		outpoint, script, _ := staleHintSpend("hint-origin-progress")
		cache := newMockHintCache()
		n := chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)

		late, err := n.RegisterSpend(&outpoint, script, lateHint)
		require.NoError(t, err)
		require.NotNil(t, late.HistoricalDispatch)
		require.NoError(t, cache.CommitSpendHints(
			late.HistoricalDispatch.ProgressHints(startHeight),
		))

		n.TearDown()
		n = chainntnfs.NewTxNotifier(
			startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
		)
		t.Cleanup(n.TearDown)
		early, err := n.RegisterSpend(&outpoint, script, earlyHint)
		require.NoError(t, err)
		require.NotNil(t, early.HistoricalDispatch)
		require.Equal(
			t, earlyHint, early.HistoricalDispatch.StartHeight,
		)
	})
}

// staleHintTx returns a transaction with a single output to a script unique to
// the tag, along with that script.
func staleHintTx(tag string) ([]byte, *wire.MsgTx) {
	program := sha256.Sum256([]byte(tag))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})

	return script, tx
}

// staleHintSpend returns an outpoint unique to the tag, the script used to
// watch it, and a transaction with a witness that spends it.
func staleHintSpend(tag string) (wire.OutPoint, []byte, *wire.MsgTx) {
	hash := sha256.Sum256([]byte(tag))
	outpoint := wire.OutPoint{Index: 1}
	copy(outpoint.Hash[:], hash[:])
	script := chainntnfs.ZeroTaprootPkScript.Script()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outpoint,
		Witness:          wire.TxWitness{{0x01}},
	})
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})

	return outpoint, script, tx
}

// assertNoSpend asserts that no spend notification is waiting on the event.
func assertNoSpend(t *testing.T, event *chainntnfs.SpendEvent) {
	t.Helper()
	select {
	case <-event.Spend:
		t.Fatal("unexpected spend")
	default:
	}
}

// advanceEmptyChain connects and notifies empty blocks at every height from
// start to end, inclusive.
func advanceEmptyChain(t *testing.T, n *chainntnfs.TxNotifier, start,
	end uint32) {

	t.Helper()
	for height := start; height <= end; height++ {
		require.NoError(t, n.ConnectTip(
			btcutil.NewBlock(&wire.MsgBlock{}), height,
		))
		require.NoError(t, n.NotifyHeight(height))
	}
}
