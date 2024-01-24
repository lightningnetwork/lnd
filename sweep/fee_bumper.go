package sweep

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrNotEnoughBudget is returned when the fee bumper decides the
	// current budget cannot cover the fee.
	ErrNotEnoughBudget = errors.New("not enough budget")
)

type FeeBumpStrategy uint8

const (
	// FeeBumpStrategyAggressive indicates that the fee bumping should
	// always use the larger fee rate between the calculated fee rate and
	// the estimated fee rate.
	FeeBumpStrategyAggressive FeeBumpStrategy = iota

	// FeeBumpStrategyConservative indicates that the fee bumping should
	// always use the smaller fee rate between the calculated fee rate and
	// the estimated fee rate.
	FeeBumpStrategyConservative

	// TODO(yy): define more sophisticated strategies.
)

// Bumper defines an interface that can be used by other subsystems for fee
// bumping.
type Bumper interface {
	// Broadcast is used to publish the tx created from the given inputs
	// specified in the request. It handles the tx creation, broadcasts it,
	// and monitors its confirmation status for potential fee bumping. It
	// returns a chan that the caller can use to receive updates about the
	// broadcast result and potential RBF attempts.
	//
	// NOTE: it's expected to have a BumpResult sent immediately once the
	// first broadcast attempt is finished.
	Broadcast(req *BumpRequest) (<-chan *BumpResult, error)
}

// BumpRequest is used by the caller to give the Bumper the necessary info to
// create and manage potential fee bumps for a set of inputs.
type BumpRequest struct {
	// Inputs is the set of inputs to sweep.
	Inputs InputSet

	// ChangeScript is the script to send the change output to.
	ChangeScript []byte

	// MaxFeeRate is the maximum fee rate that can be used for fee bumping.
	MaxFeeRate chainfee.SatPerKWeight

	// Strategy is the fee bumping strategy to use.
	Strategy FeeBumpStrategy
}

// BumpResult is used by the Bumper to send updates about the initial broadcast
// result and future RBF results if attemptted.
type BumpResult struct {
	// Confirmed indicates whether the `CurrentTx` was confirmed or not.
	// This is used as a signal to terminate the subscription of the
	// returned result chan.
	Confirmed bool

	// PreviousTx is the tx that's being replaced. Initially, this is the
	// tx that was broadcasted from the initial request. If a fee bump is
	// attempted, this will be the tx that's being replaced by the new tx.
	PreviousTx *wire.MsgTx

	// CurrentTx is the replacing tx if a fee bump is attempted.
	CurrentTx *wire.MsgTx

	// FeeRate is the fee rate used for the new tx.
	FeeRate chainfee.SatPerKWeight

	// Fee is the fee paid by the new tx.
	Fee btcutil.Amount

	// Err is the error that occurred during the broadcast.
	Err error

	// requestID is the ID of the request that created this record.
	requestID uint64
}

// TxPublisherConfig is the config used to create a new TxPublisher.
type TxPublisherConfig struct {
	// Signer is used to create the tx signature.
	Signer input.Signer

	// Wallet is used primarily to publish the tx.
	Wallet Wallet

	// Estimator is used to estimate the fee rate for the new tx based on
	// its deadline conf target.
	Estimator chainfee.Estimator

	// Notifier is used to monitor the confirmation status of the tx.
	Notifier chainntnfs.ChainNotifier

	// WeightEstimator is used to calculate the weight of the tx.
	WeightEstimator input.TxWeightEstimator
}

// TxPublisher is an implementation of the Bumper interface. It utilizes the
// `testmempoolaccept` RPC to bump the fee of txns it created based on
// different fee function selected or configed by the caller. Its purpose is to
// take a list of inputs specified, and create a tx that spends them to a
// specified output. It will then monitor the confirmation status of the tx,
// and if it's not confirmed within a certain time frame, it will attempt to
// bump the fee of the tx by creating a new tx that spends the same inputs to
// the same output, but with a higher fee rate. It will continue to do this
// until the tx is confirmed or the fee rate reaches the maximum fee rate
// specified by the caller.
type TxPublisher struct {
	cfg *TxPublisherConfig
	wg  sync.WaitGroup

	// currentHeight is the current block height.
	currentHeight int32

	// records is a map keyed by the txid of the txns being monitored.
	records lnutils.SyncMap[uint64, *monitorRecord]

	// requestCounter is a monotonically increasing counter used to keep
	// track of how many requests have been made.
	requestCounter atomic.Uint64

	// resultChans is a map keyed by the requestCounter, each item is the
	// chan that the publisher sends the fee bump result to.
	resultChans lnutils.SyncMap[uint64, chan *BumpResult]

	// quit is used to signal the publisher to stop.
	quit chan struct{}
}

// Compile-time constraint to ensure TxPublisher implements Bumper.
var _ Bumper = (*TxPublisher)(nil)

// NewTxPublisher creates a new TxPublisher.
func NewTxPublisher(cfg *TxPublisherConfig) *TxPublisher {
	return &TxPublisher{
		cfg:     cfg,
		records: lnutils.SyncMap[uint64, *monitorRecord]{},
		// TODO(yy): use queue instead?
		resultChans: lnutils.SyncMap[uint64, chan *BumpResult]{},
		quit:        make(chan struct{}),
	}
}

// Broadcast is used to publish the tx created from the given inputs. It will,
// 1. init a fee function based on the given strategy.
// 2. create an RBF-compliant tx and monitor it for confimation.
// 3. notify the initial broadcast result back to the caller.
// The initial broadcast is guaranteed to be RBF-compliant unless the budget
// specified cannot cover the fee.
//
// NOTE: part of the Bumper interface.
func (t *TxPublisher) Broadcast(req *BumpRequest) (<-chan *BumpResult, error) {
	log.Tracef("Received broadcast for inputs=%v", req.Inputs)

	// Attempt an initial broadcast which is guaranteed to comply with the
	// RBF rules.
	result, err := t.initialBroadcast(req)
	if err != nil {
		return nil, err
	}

	// Create a chan to send the result to the caller.
	resultChan := make(chan *BumpResult, 1)
	t.resultChans.Store(result.requestID, resultChan)

	// Send the initial broadcast result to the caller.
	t.notifyAndRemove(result)

	return resultChan, nil
}

// initialBroadcast initializes a fee function, creates an RBF-compliant tx and
// broadcasts it.
func (t *TxPublisher) initialBroadcast(req *BumpRequest) (*BumpResult, error) {
	// Create a fee bumping algorithm to be used for future RBF.
	feeAlgo, err := t.initializeFeeFunction(req)
	if err != nil {
		return nil, fmt.Errorf("init fee function: %w", err)
	}

	// Create the initial tx to be broadcasted. This tx is guaranteed to
	// comply with the RBF restrictions.
	requestID, err := t.createRBFCompliantTx(req, feeAlgo)
	if err != nil {
		return nil, fmt.Errorf("create RBF-compliant tx: %w", err)
	}

	// Broadcast the tx and return the monitored record.
	result, err := t.broadcast(requestID)
	if err != nil {
		return nil, fmt.Errorf("broadcast sweep tx: %w", err)
	}

	return result, nil
}

// initializeFeeFunction initializes a fee function to be used for this request
// for future fee bumping.
func (t *TxPublisher) initializeFeeFunction(
	req *BumpRequest) (FeeFunction, error) {

	// Get the size of the sweep tx, which will be used to calculate the
	// budget fee rate.
	size, err := t.calcSweepTxWeight(req.Inputs.Inputs(), req.ChangeScript)
	if err != nil {
		return nil, err
	}

	// Use the budget and MaxFeeRate to decide the max allowed fee rate.
	// This is needed as, when the input has a large value and the user
	// sets the budget to be proportional to the input value, the fee rate
	// can be very high and we need to make sure it doesn't exceed the max
	// fee rate.
	maxFeeRateAllowed := chainfee.NewSatPerKWeight(
		req.Inputs.Budget(), size,
	)
	if maxFeeRateAllowed > req.MaxFeeRate {
		log.Debugf("Budget fee rate %v exceeds max fee rate %v, use "+
			"max instead", maxFeeRateAllowed, req.MaxFeeRate)

		maxFeeRateAllowed = req.MaxFeeRate
	}

	// Get the initial conf target.
	confTarget := t.calcCurrentConfTarget(req.Inputs.DeadlineHeight())

	// Initialize the fee function and return it.
	//
	// TODO(yy): return based on differet req.Strategy?
	return NewLinearFeeFunction(
		maxFeeRateAllowed, confTarget, t.cfg.Estimator,
	)
}

// calcCurrentConfTarget calculates the current confirmation target based on
// the deadline height. The conf target is capped at 0 if the deadline has
// already been past.
func (t *TxPublisher) calcCurrentConfTarget(deadline int32) uint32 {
	var confTarget uint32

	// Calculate how many blocks left until the deadline.
	deadlineDelta := deadline - t.currentHeight

	// If we are already past the deadline, we will set the conf target to
	// be 0.
	if deadlineDelta < 0 {
		log.Warnf("Deadline is %d blocks behind current height %v",
			-deadlineDelta, t.currentHeight)

		confTarget = 0
	} else {
		confTarget = uint32(deadlineDelta)
	}

	return confTarget
}

// calcSweepTxWeight calculates the weight of the sweep tx. It assumes a
// sweeping tx always has a single output(change).
func (t *TxPublisher) calcSweepTxWeight(inputs []input.Input,
	output []byte) (uint64, error) {

	// Use a const fee rate as we only use the weight estimator to
	// calculate the size.
	const feeRate = 1

	// Initialize the tx weight estimator with,
	// - nil outputs as we only have one single change output.
	// - const fee rate as we don't care about the fees here.
	// - 0 maxfeerate as we don't care about fees here.
	//
	// TODO(yy): we should refactor the weight estimator to not require a
	// fee rate and max fee rate and make it a pure tx weight calculator.
	_, estimator, err := getWeightEstimate(
		inputs, nil, feeRate, 0, output,
	)
	if err != nil {
		return 0, err
	}

	return uint64(estimator.weight()), nil
}

// createRBFCompliantTx creates a tx that is compliant with RBF rules. It does
// so by creating a tx, validate it using `TestMempoolAccept`, and bump its fee
// and redo the process until the tx is valid, or return an error when non-RBF
// related errors occur or the budget has been used up.
func (t *TxPublisher) createRBFCompliantTx(req *BumpRequest,
	f FeeFunction) (uint64, error) {

	for {
		// Create a new tx with the given fee rate and check its
		// mempool acceptance.
		tx, fee, err := t.createAndCheckTx(
			req.Inputs, req.ChangeScript, f,
		)

		switch {
		case err == nil:
			// Increase the request counter.
			//
			// NOTE: this is the only place where we increase the
			// counter.
			requestID := t.requestCounter.Add(1)

			// Register the record.
			t.records.Store(requestID, &monitorRecord{
				tx:           tx,
				set:          req.Inputs,
				changeScript: req.ChangeScript,
				feeFunction:  f,
				fee:          fee,
			})

			return requestID, nil

		// If the error indicates the fees paid is not enough, we will
		// ask the fee function to increase the fee rate and retry.
		//
		// TODO(yy): add this new error in btcwallet to map RBF related
		// reject reasons:
		// case errors.Is(err, lnwallet.ErrInsufficientFee):
		case errors.Is(err, lnwallet.ErrMempoolFee):
			// If the fee function tells us that we have used up
			// the budget, we will return an error indicating this
			// tx cannot be made. The sweeper should handle this
			// error and try to cluster these inputs differetly.
			err = f.IncreaseFeeRate()
			if err != nil {
				return 0, err
			}

		// TODO(yy): suppose there's only one bad input, we can do a
		// binary search to find out which input is causing this error
		// by recreating a tx using half of the inputs and check its
		// mempool acceptance.
		default:
			return 0, err
		}
	}
}

// createAndCheckTx creates a tx based on the given inputs, change output
// script, and the fee rate. In addition, it validates the tx's mempool
// acceptance before returning a tx that can be published directly, along with
// its fee.
func (t *TxPublisher) createAndCheckTx(set InputSet, changeScript []byte,
	f FeeFunction) (*wire.MsgTx, btcutil.Amount, error) {

	// Create the sweep tx with max fee rate of 0 as the fee function
	// guarantees the fee rate used here won't exceed the max fee rate.
	//
	// TODO(yy): refactor this function to not require a max fee rate.
	tx, fee, err := createSweepTx(
		set, nil, changeScript, uint32(t.currentHeight),
		f.FeeRate(), 0, t.cfg.Signer,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("create sweep tx: %v", err)
	}

	// Sanity check the budget still covers the fee.
	if fee > set.Budget() {
		return nil, 0, fmt.Errorf("%w: budget=%v, fee=%v",
			ErrNotEnoughBudget, set.Budget(), fee)
	}

	// TODO(yy): skip this check for neutrino.
	err = t.TestMempoolAccept(tx, 0)
	if err != nil {
		return nil, 0, err
	}

	return tx, fee, nil
}

// broadcast takes a monitored tx and publishes it to the network. Prior to the
// broadcast, it will subscribe the tx's confirmation notification and attach
// the event channel on the record. Any broadcast-related errors will not be
// returned here, instead, they will be put inside the `BumpResult` and
// returned to the caller.
func (t *TxPublisher) broadcast(requestID uint64) (*BumpResult, error) {
	// Get the record being monitored.
	record, ok := t.records.Load(requestID)
	if !ok {
		return nil, fmt.Errorf("tx record %v not found", requestID)
	}

	txid := record.tx.TxHash()

	// Subscribe to its confirmation notification.
	confEvent, err := t.cfg.Notifier.RegisterConfirmationsNtfn(
		&txid, nil, 1, uint32(t.currentHeight),
	)
	if err != nil {
		return nil, fmt.Errorf("register confirmation ntfn: %v", err)
	}

	// Attach the confirmation event channel to the record.
	record.confEvent = confEvent

	tx := record.tx
	log.Debugf("Publishing sweep tx %v, num_inputs=%v, height=%v",
		txid, len(tx.TxIn), t.currentHeight)

	// Publish the sweeping tx with customized label. If the publish fails,
	// this error will be saved in the `BumpResult` and it will be removed
	// from being monitored.
	err = t.cfg.Wallet.PublishTransaction(
		tx, labels.MakeLabel(labels.LabelTypeSweepTransaction, nil),
	)
	if err != nil {
		// NOTE: we decide to attach this error to the result instead
		// of returning it here because by the time the tx reaches
		// here, it should have passed the mempool acceptance check. If
		// it still fails to be broadcast, it's likely a non-RBF
		// related errors happened. So we send this error back to the
		// caller so that it can handle it properly.
		//
		// TODO(yy): find out which input is causing the failure.
		log.Errorf("Failed to publish tx %v: %v", txid, err)
	}

	result := &BumpResult{
		CurrentTx: record.tx,
		Fee:       record.fee,
		FeeRate:   record.feeFunction.FeeRate(),
		Err:       err,
		requestID: requestID,
	}

	return result, nil
}

// notifyAndRemove sends the result to the resultChan specified by the
// requestID. This channel is expected to be read by the caller. If the result
// contains a non-nil error, or the tx is confirmed, the record will be removed
// from the maps.
func (t *TxPublisher) notifyAndRemove(result *BumpResult) {
	id := result.requestID
	resultChan, ok := t.resultChans.Load(id)
	if !ok {
		log.Errorf("Result chan for id=%v not found", id)
		return
	}

	log.Debugf("Sending result for requestID=%v", id)

	select {
	// TODO(yy): Add timeout in case it's blocking?
	case resultChan <- result:

	case <-t.quit:
		log.Debug("Fee bumper stopped")
	}

	// Remove the record from the maps if there's an error. This means this
	// tx has failed its broadcast and cannot be retried. There are two
	// cases,
	// - when the budget cannot cover the fee.
	// - when a non-RBF related error occurs.
	if result.Err != nil {
		log.Errorf("Removing monitor record=%v due to err: %v",
			id, result.Err)
		t.resultChans.Delete(id)
		t.records.Delete(id)
	}

	// Remove the record is the tx is confirmed.
	if result.Confirmed {
		log.Infof("Removing confirmed monitor record=%v", id)
		t.resultChans.Delete(id)
		t.records.Delete(id)
	}
}

// monitorRecord is used to keep track of the tx being monitored by the
// publisher internally.
type monitorRecord struct {
	// tx is the tx being monitored.
	tx *wire.MsgTx

	// set is the input set used to create the tx. It's supplied by the
	// caller.
	set InputSet

	// changeScript is the script to send the change output to.
	changeScript []byte

	// confEvent is the subscription to the confirmation event of the tx.
	confEvent *chainntnfs.ConfirmationEvent

	// feeFunction is the fee bumping algorithm used by the publisher.
	feeFunction FeeFunction

	// fee is the fee paid by the tx.
	fee btcutil.Amount
}

// Start starts the publisher by subscribing to block epoch updates and kicking
// off the monitor loop.
func (t *TxPublisher) Start() error {
	log.Info("TxPublisher starting...")

	blockEvent, err := t.cfg.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return fmt.Errorf("register block epoch ntfn: %v", err)
	}

	select {
	case bestBlock := <-blockEvent.Epochs:
		t.currentHeight = bestBlock.Height

	case <-t.quit:
		log.Debugf("TxPublisher shutting down, exit start process")
		return nil
	}

	t.wg.Add(1)
	go t.monitor(blockEvent)

	return nil
}

// Stop stops the publisher and waits for the monitor loop to exit.
func (t *TxPublisher) Stop() {
	log.Info("TxPublisher stopping...")

	t.wg.Wait()
	close(t.quit)
}

// monitor is the main loop driven by new blocks. Whevenr a new block arrives,
// it will examine all the txns being monitored, and check if any of them needs
// to be bumped. If so, it will attempt to bump the fee of the tx.
func (t *TxPublisher) monitor(blockEvent *chainntnfs.BlockEpochEvent) {
	defer blockEvent.Cancel()
	defer t.wg.Done()

	select {
	case epoch, ok := <-blockEvent.Epochs:
		if !ok {
			// We should stop the publisher before stopping the
			// chain service. Otherwise it indicates an error.
			log.Error("Block epoch channel closed, exit monitor")

			return
		}

		// Update the best known height for the publisher.
		t.currentHeight = epoch.Height

		// Check all monitored txns to see if any of them needs to be
		// bumped.
		t.processRecords()

	case <-t.quit:
		log.Debug("Fee bumper stopped, exit monitor")
		return
	}
}

// processRecords checks all the txns being monitored, and check if any of them
// needs to be bumped. If so, it will attempt to bump the fee of the tx.
func (t *TxPublisher) processRecords() {
	// confirmed is a helper closure that returns a boolean indicating
	// whether the tx is confirmed.
	confirmed := func(txid chainhash.Hash,
		event *chainntnfs.ConfirmationEvent) bool {

		select {
		case event := <-event.Confirmed:
			log.Debugf("Sweep tx %v confirmed in block %v", txid,
				event.BlockHeight)

			return true

		default:
			return false
		}
	}

	// visitor is a helper closure that visits each record and performs an
	// RBF if necessary.
	visitor := func(requestID uint64, r *monitorRecord) error {
		oldTxid := r.tx.TxHash()
		log.Tracef("Checking monitor record=%v for tx=%v", requestID,
			oldTxid)

		// If the tx is already confirmed, we can stop monitoring it.
		if confirmed(oldTxid, r.confEvent) {
			// Create a result that will be sent to the resultChan
			// which is listened by the caller.
			result := &BumpResult{
				CurrentTx: r.tx,
				Confirmed: true,
				requestID: requestID,
			}

			// Notify that this tx is confirmed and remove the
			// record from the map.
			t.notifyAndRemove(result)

			// Move to the next record.
			return nil
		}

		// Get the current conf target for this record.
		confTarget := t.calcCurrentConfTarget(r.set.DeadlineHeight())

		// Ask the fee function whether a bump is needed. We expect the
		// fee function to increase its returned fee rate after calling
		// this method.
		if r.feeFunction.SkipFeeBump(confTarget) {
			log.Debugf("Skip bumping tx %v at height=%v", oldTxid,
				t.currentHeight)

			return nil
		}

		// The fee function now has a new fee rate, we will use it to
		// bump the fee of the tx.
		result, err := t.createAndPublishTx(requestID, r)
		if err != nil {
			log.Errorf("Failed to bump tx %v: %v", oldTxid, err)

			// Return nil so the visitor can continue to the next
			// record.
			return nil
		}

		// Notify the new result.
		t.notifyAndRemove(result)

		return nil
	}

	// TODO(yy): need to double check as the above `visitor` will put data
	// in the sync map, so we may need to do a read first.
	t.records.ForEach(visitor)
}

// createAndPublishTx creates a new tx with a higher fee rate and publishes it
// to the network. It will update the record with the new tx and fee rate if
// successfully created, and return the result when published successfully.
func (t *TxPublisher) createAndPublishTx(requestID uint64,
	r *monitorRecord) (*BumpResult, error) {

	// Fetch the old tx.
	oldTx := r.tx

	// Create a new tx with the new fee rate.
	tx, fee, err := t.createAndCheckTx(r.set, r.changeScript, r.feeFunction)

	// If the tx doesn't not have enought budget, we will return a result
	// so the sweeper can handle it by re-clustering the utxos.
	if errors.Is(err, ErrNotEnoughBudget) {
		return &BumpResult{
			PreviousTx: oldTx,
			Err:        err,
			requestID:  requestID,
		}, nil

	}

	// If the error is not budget related, we will return an error and let
	// the fee bumper retry it at next block.
	//
	// NOTE: we can check the RBF error here and ask the fee function to
	// recalculate the fee rate. However, this would defeat the purpose of
	// using a deadline based fee function:
	// - if the deadline is far away, there's no rush to RBF the tx.
	// - if the deadline is close, we expect the fee function to give us a
	//   higher fee rate. If the fee rate cannot satisfy the RBF rules, it
	//   means the budget is not enough.
	if err != nil {
		return nil, err
	}

	// Register a new record by overwriting the same requestID.
	t.records.Store(requestID, &monitorRecord{
		tx:           tx,
		set:          r.set,
		changeScript: r.changeScript,
		feeFunction:  r.feeFunction,
		fee:          fee,
	})

	// Attempt to broadcast this new tx.
	result, err := t.broadcast(requestID)
	if err != nil {
		return nil, err
	}

	// Attach the old tx and return.
	result.PreviousTx = oldTx

	return result, nil
}

// TODO(yy): temp, remove this placeholder once the TestMempoolAccept PR is
// merged.
func (t *TxPublisher) TestMempoolAccept(*wire.MsgTx,
	chainfee.SatPerKWeight) error {

	return nil
}
