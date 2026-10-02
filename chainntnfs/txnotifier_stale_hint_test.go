package chainntnfs_test

import (
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// TestTxNotifierStaleConfirmHintAfterRestart asserts that a cached hint from a
// pre-reorg chain cannot hide a confirmation on the replacement chain.
// Canceling the only subscriber purges the confirm hint before the restart.
// Without that purge, a hint committed while scanning the old chain could
// point above the height of a confirmation that only exists on the
// replacement chain, and a fresh notifier trusting that hint would silently
// skip the block that contains it.
func TestTxNotifierStaleConfirmHintAfterRestart(t *testing.T) {
	const (
		startHeight = uint32(100)
		oldTip      = uint32(103)
		newTip      = uint32(102)
		heightHint  = uint32(1)
	)

	// Build a single subscriber for a transaction that never actually
	// confirms on the chain constructed below.
	program := sha256.Sum256([]byte("stale-confirm-hint"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})
	txid := tx.TxHash()
	cache := newMockHintCache()

	// Register the subscriber and complete its historical scan with no
	// confirmation found, then advance the chain and cancel. With no
	// subscribers left, the notifier must purge the cached hint.
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	registration, err := n.RegisterConf(&txid, script, 1, heightHint)
	require.NoError(t, err)
	require.NoError(t, n.UpdateConfDetails(
		registration.HistoricalDispatch, nil,
	))
	advanceEmptyChain(t, n, startHeight+1, oldTip)
	registration.Event.Cancel()
	_, err = cache.confirmHeight(
		registration.HistoricalDispatch.ConfRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
	n.TearDown()

	// Restart at the old chain's tip, then reorg back two blocks and
	// connect a replacement chain whose tip block actually contains the
	// transaction.
	n = chainntnfs.NewTxNotifier(
		oldTip, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)
	require.NoError(t, n.DisconnectTip(oldTip))
	require.NoError(t, n.DisconnectTip(oldTip-1))
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})
	require.NoError(t, n.ConnectTip(block, newTip))
	require.NoError(t, n.NotifyHeight(newTip))

	// Re-register at the same low height hint. Since the earlier
	// cancellation purged the cache, the dispatch must start scanning
	// from that honest hint instead of a leftover height that would skip
	// the confirmation on the replacement chain.
	registration, err = n.RegisterConf(&txid, script, 1, heightHint)
	require.NoError(t, err)
	require.NotNil(t, registration.HistoricalDispatch)
	require.Equal(
		t, heightHint, registration.HistoricalDispatch.StartHeight,
	)
}

// TestTxNotifierStaleSpendHintAfterRestart asserts that a cached hint from a
// pre-reorg chain cannot hide a spend on the replacement chain. This mirrors
// TestTxNotifierStaleConfirmHintAfterRestart for spend requests: canceling
// the only subscriber purges the spend hint, so a restart followed by a
// reorg cannot trust a height left over from the old chain and skip the
// block on the replacement chain that actually spends the outpoint.
func TestTxNotifierStaleSpendHintAfterRestart(t *testing.T) {
	const (
		startHeight = uint32(100)
		oldTip      = uint32(103)
		newTip      = uint32(102)
		heightHint  = uint32(1)
	)

	// Build a single subscriber for an outpoint that is not spent on the
	// chain constructed below.
	program := sha256.Sum256([]byte("stale-spend-hint"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	outpoint := wire.OutPoint{
		Hash:  chainntnfs.ZeroHash,
		Index: 1,
	}
	cache := newMockHintCache()

	// Register the subscriber and complete its historical scan with no
	// spend found, then advance the chain and cancel so the cached hint
	// is purged.
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	registration, err := n.RegisterSpend(&outpoint, script, heightHint)
	require.NoError(t, err)
	require.NoError(t, n.UpdateSpendDetails(
		registration.HistoricalDispatch, nil,
	))
	advanceEmptyChain(t, n, startHeight+1, oldTip)
	registration.Event.Cancel()
	_, err = cache.spendHeight(
		registration.HistoricalDispatch.SpendRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)
	n.TearDown()

	// Restart at the old chain's tip, then reorg back two blocks and
	// connect a replacement chain whose tip block spends the outpoint.
	n = chainntnfs.NewTxNotifier(
		oldTip, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)
	require.NoError(t, n.DisconnectTip(oldTip))
	require.NoError(t, n.DisconnectTip(oldTip-1))
	spendingTx := wire.NewMsgTx(2)
	spendingTx.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	spendingTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendingTx},
	})
	require.NoError(t, n.ConnectTip(block, newTip))
	require.NoError(t, n.NotifyHeight(newTip))

	// Re-register at the same low height hint. The dispatch must start
	// scanning from that honest hint rather than a leftover height that
	// would skip the spend on the replacement chain.
	registration, err = n.RegisterSpend(&outpoint, script, heightHint)
	require.NoError(t, err)
	require.NotNil(t, registration.HistoricalDispatch)
	require.Equal(
		t, heightHint, registration.HistoricalDispatch.StartHeight,
	)
}

// TestTxNotifierCanceledPendingConfirmScan asserts that a completed historical
// callback cannot recreate a purged hint for a subscriberless request. The
// subscriber cancels while its historical scan is still outstanding, which
// purges the hint immediately since no subscriber remains. If the callback
// that lands afterward were allowed to write the hint again, a subsequently
// removed set would leave a stale hint behind for the next registration to
// pick up.
func TestTxNotifierCanceledPendingConfirmScan(t *testing.T) {
	const startHeight = uint32(100)

	script, tx := staleHintTx("pending-confirm-cleanup")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a subscriber and cancel it before the historical scan
	// returns, leaving the request set without any subscribers while a
	// scan is still pending.
	registration, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	dispatch := registration.HistoricalDispatch
	require.NotNil(t, dispatch)
	registration.Event.Cancel()

	// Connect a block containing the transaction, and confirm the hint
	// was already purged by the cancellation above. Complete the
	// outstanding scan and notify the new height; the hint must stay
	// purged since the request set has no subscribers left to consume
	// it.
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	_, err = cache.confirmHeight(dispatch.ConfRequest)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
	require.NoError(t, n.UpdateConfDetails(dispatch, nil))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	_, err = cache.confirmHeight(dispatch.ConfRequest)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
}

// TestTxNotifierCanceledPendingConfirmReorg asserts that cancellation retains
// reorg tracking until the outstanding historical callback has returned. The
// first subscriber confirms, cancels, and is then reorged out before its
// initial scan's callback ever lands. A second subscriber re-registers into
// the same request at that point. If the reorg had dropped the request's
// tracking early, the notifier could not un-confirm it, and the late
// callback delivering a stale positive result to the second subscriber would
// go unnoticed.
func TestTxNotifierCanceledPendingConfirmReorg(t *testing.T) {
	const startHeight = uint32(100)

	script, tx := staleHintTx("pending-confirm-reorg")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register the first subscriber, confirm the transaction at tip, and
	// then cancel and reorg the confirming block away, all before the
	// historical scan's callback has returned.
	first, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	first.Event.Cancel()
	require.NoError(t, n.DisconnectTip(startHeight+1))

	// A second subscriber re-registers into the same, now reorged-out,
	// request. It must not see a confirmation yet. Deliver the first
	// subscriber's stale scan result and reconnect an empty block; the
	// second subscriber still must not observe a confirmation, since the
	// transaction is no longer on chain.
	second, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	assertNoConfirmation(t, second.Event)
	require.NoError(t, n.UpdateConfDetails(dispatch, nil))
	require.NoError(t, n.ConnectTip(
		btcutil.NewBlock(&wire.MsgBlock{}), startHeight+1,
	))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	assertNoConfirmation(t, second.Event)
}

// TestTxNotifierCanceledPendingSpendScan asserts that a completed historical
// callback cannot recreate a purged hint for a subscriberless request. This
// is the spend-side counterpart to TestTxNotifierCanceledPendingConfirmScan:
// the subscriber cancels while its historical scan is outstanding, and the
// hint must stay purged once that scan's callback finally returns.
func TestTxNotifierCanceledPendingSpendScan(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("pending-spend-cleanup")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a subscriber and cancel it before the historical scan
	// returns, leaving the request set without any subscribers while a
	// scan is still pending.
	registration, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := registration.HistoricalDispatch
	require.NotNil(t, dispatch)
	registration.Event.Cancel()
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	_, err = cache.spendHeight(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)

	// Complete the outstanding scan and notify the new height; the hint
	// must stay purged since the request set has no subscribers left to
	// consume it.
	require.NoError(t, n.UpdateSpendDetails(dispatch, nil))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	_, err = cache.spendHeight(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)
}

// TestTxNotifierCanceledPendingSpendReorg asserts that cancellation retains
// reorg tracking until the outstanding historical callback has returned.
// This is the spend-side counterpart to
// TestTxNotifierCanceledPendingConfirmReorg: the first subscriber's spend is
// reorged out and canceled before its historical scan's callback returns,
// and a second subscriber must not receive a stale positive result once
// that callback finally lands.
func TestTxNotifierCanceledPendingSpendReorg(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("pending-spend-reorg")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register the first subscriber, spend the outpoint at tip, and then
	// cancel and reorg the spending block away, all before the
	// historical scan's callback has returned. The hint must be purged
	// by the reorg reaching back past the request's original height
	// hint.
	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	first.Event.Cancel()
	require.NoError(t, n.DisconnectTip(startHeight+1))
	require.NoError(t, n.DisconnectTip(startHeight))
	_, err = cache.spendHeight(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)

	// A second subscriber re-registers into the same, now reorged-out,
	// request. It must not see a spend yet. Deliver the first
	// subscriber's stale scan result and reconnect the chain; the second
	// subscriber still must not observe a spend, since the outpoint is
	// unspent again.
	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	assertNoSpend(t, second.Event)
	require.NoError(t, n.UpdateSpendDetails(dispatch, nil))
	advanceEmptyChain(t, n, startHeight, startHeight+1)
	assertNoSpend(t, second.Event)
}

// TestTxNotifierCanceledPrefixResultReorg asserts that a positive prefix scan
// retained without subscribers is still invalidated by a reorg. The second
// subscriber's earlier height hint causes the notifier to dispatch a
// supplemental scan of the uncovered prefix below the first subscriber's
// scan. Both subscribers cancel, and the prefix scan's positive result
// arrives for a block that is then reorged out entirely, before a third
// subscriber re-registers. The stale positive result held by the
// subscriberless set must not survive the reorg and must not be handed to
// the third subscriber.
func TestTxNotifierCanceledPrefixResultReorg(t *testing.T) {
	const (
		startHeight = uint32(100)
		txHeight    = uint32(40)
	)

	script, tx := staleHintTx("canceled-prefix-result")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a first subscriber with a height hint of 50, which starts
	// the main scan, then a second with an earlier hint of 1, which
	// triggers a supplemental prefix scan for the range below 50. Cancel
	// both subscribers before either scan's callback returns.
	first, err := n.RegisterConf(&txid, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, first.HistoricalDispatch)
	second, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, second.HistoricalDispatch)
	first.Event.Cancel()
	second.Event.Cancel()

	// Complete the prefix scan with a positive result at txHeight, then
	// reorg the chain back past that height. The purged hint confirms
	// the positive result did not survive the reorg.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})
	require.NoError(t, n.UpdateConfDetails(
		second.HistoricalDispatch,
		&chainntnfs.TxConfirmation{
			BlockHash:   block.Hash(),
			BlockHeight: txHeight,
			Tx:          tx,
		},
	))
	for height := startHeight; height >= txHeight-1; height-- {
		require.NoError(t, n.DisconnectTip(height))
	}
	_, err = cache.confirmHeight(
		second.HistoricalDispatch.ConfRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)

	// A third subscriber re-registers into the request. It must not see
	// a confirmation. Complete the still-outstanding main scan with a
	// negative result and reconnect the chain up to txHeight; the third
	// subscriber still must not observe a confirmation.
	third, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.Nil(t, third.HistoricalDispatch)
	assertNoConfirmation(t, third.Event)
	require.NoError(t, n.UpdateConfDetails(
		first.HistoricalDispatch, nil,
	))
	advanceEmptyChain(t, n, txHeight-1, txHeight)
	assertNoConfirmation(t, third.Event)
}

// TestTxNotifierRelevantSpendPreservesPendingScan asserts that a spend learned
// outside the historical callback does not release that callback's ownership.
// ProcessRelevantSpendTx lets a caller report a spend it observed directly,
// independent of the historical scan dispatched at registration. That path
// must not decrement pendingRescans or otherwise mark the scan complete,
// since the historical callback for the original dispatch is still
// outstanding and must be allowed to return later without corrupting the
// request's bookkeeping.
func TestTxNotifierRelevantSpendPreservesPendingScan(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("relevant-pending-spend")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a subscriber, leaving its historical scan outstanding.
	// Report the spend directly through ProcessRelevantSpendTx instead
	// of completing the scan, then cancel the subscriber and advance the
	// chain further.
	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	advanceEmptyChain(t, n, startHeight+1, startHeight+1)
	require.NoError(t, n.ProcessRelevantSpendTx(
		btcutil.NewTx(tx), startHeight+1,
	))
	first.Event.Cancel()
	advanceEmptyChain(t, n, startHeight+2, startHeight+2)

	// A second subscriber re-registers into the request and must
	// immediately observe the spend learned above, without triggering a
	// new historical scan. Delivering the original scan's callback
	// afterward must not disturb that result.
	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	select {
	case details := <-second.Event.Spend:
		require.Equal(t, int32(startHeight+1), details.SpendingHeight)
	default:
		t.Fatal("re-registration missed relevant spend")
	}
	require.NoError(t, n.UpdateSpendDetails(dispatch, nil))
}

// TestTxNotifierCanceledPendingRelevantSpend asserts that a relevant spend
// retains an empty set until its historical callback has returned. Unlike
// TestTxNotifierRelevantSpendPreservesPendingScan, the only subscriber
// cancels before the relevant spend is reported, so ProcessRelevantSpendTx
// must operate on a request set with no active subscribers and still keep
// that set alive until the outstanding scan's callback lands.
func TestTxNotifierCanceledPendingRelevantSpend(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("canceled-relevant-spend")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a subscriber, cancel it immediately, and only then report
	// the spend directly through ProcessRelevantSpendTx. The request set
	// has no subscribers at that point, but its historical scan is still
	// outstanding.
	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	first.Event.Cancel()
	advanceEmptyChain(t, n, startHeight+1, startHeight+1)
	require.NoError(t, n.ProcessRelevantSpendTx(
		btcutil.NewTx(tx), startHeight+1,
	))

	// A second subscriber re-registers into the request and must
	// immediately observe the spend learned above, without triggering a
	// new historical scan. Delivering the original scan's callback
	// afterward must not disturb that result.
	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	select {
	case details := <-second.Event.Spend:
		require.Equal(t, int32(startHeight+1), details.SpendingHeight)
	default:
		t.Fatal("re-registration missed relevant spend")
	}
	require.NoError(t, n.UpdateSpendDetails(dispatch, nil))
}

// TestTxNotifierRepeatedScriptSpendCleanup asserts that a script-only request
// retains only its first spend and its matching reorg index. A script-only
// spend request (nil outpoint) can match more than one input across
// different blocks, since any input carrying the watched witness satisfies
// it. Only the first match should be tracked for reorg purposes; if a later,
// second match were also indexed, disconnecting back past only the first
// match's block would leave a dangling reorg index for the second and could
// panic or misbehave during cleanup.
func TestTxNotifierRepeatedScriptSpendCleanup(t *testing.T) {
	const startHeight = uint32(100)

	witness := wire.TxWitness{{0x01}}
	pkScript, err := txscript.ComputePkScript(nil, witness)
	require.NoError(t, err)
	script := pkScript.Script()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a script-only spend request and complete its historical
	// scan with no match found.
	registration, err := n.RegisterSpend(nil, script, 1)
	require.NoError(t, err)
	require.NoError(t, n.UpdateSpendDetails(
		registration.HistoricalDispatch, nil,
	))

	// Connect two separate blocks, each spending a different outpoint
	// with the same watched witness, so the script matches twice at
	// tip.
	for i := uint32(1); i <= 2; i++ {
		spendingTx := wire.NewMsgTx(2)
		spendingTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Index: i},
			Witness:          witness,
		})
		require.NoError(t, n.ConnectTip(btcutil.NewBlock(
			&wire.MsgBlock{Transactions: []*wire.MsgTx{spendingTx}},
		), startHeight+i))
		require.NoError(t, n.NotifyHeight(startHeight+i))
	}

	// Cancel the subscriber and disconnect both blocks. This must not
	// panic or error, which would indicate the second, redundant match
	// left behind an inconsistent reorg index.
	registration.Event.Cancel()
	require.NoError(t, n.DisconnectTip(startHeight+2))
	require.NoError(t, n.DisconnectTip(startHeight+1))
}

// TestTxNotifierCanceledConfirmResultSurvives asserts that a positive result
// from a canceled historical scan remains available to live-process polling.
// The only subscriber cancels before its scan's callback returns, so the
// hint is purged for lack of subscribers, but the confirmation is still real
// and must be recorded when the callback finally lands, so that a later
// subscriber re-registering into the same request is notified immediately
// instead of triggering a redundant rescan.
func TestTxNotifierCanceledConfirmResultSurvives(t *testing.T) {
	const (
		startHeight = uint32(100)
		txHeight    = uint32(90)
	)

	script, tx := staleHintTx("canceled-confirm-result")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a subscriber and cancel it before its historical scan
	// returns.
	first, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	first.Event.Cancel()

	// Complete the scan with a positive confirmation. The hint is still
	// purged, since no subscriber remains to make use of it, but the
	// confirmation details themselves must be retained.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})
	require.NoError(t, n.UpdateConfDetails(
		dispatch, &chainntnfs.TxConfirmation{
			BlockHash:   block.Hash(),
			BlockHeight: txHeight,
			Tx:          tx,
		},
	))
	_, err = cache.confirmHeight(dispatch.ConfRequest)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)

	// A second subscriber re-registers into the same request and must be
	// notified of the retained confirmation immediately, without a new
	// historical dispatch.
	second, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	select {
	case details := <-second.Event.Confirmed:
		require.Equal(t, txHeight, details.BlockHeight)
	default:
		t.Fatal("re-registration missed historical confirmation")
	}
}

// TestTxNotifierCanceledSpendResultSurvives asserts that a positive result
// from a canceled historical scan remains available to live-process polling.
// This is the spend-side counterpart to
// TestTxNotifierCanceledConfirmResultSurvives: the only subscriber cancels
// before its scan's callback returns, purging the hint, but the spend
// details must still be retained and handed to a later subscriber that
// re-registers into the same request.
func TestTxNotifierCanceledSpendResultSurvives(t *testing.T) {
	const (
		startHeight = uint32(100)
		spendHeight = int32(90)
	)

	outpoint, script, tx := staleHintSpend("canceled-spend-result")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a subscriber and cancel it before its historical scan
	// returns.
	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	first.Event.Cancel()

	// Complete the scan with a positive spend. The hint is still purged,
	// since no subscriber remains to make use of it, but the spend
	// details themselves must be retained.
	spenderHash := tx.TxHash()
	require.NoError(t, n.UpdateSpendDetails(
		dispatch, &chainntnfs.SpendDetail{
			SpentOutPoint:  &outpoint,
			SpenderTxHash:  &spenderHash,
			SpendingTx:     tx,
			SpendingHeight: spendHeight,
		},
	))
	_, err = cache.spendHeight(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)

	// A second subscriber re-registers into the same request and must be
	// notified of the retained spend immediately, without a new
	// historical dispatch.
	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	select {
	case details := <-second.Event.Spend:
		require.Equal(t, spendHeight, details.SpendingHeight)
	default:
		t.Fatal("re-registration missed historical spend")
	}
}

// TestTxNotifierKnownConfirmSurvivesCancellation asserts that cancellation
// preserves live-process polling while a reorg still invalidates the
// details. It chains the two prior cases together: a confirmation learned at
// tip (not through a historical scan) survives its subscriber's
// cancellation and is handed to a second subscriber, but once that
// confirming block is reorged out, a third subscriber re-registering into
// the request must trigger a fresh historical scan rather than reuse the
// invalidated details.
func TestTxNotifierKnownConfirmSurvivesCancellation(t *testing.T) {
	const startHeight = uint32(100)

	script, tx := staleHintTx("known-confirm-cancellation")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a subscriber, complete its historical scan with no match,
	// then confirm the transaction at tip and cancel the subscriber.
	// Confirming at tip, rather than through the historical scan, is
	// what makes the confirmation "known" independent of any pending
	// callback.
	first, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NoError(t, n.UpdateConfDetails(
		first.HistoricalDispatch, nil,
	))
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	first.Event.Cancel()
	_, err = cache.confirmHeight(
		first.HistoricalDispatch.ConfRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)

	// A second subscriber re-registers and must be notified of the
	// retained confirmation immediately. Cancel it too, then reorg the
	// confirming block away, which must invalidate the retained details.
	second, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	select {
	case details := <-second.Event.Confirmed:
		require.Equal(t, startHeight+1, details.BlockHeight)
	default:
		t.Fatal("re-registration missed known confirmation")
	}
	second.Event.Cancel()
	require.NoError(t, n.DisconnectTip(startHeight+1))

	// A third subscriber re-registering after the reorg must trigger a
	// fresh historical scan, since the previously retained confirmation
	// no longer reflects the chain.
	third, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, third.HistoricalDispatch)
}

// TestTxNotifierKnownSpendSurvivesCancellation asserts that cancellation
// preserves live-process polling while a reorg still invalidates the
// details. This is the spend-side counterpart to
// TestTxNotifierKnownConfirmSurvivesCancellation: a spend learned at tip
// survives its subscriber's cancellation and is handed to a second
// subscriber, but once that spending block is reorged out, a third
// subscriber re-registering into the request must trigger a fresh
// historical scan.
func TestTxNotifierKnownSpendSurvivesCancellation(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("known-spend-cancellation")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// Register a subscriber, complete its historical scan with no match,
	// then spend the outpoint at tip and cancel the subscriber.
	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.NoError(t, n.UpdateSpendDetails(
		first.HistoricalDispatch, nil,
	))
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	first.Event.Cancel()
	_, err = cache.spendHeight(
		first.HistoricalDispatch.SpendRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)

	// A second subscriber re-registers and must be notified of the
	// retained spend immediately. Cancel it too, then reorg the spending
	// block away, which must invalidate the retained details.
	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	select {
	case details := <-second.Event.Spend:
		require.Equal(t, int32(startHeight+1), details.SpendingHeight)
	default:
		t.Fatal("re-registration missed known spend")
	}
	second.Event.Cancel()
	require.NoError(t, n.DisconnectTip(startHeight+1))

	// A third subscriber re-registering after the reorg must trigger a
	// fresh historical scan, since the previously retained spend no
	// longer reflects the chain.
	third, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.NotNil(t, third.HistoricalDispatch)
}

// TestTxNotifierReleasedConfirmHintIsReorgSafe asserts that the final
// cancellation lowers the confirm hint to a height no reorg can replace,
// rather than deleting it. A re-registration after a restart then scans from
// that height. It covers every block a reorg within the safety limit could
// have replaced, without rescanning from the caller's original hint.
func TestTxNotifierReleasedConfirmHintIsReorgSafe(t *testing.T) {
	const (
		startHeight = uint32(1000)
		tipHeight   = uint32(1010)
		safeHint    = tipHeight - chainntnfs.ReorgSafetyLimit
	)

	script, tx := staleHintTx("released-confirm-hint")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)

	// Track the request at tip until its hint follows the tip.
	registration, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	dispatch := registration.HistoricalDispatch
	require.NotNil(t, dispatch)
	require.NoError(t, n.UpdateConfDetails(dispatch, nil))
	advanceEmptyChain(t, n, startHeight+1, tipHeight)
	hint, err := cache.confirmHeight(dispatch.ConfRequest)
	require.NoError(t, err)
	require.Equal(t, tipHeight, hint)

	// Without a subscriber, the hint falls back to the reorg-safe height.
	registration.Event.Cancel()
	hint, err = cache.confirmHeight(dispatch.ConfRequest)
	require.NoError(t, err)
	require.Equal(t, safeHint, hint)
	n.TearDown()

	// After a restart, the new scan starts at that height.
	n = chainntnfs.NewTxNotifier(
		tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)
	registration, err = n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, registration.HistoricalDispatch)
	require.Equal(
		t, safeHint, registration.HistoricalDispatch.StartHeight,
	)
}

// TestTxNotifierReleasedSpendHintIsReorgSafe asserts that the final
// cancellation lowers the spend hint to a height no reorg can replace, rather
// than deleting it, so a re-registration after a restart scans only from that
// height.
func TestTxNotifierReleasedSpendHintIsReorgSafe(t *testing.T) {
	const (
		startHeight = uint32(1000)
		tipHeight   = uint32(1010)
		safeHint    = tipHeight - chainntnfs.ReorgSafetyLimit
	)

	outpoint, script, _ := staleHintSpend("released-spend-hint")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)

	// Track the request at tip until its hint follows the tip.
	registration, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := registration.HistoricalDispatch
	require.NotNil(t, dispatch)
	require.NoError(t, n.UpdateSpendDetails(dispatch, nil))
	advanceEmptyChain(t, n, startHeight+1, tipHeight)
	hint, err := cache.spendHeight(dispatch.SpendRequest)
	require.NoError(t, err)
	require.Equal(t, tipHeight, hint)

	// Without a subscriber, the hint falls back to the reorg-safe height.
	registration.Event.Cancel()
	hint, err = cache.spendHeight(dispatch.SpendRequest)
	require.NoError(t, err)
	require.Equal(t, safeHint, hint)
	n.TearDown()

	// After a restart, the new scan starts at that height.
	n = chainntnfs.NewTxNotifier(
		tipHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)
	registration, err = n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.NotNil(t, registration.HistoricalDispatch)
	require.Equal(
		t, safeHint, registration.HistoricalDispatch.StartHeight,
	)
}

// TestTxNotifierDeepConfirmResultReleased asserts that a subscriberless set
// is removed once its only details are deeper than the reorg safety limit.
// Such details are never indexed for reorgs, so ConnectTip would never prune
// the set and it would stay in memory for the life of the process. A later
// registration observes the removal as a fresh historical dispatch.
func TestTxNotifierDeepConfirmResultReleased(t *testing.T) {
	const (
		startHeight      = uint32(100)
		reorgSafetyLimit = uint32(6)
		txHeight         = uint32(90)
	)

	script, tx := staleHintTx("deep-confirm-result")
	txid := tx.TxHash()
	details := &chainntnfs.TxConfirmation{
		BlockHeight: txHeight,
		Tx:          tx,
	}
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, reorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// A deep result reported after the only subscriber canceled is not
	// retained.
	first, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, first.HistoricalDispatch)
	first.Event.Cancel()
	require.NoError(t, n.UpdateConfDetails(
		first.HistoricalDispatch, details,
	))

	second, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, second.HistoricalDispatch)

	// A deep result delivered to a subscriber is not retained after that
	// subscriber cancels.
	require.NoError(t, n.UpdateConfDetails(
		second.HistoricalDispatch, details,
	))
	select {
	case got := <-second.Event.Confirmed:
		require.Equal(t, txHeight, got.BlockHeight)
	default:
		t.Fatal("subscriber missed historical confirmation")
	}
	second.Event.Cancel()

	third, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, third.HistoricalDispatch)
}

// TestTxNotifierDeepSpendResultReleased asserts that a subscriberless spend
// set is removed once its only details are deeper than the reorg safety
// limit, and that such details are never added to the reorg index, where
// ConnectTip would never sweep them.
func TestTxNotifierDeepSpendResultReleased(t *testing.T) {
	const (
		startHeight      = uint32(100)
		reorgSafetyLimit = uint32(6)
		spendHeight      = int32(90)
	)

	outpoint, script, tx := staleHintSpend("deep-spend-result")
	spenderHash := tx.TxHash()
	details := &chainntnfs.SpendDetail{
		SpentOutPoint:  &outpoint,
		SpenderTxHash:  &spenderHash,
		SpendingTx:     tx,
		SpendingHeight: spendHeight,
	}
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, reorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// A deep result reported after the only subscriber canceled is not
	// retained.
	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.NotNil(t, first.HistoricalDispatch)
	first.Event.Cancel()
	require.NoError(t, n.UpdateSpendDetails(
		first.HistoricalDispatch, details,
	))

	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.NotNil(t, second.HistoricalDispatch)

	// A deep result delivered to a subscriber is not retained after that
	// subscriber cancels.
	require.NoError(t, n.UpdateSpendDetails(
		second.HistoricalDispatch, details,
	))
	select {
	case got := <-second.Event.Spend:
		require.Equal(t, spendHeight, got.SpendingHeight)
	default:
		t.Fatal("subscriber missed historical spend")
	}
	second.Event.Cancel()

	third, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.NotNil(t, third.HistoricalDispatch)
}

// TestTxNotifierAboveTipResultReleased asserts that a subscriberless set is
// removed when its last scan reports details above the notifier's tip. A
// backend's view can run ahead of the notifier, and such details are not
// recorded until the block connects, so retaining the set for them would
// leak it if the transaction never confirms at that height.
func TestTxNotifierAboveTipResultReleased(t *testing.T) {
	const startHeight = uint32(100)

	script, tx := staleHintTx("above-tip-confirm-result")
	txid := tx.TxHash()
	outpoint, spendScript, spendTx := staleHintSpend("above-tip-spend")
	spenderHash := spendTx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	conf, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	spend, err := n.RegisterSpend(&outpoint, spendScript, 1)
	require.NoError(t, err)
	conf.Event.Cancel()
	spend.Event.Cancel()

	require.NoError(t, n.UpdateConfDetails(
		conf.HistoricalDispatch, &chainntnfs.TxConfirmation{
			BlockHeight: startHeight + 1,
			Tx:          tx,
		},
	))
	require.NoError(t, n.UpdateSpendDetails(
		spend.HistoricalDispatch, &chainntnfs.SpendDetail{
			SpentOutPoint:  &outpoint,
			SpenderTxHash:  &spenderHash,
			SpendingTx:     spendTx,
			SpendingHeight: int32(startHeight + 1),
		},
	))

	// Both sets were removed, so new registrations dispatch fresh scans
	// instead of joining sets that would never be cleaned up.
	conf, err = n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, conf.HistoricalDispatch)
	spend, err = n.RegisterSpend(&outpoint, spendScript, 1)
	require.NoError(t, err)
	require.NotNil(t, spend.HistoricalDispatch)
}

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

// assertNoConfirmation fails the test if a confirmation is waiting on
// event's Confirmed channel.
func assertNoConfirmation(t *testing.T, event *chainntnfs.ConfirmationEvent) {
	t.Helper()
	select {
	case <-event.Confirmed:
		t.Fatal("subscriber received duplicate confirmation")
	default:
	}
}

// TestTxNotifierStaleConfirmScanResultAfterMaturity is the confirmation side
// of TestTxNotifierStaleScanResultAfterMaturity: a matured set is removed with
// its historical scan still outstanding, and that scan's late result must be
// rejected rather than credited to the set a new registration creates.
func TestTxNotifierStaleConfirmScanResultAfterMaturity(t *testing.T) {
	const (
		startHeight      = uint32(100)
		reorgSafetyLimit = uint32(4)
	)

	script, tx := staleHintTx("stale-confirm-scan-after-maturity")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, reorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	// The confirmation is found at tip while the first scan is
	// outstanding, and then matures.
	first, err := n.RegisterConf(&txid, script, 1, 95)
	require.NoError(t, err)
	stale := first.HistoricalDispatch
	require.NotNil(t, stale)
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})
	require.NoError(t, n.ConnectTip(block, startHeight+1))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	advanceEmptyChain(
		t, n, startHeight+2, startHeight+1+reorgSafetyLimit,
	)
	select {
	case <-first.Event.Done:
	default:
		t.Fatal("matured confirmation not retired")
	}

	// A new registration dispatches its own scan, and the first scan's
	// result is rejected without touching the new set's hint.
	second, err := n.RegisterConf(&txid, script, 1, 95)
	require.NoError(t, err)
	fresh := second.HistoricalDispatch
	require.NotNil(t, fresh)
	before, err := cache.QueryConfirmHint(fresh.ConfRequest)
	require.NoError(t, err)
	err = n.UpdateConfDetails(stale, nil)
	require.ErrorIs(t, err, chainntnfs.ErrStaleScanResult)
	after, err := cache.QueryConfirmHint(fresh.ConfRequest)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// The new set's own scan still completes normally.
	details := &chainntnfs.TxConfirmation{
		BlockHash:   block.Hash(),
		BlockHeight: startHeight + 1,
		Tx:          tx,
	}
	require.NoError(t, n.UpdateConfDetails(fresh, details))
	select {
	case details := <-second.Event.Confirmed:
		require.Equal(t, startHeight+1, details.BlockHeight)
	default:
		t.Fatal("new subscriber missed confirmation")
	}
}
