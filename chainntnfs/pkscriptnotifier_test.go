package chainntnfs_test

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

var testRawScript2 = []byte{
	// OP_HASH160
	0xa9,
	// OP_DATA_20
	0x14,
	// <20-byte script hash>
	0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
	0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00,
	0x12, 0x34, 0x56, 0x78,
	// OP_EQUAL
	0x87,
}

// makeTestPkScripts creates count distinct scripts based on the test script
// template.
func makeTestPkScripts(count int) [][]byte {
	pkScripts := make([][]byte, count)
	for i := range pkScripts {
		pkScript := append([]byte(nil), testRawScript...)
		pkScript[2] = byte(i)
		pkScript[3] = byte(i >> 8)
		pkScript[4] = byte(i >> 16)
		pkScript[5] = byte(i >> 24)
		pkScripts[i] = pkScript
	}

	return pkScripts
}

// recvPkScriptNotifications reads count pkScript notifications from a
// registration.
func recvPkScriptNotifications(t *testing.T,
	reg *chainntnfs.PkScriptNotificationRegistration,
	count int) []*chainntnfs.PkScriptNotification {

	t.Helper()

	ntfns := make([]*chainntnfs.PkScriptNotification, 0, count)
	for i := 0; i < count; i++ {
		ntfns = append(ntfns, <-reg.Notifications)
	}

	return ntfns
}

// recvPkScriptNotificationTimeout reads one pkScript notification or fails the
// test on timeout.
func recvPkScriptNotificationTimeout(t *testing.T,
	reg *chainntnfs.PkScriptNotificationRegistration,
) *chainntnfs.PkScriptNotification {

	t.Helper()

	select {
	case ntfn, ok := <-reg.Notifications:
		require.True(t, ok, "pkScript notification channel closed")
		return ntfn

	case <-time.After(2 * time.Second):
		t.Fatal("pkScript notification not received")
		return nil
	}
}

// registerPkScriptNotifier registers scripts with a historical scan height on a
// TxNotifier.
func registerPkScriptNotifier(t *testing.T, n *chainntnfs.TxNotifier,
	pkScripts [][]byte, events chainntnfs.PkScriptEventType,
	numConfs, historicalScanFrom uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.PkScriptRegistration,
	*chainntnfs.HistoricalPkScriptDispatch) {

	t.Helper()

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	addOpts := []chainntnfs.NotifierOption{
		chainntnfs.WithEvents(events),
		chainntnfs.WithNumConfs(numConfs),
		chainntnfs.WithHistoricalScanFrom(historicalScanFrom),
	}
	addOpts = append(addOpts, opts...)

	dispatch, _, _, err := reg.AddPkScripts(pkScripts, addOpts...)
	require.NoError(t, err)

	return reg, dispatch
}

// registerFuturePkScriptNotifier registers scripts for future-only TxNotifier
// pkScript notifications.
func registerFuturePkScriptNotifier(t *testing.T, n *chainntnfs.TxNotifier,
	pkScripts [][]byte, events chainntnfs.PkScriptEventType,
	numConfs uint32,
	opts ...chainntnfs.NotifierOption) *chainntnfs.PkScriptRegistration {

	t.Helper()

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	addOpts := []chainntnfs.NotifierOption{
		chainntnfs.WithEvents(events),
		chainntnfs.WithNumConfs(numConfs),
	}
	addOpts = append(addOpts, opts...)

	_, _, _, err = reg.AddPkScripts(pkScripts, addOpts...)
	require.NoError(t, err)

	return reg
}

// TestTxNotifierPkScriptBatchLimit ensures add/remove mutations are bounded.
func TestTxNotifierPkScriptBatchLimit(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	oversizedBatch := makeTestPkScripts(chainntnfs.MaxPkScriptsPerBatch + 1)
	_, _, _, err = reg.AddPkScripts(oversizedBatch)
	require.True(t, errors.Is(err, chainntnfs.ErrTooManyPkScripts))

	err = reg.RemovePkScripts(oversizedBatch)
	require.True(t, errors.Is(err, chainntnfs.ErrTooManyPkScripts))
}

// TestTxNotifierLegacyNotificationsWithActivePkScriptNotifier ensures legacy
// confirmation and spend notifications still dispatch while pkScript
// subscriptions are active on the same block.
func TestTxNotifierLegacyNotificationsWithActivePkScriptNotifier(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	pkScriptReg := registerFuturePkScriptNotifier(
		t, n, [][]byte{testRawScript2},
		chainntnfs.PkScriptEventConfirm, 1,
	)

	confTx := wire.NewMsgTx(2)
	confTx.AddTxOut(&wire.TxOut{PkScript: testRawScript})
	confHash := confTx.TxHash()

	confNtfn, err := n.RegisterConf(&confHash, testRawScript, 1, 1)
	require.NoError(t, err)

	spendOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 0,
	}
	spendNtfn, err := n.RegisterSpend(
		&spendOutpoint, testRawScript, 1,
	)
	require.NoError(t, err)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: spendOutpoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})

	pkScriptTx := wire.NewMsgTx(2)
	pkScriptTx.AddTxOut(&wire.TxOut{PkScript: testRawScript2})
	pkScriptHash := pkScriptTx.TxHash()
	pkScriptOutpoint := wire.OutPoint{
		Hash:  pkScriptHash,
		Index: 0,
	}

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{confTx, spendTx, pkScriptTx},
	})

	err = n.ConnectTip(block11, 11)
	require.NoError(t, err)

	requirePkScriptNotification(
		t, recvPkScriptNotificationTimeout(t, pkScriptReg.Event),
		chainntnfs.PkScriptNotificationConfirm, false, 11, 1, 2,
		&pkScriptHash, block11.Hash(), pkScriptOutpoint,
	)

	err = n.NotifyHeight(11)
	require.NoError(t, err)

	select {
	case conf := <-confNtfn.Event.Confirmed:
		require.Equal(t, confHash, conf.Tx.TxHash())

	case <-time.After(2 * time.Second):
		t.Fatal("legacy confirmation notification not received")
	}

	select {
	case spend := <-spendNtfn.Event.Spend:
		require.Equal(t, spendOutpoint, *spend.SpentOutPoint)
		require.Equal(t, spendTx.TxHash(), *spend.SpenderTxHash)

	case <-time.After(2 * time.Second):
		t.Fatal("legacy spend notification not received")
	}
}

// TestTxNotifierHistoricalPkScriptDispatch tests that pkScript notifications are
// replayed correctly from historical blocks.
func TestTxNotifierHistoricalPkScriptDispatch(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		13, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm|
			chainntnfs.PkScriptEventSpend,
		2, 11, chainntnfs.WithIncludeBlock(),
		chainntnfs.WithIncludeTx(),
	)
	require.NotNil(t, dispatch)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendHash := spendTx.TxHash()

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})
	block12 := btcutil.NewBlock(&wire.MsgBlock{})
	block13 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})

	err := n.ProcessHistoricalPkScriptBlock(
		dispatch.SubscriptionID, block11, 11,
		dispatch.PkScripts,
	)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("received unexpected pkScript notification: %v", ntfn)
	default:
	}

	err = n.ProcessHistoricalPkScriptBlock(
		dispatch.SubscriptionID, block12, 12,
		dispatch.PkScripts,
	)
	require.NoError(t, err)

	confirmNtfn := <-reg.Event.Notifications
	requirePkScriptNotification(
		t, confirmNtfn,
		chainntnfs.PkScriptNotificationConfirm, false, 12, 2, 0,
		&receiveHash, block12.Hash(), receiveOutPoint,
	)
	require.NotNil(t, confirmNtfn.Tx)
	require.Equal(t, receiveHash, confirmNtfn.Tx.TxHash())
	require.NotNil(t, confirmNtfn.Block)
	require.Equal(t, *block12.Hash(), confirmNtfn.Block.BlockHash())

	err = n.ProcessHistoricalPkScriptBlock(
		dispatch.SubscriptionID, block13, 13,
		dispatch.PkScripts,
	)
	require.NoError(t, err)

	spendNtfn := <-reg.Event.Notifications
	requirePkScriptNotification(
		t, spendNtfn,
		chainntnfs.PkScriptNotificationSpend, false, 13, 0, 0,
		&spendHash, block13.Hash(), receiveOutPoint,
	)
	require.NotNil(t, spendNtfn.Tx)
	require.Equal(t, spendHash, spendNtfn.Tx.TxHash())
	require.NotNil(t, spendNtfn.Block)
	require.Equal(t, *block13.Hash(), spendNtfn.Block.BlockHash())
}

// TestTxNotifierHistoricalPkScriptScanComplete ensures historical pkScript scans
// emit a terminal lifecycle event after replayed notifications.
func TestTxNotifierHistoricalPkScriptScanComplete(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		6, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript}, chainntnfs.PkScriptEventConfirm,
		1, 5,
	)
	require.NotNil(t, dispatch)
	require.NotZero(t, dispatch.ScanID)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}
	block5 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})

	err := n.SyncHistoricalPkScriptDispatch(
		dispatch,
		func(height uint32) error {
			block := btcutil.NewBlock(&wire.MsgBlock{})
			if height == 5 {
				block = block5
			}

			return n.ProcessHistoricalPkScriptBlockWithDispatch(
				dispatch, block, height,
			)
		},
	)
	require.NoError(t, err)

	confirmNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, confirmNtfn, chainntnfs.PkScriptNotificationConfirm, false,
		5, 1, 0, &receiveHash, block5.Hash(), receiveOutPoint,
	)

	scanNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	require.Equal(
		t, chainntnfs.PkScriptNotificationHistoricalScanComplete,
		scanNtfn.Type,
	)
	require.NotNil(t, scanNtfn.HistoricalScan)
	require.Equal(t, dispatch.ScanID, scanNtfn.HistoricalScan.ScanID)
	require.Equal(t, uint32(5), scanNtfn.HistoricalScan.StartHeight)
	require.Equal(t, uint32(6), scanNtfn.HistoricalScan.EndHeight)
	require.Equal(t, uint32(6), scanNtfn.HistoricalScan.CompletedHeight)
	require.Empty(t, scanNtfn.HistoricalScan.Error)
}

// TestTxNotifierHistoricalPkScriptScanFailure ensures historical scan failures
// are surfaced to clients instead of only being logged by the backend.
func TestTxNotifierHistoricalPkScriptScanFailure(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		6, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript}, chainntnfs.PkScriptEventConfirm,
		1, 5,
	)
	require.NotNil(t, dispatch)

	expectedErr := errors.New("scan failed")
	err := n.SyncHistoricalPkScriptDispatch(
		dispatch,
		func(height uint32) error {
			if height == 6 {
				return expectedErr
			}

			return n.ProcessHistoricalPkScriptBlockWithDispatch(
				dispatch, btcutil.NewBlock(&wire.MsgBlock{}), height,
			)
		},
	)
	require.ErrorIs(t, err, expectedErr)

	scanNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	require.Equal(
		t, chainntnfs.PkScriptNotificationHistoricalScanComplete,
		scanNtfn.Type,
	)
	require.NotNil(t, scanNtfn.HistoricalScan)
	require.Equal(t, dispatch.ScanID, scanNtfn.HistoricalScan.ScanID)
	require.Equal(t, uint32(5), scanNtfn.HistoricalScan.CompletedHeight)
	require.Contains(t, scanNtfn.HistoricalScan.Error, expectedErr.Error())
}

// TestTxNotifierHistoricalPkScriptDispatchCatchesLiveTip ensures historical
// pkScript replay rescans live blocks that arrived while the replay was still
// catching up. This prevents a spend from being missed when its funding output
// is discovered by the historical replay after the live block was processed.
func TestTxNotifierHistoricalPkScriptDispatchCatchesLiveTip(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript}, chainntnfs.PkScriptEventSpend,
		0, 5,
	)
	require.NotNil(t, dispatch)
	require.Equal(t, uint32(10), dispatch.EndHeight)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendHash := spendTx.TxHash()

	block5 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})
	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})

	// The live spend is processed before the historical replay has
	// discovered the funding output, so it cannot be matched yet.
	err := n.ConnectTip(block11, 11)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("received unexpected pkScript notification: %v", ntfn)
	default:
	}

	err = n.SyncHistoricalPkScriptDispatch(
		dispatch,
		func(height uint32) error {
			block := btcutil.NewBlock(&wire.MsgBlock{})
			switch height {
			case 5:
				block = block5
			case 11:
				block = block11
			}

			return n.ProcessHistoricalPkScriptBlockWithDispatch(
				dispatch, block, height,
			)
		},
	)
	require.NoError(t, err)

	spendNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, spendNtfn, chainntnfs.PkScriptNotificationSpend, false,
		11, 0, 0, &spendHash, block11.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(0), spendNtfn.InputIndex)
}

// TestTxNotifierHistoricalPkScriptDispatchGatesLiveTipBeforeStart ensures live
// blocks for a historical add request are withheld even while the historical
// scan is still queued and has not started yet.
func TestTxNotifierHistoricalPkScriptDispatchGatesLiveTipBeforeStart(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript}, chainntnfs.PkScriptEventConfirm,
		1, 5,
	)
	require.NotNil(t, dispatch)
	require.Equal(t, uint32(10), dispatch.EndHeight)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}
	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})

	// The live block arrives before the queued historical scan starts. It must
	// not be delivered directly because older historical events for this script
	// may still need to be replayed first.
	err := n.ConnectTip(block11, 11)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("received unexpected live notification: %v", ntfn)

	case <-time.After(100 * time.Millisecond):
	}

	var scannedLiveBlock bool
	err = n.SyncHistoricalPkScriptDispatch(
		dispatch,
		func(height uint32) error {
			block := btcutil.NewBlock(&wire.MsgBlock{})
			if height == 11 {
				scannedLiveBlock = true
				block = block11
			}

			return n.ProcessHistoricalPkScriptBlockWithDispatch(
				dispatch, block, height,
			)
		},
	)
	require.NoError(t, err)
	require.True(t, scannedLiveBlock)

	confirmNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, confirmNtfn, chainntnfs.PkScriptNotificationConfirm, false,
		11, 1, 0, &receiveHash, block11.Hash(), receiveOutPoint,
	)
}

// TestTxNotifierStaleHistoricalDispatchDoesNotAffectReAdd ensures a historical
// scan queued for a removed script cannot replay or clear a later re-add of that
// same script.
func TestTxNotifierStaleHistoricalDispatchDoesNotAffectReAdd(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, oldDispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript}, chainntnfs.PkScriptEventConfirm,
		1, 5,
	)
	require.NotNil(t, oldDispatch)

	err := reg.RemovePkScripts([][]byte{testRawScript})
	require.NoError(t, err)

	newDispatch, _, _, err := reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
		chainntnfs.WithHistoricalScanFrom(8),
	)
	require.NoError(t, err)
	require.NotNil(t, newDispatch)

	var staleScanCalled bool
	err = n.SyncHistoricalPkScriptDispatch(
		oldDispatch,
		func(height uint32) error {
			staleScanCalled = true

			return nil
		},
	)
	require.NoError(t, err)
	require.False(t, staleScanCalled)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("stale historical dispatch sent notification: %v", ntfn)
	default:
	}

	liveTx := wire.NewMsgTx(2)
	liveTx.AddTxOut(&wire.TxOut{
		Value:    2000,
		PkScript: testRawScript,
	})
	liveHash := liveTx.TxHash()
	liveOutPoint := wire.OutPoint{Hash: liveHash, Index: 0}
	liveBlock := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{liveTx},
	})

	err = n.ConnectTip(liveBlock, 11)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("live notification was not gated: %v", ntfn)
	default:
	}

	err = n.SyncHistoricalPkScriptDispatch(
		newDispatch,
		func(height uint32) error {
			block := btcutil.NewBlock(&wire.MsgBlock{})
			if height == 11 {
				block = liveBlock
			}

			return n.ProcessHistoricalPkScriptBlockWithDispatch(
				newDispatch, block, height,
			)
		},
	)
	require.NoError(t, err)

	confirmNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, confirmNtfn, chainntnfs.PkScriptNotificationConfirm, false,
		11, 1, 0, &liveHash, liveBlock.Hash(), liveOutPoint,
	)
}

// TestTxNotifierHistoricalPkScriptDispatchDoesNotConfirmUnrelatedScripts ensures
// the confirmation pass at the end of each historical block only dispatches
// confirmations for scripts that belong to the historical add request.
func TestTxNotifierHistoricalPkScriptDispatchDoesNotConfirmUnrelatedScripts(
	t *testing.T) {

	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg := registerFuturePkScriptNotifier(
		t, n, [][]byte{testRawScript2},
		chainntnfs.PkScriptEventConfirm, 2,
	)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript2,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}
	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})

	err := n.ConnectTip(block11, 11)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("received unexpected confirmation: %v", ntfn)
	default:
	}

	dispatch, _, _, err := reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
		chainntnfs.WithNumConfs(1),
		chainntnfs.WithHistoricalScanFrom(0),
	)
	require.NoError(t, err)
	require.NotNil(t, dispatch)

	block12 := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ProcessHistoricalPkScriptBlockWithDispatch(dispatch, block12, 12)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("historical dispatch confirmed unrelated script: %v", ntfn)
	case <-time.After(100 * time.Millisecond):
	}

	err = n.ConnectTip(block12, 12)
	require.NoError(t, err)

	confirmNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, confirmNtfn, chainntnfs.PkScriptNotificationConfirm, false,
		12, 2, 0, &receiveHash, block12.Hash(), receiveOutPoint,
	)
}

// TestTxNotifierHistoricalPkScriptDispatchOrdersLiveTip ensures live blocks that
// arrive while a historical replay is active are replayed by the historical
// dispatcher, so notifications for that script remain height ordered.
func TestTxNotifierHistoricalPkScriptDispatchOrdersLiveTip(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm|
			chainntnfs.PkScriptEventSpend,
		2, 5,
	)
	require.NotNil(t, dispatch)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendHash := spendTx.TxHash()

	block5 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})
	block6 := btcutil.NewBlock(&wire.MsgBlock{})
	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})

	var connectedLiveTip bool
	err := n.SyncHistoricalPkScriptDispatch(
		dispatch,
		func(height uint32) error {
			block := btcutil.NewBlock(&wire.MsgBlock{})
			switch height {
			case 5:
				block = block5
			case 6:
				block = block6
			case 11:
				block = block11
			}

			err := n.ProcessHistoricalPkScriptBlockWithDispatch(
				dispatch, block, height,
			)
			if err != nil {
				return err
			}

			if height == 5 && !connectedLiveTip {
				connectedLiveTip = true
				return n.ConnectTip(block11, 11)
			}

			return nil
		},
	)
	require.NoError(t, err)

	confirmNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, confirmNtfn, chainntnfs.PkScriptNotificationConfirm, false,
		6, 2, 0, &receiveHash, block6.Hash(), receiveOutPoint,
	)

	spendNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, spendNtfn, chainntnfs.PkScriptNotificationSpend, false,
		11, 0, 0, &spendHash, block11.Hash(), receiveOutPoint,
	)
}

// TestTxNotifierHistoricalPkScriptDispatchMultipleScripts ensures all scripts
// supplied at registration are replayed from the provided height hint.
func TestTxNotifierHistoricalPkScriptDispatchMultipleScripts(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		12, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	scriptA := testRawScript
	scriptB := testRawScript2

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{scriptA, scriptB},
		chainntnfs.PkScriptEventConfirm|
			chainntnfs.PkScriptEventSpend,
		1, 11,
	)
	require.NotNil(t, dispatch)
	require.Len(t, dispatch.PkScripts, 2)

	receiveTxA := wire.NewMsgTx(2)
	receiveTxA.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: scriptA,
	})
	receiveHashA := receiveTxA.TxHash()
	receiveOutPointA := wire.OutPoint{Hash: receiveHashA, Index: 0}

	receiveTxB := wire.NewMsgTx(2)
	receiveTxB.AddTxOut(&wire.TxOut{
		Value:    2000,
		PkScript: scriptB,
	})
	receiveHashB := receiveTxB.TxHash()
	receiveOutPointB := wire.OutPoint{Hash: receiveHashB, Index: 0}

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTxA, receiveTxB},
	})

	err := n.ProcessHistoricalPkScriptBlock(
		dispatch.SubscriptionID, block11, 11,
		dispatch.PkScripts,
	)
	require.NoError(t, err)

	confirmNtfns := recvPkScriptNotifications(t, reg.Event, 2)
	seenConfirm := make(map[wire.OutPoint]struct{}, 2)
	for _, ntfn := range confirmNtfns {
		require.Equal(t, chainntnfs.PkScriptNotificationConfirm, ntfn.Type)
		switch ntfn.UTXO.OutPoint {
		case receiveOutPointA:
			requirePkScriptNotification(
				t, ntfn, chainntnfs.PkScriptNotificationConfirm,
				false, 11, 1, 0, &receiveHashA, block11.Hash(),
				receiveOutPointA,
			)
			seenConfirm[receiveOutPointA] = struct{}{}

		case receiveOutPointB:
			requirePkScriptNotification(
				t, ntfn, chainntnfs.PkScriptNotificationConfirm,
				false, 11, 1, 1, &receiveHashB, block11.Hash(),
				receiveOutPointB,
			)
			seenConfirm[receiveOutPointB] = struct{}{}
		}
	}
	require.Len(t, seenConfirm, 2)

	spendTxA := wire.NewMsgTx(2)
	spendTxA.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPointA,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendHashA := spendTxA.TxHash()

	spendTxB := wire.NewMsgTx(2)
	spendTxB.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPointB,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendHashB := spendTxB.TxHash()

	block12 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTxA, spendTxB},
	})

	err = n.ProcessHistoricalPkScriptBlock(
		dispatch.SubscriptionID, block12, 12,
		dispatch.PkScripts,
	)
	require.NoError(t, err)

	spendNtfns := recvPkScriptNotifications(t, reg.Event, 2)
	seenSpend := make(map[wire.OutPoint]struct{}, 2)
	for _, ntfn := range spendNtfns {
		require.Equal(t, chainntnfs.PkScriptNotificationSpend, ntfn.Type)
		switch ntfn.UTXO.OutPoint {
		case receiveOutPointA:
			requirePkScriptNotification(
				t, ntfn, chainntnfs.PkScriptNotificationSpend,
				false, 12, 0, 0, &spendHashA, block12.Hash(),
				receiveOutPointA,
			)
			seenSpend[receiveOutPointA] = struct{}{}

		case receiveOutPointB:
			requirePkScriptNotification(
				t, ntfn, chainntnfs.PkScriptNotificationSpend,
				false, 12, 0, 1, &spendHashB, block12.Hash(),
				receiveOutPointB,
			)
			seenSpend[receiveOutPointB] = struct{}{}
		}
	}
	require.Len(t, seenSpend, 2)
}

// TestTxNotifierPkScriptAddRemove ensures pkScripts can be added and removed on
// the fly.
func TestTxNotifierPkScriptAddRemove(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		12, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	dispatch, _, _, err := reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithHistoricalScanFrom(11),
	)
	require.NoError(t, err)
	require.NotNil(t, dispatch)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})

	err = n.ProcessHistoricalPkScriptBlock(
		dispatch.SubscriptionID, block11, 11, dispatch.PkScripts,
	)
	require.NoError(t, err)

	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationConfirm, false, 11, 1, 0,
		&receiveHash, block11.Hash(), receiveOutPoint,
	)

	err = reg.RemovePkScripts([][]byte{testRawScript})
	require.NoError(t, err)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	block13 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})

	err = n.ConnectTip(block13, 13)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("received unexpected pkScript notification: %v", ntfn)
	default:
	}
}

// TestTxNotifierPkScriptAddMultipleScriptsHistorical ensures AddPkScripts
// replays all provided scripts from the supplied height hint.
func TestTxNotifierPkScriptAddMultipleScriptsHistorical(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		12, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	dispatch, _, _, err := reg.AddPkScripts(
		[][]byte{testRawScript, testRawScript2},
		chainntnfs.WithHistoricalScanFrom(11),
	)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	require.Len(t, dispatch.PkScripts, 2)

	receiveTxA := wire.NewMsgTx(2)
	receiveTxA.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHashA := receiveTxA.TxHash()
	receiveOutPointA := wire.OutPoint{Hash: receiveHashA, Index: 0}

	receiveTxB := wire.NewMsgTx(2)
	receiveTxB.AddTxOut(&wire.TxOut{
		Value:    2000,
		PkScript: testRawScript2,
	})
	receiveHashB := receiveTxB.TxHash()
	receiveOutPointB := wire.OutPoint{Hash: receiveHashB, Index: 0}

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTxA, receiveTxB},
	})

	err = n.ProcessHistoricalPkScriptBlock(
		dispatch.SubscriptionID, block11, 11, dispatch.PkScripts,
	)
	require.NoError(t, err)

	confirmNtfns := recvPkScriptNotifications(t, reg.Event, 2)
	seenConfirm := make(map[wire.OutPoint]struct{}, 2)
	for _, ntfn := range confirmNtfns {
		require.Equal(t, chainntnfs.PkScriptNotificationConfirm, ntfn.Type)
		switch ntfn.UTXO.OutPoint {
		case receiveOutPointA:
			requirePkScriptNotification(
				t, ntfn, chainntnfs.PkScriptNotificationConfirm,
				false, 11, 1, 0, &receiveHashA, block11.Hash(),
				receiveOutPointA,
			)
			seenConfirm[receiveOutPointA] = struct{}{}

		case receiveOutPointB:
			requirePkScriptNotification(
				t, ntfn, chainntnfs.PkScriptNotificationConfirm,
				false, 11, 1, 1, &receiveHashB, block11.Hash(),
				receiveOutPointB,
			)
			seenConfirm[receiveOutPointB] = struct{}{}
		}
	}
	require.Len(t, seenConfirm, 2)
}

// TestTxNotifierPkScriptSkipHistoricalScan ensures pkScript registrations can opt
// out of historical replay while still receiving future notifications.
func TestTxNotifierPkScriptSkipHistoricalScan(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		12, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg := registerFuturePkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm|
			chainntnfs.PkScriptEventSpend,
		1,
	)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	block13 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})
	err := n.ConnectTip(block13, 13)
	require.NoError(t, err)

	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationConfirm, false, 13, 1, 0,
		&receiveHash, block13.Hash(), receiveOutPoint,
	)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendHash := spendTx.TxHash()

	block14 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})
	err = n.ConnectTip(block14, 14)
	require.NoError(t, err)

	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationSpend, false, 14, 0, 0,
		&spendHash, block14.Hash(), receiveOutPoint,
	)
}

// TestTxNotifierPkScriptAddSkipHistoricalScan ensures scripts added to an
// existing subscription can opt out of historical replay independently.
func TestTxNotifierPkScriptAddSkipHistoricalScan(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		12, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	dispatch, _, _, err := reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
	)
	require.NoError(t, err)
	require.Nil(t, dispatch)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	block13 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})
	err = n.ConnectTip(block13, 13)
	require.NoError(t, err)

	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationConfirm, false, 13, 1, 0,
		&receiveHash, block13.Hash(), receiveOutPoint,
	)
}

// TestTxNotifierHistoricalPkScriptDispatchCachedSetRespectsRemove ensures the
// cached historical script set doesn't resurrect scripts removed mid-replay.
func TestTxNotifierHistoricalPkScriptDispatchCachedSetRespectsRemove(
	t *testing.T) {

	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		12, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript, testRawScript2},
		chainntnfs.PkScriptEventConfirm, 1, 11,
	)
	require.NotNil(t, dispatch)

	err := reg.RemovePkScripts([][]byte{testRawScript2})
	require.NoError(t, err)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript2,
	})
	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})

	err = n.ProcessHistoricalPkScriptBlockWithScriptSet(
		dispatch.SubscriptionID, block11, 11,
		dispatch.PkScriptSet(), nil,
	)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("received unexpected pkScript notification: %v", ntfn)
	default:
	}
}

// TestTxNotifierPkScriptReorg ensures pkScript notifications are invalidated and
// redelivered across reorgs.
func TestTxNotifierPkScriptReorg(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg := registerFuturePkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm|
			chainntnfs.PkScriptEventSpend,
		2,
	)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendHash := spendTx.TxHash()

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})
	block12 := btcutil.NewBlock(&wire.MsgBlock{})
	block13 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})
	block12Reorg := btcutil.NewBlock(&wire.MsgBlock{})
	block13Reorg := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})

	err := n.ConnectTip(block11, 11)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("received unexpected pkScript notification: %v", ntfn)
	default:
	}

	err = n.ConnectTip(block12, 12)
	require.NoError(t, err)
	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationConfirm, false, 12, 2, 0,
		&receiveHash, block12.Hash(), receiveOutPoint,
	)

	err = n.ConnectTip(block13, 13)
	require.NoError(t, err)
	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationSpend, false, 13, 0, 0,
		&spendHash, block13.Hash(), receiveOutPoint,
	)

	err = n.DisconnectTip(13)
	require.NoError(t, err)
	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationSpend, true, 13, 0, 0,
		&spendHash, block13.Hash(), receiveOutPoint,
	)

	err = n.DisconnectTip(12)
	require.NoError(t, err)
	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationConfirm, true, 12, 2, 0,
		&receiveHash, block12.Hash(), receiveOutPoint,
	)

	err = n.ConnectTip(block12Reorg, 12)
	require.NoError(t, err)
	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationConfirm, false, 12, 2, 0,
		&receiveHash, block12Reorg.Hash(), receiveOutPoint,
	)

	err = n.ConnectTip(block13Reorg, 13)
	require.NoError(t, err)
	requirePkScriptNotification(
		t, <-reg.Event.Notifications,
		chainntnfs.PkScriptNotificationSpend, false, 13, 0, 0,
		&spendHash, block13Reorg.Hash(), receiveOutPoint,
	)
}

// TestTxNotifierPkScriptPartialConfirmUpdates ensures pkScript registrations can
// opt in to confirmation progress before the final confirmation notification.
func TestTxNotifierPkScriptPartialConfirmUpdates(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg := registerFuturePkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm, 3,
		chainntnfs.WithIncludeConfirmationUpdates(),
	)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})
	err := n.ConnectTip(block11, 11)
	require.NoError(t, err)

	update1 := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, update1, chainntnfs.PkScriptNotificationConfirmUpdate,
		false, 11, 1, 0, &receiveHash, block11.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(3), update1.RequiredConfs)

	block12 := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(block12, 12)
	require.NoError(t, err)

	update2 := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, update2, chainntnfs.PkScriptNotificationConfirmUpdate,
		false, 12, 2, 0, &receiveHash, block12.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(3), update2.RequiredConfs)

	err = n.DisconnectTip(12)
	require.NoError(t, err)

	update2Reorg := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, update2Reorg, chainntnfs.PkScriptNotificationConfirmUpdate,
		true, 12, 2, 0, &receiveHash, block12.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(3), update2Reorg.RequiredConfs)

	block12Reorg := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(block12Reorg, 12)
	require.NoError(t, err)

	update2Redelivered := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, update2Redelivered,
		chainntnfs.PkScriptNotificationConfirmUpdate, false, 12, 2,
		0, &receiveHash, block12Reorg.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(3), update2Redelivered.RequiredConfs)

	block13 := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(block13, 13)
	require.NoError(t, err)

	finalNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, finalNtfn, chainntnfs.PkScriptNotificationConfirm, false,
		13, 3, 0, &receiveHash, block13.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(3), finalNtfn.RequiredConfs)
}

// TestTxNotifierPkScriptNotificationQueueDoesNotBlock ensures pkScript
// notifications are queued outside of the TxNotifier lock, so a slow client
// cannot stall chain processing or cancellation.
func TestTxNotifierPkScriptNotificationQueueDoesNotBlock(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg := registerFuturePkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm, 1,
	)

	const numOutputs = 150
	receiveTx := wire.NewMsgTx(2)
	for i := 0; i < numOutputs; i++ {
		receiveTx.AddTxOut(&wire.TxOut{
			Value:    int64(1000 + i),
			PkScript: testRawScript,
		})
	}

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})

	connectDone := make(chan error, 1)
	go func() {
		connectDone <- n.ConnectTip(block11, 11)
	}()

	select {
	case err := <-connectDone:
		require.NoError(t, err)

	case <-time.After(2 * time.Second):
		t.Fatal("ConnectTip blocked on pkScript notification delivery")
	}

	cancelDone := make(chan struct{})
	go func() {
		reg.Event.Cancel()
		close(cancelDone)
	}()

	select {
	case <-cancelDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pkScript notification cancellation blocked")
	}
}

// TestTxNotifierPkScriptNotificationQueueOverflowCancels ensures a slow
// consumer is canceled instead of allowing its notification queue to grow without
// bound.
func TestTxNotifierPkScriptNotificationQueueOverflowCancels(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg := registerFuturePkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm, 1,
	)

	numOutputs := cap(reg.Event.Notifications) +
		chainntnfs.MaxPkScriptNotificationQueueSize + 10
	receiveTx := wire.NewMsgTx(2)
	for i := 0; i < numOutputs; i++ {
		receiveTx.AddTxOut(&wire.TxOut{
			Value:    int64(1000 + i),
			PkScript: testRawScript,
		})
	}

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})

	err := n.ConnectTip(block11, 11)
	require.NoError(t, err)

	for {
		select {
		case _, ok := <-reg.Event.Notifications:
			if !ok {
				_, _, _, err = reg.AddPkScripts(
					[][]byte{testRawScript2},
				)
				require.Error(t, err)

				return
			}

		case <-time.After(2 * time.Second):
			t.Fatal("expected overflowing notification channel to close")
		}
	}
}

// TestTxNotifierPkScriptAddExistingScriptNoop ensures re-adding an already
// watched script is a no-op and does not rewind its height hint.
func TestTxNotifierPkScriptAddExistingScriptNoop(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, dispatch := registerPkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm, 1, 11,
	)
	require.Nil(t, dispatch)

	dispatch, _, _, err := reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
		chainntnfs.WithHistoricalScanFrom(5),
	)
	require.NoError(t, err)
	require.Nil(t, dispatch)

	err = reg.RemovePkScripts([][]byte{testRawScript})
	require.NoError(t, err)

	dispatch, _, _, err = reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
		chainntnfs.WithHistoricalScanFrom(5),
	)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	require.Equal(t, uint32(5), dispatch.StartHeight)
	require.Equal(t, uint32(10), dispatch.EndHeight)
	require.Len(t, dispatch.PkScripts, 1)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: testRawScript,
	})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 0}

	block5 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx},
	})

	err = n.ProcessHistoricalPkScriptBlock(
		dispatch.SubscriptionID, block5, 5, dispatch.PkScripts,
	)
	require.NoError(t, err)

	ntfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, ntfn, chainntnfs.PkScriptNotificationConfirm, false, 5,
		1, 0, &receiveHash, block5.Hash(), receiveOutPoint,
	)
	require.NotNil(t, ntfn.UTXO.BlockHash)
	require.True(t, ntfn.UTXO.BlockHash.IsEqual(block5.Hash()))
	require.Equal(t, uint32(0), ntfn.UTXO.TxIndex)
}

// TestTxNotifierPkScriptTransactionIndexes ensures pkScript notifications report
// transaction indexes separately from output and input indexes.
func TestTxNotifierPkScriptTransactionIndexes(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg := registerFuturePkScriptNotifier(
		t, n,
		[][]byte{testRawScript},
		chainntnfs.PkScriptEventConfirm|
			chainntnfs.PkScriptEventSpend,
		1,
	)

	dummyTx1 := wire.NewMsgTx(2)
	dummyTx1.AddTxOut(&wire.TxOut{Value: 1, PkScript: testRawScript2})
	dummyTx2 := wire.NewMsgTx(2)
	dummyTx2.AddTxOut(&wire.TxOut{Value: 2, PkScript: testRawScript2})

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: testRawScript2})
	receiveTx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: testRawScript})
	receiveHash := receiveTx.TxHash()
	receiveOutPoint := wire.OutPoint{Hash: receiveHash, Index: 1}

	block11 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{dummyTx1, dummyTx2, receiveTx},
	})

	err := n.ConnectTip(block11, 11)
	require.NoError(t, err)

	confirmNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, confirmNtfn, chainntnfs.PkScriptNotificationConfirm, false,
		11, 1, 2, &receiveHash, block11.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(2), confirmNtfn.UTXO.TxIndex)
	require.NotNil(t, confirmNtfn.UTXO.BlockHash)
	require.True(t, confirmNtfn.UTXO.BlockHash.IsEqual(block11.Hash()))

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainntnfs.ZeroHash},
	})
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	spendHash := spendTx.TxHash()

	spendDummy := wire.NewMsgTx(2)
	spendDummy.AddTxOut(&wire.TxOut{Value: 3, PkScript: testRawScript2})

	block12 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendDummy, spendTx},
	})

	err = n.ConnectTip(block12, 12)
	require.NoError(t, err)

	spendNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, spendNtfn, chainntnfs.PkScriptNotificationSpend, false,
		12, 0, 1, &spendHash, block12.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(1), spendNtfn.TxIndex)
	require.Equal(t, uint32(1), spendNtfn.InputIndex)
}

// TestTxNotifierPkScriptTearDown ensures that tearing down the TxNotifier closes
// pkScript notification streams and rejects new pkScript registrations.
func TestTxNotifierPkScriptTearDown(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	pkScriptNtfn, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err, "unable to register pkScript ntfn")

	n.TearDown()

	select {
	case _, ok := <-pkScriptNtfn.Event.Notifications:
		require.False(
			t, ok, "expected closed Notifications channel for "+
				"pkScript ntfn",
		)

	default:
		t.Fatalf("expected closed notification channel for pkScript ntfn")
	}

	_, err = n.RegisterPkScriptNotifier()
	require.ErrorIs(t, err, chainntnfs.ErrTxNotifierExiting)
}

// requirePkScriptNotification checks the common fields of a pkScript
// notification.
func requirePkScriptNotification(t *testing.T,
	result *chainntnfs.PkScriptNotification,
	expectedType chainntnfs.PkScriptNotificationType,
	disconnected bool, height, numConfs, txIndex uint32,
	txHash, blockHash *chainhash.Hash, outpoint wire.OutPoint) {

	t.Helper()

	require.NotNil(t, result)
	require.Equal(t, expectedType, result.Type)
	require.Equal(t, disconnected, result.Disconnected)
	require.Equal(t, height, result.Height)
	require.Equal(t, numConfs, result.NumConfirmations)
	require.Equal(t, txIndex, result.TxIndex)

	if txHash == nil {
		require.Nil(t, result.TxHash)
	} else {
		require.NotNil(t, result.TxHash)
		require.True(t, result.TxHash.IsEqual(txHash))
	}

	if blockHash == nil {
		require.Nil(t, result.BlockHash)
	} else {
		require.NotNil(t, result.BlockHash)
		require.True(t, result.BlockHash.IsEqual(blockHash))
	}

	require.NotNil(t, result.UTXO)
	require.Equal(t, outpoint, result.UTXO.OutPoint)
}
