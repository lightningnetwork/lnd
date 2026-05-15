package chainntnfs

import (
	"container/list"
	"errors"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var internalTestPkScript = []byte{
	// OP_HASH160 OP_DATA_20 <20-byte script hash> OP_EQUAL.
	0xa9, 0x14,
	0x90, 0x1c, 0x86, 0x94, 0xc0, 0x3f, 0xaf, 0xd5,
	0x52, 0x28, 0x10, 0xe0, 0x33, 0x0f, 0x26, 0xe6,
	0x7a, 0x85, 0x33, 0xcd,
	0x87,
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

// TestTxNotifierPkScriptRemoveCleansEpochsWithoutRevivingStaleDispatch ensures
// removed script epochs are freed without allowing an old historical dispatch to
// match a script that was later re-added.
func TestTxNotifierPkScriptRemoveCleansEpochsWithoutRevivingStaleDispatch(
	t *testing.T) {

	t.Parallel()

	hintCache := internalHintCache{}
	n := NewTxNotifier(10, ReorgSafetyLimit, hintCache, hintCache)
	reg, err := n.RegisterPkScriptNotifier()
	if err != nil {
		t.Fatalf("unable to register pkScript notifier: %v", err)
	}
	defer reg.Event.Cancel()

	dispatch1, _, _, err := reg.AddPkScripts(
		[][]byte{internalTestPkScript}, WithHistoricalScanFrom(1),
	)
	if err != nil {
		t.Fatalf("unable to add first script: %v", err)
	}
	if dispatch1 == nil {
		t.Fatalf("expected first historical dispatch")
	}

	sub := n.pkScriptNotifications[dispatch1.SubscriptionID]
	key := string(internalTestPkScript)
	if sub.scriptEpochs[key] == 0 {
		t.Fatalf("expected script epoch to be set")
	}

	err = reg.RemovePkScripts([][]byte{internalTestPkScript})
	if err != nil {
		t.Fatalf("unable to remove script: %v", err)
	}
	if _, ok := sub.scriptEpochs[key]; ok {
		t.Fatalf("expected removed script epoch to be deleted")
	}
	if sub.scriptBytes != 0 {
		t.Fatalf("expected removed script bytes to be freed")
	}
	if n.numPkScriptWatchBytes != 0 {
		t.Fatalf("expected global script bytes to be freed")
	}

	dispatch2, _, _, err := reg.AddPkScripts(
		[][]byte{internalTestPkScript}, WithHistoricalScanFrom(1),
	)
	if err != nil {
		t.Fatalf("unable to re-add script: %v", err)
	}
	if dispatch2 == nil {
		t.Fatalf("expected second historical dispatch")
	}
	if dispatch1.scriptEpochs[key] == dispatch2.scriptEpochs[key] {
		t.Fatalf("expected re-added script to use a new epoch")
	}
	if historicalDispatchHasCurrentScript(sub, dispatch1.scriptEpochs) {
		t.Fatalf("expected stale dispatch to be inactive")
	}
	if !historicalDispatchHasCurrentScript(sub, dispatch2.scriptEpochs) {
		t.Fatalf("expected current dispatch to remain active")
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
		scripts: map[string]pkScriptWatchConfig{string(internalTestPkScript): {
			events:   PkScriptEventConfirm | PkScriptEventSpend,
			numConfs: 1,
		}},
		scriptEpochs:      make(map[string]uint64),
		historicalScripts: make(map[string]uint64),
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
		t.Fatalf("expected height-zero confirmation index to be removed")
	}
	if len(n.pkScriptConfirmedByHeight) != 0 {
		t.Fatalf("expected height-zero confirmed index to be removed")
	}
	if len(n.pkScriptReceivesByHeight) != 0 {
		t.Fatalf("expected height-zero receive index to be removed")
	}
}
