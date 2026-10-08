package chainntnfs

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// MaxPkScriptNotificationQueueSize is the maximum number of pending
	// pkScript notifications that may be queued for a single subscription
	// beyond the registration channel's own buffer.
	MaxPkScriptNotificationQueueSize = 1000

	// MaxPkScriptNotificationQueueBytes is the approximate maximum memory
	// footprint of pending pkScript notifications for a single subscription
	// beyond the registration channel's own buffer.
	MaxPkScriptNotificationQueueBytes = 32 << 20

	// MaxPkScriptRegistrations is the maximum number of active pkScript
	// notification streams.
	MaxPkScriptRegistrations = 1000

	// MaxPkScriptsPerBatch is the maximum number of pkScripts that may be
	// added or removed in a single mutation.
	MaxPkScriptsPerBatch = 10000

	// MaxPkScriptBatchBytes is the maximum total byte size of all pkScripts
	// accepted in a single mutation.
	MaxPkScriptBatchBytes = 4 << 20

	// MaxPkScriptsPerRegistration is the maximum number of pkScripts that a
	// single pkScript notification registration may watch.
	MaxPkScriptsPerRegistration = 100000

	// MaxPkScriptBytesPerRegistration is the maximum total byte size of all
	// active pkScripts watched by a single registration.
	MaxPkScriptBytesPerRegistration = 32 << 20

	// MaxPkScriptWatches is the maximum number of pkScript watches
	// across all active pkScript notification registrations.
	MaxPkScriptWatches = 500000

	// MaxPkScriptWatchBytes is the maximum total byte size of all active
	// pkScript watches across all registrations.
	MaxPkScriptWatchBytes = 128 << 20

	// MaxPkScriptMatchesPerRegistration is the maximum number of matched
	// outputs retained by a single pkScript notification registration.
	MaxPkScriptMatchesPerRegistration = 100000

	// MaxPkScriptMatchBytesPerRegistration is the approximate maximum
	// memory footprint of matched outputs retained by one registration.
	MaxPkScriptMatchBytesPerRegistration = 64 << 20

	// MaxPkScriptMatches is the maximum number of matched outputs retained
	// across all active pkScript notification registrations.
	MaxPkScriptMatches = 500000

	// MaxPkScriptMatchBytes is the approximate maximum memory footprint of
	// matched outputs retained across all active registrations.
	MaxPkScriptMatchBytes = 256 << 20
)

var (
	// ErrPkScriptNotificationQueueFull is returned when a pkScript
	// registration's notification queue is full. The registration is
	// canceled when this happens so the queue cannot grow without bound.
	ErrPkScriptNotificationQueueFull = errors.New("pkScript notification " +
		"queue full")

	// ErrPkScriptMatchLimit is the terminal error for a registration whose
	// retained matched-output state would exceed a resource limit.
	ErrPkScriptMatchLimit = errors.New("pkScript matched output limit " +
		"exceeded")

	// ErrTooManyPkScripts is returned when a pkScript registration or
	// mutation exceeds one of the notifier's resource limits.
	ErrTooManyPkScripts = errors.New("too many pkScripts")

	// ErrTooManyPkScriptRegistrations is returned when too many pkScript
	// notification streams are active.
	ErrTooManyPkScriptRegistrations = errors.New("too many pkScript " +
		"registrations")

	// ErrPkScriptTooLarge is returned when a watched pkScript exceeds the
	// maximum script size accepted by the txscript engine.
	ErrPkScriptTooLarge = errors.New("pkScript too large")

	// ErrUnsupportedPkScript is returned when a backend cannot watch a
	// particular pkScript.
	ErrUnsupportedPkScript = errors.New("unsupported pkScript")
)

// pkScriptMatch tracks the lifecycle of an output matched by a pkScript
// subscription.
type pkScriptMatch struct {
	watchConfig pkScriptWatchConfig

	// retainedBytes is this match's contribution to the notifier's retained
	// match byte accounting. A zero value means the match was not
	// accounted, which is useful for manually assembled test state.
	retainedBytes uint64

	utxo *PkScriptUTXO

	fundingTx *wire.MsgTx

	confirmHeight     uint32
	confirmBlockHash  *chainhash.Hash
	confirmBlock      *wire.MsgBlock
	confirmDispatched bool
	confirmUpdates    map[uint32]*pkScriptConfirmUpdate

	spendTxHash     *chainhash.Hash
	spendBlockHash  *chainhash.Hash
	spendTx         *wire.MsgTx
	spendBlock      *wire.MsgBlock
	spendHeight     uint32
	spendTxIndex    uint32
	spendInputIndex uint32
	spendDispatched bool
}

// pkScriptWatchConfig holds the notification behavior for one watched script.
type pkScriptWatchConfig struct {
	events             PkScriptEventType
	numConfs           uint32
	includeTx          bool
	includeBlock       bool
	includeConfUpdates bool
}

// pkScriptConfirmUpdate tracks one dispatched partial confirmation update so it
// can be invalidated if its block is disconnected.
type pkScriptConfirmUpdate struct {
	blockHeight uint32
	blockHash   *chainhash.Hash
	block       *wire.MsgBlock
	numConfs    uint32
}

// pkScriptPendingSpend holds a spend discovered while scanning a connected
// block. Spends are dispatched after receives and confirmations for that block
// so same-block parent/child activity is delivered in causal order.
type pkScriptPendingSpend struct {
	subID      uint64
	outpoint   wire.OutPoint
	txHash     *chainhash.Hash
	txIndex    uint32
	inputIndex uint32
	height     uint32
	blockHash  *chainhash.Hash
	tx         *wire.MsgTx
	block      *wire.MsgBlock
}

// pkScriptSubscription tracks pkScript notifications for a single client.
type pkScriptSubscription struct {
	id uint64

	// scripts maps a watched script to its notification config.
	scripts map[string]pkScriptWatchConfig

	// scriptBytes tracks the total byte size of all active watched scripts.
	scriptBytes uint64

	// matches stores all outputs currently tracked by this subscription.
	matches map[wire.OutPoint]*pkScriptMatch

	// numMatches and matchBytes account for the matched output state
	// retained by this subscription.
	numMatches int
	matchBytes uint64

	notificationRegistration *PkScriptNotificationRegistration

	notificationQueue *pkScriptNotificationQueue
}

// pkScriptNotificationQueue is a bounded per-subscription queue that decouples
// chain processing from slow pkScript notification consumers.
type pkScriptNotificationQueue struct {
	out chan<- *PkScriptNotification

	mtx          sync.Mutex
	cond         *sync.Cond
	pending      *list.List
	pendingBytes uint64

	stopped bool
	err     error
	quit    chan struct{}
	wg      sync.WaitGroup
}

// queuedPkScriptNotification stores a queued notification with its estimated
// memory size.
type queuedPkScriptNotification struct {
	ntfn *PkScriptNotification
	size uint64
}

// newPkScriptNotificationQueue creates and starts a bounded pkScript
// notification queue.
func newPkScriptNotificationQueue(
	out chan<- *PkScriptNotification) *pkScriptNotificationQueue {

	q := &pkScriptNotificationQueue{
		out:     out,
		pending: list.New(),
		quit:    make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mtx)

	q.wg.Add(1)
	go q.run()

	return q
}

// enqueue adds a notification to the bounded queue or stops the queue if a
// resource limit would be exceeded.
func (q *pkScriptNotificationQueue) enqueue(
	ntfn *PkScriptNotification) error {

	q.mtx.Lock()
	defer q.mtx.Unlock()

	if q.stopped {
		if q.err != nil {
			return q.err
		}

		return ErrTxNotifierExiting
	}

	if q.pending.Len() >= MaxPkScriptNotificationQueueSize {
		q.stopLocked(ErrPkScriptNotificationQueueFull)

		return ErrPkScriptNotificationQueueFull
	}

	size := pkScriptNotificationSize(ntfn)
	if q.pendingBytes+size > MaxPkScriptNotificationQueueBytes {
		q.stopLocked(ErrPkScriptNotificationQueueFull)

		return ErrPkScriptNotificationQueueFull
	}

	q.pending.PushBack(&queuedPkScriptNotification{
		ntfn: ntfn,
		size: size,
	})
	q.pendingBytes += size
	q.cond.Signal()

	return nil
}

// run drains queued notifications to the subscriber channel until stopped.
func (q *pkScriptNotificationQueue) run() {
	defer q.wg.Done()
	defer close(q.out)

	for {
		q.mtx.Lock()
		for q.pending.Len() == 0 && !q.stopped {
			q.cond.Wait()
		}

		if q.stopped {
			q.mtx.Unlock()
			return
		}

		next := q.pending.Front()
		queued, ok := next.Value.(*queuedPkScriptNotification)
		if !ok {
			q.pending.Remove(next)
			q.mtx.Unlock()

			return
		}
		q.pending.Remove(next)
		q.pendingBytes -= queued.size
		q.mtx.Unlock()

		select {
		case q.out <- queued.ntfn:
		case <-q.quit:
			return
		}
	}
}

// stop terminates the queue and waits for its worker to exit.
func (q *pkScriptNotificationQueue) stop() {
	q.stopWithError(ErrTxNotifierExiting)
}

// stopWithError terminates the queue with the given terminal error and waits
// for its worker to exit.
func (q *pkScriptNotificationQueue) stopWithError(err error) {
	q.mtx.Lock()
	q.stopLocked(err)
	q.mtx.Unlock()

	q.wg.Wait()
}

// stopLocked stops the queue with the given error. The caller must hold q.mtx.
func (q *pkScriptNotificationQueue) stopLocked(err error) {
	if q.stopped {
		return
	}

	q.stopped = true
	q.err = err
	q.pending.Init()
	q.pendingBytes = 0
	close(q.quit)
	q.cond.Broadcast()
}

// terminalErr returns the terminal error that stopped the notification
// queue, if any.
func (q *pkScriptNotificationQueue) terminalErr() error {
	q.mtx.Lock()
	defer q.mtx.Unlock()

	return q.err
}

// PkScriptRegistration encompasses all of the information required for callers
// to retrieve details about an pkScript notification stream.
type PkScriptRegistration struct {
	// Event contains the registration details and notification channel.
	Event *PkScriptNotificationRegistration

	// Height is the height of the TxNotifier at the time the pkScript
	// notification was registered.
	Height uint32

	// AddPkScripts adds more scripts to this registration and returns a
	// notifier's current height, and the scripts that were newly added.
	// Scripts already watched by this registration are ignored.
	AddPkScripts func(pkScripts [][]byte,
		opts ...NotifierOption) (uint32, [][]byte, error)

	// RemovePkScripts removes scripts from this registration.
	RemovePkScripts func(pkScripts [][]byte) error
}

// validatePkScripts ensures that all provided scripts are non-empty.
func validatePkScripts(pkScripts [][]byte) error {
	if len(pkScripts) == 0 {
		return ErrNoScript
	}
	if len(pkScripts) > MaxPkScriptsPerBatch {
		return fmt.Errorf(
			"%w: batch size %d exceeds limit %d",
			ErrTooManyPkScripts, len(pkScripts),
			MaxPkScriptsPerBatch,
		)
	}

	var batchBytes uint64
	for _, pkScript := range pkScripts {
		if len(pkScript) == 0 {
			return ErrNoScript
		}
		if len(pkScript) > txscript.MaxScriptSize {
			return fmt.Errorf("%w: script size %d exceeds limit %d",
				ErrPkScriptTooLarge, len(pkScript),
				txscript.MaxScriptSize)
		}

		batchBytes += uint64(len(pkScript))
		if batchBytes > MaxPkScriptBatchBytes {
			return fmt.Errorf(
				"%w: batch byte size %d exceeds limit %d",
				ErrTooManyPkScripts, batchBytes,
				MaxPkScriptBatchBytes,
			)
		}
	}

	return nil
}

// validatePkScriptResourceLimits ensures a mutation will not exceed the
// notifier's pkScript watch limits.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) validatePkScriptResourceLimits(
	sub *pkScriptSubscription, pkScripts [][]byte) error {

	newKeys := make(map[string]uint64, len(pkScripts))
	var newBytes uint64
	for _, pkScript := range pkScripts {
		key := string(pkScript)
		if _, ok := sub.scripts[key]; ok {
			continue
		}
		if _, ok := newKeys[key]; ok {
			continue
		}

		scriptBytes := uint64(len(pkScript))
		newKeys[key] = scriptBytes
		newBytes += scriptBytes
	}

	newWatches := len(newKeys)
	if len(sub.scripts)+newWatches > MaxPkScriptsPerRegistration {
		return fmt.Errorf(
			"%w: registration would watch %d scripts, limit "+
				"is %d", ErrTooManyPkScripts,
			len(sub.scripts)+newWatches,
			MaxPkScriptsPerRegistration,
		)
	}

	if n.numPkScriptWatches+newWatches > MaxPkScriptWatches {
		return fmt.Errorf("%w: notifier would watch %d scripts, "+
			"limit is %d", ErrTooManyPkScripts,
			n.numPkScriptWatches+newWatches, MaxPkScriptWatches)
	}
	if sub.scriptBytes+newBytes > MaxPkScriptBytesPerRegistration {
		return fmt.Errorf(
			"%w: registration would watch %d script bytes, limit "+
				"is %d", ErrTooManyPkScripts,
			sub.scriptBytes+newBytes,
			MaxPkScriptBytesPerRegistration,
		)
	}
	if n.numPkScriptWatchBytes+newBytes > MaxPkScriptWatchBytes {
		return fmt.Errorf("%w: notifier would watch %d script bytes, "+
			"limit is %d", ErrTooManyPkScripts,
			n.numPkScriptWatchBytes+newBytes, MaxPkScriptWatchBytes)
	}

	return nil
}

// validatePkScriptOptions ensures the pkScript watch options are well formed.
func (n *TxNotifier) validatePkScriptOptions(opts *NotifierOptions) error {
	const validEvents = PkScriptEventConfirm | PkScriptEventSpend

	if opts.Events == 0 {
		return errors.New("a pkScript event type must be provided")
	}
	if opts.Events&^validEvents != 0 {
		return errors.New("unknown pkScript event type")
	}

	if opts.Events.Has(PkScriptEventConfirm) {
		if opts.NumConfs == 0 || opts.NumConfs > n.reorgSafetyLimit {
			return ErrNumConfsOutOfRange
		}
	} else if opts.IncludeConfirmationUpdates {
		return errors.New(
			"confirmation updates require confirmation events",
		)
	}

	return nil
}

// pkScriptWatchConfigFromOptions converts public notifier options into the
// internal pkScript watch configuration.
func pkScriptWatchConfigFromOptions(opts *NotifierOptions) pkScriptWatchConfig {
	return pkScriptWatchConfig{
		events:             opts.Events,
		numConfs:           opts.NumConfs,
		includeTx:          opts.IncludeTx,
		includeBlock:       opts.IncludeBlock,
		includeConfUpdates: opts.IncludeConfirmationUpdates,
	}
}

// RegisterPkScriptNotifier creates a new pkScript notification stream.
func (n *TxNotifier) RegisterPkScriptNotifier() (*PkScriptRegistration, error) {
	n.Lock()
	defer n.Unlock()

	select {
	case <-n.quit:
		return nil, ErrTxNotifierExiting
	default:
	}

	if len(n.pkScriptNotifications) >= MaxPkScriptRegistrations {
		return nil, fmt.Errorf("%w: active registrations %d exceeds "+
			"limit %d", ErrTooManyPkScriptRegistrations,
			len(n.pkScriptNotifications), MaxPkScriptRegistrations)
	}

	subID := atomic.AddUint64(&n.pkScriptClientCounter, 1)
	sub := &pkScriptSubscription{
		id:      subID,
		scripts: make(map[string]pkScriptWatchConfig),
		matches: make(map[wire.OutPoint]*pkScriptMatch),
	}
	sub.notificationRegistration = NewPkScriptNotificationRegistration(
		func() {
			n.CancelPkScript(subID)
		},
	)
	sub.notificationQueue = newPkScriptNotificationQueue(
		sub.notificationRegistration.notifications,
	)
	sub.notificationRegistration.Err = sub.notificationQueue.terminalErr

	n.pkScriptNotifications[subID] = sub

	return &PkScriptRegistration{
		Event:  sub.notificationRegistration,
		Height: n.currentHeight,
		AddPkScripts: func(
			pkScripts [][]byte, addOptFuncs ...NotifierOption,
		) (uint32, [][]byte, error) {

			return n.AddPkScripts(subID, pkScripts, addOptFuncs...)
		},
		RemovePkScripts: func(pkScripts [][]byte) error {
			return n.RemovePkScripts(subID, pkScripts)
		},
	}, nil
}

// AddPkScripts adds a set of pkScripts to an existing pkScript subscription.
func (n *TxNotifier) AddPkScripts(id uint64, pkScripts [][]byte,
	optFuncs ...NotifierOption) (uint32, [][]byte, error) {

	select {
	case <-n.quit:
		return 0, nil, ErrTxNotifierExiting
	default:
	}

	err := validatePkScripts(pkScripts)
	if err != nil {
		return 0, nil, err
	}
	opts := DefaultNotifierOptions()
	for _, optFunc := range optFuncs {
		optFunc(opts)
	}

	err = n.validatePkScriptOptions(opts)
	if err != nil {
		return 0, nil, err
	}

	n.Lock()
	defer n.Unlock()

	sub, ok := n.pkScriptNotifications[id]
	if !ok {
		return 0, nil, fmt.Errorf(
			"pkScript subscription %d not found", id,
		)
	}
	err = n.validatePkScriptResourceLimits(sub, pkScripts)
	if err != nil {
		return 0, nil, err
	}

	cfg := pkScriptWatchConfigFromOptions(opts)
	addedScripts := n.addPkScripts(sub, pkScripts, cfg)

	return n.currentHeight, addedScripts, nil
}

// RemovePkScripts removes a set of pkScripts from an existing pkScript
// subscription.
func (n *TxNotifier) RemovePkScripts(id uint64, pkScripts [][]byte) error {
	select {
	case <-n.quit:
		return ErrTxNotifierExiting
	default:
	}

	err := validatePkScripts(pkScripts)
	if err != nil {
		return err
	}

	n.Lock()
	defer n.Unlock()

	sub, ok := n.pkScriptNotifications[id]
	if !ok {
		return fmt.Errorf("pkScript subscription %d not found", id)
	}

	removeKeys := make(map[string]struct{}, len(pkScripts))
	for _, pkScript := range pkScripts {
		removeKeys[string(pkScript)] = struct{}{}
	}

	for key := range removeKeys {
		if _, ok := sub.scripts[key]; !ok {
			continue
		}
		scriptBytes := uint64(len(key))
		delete(sub.scripts, key)
		n.numPkScriptWatches--
		if sub.scriptBytes >= scriptBytes {
			sub.scriptBytes -= scriptBytes
		} else {
			sub.scriptBytes = 0
		}
		if n.numPkScriptWatchBytes >= scriptBytes {
			n.numPkScriptWatchBytes -= scriptBytes
		} else {
			n.numPkScriptWatchBytes = 0
		}

		subs := n.pkScriptByScript[key]
		delete(subs, id)
		if len(subs) == 0 {
			delete(n.pkScriptByScript, key)
		}
	}

	for outpoint, match := range sub.matches {
		if _, ok := removeKeys[string(match.utxo.PkScript)]; !ok {
			continue
		}

		n.removePkScriptMatch(sub, outpoint)
	}

	return nil
}

// CancelPkScript cancels an existing pkScript subscription.
func (n *TxNotifier) CancelPkScript(id uint64) {
	n.Lock()
	defer n.Unlock()

	n.cancelPkScriptLocked(id)
}

// cancelPkScriptLocked cancels an existing pkScript subscription. The
// caller must hold the TxNotifier's lock.
func (n *TxNotifier) cancelPkScriptLocked(id uint64) {
	sub, ok := n.pkScriptNotifications[id]
	if !ok {
		return
	}

	for scriptKey := range sub.scripts {
		n.numPkScriptWatches--

		subs := n.pkScriptByScript[scriptKey]
		delete(subs, id)
		if len(subs) == 0 {
			delete(n.pkScriptByScript, scriptKey)
		}
	}
	if n.numPkScriptWatchBytes >= sub.scriptBytes {
		n.numPkScriptWatchBytes -= sub.scriptBytes
	} else {
		n.numPkScriptWatchBytes = 0
	}

	for outpoint := range sub.matches {
		n.removePkScriptMatch(sub, outpoint)
	}
	if sub.numMatches > 0 {
		if n.numPkScriptMatches >= sub.numMatches {
			n.numPkScriptMatches -= sub.numMatches
		} else {
			n.numPkScriptMatches = 0
		}
		sub.numMatches = 0
	}
	if sub.matchBytes > 0 {
		if n.numPkScriptMatchBytes >= sub.matchBytes {
			n.numPkScriptMatchBytes -= sub.matchBytes
		} else {
			n.numPkScriptMatchBytes = 0
		}
		sub.matchBytes = 0
	}

	if sub.notificationQueue != nil {
		sub.notificationQueue.stop()
	}
	delete(n.pkScriptNotifications, id)
}

// addPkScripts adds new scripts to a subscription and updates indexes.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) addPkScripts(sub *pkScriptSubscription,
	pkScripts [][]byte, cfg pkScriptWatchConfig) [][]byte {

	var addedScripts [][]byte

	for _, pkScript := range pkScripts {
		key := string(pkScript)
		if _, ok := sub.scripts[key]; ok {
			continue
		}

		sub.scripts[key] = cfg
		sub.scriptBytes += uint64(len(pkScript))
		n.numPkScriptWatches++
		n.numPkScriptWatchBytes += uint64(len(pkScript))

		scriptCopy := copyBytes(pkScript)
		addedScripts = append(addedScripts, scriptCopy)

		subSet, ok := n.pkScriptByScript[key]
		if !ok {
			subSet = make(map[uint64]struct{})
			n.pkScriptByScript[key] = subSet
		}
		subSet[sub.id] = struct{}{}
	}

	return addedScripts
}

// NewPkScriptAddResult returns caller-facing metadata for an AddPkScripts
// mutation.
func NewPkScriptAddResult(addedScripts [][]byte) *PkScriptAddResult {
	return &PkScriptAddResult{
		NumAdded: uint32(len(addedScripts)),
	}
}

// removeOutpointSubscription removes a subscription's UTXO from the global
// outpoint map.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) removeOutpointSubscription(outpoint wire.OutPoint,
	subID uint64) {

	subMap, ok := n.pkScriptByOutpoint[outpoint]
	if !ok {
		return
	}

	delete(subMap, subID)
	if len(subMap) == 0 {
		delete(n.pkScriptByOutpoint, outpoint)
	}
}

// dispatchPkScriptNotification sends an pkScript notification to a subscriber.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptNotification(sub *pkScriptSubscription,
	ntfn *PkScriptNotification) error {

	select {
	case <-n.quit:
		return ErrTxNotifierExiting
	default:
	}

	err := sub.notificationQueue.enqueue(ntfn)
	if errors.Is(err, ErrPkScriptNotificationQueueFull) {
		Log.Warnf("Canceling pkScript notification registration %d: "+
			"notification queue exceeded limit %d", sub.id,
			MaxPkScriptNotificationQueueSize)

		n.cancelPkScriptLocked(sub.id)
	}

	return err
}

// copyBytes returns a detached copy of b.
func copyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)

	return out
}

// copyHash returns a detached copy of hash.
func copyHash(hash *chainhash.Hash) *chainhash.Hash {
	if hash == nil {
		return nil
	}

	hashCopy := *hash

	return &hashCopy
}

// copyPkScriptUTXO returns a detached copy of utxo.
func copyPkScriptUTXO(utxo *PkScriptUTXO) *PkScriptUTXO {
	if utxo == nil {
		return nil
	}

	utxoCopy := *utxo
	utxoCopy.PkScript = copyBytes(utxo.PkScript)
	utxoCopy.BlockHash = copyHash(utxo.BlockHash)

	return &utxoCopy
}

// copyMsgTx returns a detached copy of tx.
func copyMsgTx(tx *wire.MsgTx) *wire.MsgTx {
	if tx == nil {
		return nil
	}

	return tx.Copy()
}

// copyMsgBlock returns a detached copy of block.
func copyMsgBlock(block *wire.MsgBlock) *wire.MsgBlock {
	if block == nil {
		return nil
	}

	return block.Copy()
}

// retainMsgBlock returns one detached block copy shared by notifier-internal
// state for the duration of processing a connected block.
func retainMsgBlock(block *wire.MsgBlock,
	retained **wire.MsgBlock) *wire.MsgBlock {

	if block == nil {
		return nil
	}
	if *retained == nil {
		*retained = copyMsgBlock(block)
	}

	return *retained
}

// pkScriptNotificationSize estimates a notification's queued memory footprint.
func pkScriptNotificationSize(ntfn *PkScriptNotification) uint64 {
	if ntfn == nil {
		return 0
	}

	// Account for fixed metadata and pointer fields. This is deliberately
	// an estimate because the queue limit is a resource guard, not
	// accounting.
	var size uint64 = 256
	if ntfn.UTXO != nil {
		size += uint64(len(ntfn.UTXO.PkScript)) + 128
	}
	if ntfn.Tx != nil {
		size += uint64(ntfn.Tx.SerializeSize())
	}
	if ntfn.Block != nil {
		size += uint64(ntfn.Block.SerializeSize())
	}

	return size
}

// pkScriptMatchSize estimates the memory retained for a matched output. Shared
// transaction and block payloads are deliberately charged to every match that
// references them, keeping the resource guard conservative.
func pkScriptMatchSize(match *pkScriptMatch) uint64 {
	if match == nil {
		return 0
	}

	var size uint64 = 256
	if match.utxo != nil {
		size += uint64(len(match.utxo.PkScript)) + 128
	}
	if match.fundingTx != nil {
		size += uint64(match.fundingTx.SerializeSize())
	}
	if match.confirmBlock != nil {
		size += uint64(match.confirmBlock.SerializeSize())
	}
	if match.spendTx != nil {
		size += uint64(match.spendTx.SerializeSize())
	}
	if match.spendBlock != nil {
		size += uint64(match.spendBlock.SerializeSize())
	}
	for _, update := range match.confirmUpdates {
		size += 128
		if update.block != nil {
			size += uint64(update.block.SerializeSize())
		}
	}

	return size
}

// pkScriptMatchBytesExceeded returns true if adding delta to current would
// exceed limit without relying on overflowing unsigned addition.
func pkScriptMatchBytesExceeded(current, delta, limit uint64) bool {
	return current > limit || delta > limit-current
}

// validatePkScriptMatchResourceLimits checks whether a new match can be
// retained by a subscription and the notifier.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) validatePkScriptMatchResourceLimits(
	sub *pkScriptSubscription, size uint64) error {

	if sub.numMatches >= MaxPkScriptMatchesPerRegistration {
		return fmt.Errorf("%w: registration would retain more than %d "+
			"matched outputs", ErrPkScriptMatchLimit,
			MaxPkScriptMatchesPerRegistration)
	}
	if n.numPkScriptMatches >= MaxPkScriptMatches {
		return fmt.Errorf("%w: notifier would retain more than %d "+
			"matched outputs", ErrPkScriptMatchLimit,
			MaxPkScriptMatches)
	}
	if pkScriptMatchBytesExceeded(
		sub.matchBytes, size, MaxPkScriptMatchBytesPerRegistration,
	) {

		return fmt.Errorf("%w: registration matched output state "+
			"would exceed %d bytes", ErrPkScriptMatchLimit,
			MaxPkScriptMatchBytesPerRegistration)
	}
	if pkScriptMatchBytesExceeded(
		n.numPkScriptMatchBytes, size, MaxPkScriptMatchBytes,
	) {

		return fmt.Errorf("%w: notifier matched output state would "+
			"exceed %d bytes", ErrPkScriptMatchLimit,
			MaxPkScriptMatchBytes)
	}

	return nil
}

// validatePkScriptMatchByteGrowth checks whether an existing match can retain
// an additional payload.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) validatePkScriptMatchByteGrowth(
	sub *pkScriptSubscription, delta uint64) error {

	if pkScriptMatchBytesExceeded(
		sub.matchBytes, delta, MaxPkScriptMatchBytesPerRegistration,
	) {

		return fmt.Errorf("%w: registration matched output state "+
			"would exceed %d bytes", ErrPkScriptMatchLimit,
			MaxPkScriptMatchBytesPerRegistration)
	}
	if pkScriptMatchBytesExceeded(
		n.numPkScriptMatchBytes, delta, MaxPkScriptMatchBytes,
	) {

		return fmt.Errorf("%w: notifier matched output state would "+
			"exceed %d bytes", ErrPkScriptMatchLimit,
			MaxPkScriptMatchBytes)
	}

	return nil
}

// failPkScriptMatchLimit cancels a subscription with a terminal resource
// error. Other subscriptions and block processing remain unaffected.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) failPkScriptMatchLimit(
	sub *pkScriptSubscription, err error) {

	Log.Warnf("Canceling pkScript notification registration %d: %v",
		sub.id, err)
	if sub.notificationQueue != nil {
		sub.notificationQueue.stopWithError(err)
	}
	n.cancelPkScriptLocked(sub.id)
}

// reservePkScriptMatchSize grows a match's byte accounting before a payload is
// exposed to the subscriber. It returns false if growth canceled the
// subscription.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) reservePkScriptMatchSize(
	sub *pkScriptSubscription, match *pkScriptMatch, newSize uint64) bool {

	if match.retainedBytes == 0 || newSize <= match.retainedBytes {
		return true
	}

	delta := newSize - match.retainedBytes
	err := n.validatePkScriptMatchByteGrowth(sub, delta)
	if err != nil {
		n.failPkScriptMatchLimit(sub, err)

		return false
	}

	sub.matchBytes += delta
	n.numPkScriptMatchBytes += delta
	match.retainedBytes = newSize

	return true
}

// releasePkScriptMatchSize rolls back a prior reservation.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) releasePkScriptMatchSize(
	sub *pkScriptSubscription, match *pkScriptMatch, oldSize uint64) {

	if match.retainedBytes == 0 || oldSize >= match.retainedBytes {
		return
	}

	delta := match.retainedBytes - oldSize
	sub.matchBytes -= delta
	n.numPkScriptMatchBytes -= delta
	match.retainedBytes = oldSize
}

// updatePkScriptMatchSize refreshes byte accounting after a retained payload
// is added or released. It returns false if growth canceled the subscription.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) updatePkScriptMatchSize(
	sub *pkScriptSubscription, match *pkScriptMatch) bool {

	// Manually assembled test matches are not part of resource accounting.
	if match.retainedBytes == 0 {
		return true
	}

	newSize := pkScriptMatchSize(match)
	oldSize := match.retainedBytes
	if newSize > oldSize {
		delta := newSize - oldSize
		err := n.validatePkScriptMatchByteGrowth(sub, delta)
		if err != nil {
			n.failPkScriptMatchLimit(sub, err)

			return false
		}

		sub.matchBytes += delta
		n.numPkScriptMatchBytes += delta
	} else {
		delta := oldSize - newSize
		if sub.matchBytes >= delta {
			sub.matchBytes -= delta
		} else {
			sub.matchBytes = 0
		}
		if n.numPkScriptMatchBytes >= delta {
			n.numPkScriptMatchBytes -= delta
		} else {
			n.numPkScriptMatchBytes = 0
		}
	}
	match.retainedBytes = newSize

	return true
}

// addPkScriptHeightIndex indexes an outpoint by height and subscription ID.
func addPkScriptHeightIndex(
	index map[uint32]map[uint64]map[wire.OutPoint]struct{}, height uint32,
	subID uint64, outpoint wire.OutPoint) {

	heightIndex, ok := index[height]
	if !ok {
		heightIndex = make(map[uint64]map[wire.OutPoint]struct{})
		index[height] = heightIndex
	}

	subIndex, ok := heightIndex[subID]
	if !ok {
		subIndex = make(map[wire.OutPoint]struct{})
		heightIndex[subID] = subIndex
	}

	subIndex[outpoint] = struct{}{}
}

// removePkScriptHeightIndex removes an outpoint from a height/subscription
// index and prunes empty buckets.
func removePkScriptHeightIndex(
	index map[uint32]map[uint64]map[wire.OutPoint]struct{},
	height uint32, subID uint64, outpoint wire.OutPoint) {

	heightIndex, ok := index[height]
	if !ok {
		return
	}

	subIndex, ok := heightIndex[subID]
	if !ok {
		return
	}

	delete(subIndex, outpoint)
	if len(subIndex) == 0 {
		delete(heightIndex, subID)
	}
	if len(heightIndex) == 0 {
		delete(index, height)
	}
}

// schedulePkScriptConfirmUpdate schedules a future partial confirmation update
// for an pkScript match.
func (n *TxNotifier) schedulePkScriptConfirmUpdate(
	sub *pkScriptSubscription, match *pkScriptMatch, outpoint wire.OutPoint,
	height uint32) {

	if !match.watchConfig.includeConfUpdates {
		return
	}
	if height >= match.confirmHeight {
		return
	}

	addPkScriptHeightIndex(
		n.pkScriptConfUpdatesByHeight, height, sub.id, outpoint,
	)
}

// addDispatchedPkScriptConfirmUpdate records a delivered partial confirmation
// update so it can be invalidated by a reorg.
func (n *TxNotifier) addDispatchedPkScriptConfirmUpdate(
	sub *pkScriptSubscription, match *pkScriptMatch, outpoint wire.OutPoint,
	update *pkScriptConfirmUpdate) {

	if match.confirmUpdates == nil {
		match.confirmUpdates = make(map[uint32]*pkScriptConfirmUpdate)
	}
	match.confirmUpdates[update.blockHeight] = update

	addPkScriptHeightIndex(
		n.pkScriptConfUpdatesDispatchedByHeight, update.blockHeight,
		sub.id, outpoint,
	)
}

// removeDispatchedPkScriptConfirmUpdate removes a delivered partial
// confirmation update from the reorg tracking indexes.
func (n *TxNotifier) removeDispatchedPkScriptConfirmUpdate(
	sub *pkScriptSubscription, match *pkScriptMatch, outpoint wire.OutPoint,
	height uint32) {

	delete(match.confirmUpdates, height)
	if len(match.confirmUpdates) == 0 {
		match.confirmUpdates = nil
	}

	removePkScriptHeightIndex(
		n.pkScriptConfUpdatesDispatchedByHeight, height, sub.id,
		outpoint,
	)
	n.updatePkScriptMatchSize(sub, match)
}

// shouldRetainPkScriptMatch returns true if the match is still needed for
// future notifications or reorg handling.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) shouldRetainPkScriptMatch(sub *pkScriptSubscription,
	match *pkScriptMatch) bool {

	if match.watchConfig.events.Has(PkScriptEventConfirm) {
		if !match.confirmDispatched {
			return true
		}

		if match.confirmHeight+n.reorgSafetyLimit > n.currentHeight {
			return true
		}
	}

	if match.watchConfig.events.Has(PkScriptEventSpend) {
		if match.spendTxHash == nil {
			return true
		}

		if match.spendHeight+n.reorgSafetyLimit > n.currentHeight {
			return true
		}
	}

	return false
}

// removePkScriptMatch clears all indexes associated with a tracked output.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) removePkScriptMatch(sub *pkScriptSubscription,
	outpoint wire.OutPoint) {

	match, ok := sub.matches[outpoint]
	if !ok {
		return
	}

	if match.utxo != nil {
		removePkScriptHeightIndex(
			n.pkScriptReceivesByHeight, match.utxo.BlockHeight,
			sub.id, outpoint,
		)
	}
	if match.watchConfig.events.Has(PkScriptEventConfirm) {
		removePkScriptHeightIndex(
			n.pkScriptConfsByHeight, match.confirmHeight,
			sub.id, outpoint,
		)
		removePkScriptHeightIndex(
			n.pkScriptConfirmedByHeight, match.confirmHeight,
			sub.id, outpoint,
		)
	}
	if match.watchConfig.includeConfUpdates && match.utxo != nil {
		startHeight := match.utxo.BlockHeight
		endHeight := match.confirmHeight
		for height := startHeight; height < endHeight; height++ {
			removePkScriptHeightIndex(
				n.pkScriptConfUpdatesByHeight, height,
				sub.id, outpoint,
			)
			removePkScriptHeightIndex(
				n.pkScriptConfUpdatesDispatchedByHeight, height,
				sub.id, outpoint,
			)
		}
	}
	if match.spendTxHash != nil || match.spendDispatched {
		removePkScriptHeightIndex(
			n.pkScriptSpendsByHeight, match.spendHeight,
			sub.id, outpoint,
		)
	}

	n.removeOutpointSubscription(outpoint, sub.id)
	if match.retainedBytes > 0 {
		if sub.numMatches > 0 {
			sub.numMatches--
		}
		if n.numPkScriptMatches > 0 {
			n.numPkScriptMatches--
		}
		if sub.matchBytes >= match.retainedBytes {
			sub.matchBytes -= match.retainedBytes
		} else {
			sub.matchBytes = 0
		}
		if n.numPkScriptMatchBytes >= match.retainedBytes {
			n.numPkScriptMatchBytes -= match.retainedBytes
		} else {
			n.numPkScriptMatchBytes = 0
		}
	}
	delete(sub.matches, outpoint)
}

// trackPkScriptReceive adds a matched output to the subscription state.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) trackPkScriptReceive(sub *pkScriptSubscription,
	outpoint wire.OutPoint, value btcutil.Amount, pkScript []byte,
	fundingTx *wire.MsgTx, blockHeight uint32, blockHash *chainhash.Hash,
	txIndex uint32, cfg pkScriptWatchConfig) {

	if _, ok := sub.matches[outpoint]; ok {
		return
	}

	match := &pkScriptMatch{
		watchConfig: cfg,
		utxo: &PkScriptUTXO{
			OutPoint:    outpoint,
			Value:       value,
			PkScript:    copyBytes(pkScript),
			BlockHeight: blockHeight,
			BlockHash:   copyHash(blockHash),
			TxIndex:     txIndex,
		},
	}
	if cfg.includeTx && cfg.events.Has(PkScriptEventConfirm) {
		match.fundingTx = fundingTx
	}

	matchSize := pkScriptMatchSize(match)
	err := n.validatePkScriptMatchResourceLimits(sub, matchSize)
	if err != nil {
		n.failPkScriptMatchLimit(sub, err)

		return
	}
	match.retainedBytes = matchSize
	sub.matches[outpoint] = match
	sub.numMatches++
	sub.matchBytes += matchSize
	n.numPkScriptMatches++
	n.numPkScriptMatchBytes += matchSize

	if blockHeight+n.reorgSafetyLimit > n.currentHeight {
		addPkScriptHeightIndex(
			n.pkScriptReceivesByHeight, blockHeight, sub.id,
			outpoint,
		)
	}

	if cfg.events.Has(PkScriptEventSpend) {
		subMap, ok := n.pkScriptByOutpoint[outpoint]
		if !ok {
			subMap = make(map[uint64]struct{})
			n.pkScriptByOutpoint[outpoint] = subMap
		}
		subMap[sub.id] = struct{}{}
	}

	if cfg.events.Has(PkScriptEventConfirm) {
		match.confirmHeight = blockHeight + cfg.numConfs - 1
		addPkScriptHeightIndex(
			n.pkScriptConfsByHeight, match.confirmHeight,
			sub.id, outpoint,
		)
		n.schedulePkScriptConfirmUpdate(
			sub, match, outpoint, blockHeight,
		)
	}
}

// dispatchPkScriptConfirmUpdate sends a partial confirmation progress
// notification for a matched output.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptConfirmUpdate(sub *pkScriptSubscription,
	match *pkScriptMatch, outpoint wire.OutPoint, blockHeight uint32,
	blockHash *chainhash.Hash, block *wire.MsgBlock,
	retainedBlock **wire.MsgBlock) error {

	cfg := match.watchConfig
	if !cfg.events.Has(PkScriptEventConfirm) || !cfg.includeConfUpdates ||
		match.confirmDispatched || blockHeight >= match.confirmHeight ||
		blockHeight < match.utxo.BlockHeight {

		return nil
	}

	numConfs := blockHeight - match.utxo.BlockHeight + 1
	if numConfs == 0 || numConfs >= cfg.numConfs {
		return nil
	}

	var (
		txCopy        *wire.MsgTx
		blockCopy     *wire.MsgBlock
		internalBlock *wire.MsgBlock
	)
	if cfg.includeTx {
		txCopy = copyMsgTx(match.fundingTx)
	}
	if cfg.includeBlock {
		internalBlock = retainMsgBlock(block, retainedBlock)
		blockCopy = copyMsgBlock(internalBlock)
	}

	oldSize := match.retainedBytes
	newSize := oldSize + 128
	if internalBlock != nil {
		newSize += uint64(internalBlock.SerializeSize())
	}
	if !n.reservePkScriptMatchSize(sub, match, newSize) {
		return nil
	}

	err := n.dispatchPkScriptNotification(sub, &PkScriptNotification{
		Type:             PkScriptNotificationConfirmUpdate,
		Height:           blockHeight,
		BlockHash:        copyHash(blockHash),
		TxHash:           copyHash(&outpoint.Hash),
		TxIndex:          match.utxo.TxIndex,
		NumConfirmations: numConfs,
		RequiredConfs:    cfg.numConfs,
		UTXO:             copyPkScriptUTXO(match.utxo),
		Tx:               txCopy,
		Block:            blockCopy,
	})
	if err != nil {
		if errors.Is(err, ErrPkScriptNotificationQueueFull) {
			return nil
		}
		n.releasePkScriptMatchSize(sub, match, oldSize)

		return err
	}

	removePkScriptHeightIndex(
		n.pkScriptConfUpdatesByHeight, blockHeight, sub.id, outpoint,
	)
	n.addDispatchedPkScriptConfirmUpdate(
		sub, match, outpoint, &pkScriptConfirmUpdate{
			blockHeight: blockHeight,
			blockHash:   copyHash(blockHash),
			block:       internalBlock,
			numConfs:    numConfs,
		},
	)

	n.schedulePkScriptConfirmUpdate(sub, match, outpoint, blockHeight+1)

	return nil
}

// dispatchPkScriptConfirmUpdateReorg invalidates a dispatched partial
// confirmation progress notification.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptConfirmUpdateReorg(
	sub *pkScriptSubscription, match *pkScriptMatch, outpoint wire.OutPoint,
	blockHeight uint32) error {

	update := match.confirmUpdates[blockHeight]
	if update == nil {
		return nil
	}

	err := n.dispatchPkScriptNotification(sub, &PkScriptNotification{
		Type:             PkScriptNotificationConfirmUpdate,
		Height:           update.blockHeight,
		BlockHash:        copyHash(update.blockHash),
		TxHash:           copyHash(&outpoint.Hash),
		TxIndex:          match.utxo.TxIndex,
		NumConfirmations: update.numConfs,
		RequiredConfs:    match.watchConfig.numConfs,
		Disconnected:     true,
		UTXO:             copyPkScriptUTXO(match.utxo),
		Tx:               copyMsgTx(match.fundingTx),
		Block:            copyMsgBlock(update.block),
	})
	if err != nil {
		if errors.Is(err, ErrPkScriptNotificationQueueFull) {
			return nil
		}

		return err
	}

	n.removeDispatchedPkScriptConfirmUpdate(
		sub, match, outpoint, blockHeight,
	)

	return nil
}

// dispatchPkScriptConfirm sends a confirmation notification for the matched
// output when it reaches the configured confirmation height.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptConfirm(sub *pkScriptSubscription,
	match *pkScriptMatch, outpoint wire.OutPoint, blockHeight uint32,
	blockHash *chainhash.Hash, block *wire.MsgBlock,
	retainedBlock **wire.MsgBlock) error {

	cfg := match.watchConfig
	if !cfg.events.Has(PkScriptEventConfirm) ||
		match.confirmDispatched || match.confirmHeight != blockHeight {

		return nil
	}

	var (
		txCopy        *wire.MsgTx
		blockCopy     *wire.MsgBlock
		internalBlock *wire.MsgBlock
	)
	if cfg.includeTx {
		txCopy = copyMsgTx(match.fundingTx)
	}
	if cfg.includeBlock {
		internalBlock = retainMsgBlock(block, retainedBlock)
		blockCopy = copyMsgBlock(internalBlock)
	}

	oldSize := match.retainedBytes
	newSize := oldSize
	if internalBlock != nil {
		newSize += uint64(internalBlock.SerializeSize())
	}
	if !n.reservePkScriptMatchSize(sub, match, newSize) {
		return nil
	}

	err := n.dispatchPkScriptNotification(sub, &PkScriptNotification{
		Type:             PkScriptNotificationConfirm,
		Height:           blockHeight,
		BlockHash:        copyHash(blockHash),
		TxHash:           copyHash(&outpoint.Hash),
		TxIndex:          match.utxo.TxIndex,
		NumConfirmations: cfg.numConfs,
		RequiredConfs:    cfg.numConfs,
		UTXO:             copyPkScriptUTXO(match.utxo),
		Tx:               txCopy,
		Block:            blockCopy,
	})
	if err != nil {
		if errors.Is(err, ErrPkScriptNotificationQueueFull) {
			return nil
		}
		n.releasePkScriptMatchSize(sub, match, oldSize)

		return err
	}

	match.confirmDispatched = true
	match.confirmBlockHash = copyHash(blockHash)
	if cfg.includeBlock {
		match.confirmBlock = internalBlock
	}
	removePkScriptHeightIndex(
		n.pkScriptConfsByHeight, blockHeight, sub.id, outpoint,
	)

	if blockHeight+n.reorgSafetyLimit > n.currentHeight {
		addPkScriptHeightIndex(
			n.pkScriptConfirmedByHeight, blockHeight, sub.id,
			outpoint,
		)
	}

	if !n.shouldRetainPkScriptMatch(sub, match) {
		n.removePkScriptMatch(sub, outpoint)
	}

	return nil
}

// dispatchPkScriptConfirmReorg invalidates a dispatched confirmation
// notification.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptConfirmReorg(sub *pkScriptSubscription,
	match *pkScriptMatch, outpoint wire.OutPoint) error {

	if !match.confirmDispatched {
		return nil
	}

	err := n.dispatchPkScriptNotification(sub, &PkScriptNotification{
		Type:             PkScriptNotificationConfirm,
		Height:           match.confirmHeight,
		BlockHash:        copyHash(match.confirmBlockHash),
		TxHash:           copyHash(&outpoint.Hash),
		TxIndex:          match.utxo.TxIndex,
		NumConfirmations: match.watchConfig.numConfs,
		RequiredConfs:    match.watchConfig.numConfs,
		Disconnected:     true,
		UTXO:             copyPkScriptUTXO(match.utxo),
		Tx:               copyMsgTx(match.fundingTx),
		Block:            copyMsgBlock(match.confirmBlock),
	})
	if err != nil {
		if errors.Is(err, ErrPkScriptNotificationQueueFull) {
			return nil
		}

		return err
	}

	match.confirmDispatched = false
	match.confirmBlockHash = nil
	match.confirmBlock = nil
	n.updatePkScriptMatchSize(sub, match)
	removePkScriptHeightIndex(
		n.pkScriptConfirmedByHeight, match.confirmHeight,
		sub.id, outpoint,
	)

	return nil
}

// dispatchPkScriptSpend sends a spend notification for a tracked output.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptSpend(sub *pkScriptSubscription,
	match *pkScriptMatch, outpoint wire.OutPoint, txHash *chainhash.Hash,
	txIndex, inputIdx uint32, blockHeight uint32,
	blockHash *chainhash.Hash, tx *wire.MsgTx,
	block *wire.MsgBlock) error {

	cfg := match.watchConfig
	if !cfg.events.Has(PkScriptEventSpend) ||
		match.spendTxHash != nil {

		return nil
	}

	var (
		txCopy    *wire.MsgTx
		blockCopy *wire.MsgBlock
	)
	if cfg.includeTx {
		txCopy = copyMsgTx(tx)
	}
	if cfg.includeBlock {
		blockCopy = copyMsgBlock(block)
	}

	oldSize := match.retainedBytes
	newSize := oldSize
	if tx != nil && cfg.includeTx {
		newSize += uint64(tx.SerializeSize())
	}
	if block != nil && cfg.includeBlock {
		newSize += uint64(block.SerializeSize())
	}
	if !n.reservePkScriptMatchSize(sub, match, newSize) {
		return nil
	}

	err := n.dispatchPkScriptNotification(sub, &PkScriptNotification{
		Type:       PkScriptNotificationSpend,
		Height:     blockHeight,
		BlockHash:  copyHash(blockHash),
		TxHash:     copyHash(txHash),
		TxIndex:    txIndex,
		InputIndex: inputIdx,
		UTXO:       copyPkScriptUTXO(match.utxo),
		Tx:         txCopy,
		Block:      blockCopy,
	})
	if err != nil {
		if errors.Is(err, ErrPkScriptNotificationQueueFull) {
			return nil
		}
		n.releasePkScriptMatchSize(sub, match, oldSize)

		return err
	}

	match.spendTxHash = copyHash(txHash)
	match.spendBlockHash = copyHash(blockHash)
	if cfg.includeTx {
		match.spendTx = tx
	}
	if cfg.includeBlock {
		match.spendBlock = block
	}
	match.spendHeight = blockHeight
	match.spendTxIndex = txIndex
	match.spendInputIndex = inputIdx
	match.spendDispatched = true

	n.removeOutpointSubscription(outpoint, sub.id)

	if blockHeight+n.reorgSafetyLimit > n.currentHeight {
		addPkScriptHeightIndex(
			n.pkScriptSpendsByHeight, blockHeight, sub.id, outpoint,
		)
	}

	if !n.shouldRetainPkScriptMatch(sub, match) {
		n.removePkScriptMatch(sub, outpoint)
	}

	return nil
}

// dispatchPkScriptSpendReorg invalidates a dispatched spend notification.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptSpendReorg(sub *pkScriptSubscription,
	match *pkScriptMatch) error {

	if !match.spendDispatched {
		return nil
	}

	err := n.dispatchPkScriptNotification(sub, &PkScriptNotification{
		Type:         PkScriptNotificationSpend,
		Height:       match.spendHeight,
		BlockHash:    copyHash(match.spendBlockHash),
		TxHash:       copyHash(match.spendTxHash),
		TxIndex:      match.spendTxIndex,
		InputIndex:   match.spendInputIndex,
		Disconnected: true,
		UTXO:         copyPkScriptUTXO(match.utxo),
		Tx:           copyMsgTx(match.spendTx),
		Block:        copyMsgBlock(match.spendBlock),
	})
	if err != nil {
		if errors.Is(err, ErrPkScriptNotificationQueueFull) {
			return nil
		}

		return err
	}

	match.spendTxHash = nil
	match.spendBlockHash = nil
	match.spendTx = nil
	match.spendBlock = nil
	match.spendHeight = 0
	match.spendTxIndex = 0
	match.spendInputIndex = 0
	match.spendDispatched = false
	n.updatePkScriptMatchSize(sub, match)

	return nil
}

// dispatchPkScriptNotificationsAtTip dispatches pkScript notifications
// scheduled at the connected tip height.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptNotificationsAtTip(blockHeight uint32,
	blockHash *chainhash.Hash, block *btcutil.Block) error {

	var msgBlock *wire.MsgBlock
	if block != nil {
		msgBlock = block.MsgBlock()
	}
	var retainedBlock *wire.MsgBlock

	if len(n.pkScriptConfUpdatesByHeight[blockHeight]) > 0 {
		err := n.dispatchPkScriptConfirmUpdatesAtTip(
			blockHeight, blockHash, msgBlock, &retainedBlock,
		)
		if err != nil {
			return err
		}
	}

	if len(n.pkScriptConfsByHeight[blockHeight]) > 0 {
		err := n.dispatchPkScriptConfsAtTip(
			blockHeight, blockHash, msgBlock, &retainedBlock,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// dispatchPkScriptConfirmUpdatesAtTip dispatches partial confirmation updates
// scheduled at the given height.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptConfirmUpdatesAtTip(blockHeight uint32,
	blockHash *chainhash.Hash, block *wire.MsgBlock,
	retainedBlock **wire.MsgBlock) error {

	subIndex := n.pkScriptConfUpdatesByHeight[blockHeight]
	for subID, outpoints := range subIndex {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			delete(subIndex, subID)
			continue
		}

		for outpoint := range outpoints {
			match := sub.matches[outpoint]
			if match == nil {
				delete(outpoints, outpoint)
				continue
			}
			err := n.dispatchPkScriptConfirmUpdate(
				sub, match, outpoint, blockHeight, blockHash,
				block, retainedBlock,
			)
			if err != nil {
				return err
			}
			if _, ok := n.pkScriptNotifications[subID]; !ok {
				break
			}
		}

		if len(outpoints) == 0 {
			delete(subIndex, subID)
		}
	}

	if len(n.pkScriptConfUpdatesByHeight[blockHeight]) == 0 {
		delete(n.pkScriptConfUpdatesByHeight, blockHeight)
	}

	return nil
}

// dispatchPkScriptConfsAtTip dispatches all pkScript confirmations that mature
// at the given height.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPkScriptConfsAtTip(blockHeight uint32,
	blockHash *chainhash.Hash, block *wire.MsgBlock,
	retainedBlock **wire.MsgBlock) error {

	subIndex := n.pkScriptConfsByHeight[blockHeight]
	for subID, outpoints := range subIndex {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			delete(subIndex, subID)
			continue
		}

		for outpoint := range outpoints {
			match := sub.matches[outpoint]
			if match == nil {
				delete(outpoints, outpoint)
				continue
			}
			err := n.dispatchPkScriptConfirm(
				sub, match, outpoint, blockHeight, blockHash,
				block, retainedBlock,
			)
			if err != nil {
				return err
			}
			if _, ok := n.pkScriptNotifications[subID]; !ok {
				break
			}
		}

		if len(outpoints) == 0 {
			delete(subIndex, subID)
		}
	}

	if len(n.pkScriptConfsByHeight[blockHeight]) == 0 {
		delete(n.pkScriptConfsByHeight, blockHeight)
	}

	return nil
}

// processPkScriptTxAtTip updates pkScript subscription state using a
// transaction seen at the chain tip.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) processPkScriptTxAtTip(tx *btcutil.Tx, txIndex,
	blockHeight uint32, block *btcutil.Block,
	pendingSpends *[]pkScriptPendingSpend) error {

	txHash := tx.Hash()
	var (
		blockHash *chainhash.Hash
		msgBlock  *wire.MsgBlock
	)
	if block != nil {
		blockHash = block.Hash()
		msgBlock = block.MsgBlock()
	}
	var (
		retainedFundingTx  *wire.MsgTx
		retainedSpendTx    *wire.MsgTx
		retainedSpendBlock *wire.MsgBlock
	)

	for inputIdx, txIn := range tx.MsgTx().TxIn {
		prevOut := txIn.PreviousOutPoint
		subscriptions := n.pkScriptByOutpoint[prevOut]
		for subID := range subscriptions {
			sub := n.pkScriptNotifications[subID]
			if sub == nil {
				continue
			}

			match := sub.matches[prevOut]
			if match == nil {
				continue
			}

			var spendTx *wire.MsgTx
			if match.watchConfig.includeTx {
				if retainedSpendTx == nil {
					retainedSpendTx = copyMsgTx(tx.MsgTx())
				}
				spendTx = retainedSpendTx
			}
			var spendBlock *wire.MsgBlock
			if match.watchConfig.includeBlock {
				if retainedSpendBlock == nil {
					retainedSpendBlock = copyMsgBlock(
						msgBlock,
					)
				}
				spendBlock = retainedSpendBlock
			}

			*pendingSpends = append(
				*pendingSpends, pkScriptPendingSpend{
					subID:      subID,
					outpoint:   prevOut,
					txHash:     copyHash(txHash),
					txIndex:    txIndex,
					inputIndex: uint32(inputIdx),
					height:     blockHeight,
					blockHash:  copyHash(blockHash),
					tx:         spendTx,
					block:      spendBlock,
				})
		}
	}

	for outIdx, txOut := range tx.MsgTx().TxOut {
		subscriptions := n.pkScriptByScript[string(txOut.PkScript)]
		if len(subscriptions) == 0 {
			continue
		}

		outpoint := wire.OutPoint{
			Hash:  *txHash,
			Index: uint32(outIdx),
		}

		for subID := range subscriptions {
			sub := n.pkScriptNotifications[subID]
			if sub == nil {
				continue
			}
			cfg, ok := sub.scripts[string(txOut.PkScript)]
			if !ok {
				continue
			}
			if cfg.includeTx &&
				cfg.events.Has(PkScriptEventConfirm) &&
				retainedFundingTx == nil {

				retainedFundingTx = copyMsgTx(tx.MsgTx())
			}

			n.trackPkScriptReceive(
				sub, outpoint, btcutil.Amount(txOut.Value),
				txOut.PkScript, retainedFundingTx, blockHeight,
				blockHash, txIndex, cfg,
			)
		}
	}

	return nil
}

// dispatchPendingPkScriptSpends dispatches spends found while scanning a block
// after confirmation/update notifications for the same block have been sent.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchPendingPkScriptSpends(
	pendingSpends []pkScriptPendingSpend) error {

	for _, spend := range pendingSpends {
		sub := n.pkScriptNotifications[spend.subID]
		if sub == nil {
			continue
		}

		match := sub.matches[spend.outpoint]
		if match == nil {
			continue
		}

		err := n.dispatchPkScriptSpend(
			sub, match, spend.outpoint, spend.txHash, spend.txIndex,
			spend.inputIndex, spend.height, spend.blockHash,
			spend.tx, spend.block,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// pruneMaturePkScriptState removes pkScript reorg indexes that are no longer
// needed and drops fully mature matches when possible.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) pruneMaturePkScriptState(matureBlockHeight uint32) {
	pruneIndex := func(
		index map[uint32]map[uint64]map[wire.OutPoint]struct{}) {

		subIndex := index[matureBlockHeight]
		for subID, outpoints := range subIndex {
			sub := n.pkScriptNotifications[subID]
			if sub == nil {
				continue
			}

			for outpoint := range outpoints {
				match := sub.matches[outpoint]
				if match == nil {
					continue
				}

				if !n.shouldRetainPkScriptMatch(sub, match) {
					n.removePkScriptMatch(sub, outpoint)
				}
			}
		}

		delete(index, matureBlockHeight)
	}

	pruneIndex(n.pkScriptReceivesByHeight)

	updateIndex :=
		n.pkScriptConfUpdatesDispatchedByHeight[matureBlockHeight]
	for subID, outpoints := range updateIndex {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			continue
		}

		for outpoint := range outpoints {
			match := sub.matches[outpoint]
			if match == nil {
				continue
			}

			n.removeDispatchedPkScriptConfirmUpdate(
				sub, match, outpoint, matureBlockHeight,
			)
			if !n.shouldRetainPkScriptMatch(sub, match) {
				n.removePkScriptMatch(sub, outpoint)
			}
		}
	}
	delete(n.pkScriptConfUpdatesDispatchedByHeight, matureBlockHeight)

	confirmedIndex := n.pkScriptConfirmedByHeight[matureBlockHeight]
	for subID, outpoints := range confirmedIndex {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			continue
		}

		for outpoint := range outpoints {
			match := sub.matches[outpoint]
			if match == nil {
				continue
			}

			// Confirmation reorg data is no longer useful once
			// the confirmation height is beyond the reorg horizon.
			// Release it even if an unspent match remains for spend
			// detection.
			match.fundingTx = nil
			match.confirmBlockHash = nil
			match.confirmBlock = nil
			n.updatePkScriptMatchSize(sub, match)

			if !n.shouldRetainPkScriptMatch(sub, match) {
				n.removePkScriptMatch(sub, outpoint)
			}
		}
	}
	delete(n.pkScriptConfirmedByHeight, matureBlockHeight)

	spendIndex := n.pkScriptSpendsByHeight[matureBlockHeight]
	for subID, outpoints := range spendIndex {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			continue
		}

		for outpoint := range outpoints {
			match := sub.matches[outpoint]
			if match == nil {
				continue
			}

			// Retain the fact that this output was spent, but
			// release the raw reorg payload once the spend cannot
			// be disconnected.
			match.spendBlockHash = nil
			match.spendTx = nil
			match.spendBlock = nil
			n.updatePkScriptMatchSize(sub, match)

			if !n.shouldRetainPkScriptMatch(sub, match) {
				n.removePkScriptMatch(sub, outpoint)
			}
		}
	}
	delete(n.pkScriptSpendsByHeight, matureBlockHeight)
}

// disconnectPkScriptTip rolls back pkScript indexes and dispatches
// disconnected notifications for the block being disconnected.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) disconnectPkScriptTip(blockHeight uint32) error {
	pkScriptSpends := n.pkScriptSpendsByHeight[blockHeight]
	for subID, outpoints := range pkScriptSpends {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			continue
		}

		for outpoint := range outpoints {
			match := sub.matches[outpoint]
			if match == nil {
				continue
			}

			err := n.dispatchPkScriptSpendReorg(sub, match)
			if err != nil {
				return err
			}
			if _, ok := n.pkScriptNotifications[subID]; !ok {
				continue
			}

			if match.watchConfig.events.Has(PkScriptEventSpend) {
				subMap, ok := n.pkScriptByOutpoint[outpoint]
				if !ok {
					subMap = make(map[uint64]struct{})
					n.pkScriptByOutpoint[outpoint] = subMap
				}
				subMap[subID] = struct{}{}
			}
		}
	}
	delete(n.pkScriptSpendsByHeight, blockHeight)

	pkScriptConfirmed := n.pkScriptConfirmedByHeight[blockHeight]
	for subID, outpoints := range pkScriptConfirmed {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			continue
		}

		for outpoint := range outpoints {
			match := sub.matches[outpoint]
			if match == nil {
				continue
			}

			err := n.dispatchPkScriptConfirmReorg(
				sub, match, outpoint,
			)
			if err != nil {
				return err
			}
			if _, ok := n.pkScriptNotifications[subID]; !ok {
				continue
			}

			addPkScriptHeightIndex(
				n.pkScriptConfsByHeight, match.confirmHeight,
				subID, outpoint,
			)
		}
	}
	delete(n.pkScriptConfirmedByHeight, blockHeight)

	pkScriptConfirmUpdates :=
		n.pkScriptConfUpdatesDispatchedByHeight[blockHeight]
	for subID, outpoints := range pkScriptConfirmUpdates {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			continue
		}

		for outpoint := range outpoints {
			match := sub.matches[outpoint]
			if match == nil {
				continue
			}

			err := n.dispatchPkScriptConfirmUpdateReorg(
				sub, match, outpoint, blockHeight,
			)
			if err != nil {
				return err
			}
			if _, ok := n.pkScriptNotifications[subID]; !ok {
				continue
			}

			n.schedulePkScriptConfirmUpdate(
				sub, match, outpoint, blockHeight,
			)
		}
	}
	delete(n.pkScriptConfUpdatesDispatchedByHeight, blockHeight)

	pkScriptReceives := n.pkScriptReceivesByHeight[blockHeight]
	for subID, outpoints := range pkScriptReceives {
		sub := n.pkScriptNotifications[subID]
		if sub == nil {
			continue
		}

		for outpoint := range outpoints {
			n.removePkScriptMatch(sub, outpoint)
		}
	}
	delete(n.pkScriptReceivesByHeight, blockHeight)

	return nil
}
