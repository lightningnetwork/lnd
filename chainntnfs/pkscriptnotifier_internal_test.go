package chainntnfs

import (
	"container/list"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

var internalTestPkScript = []byte{
	// OP_HASH160 OP_DATA_20 <20-byte script hash> OP_EQUAL.
	0xa9, 0x14,
	0x90, 0x1c, 0x86, 0x94, 0xc0, 0x3f, 0xaf, 0xd5,
	0x52, 0x28, 0x10, 0xe0, 0x33, 0x0f, 0x26, 0xe6,
	0x7a, 0x85, 0x33, 0xcd,
	0x87,
}

func registerInternalPkScript(t *testing.T, n *TxNotifier, pkScript []byte,
	opts ...NotifierOption) *PkScriptRegistration {

	t.Helper()

	reg, err := n.RegisterPkScriptNotifier()
	if err != nil {
		t.Fatalf("unable to register pkScript notifier: %v", err)
	}
	_, _, err = reg.AddPkScripts([][]byte{pkScript}, opts...)
	if err != nil {
		t.Fatalf("unable to add pkScript: %v", err)
	}

	return reg
}

func receiveInternalPkScriptNotification(t *testing.T,
	reg *PkScriptRegistration) *PkScriptNotification {

	t.Helper()

	select {
	case ntfn, ok := <-reg.Event.Notifications:
		if !ok {
			t.Fatal("pkScript notification stream closed")
		}

		return ntfn

	case <-time.After(time.Second):
		t.Fatal("pkScript notification not received")

		return nil
	}
}

func assertNoPkScriptMatches(t *testing.T, n *TxNotifier) {
	t.Helper()

	if n.numPkScriptMatches != 0 {
		t.Fatalf("expected zero matches, got %d", n.numPkScriptMatches)
	}
	if n.numPkScriptMatchBytes != 0 {
		t.Fatalf("expected zero match bytes, got %d",
			n.numPkScriptMatchBytes)
	}
}

type internalHintCache struct{}

// CommitSpendHint satisfies SpendHintCache for internal tests.
func (i internalHintCache) CommitSpendHint(uint32, ...SpendRequest) error {
	return nil
}

// QuerySpendHint satisfies SpendHintCache for internal tests.
func (i internalHintCache) QuerySpendHint(SpendRequest) (uint32, error) {
	return 0, ErrSpendHintNotFound
}

// PurgeSpendHint satisfies SpendHintCache for internal tests.
func (i internalHintCache) PurgeSpendHint(...SpendRequest) error {
	return nil
}

// CommitConfirmHint satisfies ConfirmHintCache for internal tests.
func (i internalHintCache) CommitConfirmHint(uint32, ...ConfRequest) error {
	return nil
}

// QueryConfirmHint satisfies ConfirmHintCache for internal tests.
func (i internalHintCache) QueryConfirmHint(ConfRequest) (uint32, error) {
	return 0, ErrConfirmHintNotFound
}

// PurgeConfirmHint satisfies ConfirmHintCache for internal tests.
func (i internalHintCache) PurgeConfirmHint(...ConfRequest) error {
	return nil
}

// fullPkScriptNotificationQueue creates a queue already filled to its item
// limit.
func fullPkScriptNotificationQueue() *pkScriptNotificationQueue {
	q := &pkScriptNotificationQueue{
		out:     make(chan *PkScriptNotification),
		pending: list.New(),
		quit:    make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mtx)

	for i := 0; i < MaxPkScriptNotificationQueueSize; i++ {
		q.pending.PushBack(&queuedPkScriptNotification{
			ntfn: &PkScriptNotification{},
			size: 1,
		})
		q.pendingBytes++
	}

	return q
}

// TestPkScriptNotificationQueueByteLimit ensures a notification that would push
// the queue over its byte limit is rejected.
func TestPkScriptNotificationQueueByteLimit(t *testing.T) {
	t.Parallel()

	q := &pkScriptNotificationQueue{
		out:          make(chan *PkScriptNotification),
		pending:      list.New(),
		pendingBytes: MaxPkScriptNotificationQueueBytes - 1,
		quit:         make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mtx)

	err := q.enqueue(&PkScriptNotification{})
	if !errors.Is(err, ErrPkScriptNotificationQueueFull) {
		t.Fatalf("expected queue full error, got %v", err)
	}
}

// TestValidatePkScriptsRejectsOversizedScript ensures scripts larger than
// the consensus script size limit are rejected before registration.
func TestValidatePkScriptsRejectsOversizedScript(t *testing.T) {
	t.Parallel()

	err := validatePkScripts(
		[][]byte{make([]byte, txscript.MaxScriptSize+1)},
	)
	if !errors.Is(err, ErrPkScriptTooLarge) {
		t.Fatalf("expected oversized script error, got %v", err)
	}
}

// makePkScriptBatchOverByteLimit returns a script batch whose aggregate size
// exceeds the provided byte limit while each individual script remains valid.
func makePkScriptBatchOverByteLimit(limit uint64) [][]byte {
	var pkScripts [][]byte
	for remaining := limit + 1; remaining > 0; {
		size := uint64(txscript.MaxScriptSize)
		if remaining < size {
			size = remaining
		}

		pkScripts = append(pkScripts, make([]byte, int(size)))
		remaining -= size
	}

	return pkScripts
}

// TestValidatePkScriptsRejectsAggregateBatchBytes ensures a single mutation
// cannot carry an unbounded amount of script data.
func TestValidatePkScriptsRejectsAggregateBatchBytes(t *testing.T) {
	t.Parallel()

	err := validatePkScripts(
		makePkScriptBatchOverByteLimit(MaxPkScriptBatchBytes),
	)
	if !errors.Is(err, ErrTooManyPkScripts) {
		t.Fatalf("expected aggregate script byte error, got %v", err)
	}
}

// TestTxNotifierPkScriptResourceByteLimits ensures the notifier enforces byte
// limits in addition to script count limits.
func TestTxNotifierPkScriptResourceByteLimits(t *testing.T) {
	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(10, ReorgSafetyLimit, hintCache, hintCache)
	sub := &pkScriptSubscription{
		scripts:     make(map[string]pkScriptWatchConfig),
		scriptBytes: MaxPkScriptBytesPerRegistration - 1,
	}

	err := n.validatePkScriptResourceLimits(sub, [][]byte{{0x51, 0x51}})
	if !errors.Is(err, ErrTooManyPkScripts) {
		t.Fatalf("expected registration byte limit error, got %v", err)
	}

	sub.scriptBytes = 0
	n.numPkScriptWatchBytes = MaxPkScriptWatchBytes - 1
	err = n.validatePkScriptResourceLimits(sub, [][]byte{{0x51, 0x51}})
	if !errors.Is(err, ErrTooManyPkScripts) {
		t.Fatalf("expected global byte limit error, got %v", err)
	}
}

// TestTxNotifierPkScriptMatchResourceLimits ensures both subscription and
// notifier match count and byte limits are enforced without constructing large
// blocks in the test.
func TestTxNotifierPkScriptMatchResourceLimits(t *testing.T) {
	t.Parallel()

	newNotifier := func() (*TxNotifier, *pkScriptSubscription) {
		hintCache := internalHintCache{}
		n := NewTxNotifier(10, ReorgSafetyLimit, hintCache, hintCache)

		return n, &pkScriptSubscription{}
	}

	tests := []struct {
		name  string
		setup func(*TxNotifier, *pkScriptSubscription)
	}{
		{
			name: "subscription count",
			setup: func(_ *TxNotifier, sub *pkScriptSubscription) {
				sub.numMatches =
					MaxPkScriptMatchesPerRegistration
			},
		},
		{
			name: "notifier count",
			setup: func(n *TxNotifier, _ *pkScriptSubscription) {
				n.numPkScriptMatches = MaxPkScriptMatches
			},
		},
		{
			name: "subscription bytes",
			setup: func(_ *TxNotifier, sub *pkScriptSubscription) {
				sub.matchBytes =
					MaxPkScriptMatchBytesPerRegistration
			},
		},
		{
			name: "notifier bytes",
			setup: func(n *TxNotifier, _ *pkScriptSubscription) {
				n.numPkScriptMatchBytes = MaxPkScriptMatchBytes
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			n, sub := newNotifier()
			test.setup(n, sub)

			err := n.validatePkScriptMatchResourceLimits(sub, 1)
			if !errors.Is(err, ErrPkScriptMatchLimit) {
				t.Fatalf("expected match limit error, "+
					"got %v", err)
			}
		})
	}
}

// TestTxNotifierPkScriptMatchLimitCancelsOnlySubscription ensures retained
// match exhaustion terminates the offending stream without failing block
// processing or another active stream.
func TestTxNotifierPkScriptMatchLimitCancelsOnlySubscription(t *testing.T) {
	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(10, ReorgSafetyLimit, hintCache, hintCache)

	otherScript := copyBytes(internalTestPkScript)
	otherScript[2] ^= 1
	limitedReg := registerInternalPkScript(
		t, n, internalTestPkScript, WithEvents(PkScriptEventConfirm),
	)
	healthyReg := registerInternalPkScript(
		t, n, otherScript, WithEvents(PkScriptEventConfirm),
	)

	limitedSub := n.pkScriptNotifications[1]
	limitedSub.numMatches = MaxPkScriptMatchesPerRegistration
	n.numPkScriptMatches = MaxPkScriptMatchesPerRegistration

	limitedTx := wire.NewMsgTx(2)
	limitedTx.AddTxOut(&wire.TxOut{PkScript: internalTestPkScript})
	err := n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{limitedTx},
	}), 11)
	if err != nil {
		t.Fatalf("match exhaustion failed ConnectTip: %v", err)
	}

	select {
	case _, ok := <-limitedReg.Event.Notifications:
		if ok {
			t.Fatal("limited notification stream remained open")
		}

	case <-time.After(time.Second):
		t.Fatal("limited notification stream did not close")
	}
	if !errors.Is(limitedReg.Event.Err(), ErrPkScriptMatchLimit) {
		t.Fatalf("expected terminal match limit error, got %v",
			limitedReg.Event.Err())
	}
	if n.pkScriptNotifications[2] == nil {
		t.Fatal("healthy notification stream was canceled")
	}

	healthyTx := wire.NewMsgTx(2)
	healthyTx.AddTxOut(&wire.TxOut{PkScript: otherScript})
	err = n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{healthyTx},
	}), 12)
	if err != nil {
		t.Fatalf("healthy ConnectTip failed: %v", err)
	}
	receiveInternalPkScriptNotification(t, healthyReg)

	healthyReg.Event.Cancel()
	assertNoPkScriptMatches(t, n)
}

// TestTxNotifierPkScriptCancelAfterQuit ensures a client can still release its
// registration after shutdown begins.
func TestTxNotifierPkScriptCancelAfterQuit(t *testing.T) {
	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(10, ReorgSafetyLimit, hintCache, hintCache)
	reg, err := n.RegisterPkScriptNotifier()
	if err != nil {
		t.Fatalf("unable to register pkScript notifier: %v", err)
	}

	close(n.quit)
	reg.Event.Cancel()

	if len(n.pkScriptNotifications) != 0 {
		t.Fatalf("expected registration cleanup after quit")
	}
	select {
	case _, ok := <-reg.Event.Notifications:
		if ok {
			t.Fatal("notification stream remained open")
		}

	default:
		t.Fatal("notification stream was not closed")
	}
}

// TestTxNotifierPkScriptRegisterTearDownRace ensures every registration that
// succeeds while teardown is starting is included in the teardown sweep.
func TestTxNotifierPkScriptRegisterTearDownRace(t *testing.T) {
	t.Parallel()

	const (
		iterations = 200
		registrars = 16
	)

	type registerResult struct {
		reg *PkScriptRegistration
		err error
	}

	for i := 0; i < iterations; i++ {
		hintCache := internalHintCache{}
		n := NewTxNotifier(10, ReorgSafetyLimit, hintCache, hintCache)

		start := make(chan struct{})
		results := make(chan registerResult, registrars)
		var wg sync.WaitGroup
		wg.Add(registrars)
		for j := 0; j < registrars; j++ {
			go func() {
				defer wg.Done()
				<-start

				reg, err := n.RegisterPkScriptNotifier()
				results <- registerResult{reg: reg, err: err}
			}()
		}

		tearDownDone := make(chan struct{})
		go func() {
			<-start
			n.TearDown()
			close(tearDownDone)
		}()

		close(start)
		<-tearDownDone
		wg.Wait()
		close(results)

		for result := range results {
			if result.err != nil {
				if !errors.Is(
					result.err, ErrTxNotifierExiting,
				) {

					t.Fatalf(
						"unexpected registration "+
							"error: %v",
						result.err,
					)
				}

				continue
			}

			select {
			case _, ok := <-result.reg.Event.Notifications:
				if ok {
					t.Fatal("open stream after teardown")
				}

			default:
				t.Fatalf(
					"registration remained open after "+
						"teardown in iteration %d", i,
				)
			}
		}
	}
}

// TestTxNotifierPkScriptRegistrationLimit ensures the notifier rejects new
// pkScript registrations once the registration limit has been reached.
func TestTxNotifierPkScriptRegistrationLimit(t *testing.T) {
	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(10, ReorgSafetyLimit, hintCache, hintCache)

	for i := 0; i < MaxPkScriptRegistrations; i++ {
		n.pkScriptNotifications[uint64(i)] = &pkScriptSubscription{}
	}

	_, err := n.RegisterPkScriptNotifier()
	if !errors.Is(err, ErrTooManyPkScriptRegistrations) {
		t.Fatalf("expected registration limit error, got %v", err)
	}
}

// TestTxNotifierDisconnectDoesNotReindexCanceledPkScriptSub ensures a
// subscription canceled during reorg handling is not reinserted into the
// notifier indexes.
func TestTxNotifierDisconnectDoesNotReindexCanceledPkScriptSub(t *testing.T) {
	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(10, ReorgSafetyLimit, hintCache, hintCache)

	var (
		subID     uint64 = 1
		blockHash        = chainhash.Hash{1}
		txHash           = chainhash.Hash{2}
		outpoint         = wire.OutPoint{Hash: txHash}
	)

	sub := &pkScriptSubscription{
		id: subID,
		scripts: map[string]pkScriptWatchConfig{
			string(internalTestPkScript): {
				events: PkScriptEventConfirm |
					PkScriptEventSpend,
				numConfs: 1,
			},
		},
		matches: map[wire.OutPoint]*pkScriptMatch{
			outpoint: {
				watchConfig: pkScriptWatchConfig{
					events: PkScriptEventConfirm |
						PkScriptEventSpend,
					numConfs: 1,
				},
				utxo: &PkScriptUTXO{
					OutPoint:    outpoint,
					PkScript:    internalTestPkScript,
					BlockHeight: 10,
				},
				confirmDispatched: true,
				confirmHeight:     10,
				confirmBlockHash:  &blockHash,
				spendDispatched:   true,
				spendHeight:       10,
				spendBlockHash:    &blockHash,
				spendTxHash:       &txHash,
			},
		},
		notificationQueue: fullPkScriptNotificationQueue(),
	}

	n.pkScriptNotifications[subID] = sub
	n.pkScriptByScript[string(internalTestPkScript)] = map[uint64]struct{}{
		subID: {},
	}
	n.pkScriptByOutpoint[outpoint] = map[uint64]struct{}{subID: {}}
	n.pkScriptConfirmedByHeight[10] = map[uint64]map[wire.OutPoint]struct{}{
		subID: {outpoint: {}},
	}
	n.pkScriptSpendsByHeight[10] = map[uint64]map[wire.OutPoint]struct{}{
		subID: {outpoint: {}},
	}
	n.numPkScriptWatches = 1

	err := n.DisconnectTip(10)
	if err != nil {
		t.Fatalf("unable to disconnect tip: %v", err)
	}

	if _, ok := n.pkScriptNotifications[subID]; ok {
		t.Fatalf("expected overflowing subscription to be canceled")
	}
	if len(n.pkScriptConfsByHeight) != 0 {
		t.Fatalf("expected no re-added confirmation indexes")
	}
	if len(n.pkScriptByOutpoint) != 0 {
		t.Fatalf("expected no re-added outpoint indexes")
	}
	if n.numPkScriptWatches != 0 {
		t.Fatalf("expected watch count to be zero")
	}
}

// TestTxNotifierRemovePkScriptsMatchCleansHeightZeroConfirm ensures removing a
// zero-height pkScript match also clears its confirmation indexes.
func TestTxNotifierRemovePkScriptsMatchCleansHeightZeroConfirm(t *testing.T) {
	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(0, ReorgSafetyLimit, hintCache, hintCache)

	var (
		subID    uint64 = 1
		txHash          = chainhash.Hash{3}
		outpoint        = wire.OutPoint{Hash: txHash}
	)

	sub := &pkScriptSubscription{
		id: subID,
		matches: map[wire.OutPoint]*pkScriptMatch{
			outpoint: {
				watchConfig: pkScriptWatchConfig{
					events:   PkScriptEventConfirm,
					numConfs: 1,
				},
				utxo: &PkScriptUTXO{
					OutPoint:    outpoint,
					PkScript:    internalTestPkScript,
					BlockHeight: 0,
				},
				confirmHeight: 0,
			},
		},
	}

	n.pkScriptConfsByHeight[0] = map[uint64]map[wire.OutPoint]struct{}{
		subID: {outpoint: {}},
	}
	n.pkScriptConfirmedByHeight[0] = map[uint64]map[wire.OutPoint]struct{}{
		subID: {outpoint: {}},
	}
	n.pkScriptReceivesByHeight[0] = map[uint64]map[wire.OutPoint]struct{}{
		subID: {outpoint: {}},
	}

	n.removePkScriptMatch(sub, outpoint)

	if len(sub.matches) != 0 {
		t.Fatalf("expected match to be removed")
	}
	if len(n.pkScriptConfsByHeight) != 0 {
		t.Fatalf("height-zero confirmation index remains")
	}
	if len(n.pkScriptConfirmedByHeight) != 0 {
		t.Fatalf("expected height-zero confirmed index to be removed")
	}
	if len(n.pkScriptReceivesByHeight) != 0 {
		t.Fatalf("expected height-zero receive index to be removed")
	}
}

// TestTxNotifierPkScriptFundingTxRetention ensures spend-only watches do not
// retain an unused funding transaction and confirmation watches share one
// detached transaction copy across matches from the same transaction.
func TestTxNotifierPkScriptFundingTxRetention(t *testing.T) {
	t.Parallel()

	t.Run("spend only", func(t *testing.T) {
		hintCache := internalHintCache{}
		n := NewTxNotifier(0, ReorgSafetyLimit, hintCache, hintCache)
		reg := registerInternalPkScript(
			t, n, internalTestPkScript,
			WithEvents(PkScriptEventSpend), WithIncludeTx(),
		)

		tx := wire.NewMsgTx(2)
		tx.AddTxOut(&wire.TxOut{PkScript: internalTestPkScript})
		err := n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
			Transactions: []*wire.MsgTx{tx},
		}), 1)
		if err != nil {
			t.Fatalf("unable to connect receive: %v", err)
		}

		sub := n.pkScriptNotifications[1]
		for _, match := range sub.matches {
			if match.fundingTx != nil {
				t.Fatal("spend-only match retained funding tx")
			}
		}
		reg.Event.Cancel()
		assertNoPkScriptMatches(t, n)
	})

	t.Run("shared detached copy", func(t *testing.T) {
		hintCache := internalHintCache{}
		n := NewTxNotifier(0, ReorgSafetyLimit, hintCache, hintCache)
		reg := registerInternalPkScript(
			t, n, internalTestPkScript,
			WithEvents(PkScriptEventConfirm), WithIncludeTx(),
		)

		tx := wire.NewMsgTx(2)
		tx.AddTxOut(&wire.TxOut{PkScript: internalTestPkScript})
		tx.AddTxOut(&wire.TxOut{PkScript: internalTestPkScript})
		err := n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
			Transactions: []*wire.MsgTx{tx},
		}), 1)
		if err != nil {
			t.Fatalf("unable to connect receives: %v", err)
		}

		sub := n.pkScriptNotifications[1]
		var retainedTx *wire.MsgTx
		for _, match := range sub.matches {
			if match.fundingTx == tx {
				t.Fatal("funding tx aliases caller-owned " +
					"transaction")
			}
			if retainedTx == nil {
				retainedTx = match.fundingTx
				continue
			}
			if match.fundingTx != retainedTx {
				t.Fatal("matches did not share retained " +
					"funding tx")
			}
		}
		if retainedTx == nil {
			t.Fatal("confirmation match did not retain funding tx")
		}

		reg.Event.Cancel()
		assertNoPkScriptMatches(t, n)
	})
}

// TestTxNotifierPkScriptMatchAccountingLifecycle ensures every match removal
// path balances notifier-level count and byte accounting.
func TestTxNotifierPkScriptMatchAccountingLifecycle(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"remove", "cancel", "reorg", "prune"} {
		t.Run(action, func(t *testing.T) {
			hintCache := internalHintCache{}
			n := NewTxNotifier(0, 2, hintCache, hintCache)
			reg := registerInternalPkScript(
				t, n, internalTestPkScript,
				WithEvents(PkScriptEventConfirm),
			)

			tx := wire.NewMsgTx(2)
			tx.AddTxOut(&wire.TxOut{PkScript: internalTestPkScript})
			err := n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
				Transactions: []*wire.MsgTx{tx},
			}), 1)
			if err != nil {
				t.Fatalf("unable to connect receive: %v", err)
			}
			receiveInternalPkScriptNotification(t, reg)
			if n.numPkScriptMatches != 1 ||
				n.numPkScriptMatchBytes == 0 {

				t.Fatalf("match was not accounted: count=%d "+
					"bytes=%d",
					n.numPkScriptMatches,
					n.numPkScriptMatchBytes)
			}

			canceled := false
			switch action {
			case "remove":
				err = reg.RemovePkScripts(
					[][]byte{internalTestPkScript},
				)
				if err != nil {
					t.Fatalf("unable to remove pkScript: "+
						"%v", err)
				}

			case "cancel":
				reg.Event.Cancel()
				canceled = true

			case "reorg":
				err = n.DisconnectTip(1)
				if err != nil {
					t.Fatalf("unable to disconnect "+
						"receive: %v", err)
				}

			case "prune":
				err = n.ConnectTip(
					btcutil.NewBlock(&wire.MsgBlock{}), 2,
				)
				if err != nil {
					t.Fatalf("unable to connect height 2: "+
						"%v", err)
				}
				err = n.ConnectTip(
					btcutil.NewBlock(&wire.MsgBlock{}), 3,
				)
				if err != nil {
					t.Fatalf("unable to connect height 3: "+
						"%v", err)
				}
			}

			assertNoPkScriptMatches(t, n)
			if !canceled {
				reg.Event.Cancel()
			}
		})
	}
}

// TestTxNotifierPkScriptConfirmPayloadPruned ensures confirmation reorg
// payloads are released at maturity while an unspent output remains tracked.
func TestTxNotifierPkScriptConfirmPayloadPruned(t *testing.T) {
	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(0, 2, hintCache, hintCache)
	reg := registerInternalPkScript(
		t, n, internalTestPkScript,
		WithEvents(PkScriptEventConfirm|PkScriptEventSpend),
		WithIncludeTx(), WithIncludeBlock(),
	)

	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{PkScript: internalTestPkScript})
	err := n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{tx},
	}), 1)
	if err != nil {
		t.Fatalf("unable to connect receive: %v", err)
	}
	receiveInternalPkScriptNotification(t, reg)

	sub := n.pkScriptNotifications[1]
	var match *pkScriptMatch
	for _, candidate := range sub.matches {
		match = candidate
	}
	if match == nil || match.fundingTx == nil || match.confirmBlock == nil {
		t.Fatal("confirmation reorg payload was not retained")
	}
	retainedBefore := match.retainedBytes

	for height := uint32(2); height <= 3; height++ {
		err = n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{}), height)
		if err != nil {
			t.Fatalf("unable to connect height %d: %v", height, err)
		}
	}

	if len(sub.matches) != 1 {
		t.Fatal("unspent match was not retained for spend detection")
	}
	if match.fundingTx != nil || match.confirmBlock != nil ||
		match.confirmBlockHash != nil {

		t.Fatal("mature confirmation payload was retained")
	}
	if match.retainedBytes >= retainedBefore {
		t.Fatalf("confirmation bytes not released: %d >= %d",
			match.retainedBytes, retainedBefore)
	}

	reg.Event.Cancel()
	assertNoPkScriptMatches(t, n)
}

// TestTxNotifierPkScriptSpendPayloadPruned ensures spend reorg payloads are
// released at maturity while a later confirmation remains reorg-sensitive.
func TestTxNotifierPkScriptSpendPayloadPruned(t *testing.T) {
	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(0, 4, hintCache, hintCache)
	reg := registerInternalPkScript(
		t, n, internalTestPkScript,
		WithEvents(PkScriptEventConfirm|PkScriptEventSpend),
		WithNumConfs(4), WithIncludeTx(), WithIncludeBlock(),
	)

	receiveTx := wire.NewMsgTx(2)
	receiveTx.AddTxOut(&wire.TxOut{PkScript: internalTestPkScript})
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
		Hash: receiveTx.TxHash(),
	}})
	err := n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{receiveTx, spendTx},
	}), 1)
	if err != nil {
		t.Fatalf("unable to connect receive and spend: %v", err)
	}
	receiveInternalPkScriptNotification(t, reg)

	sub := n.pkScriptNotifications[1]
	var match *pkScriptMatch
	for _, candidate := range sub.matches {
		match = candidate
	}
	if match == nil || match.spendTx == nil || match.spendBlock == nil {
		t.Fatal("spend reorg payload was not retained")
	}

	for height := uint32(2); height <= 5; height++ {
		err = n.ConnectTip(btcutil.NewBlock(&wire.MsgBlock{}), height)
		if err != nil {
			t.Fatalf("unable to connect height %d: %v", height, err)
		}
		if height == 4 {
			receiveInternalPkScriptNotification(t, reg)
		}
	}

	if len(sub.matches) != 1 {
		t.Fatal("confirmation-sensitive match was removed")
	}
	if match.spendTx == nil && match.spendBlock == nil &&
		match.spendBlockHash == nil {
		// Expected payload release.
	} else {
		t.Fatal("mature spend payload was retained")
	}
	if match.spendTxHash == nil || !match.spendDispatched {
		t.Fatal("mature spend fact was not retained")
	}

	reg.Event.Cancel()
	assertNoPkScriptMatches(t, n)
}
