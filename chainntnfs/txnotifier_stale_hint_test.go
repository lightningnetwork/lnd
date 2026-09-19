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
func TestTxNotifierStaleConfirmHintAfterRestart(t *testing.T) {
	const (
		startHeight = uint32(100)
		oldTip      = uint32(103)
		newTip      = uint32(102)
		heightHint  = uint32(1)
	)

	program := sha256.Sum256([]byte("stale-confirm-hint"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})
	txid := tx.TxHash()
	cache := newMockHintCache()

	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	registration, err := n.RegisterConf(&txid, script, 1, heightHint)
	require.NoError(t, err)
	require.NoError(t, n.UpdateConfDetails(
		registration.HistoricalDispatch.ConfRequest, nil,
	))
	advanceEmptyChain(t, n, startHeight+1, oldTip)
	registration.Event.Cancel()
	_, err = cache.QueryConfirmHint(
		registration.HistoricalDispatch.ConfRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
	n.TearDown()

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

	registration, err = n.RegisterConf(&txid, script, 1, heightHint)
	require.NoError(t, err)
	require.NotNil(t, registration.HistoricalDispatch)
	require.Equal(
		t, heightHint, registration.HistoricalDispatch.StartHeight,
	)
}

// TestTxNotifierStaleSpendHintAfterRestart asserts that a cached hint from a
// pre-reorg chain cannot hide a spend on the replacement chain.
func TestTxNotifierStaleSpendHintAfterRestart(t *testing.T) {
	const (
		startHeight = uint32(100)
		oldTip      = uint32(103)
		newTip      = uint32(102)
		heightHint  = uint32(1)
	)

	program := sha256.Sum256([]byte("stale-spend-hint"))
	script := append([]byte{0x51, 0x20}, program[:]...)
	outpoint := wire.OutPoint{
		Hash:  chainntnfs.ZeroHash,
		Index: 1,
	}
	cache := newMockHintCache()

	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	registration, err := n.RegisterSpend(&outpoint, script, heightHint)
	require.NoError(t, err)
	require.NoError(t, n.UpdateSpendDetails(
		registration.HistoricalDispatch.SpendRequest, nil,
	))
	advanceEmptyChain(t, n, startHeight+1, oldTip)
	registration.Event.Cancel()
	_, err = cache.QuerySpendHint(
		registration.HistoricalDispatch.SpendRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)
	n.TearDown()

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

	registration, err = n.RegisterSpend(&outpoint, script, heightHint)
	require.NoError(t, err)
	require.NotNil(t, registration.HistoricalDispatch)
	require.Equal(
		t, heightHint, registration.HistoricalDispatch.StartHeight,
	)
}

// TestTxNotifierCanceledPendingConfirmScan asserts that a completed historical
// callback cannot recreate a purged hint for a subscriberless request.
func TestTxNotifierCanceledPendingConfirmScan(t *testing.T) {
	const startHeight = uint32(100)

	script, tx := staleHintTx("pending-confirm-cleanup")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	registration, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	dispatch := registration.HistoricalDispatch
	require.NotNil(t, dispatch)
	registration.Event.Cancel()

	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	_, err = cache.QueryConfirmHint(dispatch.ConfRequest)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
	require.NoError(t, n.UpdateConfDetails(dispatch.ConfRequest, nil))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	_, err = cache.QueryConfirmHint(dispatch.ConfRequest)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)
}

// TestTxNotifierCanceledPendingConfirmReorg asserts that cancellation retains
// reorg tracking until the outstanding historical callback has returned.
func TestTxNotifierCanceledPendingConfirmReorg(t *testing.T) {
	const startHeight = uint32(100)

	script, tx := staleHintTx("pending-confirm-reorg")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

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

	second, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	assertNoConfirmation(t, second.Event)
	require.NoError(t, n.UpdateConfDetails(dispatch.ConfRequest, nil))
	require.NoError(t, n.ConnectTip(
		btcutil.NewBlock(&wire.MsgBlock{}), startHeight+1,
	))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	assertNoConfirmation(t, second.Event)
}

// TestTxNotifierCanceledPendingSpendScan asserts that a completed historical
// callback cannot recreate a purged hint for a subscriberless request.
func TestTxNotifierCanceledPendingSpendScan(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("pending-spend-cleanup")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	registration, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := registration.HistoricalDispatch
	require.NotNil(t, dispatch)
	registration.Event.Cancel()
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	_, err = cache.QuerySpendHint(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)
	require.NoError(t, n.UpdateSpendDetails(dispatch.SpendRequest, nil))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	_, err = cache.QuerySpendHint(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)
}

// TestTxNotifierCanceledPendingSpendReorg asserts that cancellation retains
// reorg tracking until the outstanding historical callback has returned.
func TestTxNotifierCanceledPendingSpendReorg(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("pending-spend-reorg")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

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
	_, err = cache.QuerySpendHint(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)

	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	assertNoSpend(t, second.Event)
	require.NoError(t, n.UpdateSpendDetails(dispatch.SpendRequest, nil))
	advanceEmptyChain(t, n, startHeight, startHeight+1)
	assertNoSpend(t, second.Event)
}

// TestTxNotifierCanceledPrefixResultReorg asserts that a positive prefix scan
// retained without subscribers is still invalidated by a reorg.
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

	first, err := n.RegisterConf(&txid, script, 1, 50)
	require.NoError(t, err)
	require.NotNil(t, first.HistoricalDispatch)
	second, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, second.HistoricalDispatch)
	first.Event.Cancel()
	second.Event.Cancel()

	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})
	require.NoError(t, n.UpdateConfDetails(
		second.HistoricalDispatch.ConfRequest,
		&chainntnfs.TxConfirmation{
			BlockHash:   block.Hash(),
			BlockHeight: txHeight,
			Tx:          tx,
		},
	))
	for height := startHeight; height >= txHeight-1; height-- {
		require.NoError(t, n.DisconnectTip(height))
	}
	_, err = cache.QueryConfirmHint(
		second.HistoricalDispatch.ConfRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)

	third, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.Nil(t, third.HistoricalDispatch)
	assertNoConfirmation(t, third.Event)
	require.NoError(t, n.UpdateConfDetails(
		first.HistoricalDispatch.ConfRequest, nil,
	))
	advanceEmptyChain(t, n, txHeight-1, txHeight)
	assertNoConfirmation(t, third.Event)
}

// TestTxNotifierRelevantSpendPreservesPendingScan asserts that a spend learned
// outside the historical callback does not release that callback's ownership.
func TestTxNotifierRelevantSpendPreservesPendingScan(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("relevant-pending-spend")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

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

	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	select {
	case details := <-second.Event.Spend:
		require.Equal(t, int32(startHeight+1), details.SpendingHeight)
	default:
		t.Fatal("re-registration missed relevant spend")
	}
	require.NoError(t, n.UpdateSpendDetails(dispatch.SpendRequest, nil))
}

// TestTxNotifierCanceledPendingRelevantSpend asserts that a relevant spend
// retains an empty set until its historical callback has returned.
func TestTxNotifierCanceledPendingRelevantSpend(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("canceled-relevant-spend")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	first.Event.Cancel()
	advanceEmptyChain(t, n, startHeight+1, startHeight+1)
	require.NoError(t, n.ProcessRelevantSpendTx(
		btcutil.NewTx(tx), startHeight+1,
	))

	second, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.Nil(t, second.HistoricalDispatch)
	select {
	case details := <-second.Event.Spend:
		require.Equal(t, int32(startHeight+1), details.SpendingHeight)
	default:
		t.Fatal("re-registration missed relevant spend")
	}
	require.NoError(t, n.UpdateSpendDetails(dispatch.SpendRequest, nil))
}

// TestTxNotifierRepeatedScriptSpendCleanup asserts that a script-only request
// retains only its first spend and its matching reorg index.
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

	registration, err := n.RegisterSpend(nil, script, 1)
	require.NoError(t, err)
	require.NoError(t, n.UpdateSpendDetails(
		registration.HistoricalDispatch.SpendRequest, nil,
	))
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

	registration.Event.Cancel()
	require.NoError(t, n.DisconnectTip(startHeight+2))
	require.NoError(t, n.DisconnectTip(startHeight+1))
}

// TestTxNotifierCanceledConfirmResultSurvives asserts that a positive result
// from a canceled historical scan remains available to live-process polling.
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

	first, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	first.Event.Cancel()

	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	})
	require.NoError(t, n.UpdateConfDetails(
		dispatch.ConfRequest, &chainntnfs.TxConfirmation{
			BlockHash:   block.Hash(),
			BlockHeight: txHeight,
			Tx:          tx,
		},
	))
	_, err = cache.QueryConfirmHint(dispatch.ConfRequest)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)

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

	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	dispatch := first.HistoricalDispatch
	require.NotNil(t, dispatch)
	first.Event.Cancel()

	spenderHash := tx.TxHash()
	require.NoError(t, n.UpdateSpendDetails(
		dispatch.SpendRequest, &chainntnfs.SpendDetail{
			SpentOutPoint:  &outpoint,
			SpenderTxHash:  &spenderHash,
			SpendingTx:     tx,
			SpendingHeight: spendHeight,
		},
	))
	_, err = cache.QuerySpendHint(dispatch.SpendRequest)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)

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
// preserves live-process polling while a reorg still invalidates the details.
func TestTxNotifierKnownConfirmSurvivesCancellation(t *testing.T) {
	const startHeight = uint32(100)

	script, tx := staleHintTx("known-confirm-cancellation")
	txid := tx.TxHash()
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	first, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NoError(t, n.UpdateConfDetails(
		first.HistoricalDispatch.ConfRequest, nil,
	))
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	first.Event.Cancel()
	_, err = cache.QueryConfirmHint(
		first.HistoricalDispatch.ConfRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrConfirmHintNotFound)

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

	third, err := n.RegisterConf(&txid, script, 1, 1)
	require.NoError(t, err)
	require.NotNil(t, third.HistoricalDispatch)
}

// TestTxNotifierKnownSpendSurvivesCancellation asserts that cancellation
// preserves live-process polling while a reorg still invalidates the details.
func TestTxNotifierKnownSpendSurvivesCancellation(t *testing.T) {
	const startHeight = uint32(100)

	outpoint, script, tx := staleHintSpend("known-spend-cancellation")
	cache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(
		startHeight, chainntnfs.ReorgSafetyLimit, cache, cache,
	)
	t.Cleanup(n.TearDown)

	first, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.NoError(t, n.UpdateSpendDetails(
		first.HistoricalDispatch.SpendRequest, nil,
	))
	require.NoError(t, n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), startHeight+1))
	require.NoError(t, n.NotifyHeight(startHeight+1))
	first.Event.Cancel()
	_, err = cache.QuerySpendHint(
		first.HistoricalDispatch.SpendRequest,
	)
	require.ErrorIs(t, err, chainntnfs.ErrSpendHintNotFound)

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

	third, err := n.RegisterSpend(&outpoint, script, 1)
	require.NoError(t, err)
	require.NotNil(t, third.HistoricalDispatch)
}

func staleHintTx(tag string) ([]byte, *wire.MsgTx) {
	program := sha256.Sum256([]byte(tag))
	script := append([]byte{0x51, 0x20}, program[:]...)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})

	return script, tx
}

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

func assertNoSpend(t *testing.T, event *chainntnfs.SpendEvent) {
	t.Helper()
	select {
	case <-event.Spend:
		t.Fatal("unexpected spend")
	default:
	}
}

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
