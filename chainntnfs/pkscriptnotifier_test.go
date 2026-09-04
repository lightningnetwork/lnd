package chainntnfs_test

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
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

	_, _, err = reg.AddPkScripts(pkScripts, addOpts...)
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
	_, _, err = reg.AddPkScripts(oversizedBatch)
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

// TestTxNotifierPkScriptAddRemove ensures pkScripts can be added and removed on
// the fly.
func TestTxNotifierPkScriptAddRemove(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	_, _, err = reg.AddPkScripts(
		[][]byte{testRawScript},
	)
	require.NoError(t, err)

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

	err = n.ConnectTip(block11, 11)
	require.NoError(t, err)

	requirePkScriptNotification(
		t, recvPkScriptNotificationTimeout(t, reg.Event),
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

	err = n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{}), 12)
	require.NoError(t, err)
	err = n.ConnectTip(block13, 13)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("received unexpected pkScript notification: %v", ntfn)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestTxNotifierPkScriptAddMultipleScripts ensures AddPkScripts watches all
// newly added scripts for future blocks.
func TestTxNotifierPkScriptAddMultipleScripts(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	_, addedScripts, err := reg.AddPkScripts(
		[][]byte{testRawScript, testRawScript2},
	)
	require.NoError(t, err)
	require.Len(t, addedScripts, 2)

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

	err = n.ConnectTip(block11, 11)
	require.NoError(t, err)

	confirmNtfns := recvPkScriptNotifications(t, reg.Event, 2)
	seenConfirm := make(map[wire.OutPoint]struct{}, 2)
	for _, ntfn := range confirmNtfns {
		require.Equal(
			t, chainntnfs.PkScriptNotificationConfirm, ntfn.Type,
		)
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

// TestTxNotifierPkScriptFutureNotifications ensures pkScript registrations
// receive future confirmation and spend notifications.
func TestTxNotifierPkScriptFutureNotifications(t *testing.T) {
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

// TestTxNotifierPkScriptAddFutureOnly ensures scripts added to an existing
// subscription watch future blocks.
func TestTxNotifierPkScriptAddFutureOnly(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		12, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	_, addedScripts, err := reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
	)
	require.NoError(t, err)
	require.Len(t, addedScripts, 1)

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

// TestTxNotifierPkScriptReorg ensures pkScript notifications are
// invalidated and redelivered across reorgs.
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

// TestTxNotifierPkScriptPartialConfirmUpdates ensures pkScript
// registrations can opt in to confirmation progress before the final
// confirmation notification.
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
		chainntnfs.WithIncludeBlock(),
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
	require.NotNil(t, update2.Block)
	originalNonce := update2.Block.Header.Nonce
	update2.Block.Header.Nonce++

	err = n.DisconnectTip(12)
	require.NoError(t, err)

	update2Reorg := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, update2Reorg, chainntnfs.PkScriptNotificationConfirmUpdate,
		true, 12, 2, 0, &receiveHash, block12.Hash(), receiveOutPoint,
	)
	require.Equal(t, uint32(3), update2Reorg.RequiredConfs)
	require.NotNil(t, update2Reorg.Block)
	require.Equal(t, originalNonce, update2Reorg.Block.Header.Nonce)

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
// consumer is canceled instead of allowing its notification queue to grow
// without bound.
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
				_, _, err = reg.AddPkScripts(
					[][]byte{testRawScript2},
				)
				require.Error(t, err)

				return
			}

		case <-time.After(2 * time.Second):
			t.Fatal("expected overflowing notification channel " +
				"to close")
		}
	}
}

// TestTxNotifierPkScriptAddExistingScriptNoop ensures re-adding an already
// watched script is a no-op.
func TestTxNotifierPkScriptAddExistingScriptNoop(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		10, chainntnfs.ReorgSafetyLimit, hintCache, hintCache,
	)

	reg, err := n.RegisterPkScriptNotifier()
	require.NoError(t, err)

	addHeight, addedScripts, err := reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
		chainntnfs.WithNumConfs(1),
	)
	require.NoError(t, err)
	require.Equal(t, uint32(10), addHeight)
	require.Len(t, addedScripts, 1)

	addHeight, addedScripts, err = reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
	)
	require.NoError(t, err)
	require.Equal(t, uint32(10), addHeight)
	require.Empty(t, addedScripts)

	err = reg.RemovePkScripts([][]byte{testRawScript})
	require.NoError(t, err)

	addHeight, addedScripts, err = reg.AddPkScripts(
		[][]byte{testRawScript},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
	)
	require.NoError(t, err)
	require.Equal(t, uint32(10), addHeight)
	require.Len(t, addedScripts, 1)

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

	err = n.ConnectTip(block11, 11)
	require.NoError(t, err)

	ntfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, ntfn, chainntnfs.PkScriptNotificationConfirm, false, 11,
		1, 0, &receiveHash, block11.Hash(), receiveOutPoint,
	)
	require.NotNil(t, ntfn.UTXO.BlockHash)
	require.True(t, ntfn.UTXO.BlockHash.IsEqual(block11.Hash()))
	require.Equal(t, uint32(0), ntfn.UTXO.TxIndex)
}

// TestTxNotifierPkScriptNotificationPayloadDetached ensures callers cannot
// mutate notifier state through delivered notification payloads.
func TestTxNotifierPkScriptNotificationPayloadDetached(t *testing.T) {
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

	ntfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, ntfn, chainntnfs.PkScriptNotificationConfirm, false, 11,
		1, 0, &receiveHash, block11.Hash(), receiveOutPoint,
	)
	require.NotEmpty(t, ntfn.UTXO.PkScript)

	ntfn.UTXO.PkScript[0] ^= 0xff

	err = reg.RemovePkScripts([][]byte{testRawScript})
	require.NoError(t, err)

	_, _, err = reg.AddPkScripts(
		[][]byte{testRawScript2},
		chainntnfs.WithEvents(chainntnfs.PkScriptEventConfirm),
		chainntnfs.WithNumConfs(1),
	)
	require.NoError(t, err)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: receiveOutPoint,
		Witness:          testWitness,
		SignatureScript:  testSigScript,
	})
	block12 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})

	err = n.ConnectTip(block12, 12)
	require.NoError(t, err)

	select {
	case ntfn := <-reg.Event.Notifications:
		t.Fatalf("removed script still dispatched spend: %v", ntfn)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestTxNotifierPkScriptSameBlockConfirmBeforeSpend ensures a same-block
// child spend is delivered after the watched output's confirmation.
func TestTxNotifierPkScriptSameBlockConfirmBeforeSpend(t *testing.T) {
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
		Transactions: []*wire.MsgTx{receiveTx, spendTx},
	})

	err := n.ConnectTip(block11, 11)
	require.NoError(t, err)

	confirmNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, confirmNtfn, chainntnfs.PkScriptNotificationConfirm,
		false, 11, 1, 0, &receiveHash, block11.Hash(),
		receiveOutPoint,
	)

	spendNtfn := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, spendNtfn, chainntnfs.PkScriptNotificationSpend,
		false, 11, 0, 1, &spendHash, block11.Hash(),
		receiveOutPoint,
	)

	err = n.DisconnectTip(11)
	require.NoError(t, err)

	spendReorg := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, spendReorg, chainntnfs.PkScriptNotificationSpend,
		true, 11, 0, 1, &spendHash, block11.Hash(),
		receiveOutPoint,
	)

	confirmReorg := recvPkScriptNotificationTimeout(t, reg.Event)
	requirePkScriptNotification(
		t, confirmReorg, chainntnfs.PkScriptNotificationConfirm,
		true, 11, 1, 0, &receiveHash, block11.Hash(),
		receiveOutPoint,
	)
}

// TestTxNotifierPkScriptTransactionIndexes ensures pkScript notifications
// report transaction indexes separately from output and input indexes.
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

// TestTxNotifierPkScriptTearDown ensures that tearing down the TxNotifier
// closes pkScript notification streams and rejects new registrations.
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
		t.Fatalf("pkScript notification channel remains open")
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
