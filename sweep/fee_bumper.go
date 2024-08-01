package sweep

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrInvalidBumpResult is returned when the bump result is invalid.
	ErrInvalidBumpResult = errors.New("invalid bump result")

	// ErrNotEnoughBudget is returned when the fee bumper decides the
	// current budget cannot cover the fee.
	ErrNotEnoughBudget = errors.New("not enough budget")

	// ErrLocktimeImmature is returned when sweeping an input whose
	// locktime is not reached.
	ErrLocktimeImmature = errors.New("immature input")

	// ErrTxNoOutput is returned when an output cannot be created during tx
	// preparation, usually due to the output being dust.
	ErrTxNoOutput = errors.New("tx has no output")

	// ErrThirdPartySpent is returned when a third party has spent the
	// input in the sweeping tx.
	ErrThirdPartySpent = errors.New("third party spent the output")
)

// Bumper defines an interface that can be used by other subsystems for fee
// bumping.
type Bumper interface {
	// Broadcast is used to publish the tx created from the given inputs
	// specified in the request. It handles the tx creation, broadcasts it,
	// and monitors its confirmation status for potential fee bumping. It
	// returns a chan that the caller can use to receive updates about the
	// broadcast result and potential RBF attempts.
	Broadcast(req *BumpRequest) (<-chan *BumpResult, error)
}

// BumpEvent represents the event of a fee bumping attempt.
type BumpEvent uint8

const (
	// TxPublished is sent when the broadcast attempt is finished.
	TxPublished BumpEvent = iota

	// TxFailed is sent when the broadcast attempt fails.
	TxFailed

	// TxReplaced is sent when the original tx is replaced by a new one.
	TxReplaced

	// TxConfirmed is sent when the tx is confirmed.
	TxConfirmed

	// sentinalEvent is used to check if an event is unknown.
	sentinalEvent
)

// String returns a human-readable string for the event.
func (e BumpEvent) String() string {
	switch e {
	case TxPublished:
		return "Published"
	case TxFailed:
		return "Failed"
	case TxReplaced:
		return "Replaced"
	case TxConfirmed:
		return "Confirmed"
	default:
		return "Unknown"
	}
}

// Unknown returns true if the event is unknown.
func (e BumpEvent) Unknown() bool {
	return e >= sentinalEvent
}

// BumpRequest is used by the caller to give the Bumper the necessary info to
// create and manage potential fee bumps for a set of inputs.
type BumpRequest struct {
	// Budget givens the total amount that can be used as fees by these
	// inputs.
	Budget btcutil.Amount

	// Inputs is the set of inputs to sweep.
	Inputs []input.Input

	// DeadlineHeight is the block height at which the tx should be
	// confirmed.
	DeadlineHeight int32

	// DeliveryAddress is the script to send the change output to.
	DeliveryAddress []byte

	// MaxFeeRate is the maximum fee rate that can be used for fee bumping.
	MaxFeeRate chainfee.SatPerKWeight

	// StartingFeeRate is an optional parameter that can be used to specify
	// the initial fee rate to use for the fee function.
	StartingFeeRate fn.Option[chainfee.SatPerKWeight]
}

// MaxFeeRateAllowed returns the maximum fee rate allowed for the given
// request. It calculates the feerate using the supplied budget and the weight,
// compares it with the specified MaxFeeRate, and returns the smaller of the
// two.
func (r *BumpRequest) MaxFeeRateAllowed() (chainfee.SatPerKWeight, error) {
	// Get the size of the sweep tx, which will be used to calculate the
	// budget fee rate.
	size, err := calcSweepTxWeight(r.Inputs, r.DeliveryAddress)
	if err != nil {
		return 0, err
	}

	// Use the budget and MaxFeeRate to decide the max allowed fee rate.
	// This is needed as, when the input has a large value and the user
	// sets the budget to be proportional to the input value, the fee rate
	// can be very high and we need to make sure it doesn't exceed the max
	// fee rate.
	maxFeeRateAllowed := chainfee.NewSatPerKWeight(r.Budget, size)
	if maxFeeRateAllowed > r.MaxFeeRate {
		log.Debugf("Budget feerate %v exceeds MaxFeeRate %v, use "+
			"MaxFeeRate instead, txWeight=%v", maxFeeRateAllowed,
			r.MaxFeeRate, size)

		return r.MaxFeeRate, nil
	}

	log.Debugf("Budget feerate %v below MaxFeeRate %v, use budget feerate "+
		"instead, txWeight=%v", maxFeeRateAllowed, r.MaxFeeRate, size)

	return maxFeeRateAllowed, nil
}

// calcSweepTxWeight calculates the weight of the sweep tx. It assumes a
// sweeping tx always has a single output(change).
func calcSweepTxWeight(inputs []input.Input,
	outputPkScript []byte) (lntypes.WeightUnit, error) {

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
		inputs, nil, feeRate, 0, outputPkScript,
	)
	if err != nil {
		return 0, err
	}

	return estimator.weight(), nil
}

// BumpResult is used by the Bumper to send updates about the tx being
// broadcast.
type BumpResult struct {
	// Event is the type of event that the result is for.
	Event BumpEvent

	// Tx is the tx being broadcast.
	Tx *wire.MsgTx

	// ReplacedTx is the old, replaced tx if a fee bump is attempted.
	ReplacedTx *wire.MsgTx

	// FeeRate is the fee rate used for the new tx.
	FeeRate chainfee.SatPerKWeight

	// Fee is the fee paid by the new tx.
	Fee btcutil.Amount

	// Err is the error that occurred during the broadcast.
	Err error

	// requestID is the ID of the request that created this record.
	requestID uint64
}

// Validate validates the BumpResult so it's safe to use.
func (b *BumpResult) Validate() error {
	// Every result must have a tx.
	if b.Tx == nil {
		return fmt.Errorf("%w: nil tx", ErrInvalidBumpResult)
	}

	// Every result must have a known event.
	if b.Event.Unknown() {
		return fmt.Errorf("%w: unknown event", ErrInvalidBumpResult)
	}

	// If it's a replacing event, it must have a replaced tx.
	if b.Event == TxReplaced && b.ReplacedTx == nil {
		return fmt.Errorf("%w: nil replacing tx", ErrInvalidBumpResult)
	}

	// If it's a failed event, it must have an error.
	if b.Event == TxFailed && b.Err == nil {
		return fmt.Errorf("%w: nil error", ErrInvalidBumpResult)
	}

	// If it's a confirmed event, it must have a fee rate and fee.
	if b.Event == TxConfirmed && (b.FeeRate == 0 || b.Fee == 0) {
		return fmt.Errorf("%w: missing fee rate or fee",
			ErrInvalidBumpResult)
	}

	return nil
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
	started atomic.Bool
	stopped atomic.Bool

	wg sync.WaitGroup

	// cfg specifies the configuration of the TxPublisher.
	cfg *TxPublisherConfig

	// currentHeight is the current block height.
	currentHeight atomic.Int32

	// records is a map keyed by the requestCounter and the value is the tx
	// being monitored.
	records lnutils.SyncMap[uint64, *monitorRecord]

	// requestCounter is a monotonically increasing counter used to keep
	// track of how many requests have been made.
	requestCounter atomic.Uint64

	// subscriberChans is a map keyed by the requestCounter, each item is
	// the chan that the publisher sends the fee bump result to.
	subscriberChans lnutils.SyncMap[uint64, chan *BumpResult]

	// quit is used to signal the publisher to stop.
	quit chan struct{}
}

// Compile-time constraint to ensure TxPublisher implements Bumper.
var _ Bumper = (*TxPublisher)(nil)

// NewTxPublisher creates a new TxPublisher.
func NewTxPublisher(cfg TxPublisherConfig) *TxPublisher {
	return &TxPublisher{
		cfg:             &cfg,
		records:         lnutils.SyncMap[uint64, *monitorRecord]{},
		subscriberChans: lnutils.SyncMap[uint64, chan *BumpResult]{},
		quit:            make(chan struct{}),
	}
}

// isNeutrinoBackend checks if the wallet backend is neutrino.
func (t *TxPublisher) isNeutrinoBackend() bool {
	return t.cfg.Wallet.BackEnd() == "neutrino"
}

// Broadcast is used to publish the tx created from the given inputs. It will,
// 1. init a fee function based on the given strategy.
// 2. create an RBF-compliant tx and monitor it for confirmation.
// 3. notify the initial broadcast result back to the caller.
// The initial broadcast is guaranteed to be RBF-compliant unless the budget
// specified cannot cover the fee.
//
// NOTE: part of the Bumper interface.
func (t *TxPublisher) Broadcast(req *BumpRequest) (<-chan *BumpResult, error) {
	log.Tracef("Received broadcast request: %s", lnutils.SpewLogClosure(
		req))

	// Attempt an initial broadcast which is guaranteed to comply with the
	// RBF rules.
	result, err := t.initialBroadcast(req)
	if err != nil {
		log.Errorf("Initial broadcast failed: %v", err)

		return nil, err
	}

	// Create a chan to send the result to the caller.
	subscriber := make(chan *BumpResult, 1)
	t.subscriberChans.Store(result.requestID, subscriber)

	// Send the initial broadcast result to the caller.
	t.handleResult(result)

	return subscriber, nil
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

	// Get the max allowed feerate.
	maxFeeRateAllowed, err := req.MaxFeeRateAllowed()
	if err != nil {
		return nil, err
	}

	// Get the initial conf target.
	confTarget := calcCurrentConfTarget(
		t.currentHeight.Load(), req.DeadlineHeight,
	)

	log.Debugf("Initializing fee function with conf target=%v, budget=%v, "+
		"maxFeeRateAllowed=%v", confTarget, req.Budget,
		maxFeeRateAllowed)

	// Initialize the fee function and return it.
	//
	// TODO(yy): return based on differet req.Strategy?
	return NewLinearFeeFunction(
		maxFeeRateAllowed, confTarget, t.cfg.Estimator,
		req.StartingFeeRate,
	)
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
		tx, fee, err := t.createAndCheckTx(req, f)

		switch {
		case err == nil:
			// The tx is valid, return the request ID.
			requestID := t.storeRecord(tx, req, f, fee)

			log.Infof("Created tx %v for %v inputs: feerate=%v, "+
				"fee=%v, inputs=%v", tx.TxHash(),
				len(req.Inputs), f.FeeRate(), fee,
				inputTypeSummary(req.Inputs))

			return requestID, nil

		// If the error indicates the fees paid is not enough, we will
		// ask the fee function to increase the fee rate and retry.
		case errors.Is(err, lnwallet.ErrMempoolFee):
			// We should at least start with a feerate above the
			// mempool min feerate, so if we get this error, it
			// means something is wrong earlier in the pipeline.
			log.Errorf("Current fee=%v, feerate=%v, %v", fee,
				f.FeeRate(), err)

			fallthrough

		// We are not paying enough fees so we increase it.
		case errors.Is(err, chain.ErrInsufficientFee):
			increased := false

			// Keep calling the fee function until the fee rate is
			// increased or maxed out.
			for !increased {
				log.Debugf("Increasing fee for next round, "+
					"current fee=%v, feerate=%v", fee,
					f.FeeRate())

				// If the fee function tells us that we have
				// used up the budget, we will return an error
				// indicating this tx cannot be made. The
				// sweeper should handle this error and try to
				// cluster these inputs differetly.
				increased, err = f.Increment()
				if err != nil {
					return 0, err
				}
			}

		// TODO(yy): suppose there's only one bad input, we can do a
		// binary search to find out which input is causing this error
		// by recreating a tx using half of the inputs and check its
		// mempool acceptance.
		default:
			log.Debugf("Failed to create RBF-compliant tx: %v", err)
			return 0, err
		}
	}
}

// storeRecord stores the given record in the records map.
func (t *TxPublisher) storeRecord(tx *wire.MsgTx, req *BumpRequest,
	f FeeFunction, fee btcutil.Amount) uint64 {

	// Increase the request counter.
	//
	// NOTE: this is the only place where we increase the
	// counter.
	requestID := t.requestCounter.Add(1)

	// Register the record.
	t.records.Store(requestID, &monitorRecord{
		tx:          tx,
		req:         req,
		feeFunction: f,
		fee:         fee,
	})

	return requestID
}

// createAndCheckTx creates a tx based on the given inputs, change output
// script, and the fee rate. In addition, it validates the tx's mempool
// acceptance before returning a tx that can be published directly, along with
// its fee.
func (t *TxPublisher) createAndCheckTx(req *BumpRequest, f FeeFunction) (
	*wire.MsgTx, btcutil.Amount, error) {

	// Create the sweep tx with max fee rate of 0 as the fee function
	// guarantees the fee rate used here won't exceed the max fee rate.
	tx, fee, err := t.createSweepTx(
		req.Inputs, req.DeliveryAddress, f.FeeRate(),
	)
	if err != nil {
		return nil, fee, fmt.Errorf("create sweep tx: %w", err)
	}

	// Sanity check the budget still covers the fee.
	if fee > req.Budget {
		return nil, fee, fmt.Errorf("%w: budget=%v, fee=%v",
			ErrNotEnoughBudget, req.Budget, fee)
	}

	// Validate the tx's mempool acceptance.
	err = t.cfg.Wallet.CheckMempoolAcceptance(tx)

	// Exit early if the tx is valid.
	if err == nil {
		return tx, fee, nil
	}

	// Print an error log if the chain backend doesn't support the mempool
	// acceptance test RPC.
	if errors.Is(err, rpcclient.ErrBackendVersion) {
		log.Errorf("TestMempoolAccept not supported by backend, " +
			"consider upgrading it to a newer version")
		return tx, fee, nil
	}

	// We are running on a backend that doesn't implement the RPC
	// testmempoolaccept, eg, neutrino, so we'll skip the check.
	if errors.Is(err, chain.ErrUnimplemented) {
		log.Debug("Skipped testmempoolaccept due to not implemented")
		return tx, fee, nil
	}

	return nil, fee, fmt.Errorf("tx=%v failed mempool check: %w",
		tx.TxHash(), err)
}

// broadcast takes a monitored tx and publishes it to the network. Prior to the
// broadcast, it will subscribe the tx's confirmation notification and attach
// the event channel to the record. Any broadcast-related errors will not be
// returned here, instead, they will be put inside the `BumpResult` and
// returned to the caller.
func (t *TxPublisher) broadcast(requestID uint64) (*BumpResult, error) {
	// Get the record being monitored.
	record, ok := t.records.Load(requestID)
	if !ok {
		return nil, fmt.Errorf("tx record %v not found", requestID)
	}

	txid := record.tx.TxHash()

	tx := record.tx
	log.Debugf("Publishing sweep tx %v, num_inputs=%v, height=%v",
		txid, len(tx.TxIn), t.currentHeight.Load())

	// Set the event, and change it to TxFailed if the wallet fails to
	// publish it.
	event := TxPublished

	// Publish the sweeping tx with customized label. If the publish fails,
	// this error will be saved in the `BumpResult` and it will be removed
	// from being monitored.
	err := t.cfg.Wallet.PublishTransaction(
		tx, labels.MakeLabel(labels.LabelTypeSweepTransaction, nil),
	)
	if err != nil {
		// NOTE: we decide to attach this error to the result instead
		// of returning it here because by the time the tx reaches
		// here, it should have passed the mempool acceptance check. If
		// it still fails to be broadcast, it's likely a non-RBF
		// related error happened. So we send this error back to the
		// caller so that it can handle it properly.
		//
		// TODO(yy): find out which input is causing the failure.
		log.Errorf("Failed to publish tx %v: %v", txid, err)
		event = TxFailed
	}

	result := &BumpResult{
		Event:     event,
		Tx:        record.tx,
		Fee:       record.fee,
		FeeRate:   record.feeFunction.FeeRate(),
		Err:       err,
		requestID: requestID,
	}

	return result, nil
}

// notifyResult sends the result to the resultChan specified by the requestID.
// This channel is expected to be read by the caller.
func (t *TxPublisher) notifyResult(result *BumpResult) {
	id := result.requestID
	subscriber, ok := t.subscriberChans.Load(id)
	if !ok {
		log.Errorf("Result chan for id=%v not found", id)
		return
	}

	log.Debugf("Sending result for requestID=%v, tx=%v", id,
		result.Tx.TxHash())

	select {
	// Send the result to the subscriber.
	//
	// TODO(yy): Add timeout in case it's blocking?
	case subscriber <- result:
	case <-t.quit:
		log.Debug("Fee bumper stopped")
	}
}

// removeResult removes the tracking of the result if the result contains a
// non-nil error, or the tx is confirmed, the record will be removed from the
// maps.
func (t *TxPublisher) removeResult(result *BumpResult) {
	id := result.requestID

	// Remove the record from the maps if there's an error. This means this
	// tx has failed its broadcast and cannot be retried. There are two
	// cases,
	// - when the budget cannot cover the fee.
	// - when a non-RBF related error occurs.
	switch result.Event {
	case TxFailed:
		log.Errorf("Removing monitor record=%v, tx=%v, due to err: %v",
			id, result.Tx.TxHash(), result.Err)

	case TxConfirmed:
		// Remove the record is the tx is confirmed.
		log.Debugf("Removing confirmed monitor record=%v, tx=%v", id,
			result.Tx.TxHash())

	// Do nothing if it's neither failed or confirmed.
	default:
		log.Tracef("Skipping record removal for id=%v, event=%v", id,
			result.Event)

		return
	}

	t.records.Delete(id)
	t.subscriberChans.Delete(id)
}

// handleResult handles the result of a tx broadcast. It will notify the
// subscriber and remove the record if the tx is confirmed or failed to be
// broadcast.
func (t *TxPublisher) handleResult(result *BumpResult) {
	// Notify the subscriber.
	t.notifyResult(result)

	// Remove the record if it's failed or confirmed.
	t.removeResult(result)
}

// monitorRecord is used to keep track of the tx being monitored by the
// publisher internally.
type monitorRecord struct {
	// tx is the tx being monitored.
	tx *wire.MsgTx

	// req is the original request.
	req *BumpRequest

	// feeFunction is the fee bumping algorithm used by the publisher.
	feeFunction FeeFunction

	// fee is the fee paid by the tx.
	fee btcutil.Amount
}

// Start starts the publisher by subscribing to block epoch updates and kicking
// off the monitor loop.
func (t *TxPublisher) Start() error {
	log.Info("TxPublisher starting...")

	if t.started.Swap(true) {
		return fmt.Errorf("TxPublisher started more than once")
	}

	blockEvent, err := t.cfg.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return fmt.Errorf("register block epoch ntfn: %w", err)
	}

	t.wg.Add(1)
	go t.monitor(blockEvent)

	log.Debugf("TxPublisher started")

	return nil
}

// Stop stops the publisher and waits for the monitor loop to exit.
func (t *TxPublisher) Stop() error {
	log.Info("TxPublisher stopping...")

	if t.stopped.Swap(true) {
		return fmt.Errorf("TxPublisher stopped more than once")
	}

	close(t.quit)
	t.wg.Wait()

	log.Debug("TxPublisher stopped")

	return nil
}

// monitor is the main loop driven by new blocks. Whevenr a new block arrives,
// it will examine all the txns being monitored, and check if any of them needs
// to be bumped. If so, it will attempt to bump the fee of the tx.
//
// NOTE: Must be run as a goroutine.
func (t *TxPublisher) monitor(blockEvent *chainntnfs.BlockEpochEvent) {
	defer blockEvent.Cancel()
	defer t.wg.Done()

	for {
		select {
		case epoch, ok := <-blockEvent.Epochs:
			if !ok {
				// We should stop the publisher before stopping
				// the chain service. Otherwise it indicates an
				// error.
				log.Error("Block epoch channel closed, exit " +
					"monitor")

				return
			}

			log.Debugf("TxPublisher received new block: %v",
				epoch.Height)

			// Update the best known height for the publisher.
			t.currentHeight.Store(epoch.Height)

			// Check all monitored txns to see if any of them needs
			// to be bumped.
			t.processRecords()

		case <-t.quit:
			log.Debug("Fee bumper stopped, exit monitor")
			return
		}
	}
}

// processRecords checks all the txns being monitored, and checks if any of
// them needs to be bumped. If so, it will attempt to bump the fee of the tx.
func (t *TxPublisher) processRecords() {
	// confirmedRecords stores a map of the records which have been
	// confirmed.
	confirmedRecords := make(map[uint64]*monitorRecord)

	// feeBumpRecords stores a map of the records which need to be bumped.
	feeBumpRecords := make(map[uint64]*monitorRecord)

	// failedRecords stores a map of the records which has inputs being
	// spent by a third party.
	//
	// NOTE: this is only used for neutrino backend.
	failedRecords := make(map[uint64]*monitorRecord)

	// visitor is a helper closure that visits each record and divides them
	// into two groups.
	visitor := func(requestID uint64, r *monitorRecord) error {
		log.Tracef("Checking monitor recordID=%v for tx=%v", requestID,
			r.tx.TxHash())

		// If the tx is already confirmed, we can stop monitoring it.
		if t.isConfirmed(r.tx.TxHash()) {
			confirmedRecords[requestID] = r

			// Move to the next record.
			return nil
		}

		// Check whether the inputs has been spent by a third party.
		//
		// NOTE: this check is only done for neutrino backend.
		if t.isThirdPartySpent(r.tx.TxHash(), r.req.Inputs) {
			failedRecords[requestID] = r

			// Move to the next record.
			return nil
		}

		feeBumpRecords[requestID] = r

		// Return nil to move to the next record.
		return nil
	}

	// Iterate through all the records and divide them into two groups.
	t.records.ForEach(visitor)

	// For records that are confirmed, we'll notify the caller about this
	// result.
	for requestID, r := range confirmedRecords {
		rec := r

		log.Debugf("Tx=%v is confirmed", r.tx.TxHash())
		t.wg.Add(1)
		go t.handleTxConfirmed(rec, requestID)
	}

	// Get the current height to be used in the following goroutines.
	currentHeight := t.currentHeight.Load()

	// For records that are not confirmed, we perform a fee bump if needed.
	for requestID, r := range feeBumpRecords {
		rec := r

		log.Debugf("Attempting to fee bump Tx=%v", r.tx.TxHash())
		t.wg.Add(1)
		go t.handleFeeBumpTx(requestID, rec, currentHeight)
	}

	// For records that are failed, we'll notify the caller about this
	// result.
	for requestID, r := range failedRecords {
		rec := r

		log.Debugf("Tx=%v has inputs been spent by a third party, "+
			"failing it now", r.tx.TxHash())
		t.wg.Add(1)
		go t.handleThirdPartySpent(rec, requestID)
	}
}

// handleTxConfirmed is called when a monitored tx is confirmed. It will
// notify the subscriber then remove the record from the maps .
//
// NOTE: Must be run as a goroutine to avoid blocking on sending the result.
func (t *TxPublisher) handleTxConfirmed(r *monitorRecord, requestID uint64) {
	defer t.wg.Done()

	// Create a result that will be sent to the resultChan which is
	// listened by the caller.
	result := &BumpResult{
		Event:     TxConfirmed,
		Tx:        r.tx,
		requestID: requestID,
		Fee:       r.fee,
		FeeRate:   r.feeFunction.FeeRate(),
	}

	// Notify that this tx is confirmed and remove the record from the map.
	t.handleResult(result)
}

// handleFeeBumpTx checks if the tx needs to be bumped, and if so, it will
// attempt to bump the fee of the tx.
//
// NOTE: Must be run as a goroutine to avoid blocking on sending the result.
func (t *TxPublisher) handleFeeBumpTx(requestID uint64, r *monitorRecord,
	currentHeight int32) {

	defer t.wg.Done()

	oldTxid := r.tx.TxHash()

	// Get the current conf target for this record.
	confTarget := calcCurrentConfTarget(currentHeight, r.req.DeadlineHeight)

	// Ask the fee function whether a bump is needed. We expect the fee
	// function to increase its returned fee rate after calling this
	// method.
	increased, err := r.feeFunction.IncreaseFeeRate(confTarget)
	if err != nil {
		// TODO(yy): send this error back to the sweeper so it can
		// re-group the inputs?
		log.Errorf("Failed to increase fee rate for tx %v at "+
			"height=%v: %v", oldTxid, t.currentHeight.Load(), err)

		return
	}

	// If the fee rate was not increased, there's no need to bump the fee.
	if !increased {
		log.Tracef("Skip bumping tx %v at height=%v", oldTxid,
			t.currentHeight.Load())

		return
	}

	// The fee function now has a new fee rate, we will use it to bump the
	// fee of the tx.
	resultOpt := t.createAndPublishTx(requestID, r)

	// If there's a result, we will notify the caller about the result.
	resultOpt.WhenSome(func(result BumpResult) {
		// Notify the new result.
		t.handleResult(&result)
	})
}

// handleThirdPartySpent is called when the inputs in an unconfirmed tx is
// spent. It will notify the subscriber then remove the record from the maps
// and send a TxFailed event to the subscriber.
//
// NOTE: Must be run as a goroutine to avoid blocking on sending the result.
func (t *TxPublisher) handleThirdPartySpent(r *monitorRecord,
	requestID uint64) {

	defer t.wg.Done()

	// Create a result that will be sent to the resultChan which is
	// listened by the caller.
	//
	// TODO(yy): create a new state `TxThirdPartySpent` to notify the
	// sweeper to remove the input, hence moving the monitoring of inputs
	// spent inside the fee bumper.
	result := &BumpResult{
		Event:     TxFailed,
		Tx:        r.tx,
		requestID: requestID,
		Err:       ErrThirdPartySpent,
	}

	// Notify that this tx is confirmed and remove the record from the map.
	t.handleResult(result)
}

// createAndPublishTx creates a new tx with a higher fee rate and publishes it
// to the network. It will update the record with the new tx and fee rate if
// successfully created, and return the result when published successfully.
func (t *TxPublisher) createAndPublishTx(requestID uint64,
	r *monitorRecord) fn.Option[BumpResult] {

	// Fetch the old tx.
	oldTx := r.tx

	// Create a new tx with the new fee rate.
	//
	// NOTE: The fee function is expected to have increased its returned
	// fee rate after calling the SkipFeeBump method. So we can use it
	// directly here.
	tx, fee, err := t.createAndCheckTx(r.req, r.feeFunction)

	// If the error is fee related, we will return no error and let the fee
	// bumper retry it at next block.
	//
	// NOTE: we can check the RBF error here and ask the fee function to
	// recalculate the fee rate. However, this would defeat the purpose of
	// using a deadline based fee function:
	// - if the deadline is far away, there's no rush to RBF the tx.
	// - if the deadline is close, we expect the fee function to give us a
	//   higher fee rate. If the fee rate cannot satisfy the RBF rules, it
	//   means the budget is not enough.
	if errors.Is(err, chain.ErrInsufficientFee) ||
		errors.Is(err, lnwallet.ErrMempoolFee) {

		log.Debugf("Failed to bump tx %v: %v", oldTx.TxHash(), err)
		return fn.None[BumpResult]()
	}

	// If the error is not fee related, we will return a `TxFailed` event
	// so this input can be retried.
	if err != nil {
		// If the tx doesn't not have enought budget, we will return a
		// result so the sweeper can handle it by re-clustering the
		// utxos.
		if errors.Is(err, ErrNotEnoughBudget) {
			log.Warnf("Fail to fee bump tx %v: %v", oldTx.TxHash(),
				err)
		} else {
			// Otherwise, an unexpected error occurred, we will
			// fail the tx and let the sweeper retry the whole
			// process.
			log.Errorf("Failed to bump tx %v: %v", oldTx.TxHash(),
				err)
		}

		return fn.Some(BumpResult{
			Event:     TxFailed,
			Tx:        oldTx,
			Err:       err,
			requestID: requestID,
		})
	}

	// The tx has been created without any errors, we now register a new
	// record by overwriting the same requestID.
	t.records.Store(requestID, &monitorRecord{
		tx:          tx,
		req:         r.req,
		feeFunction: r.feeFunction,
		fee:         fee,
	})

	// Attempt to broadcast this new tx.
	result, err := t.broadcast(requestID)
	if err != nil {
		log.Infof("Failed to broadcast replacement tx %v: %v",
			tx.TxHash(), err)

		return fn.None[BumpResult]()
	}

	// If the result error is fee related, we will return no error and let
	// the fee bumper retry it at next block.
	//
	// NOTE: we may get this error if we've bypassed the mempool check,
	// which means we are suing neutrino backend.
	if errors.Is(result.Err, chain.ErrInsufficientFee) ||
		errors.Is(result.Err, lnwallet.ErrMempoolFee) {

		log.Debugf("Failed to bump tx %v: %v", oldTx.TxHash(), err)
		return fn.None[BumpResult]()
	}

	// A successful replacement tx is created, attach the old tx.
	result.ReplacedTx = oldTx

	// If the new tx failed to be published, we will return the result so
	// the caller can handle it.
	if result.Event == TxFailed {
		return fn.Some(*result)
	}

	log.Infof("Replaced tx=%v with new tx=%v", oldTx.TxHash(), tx.TxHash())

	// Otherwise, it's a successful RBF, set the event and return.
	result.Event = TxReplaced

	return fn.Some(*result)
}

// isConfirmed checks the btcwallet to see whether the tx is confirmed.
func (t *TxPublisher) isConfirmed(txid chainhash.Hash) bool {
	details, err := t.cfg.Wallet.GetTransactionDetails(&txid)
	if err != nil {
		log.Warnf("Failed to get tx details for %v: %v", txid, err)
		return false
	}

	return details.NumConfirmations > 0
}

// isThirdPartySpent checks whether the inputs of the tx has already been spent
// by a third party. When a tx is not confirmed, yet its inputs has been spent,
// then it must be spent by a different tx other than the sweeping tx here.
//
// NOTE: this check is only performed for neutrino backend as it has no
// reliable way to tell a tx has been replaced.
func (t *TxPublisher) isThirdPartySpent(txid chainhash.Hash,
	inputs []input.Input) bool {

	// Skip this check for if this is not neutrino backend.
	if !t.isNeutrinoBackend() {
		return false
	}

	// Iterate all the inputs and check if they have been spent already.
	for _, inp := range inputs {
		op := inp.OutPoint()

		// For wallet utxos, the height hint is not set - we don't need
		// to monitor them for third party spend.
		heightHint := inp.HeightHint()
		if heightHint == 0 {
			log.Debugf("Skipped third party check for wallet "+
				"input %v", op)

			continue
		}

		// If the input has already been spent after the height hint, a
		// spend event is sent back immediately.
		spendEvent, err := t.cfg.Notifier.RegisterSpendNtfn(
			&op, inp.SignDesc().Output.PkScript, heightHint,
		)
		if err != nil {
			log.Criticalf("Failed to register spend ntfn for "+
				"input=%v: %v", op, err)
			return false
		}

		// Remove the subscription when exit.
		defer spendEvent.Cancel()

		// Do a non-blocking read to see if the output has been spent.
		select {
		case spend, ok := <-spendEvent.Spend:
			if !ok {
				log.Debugf("Spend ntfn for %v canceled", op)
				return false
			}

			spendingTxID := spend.SpendingTx.TxHash()

			// If the spending tx is the same as the sweeping tx
			// then we are good.
			if spendingTxID == txid {
				continue
			}

			log.Warnf("Detected third party spent of output=%v "+
				"in tx=%v", op, spend.SpendingTx.TxHash())

			return true

		// Move to the next input.
		default:
		}
	}

	return false
}

// calcCurrentConfTarget calculates the current confirmation target based on
// the deadline height. The conf target is capped at 0 if the deadline has
// already been past.
func calcCurrentConfTarget(currentHeight, deadline int32) uint32 {
	var confTarget uint32

	// Calculate how many blocks left until the deadline.
	deadlineDelta := deadline - currentHeight

	// If we are already past the deadline, we will set the conf target to
	// be 1.
	if deadlineDelta < 0 {
		log.Warnf("Deadline is %d blocks behind current height %v",
			-deadlineDelta, currentHeight)

		confTarget = 0
	} else {
		confTarget = uint32(deadlineDelta)
	}

	return confTarget
}

// createSweepTx creates a sweeping tx based on the given inputs, change
// address and fee rate.
func (t *TxPublisher) createSweepTx(inputs []input.Input, changePkScript []byte,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx, btcutil.Amount, error) {

	// Validate and calculate the fee and change amount.
	txFee, changeAmtOpt, locktimeOpt, err := prepareSweepTx(
		inputs, changePkScript, feeRate, t.currentHeight.Load(),
	)
	if err != nil {
		return nil, 0, err
	}

	var (
		// Create the sweep transaction that we will be building. We
		// use version 2 as it is required for CSV.
		sweepTx = wire.NewMsgTx(2)

		// We'll add the inputs as we go so we know the final ordering
		// of inputs to sign.
		idxs []input.Input
	)

	// We start by adding all inputs that commit to an output. We do this
	// since the input and output index must stay the same for the
	// signatures to be valid.
	for _, o := range inputs {
		if o.RequiredTxOut() == nil {
			continue
		}

		idxs = append(idxs, o)
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: o.OutPoint(),
			Sequence:         o.BlocksToMaturity(),
		})
		sweepTx.AddTxOut(o.RequiredTxOut())
	}

	// Sum up the value contained in the remaining inputs, and add them to
	// the sweep transaction.
	for _, o := range inputs {
		if o.RequiredTxOut() != nil {
			continue
		}

		idxs = append(idxs, o)
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: o.OutPoint(),
			Sequence:         o.BlocksToMaturity(),
		})
	}

	// If there's a change amount, add it to the transaction.
	changeAmtOpt.WhenSome(func(changeAmt btcutil.Amount) {
		sweepTx.AddTxOut(&wire.TxOut{
			PkScript: changePkScript,
			Value:    int64(changeAmt),
		})
	})

	// We'll default to using the current block height as locktime, if none
	// of the inputs commits to a different locktime.
	sweepTx.LockTime = uint32(locktimeOpt.UnwrapOr(t.currentHeight.Load()))

	prevInputFetcher, err := input.MultiPrevOutFetcher(inputs)
	if err != nil {
		return nil, 0, fmt.Errorf("error creating prev input fetcher "+
			"for hash cache: %v", err)
	}
	hashCache := txscript.NewTxSigHashes(sweepTx, prevInputFetcher)

	// With all the inputs in place, use each output's unique input script
	// function to generate the final witness required for spending.
	addInputScript := func(idx int, tso input.Input) error {
		inputScript, err := tso.CraftInputScript(
			t.cfg.Signer, sweepTx, hashCache, prevInputFetcher, idx,
		)
		if err != nil {
			return err
		}

		sweepTx.TxIn[idx].Witness = inputScript.Witness

		if len(inputScript.SigScript) == 0 {
			return nil
		}

		sweepTx.TxIn[idx].SignatureScript = inputScript.SigScript

		return nil
	}

	for idx, inp := range idxs {
		if err := addInputScript(idx, inp); err != nil {
			return nil, 0, err
		}
	}

	log.Debugf("Created sweep tx %v for inputs:\n%v", sweepTx.TxHash(),
		inputTypeSummary(inputs))

	return sweepTx, txFee, nil
}

// prepareSweepTx returns the tx fee, an optional change amount and an optional
// locktime after a series of validations:
// 1. check the locktime has been reached.
// 2. check the locktimes are the same.
// 3. check the inputs cover the outputs.
//
// NOTE: if the change amount is below dust, it will be added to the tx fee.
func prepareSweepTx(inputs []input.Input, changePkScript []byte,
	feeRate chainfee.SatPerKWeight, currentHeight int32) (
	btcutil.Amount, fn.Option[btcutil.Amount], fn.Option[int32], error) {

	noChange := fn.None[btcutil.Amount]()
	noLocktime := fn.None[int32]()

	// Creating a weight estimator with nil outputs and zero max fee rate.
	// We don't allow adding customized outputs in the sweeping tx, and the
	// fee rate is already being managed before we get here.
	inputs, estimator, err := getWeightEstimate(
		inputs, nil, feeRate, 0, changePkScript,
	)
	if err != nil {
		return 0, noChange, noLocktime, err
	}

	txFee := estimator.fee()

	var (
		// Track whether any of the inputs require a certain locktime.
		locktime = int32(-1)

		// We keep track of total input amount, and required output
		// amount to use for calculating the change amount below.
		totalInput     btcutil.Amount
		requiredOutput btcutil.Amount
	)

	// Go through each input and check if the required lock times have
	// reached and are the same.
	for _, o := range inputs {
		// If the input has a required output, we'll add it to the
		// required output amount.
		if o.RequiredTxOut() != nil {
			requiredOutput += btcutil.Amount(
				o.RequiredTxOut().Value,
			)
		}

		// Update the total input amount.
		totalInput += btcutil.Amount(o.SignDesc().Output.Value)

		lt, ok := o.RequiredLockTime()

		// Skip if the input doesn't require a lock time.
		if !ok {
			continue
		}

		// Check if the lock time has reached
		if lt > uint32(currentHeight) {
			return 0, noChange, noLocktime, ErrLocktimeImmature
		}

		// If another input commits to a different locktime, they
		// cannot be combined in the same transaction.
		if locktime != -1 && locktime != int32(lt) {
			return 0, noChange, noLocktime, ErrLocktimeConflict
		}

		// Update the locktime for next iteration.
		locktime = int32(lt)
	}

	// Make sure total output amount is less than total input amount.
	if requiredOutput+txFee > totalInput {
		return 0, noChange, noLocktime, fmt.Errorf("insufficient "+
			"input to create sweep tx: input_sum=%v, "+
			"output_sum=%v", totalInput, requiredOutput+txFee)
	}

	// The value remaining after the required output and fees is the
	// change output.
	changeAmt := totalInput - requiredOutput - txFee
	changeAmtOpt := fn.Some(changeAmt)

	// We'll calculate the dust limit for the given changePkScript since it
	// is variable.
	changeFloor := lnwallet.DustLimitForSize(len(changePkScript))

	// If the change amount is dust, we'll move it into the fees.
	if changeAmt < changeFloor {
		log.Infof("Change amt %v below dustlimit %v, not adding "+
			"change output", changeAmt, changeFloor)

		// If there's no required output, and the change output is a
		// dust, it means we are creating a tx without any outputs. In
		// this case we'll return an error. This could happen when
		// creating a tx that has an anchor as the only input.
		if requiredOutput == 0 {
			return 0, noChange, noLocktime, ErrTxNoOutput
		}

		// The dust amount is added to the fee.
		txFee += changeAmt

		// Set the change amount to none.
		changeAmtOpt = fn.None[btcutil.Amount]()
	}

	// Optionally set the locktime.
	locktimeOpt := fn.Some(locktime)
	if locktime == -1 {
		locktimeOpt = noLocktime
	}

	log.Debugf("Creating sweep tx for %v inputs (%s) using %v, "+
		"tx_weight=%v, tx_fee=%v, locktime=%v, parents_count=%v, "+
		"parents_fee=%v, parents_weight=%v, current_height=%v",
		len(inputs), inputTypeSummary(inputs), feeRate,
		estimator.weight(), txFee, locktimeOpt, len(estimator.parents),
		estimator.parentsFee, estimator.parentsWeight, currentHeight)

	return txFee, changeAmtOpt, locktimeOpt, nil
}
