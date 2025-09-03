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
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/tlv"
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

	// ErrUnknownSpent is returned when an unknown tx has spent an input in
	// the sweeping tx.
	ErrUnknownSpent = errors.New("unknown spend of input")

	// ErrInputMissing is returned when a given input no longer exists,
	// e.g., spending from an orphan tx.
	ErrInputMissing = errors.New("input no longer exists")
)

var (
	// dummyChangePkScript is a dummy tapscript change script that's used
	// when we don't need a real address, just something that can be used
	// for fee estimation.
	dummyChangePkScript = []byte{
		0x51, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

// Bumper defines an interface that can be used by other subsystems for fee
// bumping.
type Bumper interface {
	// Broadcast is used to publish the tx created from the given inputs
	// specified in the request. It handles the tx creation, broadcasts it,
	// and monitors its confirmation status for potential fee bumping. It
	// returns a chan that the caller can use to receive updates about the
	// broadcast result and potential RBF attempts.
	Broadcast(req *BumpRequest) <-chan *BumpResult
}

// BumpEvent represents the event of a fee bumping attempt.
type BumpEvent uint8

const (
	// TxPublished is sent when the broadcast attempt is finished.
	TxPublished BumpEvent = iota

	// TxFailed is sent when the tx has encountered a fee-related error
	// during its creation or broadcast, or an internal error from the fee
	// bumper. In either case the inputs in this tx should be retried with
	// either a different grouping strategy or an increased budget.
	//
	// TODO(yy): Remove the above usage once we remove sweeping non-CPFP
	// anchors.
	TxFailed

	// TxReplaced is sent when the original tx is replaced by a new one.
	TxReplaced

	// TxConfirmed is sent when the tx is confirmed.
	TxConfirmed

	// TxUnknownSpend is sent when at least one of the inputs is spent but
	// not by the current sweeping tx, this can happen when,
	// - a remote party has replaced our sweeping tx by spending the
	//   input(s), e.g., via the direct preimage spend on our outgoing HTLC.
	// - a third party has replaced our sweeping tx, e.g., the anchor output
	//   after 16 blocks.
	// - A previous sweeping tx has confirmed but the fee bumper is not
	//   aware of it, e.g., a restart happens right after the sweeping tx is
	//   broadcast and confirmed.
	TxUnknownSpend

	// TxFatal is sent when the inputs in this tx cannot be retried. Txns
	// will end up in this state if they have encountered a non-fee related
	// error, which means they cannot be retried with increased budget.
	TxFatal

	// sentinelEvent is used to check if an event is unknown.
	sentinelEvent
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
	case TxUnknownSpend:
		return "UnknownSpend"
	case TxFatal:
		return "Fatal"
	default:
		return "Unknown"
	}
}

// Unknown returns true if the event is unknown.
func (e BumpEvent) Unknown() bool {
	return e >= sentinelEvent
}

// BumpRequest is used by the caller to give the Bumper the necessary info to
// create and manage potential fee bumps for a set of inputs.
type BumpRequest struct {
	// Budget gives the total amount that can be used as fees by these
	// inputs.
	Budget btcutil.Amount

	// Inputs is the set of inputs to sweep.
	Inputs []input.Input

	// DeadlineHeight is the block height at which the tx should be
	// confirmed.
	DeadlineHeight int32

	// DeliveryAddress is the script to send the change output to.
	DeliveryAddress lnwallet.AddrWithKey

	// MaxFeeRate is the maximum fee rate that can be used for fee bumping.
	MaxFeeRate chainfee.SatPerKWeight

	// StartingFeeRate is an optional parameter that can be used to specify
	// the initial fee rate to use for the fee function.
	StartingFeeRate fn.Option[chainfee.SatPerKWeight]

	// ExtraTxOut tracks if this bump request has an optional set of extra
	// outputs to add to the transaction.
	ExtraTxOut fn.Option[SweepOutput]

	// Immediate is used to specify that the tx should be broadcast
	// immediately.
	Immediate bool
}

// MaxFeeRateAllowed returns the maximum fee rate allowed for the given
// request. It calculates the feerate using the supplied budget and the weight,
// compares it with the specified MaxFeeRate, and returns the smaller of the
// two.
func (r *BumpRequest) MaxFeeRateAllowed() (chainfee.SatPerKWeight, error) {
	// We'll want to know if we have any blobs, as we need to factor this
	// into the max fee rate for this bump request.
	hasBlobs := fn.Any(r.Inputs, func(i input.Input) bool {
		return fn.MapOptionZ(
			i.ResolutionBlob(), func(b tlv.Blob) bool {
				return len(b) > 0
			},
		)
	})

	sweepAddrs := [][]byte{
		r.DeliveryAddress.DeliveryAddress,
	}

	// If we have blobs, then we'll add an extra sweep addr for the size
	// estimate below. We know that these blobs will also always be based on
	// p2tr addrs.
	if hasBlobs {
		// We need to pass in a real address, so we'll use a dummy
		// tapscript change script that's used elsewhere for tests.
		sweepAddrs = append(sweepAddrs, dummyChangePkScript)
	}

	// Get the size of the sweep tx, which will be used to calculate the
	// budget fee rate.
	size, err := calcSweepTxWeight(
		r.Inputs, sweepAddrs,
	)
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
	outputPkScript [][]byte) (lntypes.WeightUnit, error) {

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

	// SpentInputs are the inputs spent by another tx which caused the
	// current tx to be failed.
	SpentInputs map[wire.OutPoint]*wire.MsgTx

	// requestID is the ID of the request that created this record.
	requestID uint64
}

// String returns a human-readable string for the result.
func (b *BumpResult) String() string {
	desc := fmt.Sprintf("Event=%v", b.Event)
	if b.Tx != nil {
		desc += fmt.Sprintf(", Tx=%v", b.Tx.TxHash())
	}

	return fmt.Sprintf("[%s]", desc)
}

// Validate validates the BumpResult so it's safe to use.
func (b *BumpResult) Validate() error {
	isFailureEvent := b.Event == TxFailed || b.Event == TxFatal ||
		b.Event == TxUnknownSpend

	// Every result must have a tx except the fatal or failed case.
	if b.Tx == nil && !isFailureEvent {
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

	// If it's a failed or fatal event, it must have an error.
	if isFailureEvent && b.Err == nil {
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

	// AuxSweeper is an optional interface that can be used to modify the
	// way sweep transaction are generated.
	AuxSweeper fn.Option[AuxSweeper]
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

	// Embed the blockbeat consumer struct to get access to the method
	// `NotifyBlockProcessed` and the `BlockbeatChan`.
	chainio.BeatConsumer

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

// Compile-time check for the chainio.Consumer interface.
var _ chainio.Consumer = (*TxPublisher)(nil)

// NewTxPublisher creates a new TxPublisher.
func NewTxPublisher(cfg TxPublisherConfig) *TxPublisher {
	tp := &TxPublisher{
		cfg:             &cfg,
		records:         lnutils.SyncMap[uint64, *monitorRecord]{},
		subscriberChans: lnutils.SyncMap[uint64, chan *BumpResult]{},
		quit:            make(chan struct{}),
	}

	// Mount the block consumer.
	tp.BeatConsumer = chainio.NewBeatConsumer(tp.quit, tp.Name())

	return tp
}

// isNeutrinoBackend checks if the wallet backend is neutrino.
func (t *TxPublisher) isNeutrinoBackend() bool {
	return t.cfg.Wallet.BackEnd() == "neutrino"
}

// Broadcast is used to publish the tx created from the given inputs. It will
// register the broadcast request and return a chan to the caller to subscribe
// the broadcast result. The initial broadcast is guaranteed to be
// RBF-compliant unless the budget specified cannot cover the fee.
//
// NOTE: part of the Bumper interface.
func (t *TxPublisher) Broadcast(req *BumpRequest) <-chan *BumpResult {
	log.Tracef("Received broadcast request: %s",
		lnutils.SpewLogClosure(req))

	// Store the request.
	record := t.storeInitialRecord(req)

	// Create a chan to send the result to the caller.
	subscriber := make(chan *BumpResult, 1)
	t.subscriberChans.Store(record.requestID, subscriber)

	// Publish the tx immediately if specified.
	if req.Immediate {
		t.handleInitialBroadcast(record)
	}

	return subscriber
}

// storeInitialRecord initializes a monitor record and saves it in the map.
func (t *TxPublisher) storeInitialRecord(req *BumpRequest) *monitorRecord {
	// Increase the request counter.
	//
	// NOTE: this is the only place where we increase the counter.
	requestID := t.requestCounter.Add(1)

	// Register the record.
	record := &monitorRecord{
		requestID: requestID,
		req:       req,
	}
	t.records.Store(requestID, record)

	return record
}

// updateRecord updates the given record's tx and fee, and saves it in the
// records map.
func (t *TxPublisher) updateRecord(r *monitorRecord,
	sweepCtx *sweepTxCtx) *monitorRecord {

	r.tx = sweepCtx.tx
	r.fee = sweepCtx.fee
	r.outpointToTxIndex = sweepCtx.outpointToTxIndex

	// Register the record.
	t.records.Store(r.requestID, r)

	return r
}

// NOTE: part of the `chainio.Consumer` interface.
func (t *TxPublisher) Name() string {
	return "TxPublisher"
}

// initializeTx initializes a fee function and creates an RBF-compliant tx. If
// succeeded, the initial tx is stored in the records map.
func (t *TxPublisher) initializeTx(r *monitorRecord) (*monitorRecord, error) {
	// Create a fee bumping algorithm to be used for future RBF.
	feeAlgo, err := t.initializeFeeFunction(r.req)
	if err != nil {
		return nil, fmt.Errorf("init fee function: %w", err)
	}

	// Attach the newly created fee function.
	//
	// TODO(yy): current we'd initialize a monitorRecord before creating the
	// fee function, while we could instead create the fee function first
	// then save it to the record. To make this happen we need to change the
	// conf target calculation below since we would be initializing the fee
	// function one block before.
	r.feeFunction = feeAlgo

	// Create the initial tx to be broadcasted. This tx is guaranteed to
	// comply with the RBF restrictions.
	record, err := t.createRBFCompliantTx(r)
	if err != nil {
		return nil, fmt.Errorf("create RBF-compliant tx: %w", err)
	}

	return record, nil
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
func (t *TxPublisher) createRBFCompliantTx(
	r *monitorRecord) (*monitorRecord, error) {

	f := r.feeFunction

	for {
		// Create a new tx with the given fee rate and check its
		// mempool acceptance.
		sweepCtx, err := t.createAndCheckTx(r)

		switch {
		case err == nil:
			// The tx is valid, store it.
			record := t.updateRecord(r, sweepCtx)

			log.Infof("Created initial sweep tx=%v for %v inputs: "+
				"feerate=%v, fee=%v, inputs:\n%v",
				sweepCtx.tx.TxHash(), len(r.req.Inputs),
				f.FeeRate(), sweepCtx.fee,
				inputTypeSummary(r.req.Inputs))

			return record, nil

		// If the error indicates the fees paid is not enough, we will
		// ask the fee function to increase the fee rate and retry.
		case errors.Is(err, lnwallet.ErrMempoolFee),
			errors.Is(err, chain.ErrMinRelayFeeNotMet),
			errors.Is(err, chain.ErrMempoolMinFeeNotMet):

			// We should at least start with a feerate above the
			// mempool min feerate, so if we get this error, it
			// means something is wrong earlier in the pipeline.
			log.Errorf("Current fee=%v, feerate=%v, %v",
				sweepCtx.fee, f.FeeRate(), err)

			fallthrough

		// We are not paying enough fees to RBF a previous tx, so we
		// increase it.
		case errors.Is(err, chain.ErrInsufficientFee):
			increased := false

			// Keep calling the fee function until the fee rate is
			// increased or maxed out.
			for !increased {
				log.Debugf("Increasing fee for next round, "+
					"current fee=%v, feerate=%v",
					sweepCtx.fee, f.FeeRate())

				// If the fee function tells us that we have
				// used up the budget, we will return an error
				// indicating this tx cannot be made. The
				// sweeper should handle this error and try to
				// cluster these inputs differently.
				increased, err = f.Increment()
				if err != nil {
					return nil, err
				}
			}

		// TODO(yy): suppose there's only one bad input, we can do a
		// binary search to find out which input is causing this error
		// by recreating a tx using half of the inputs and check its
		// mempool acceptance.
		default:
			log.Debugf("Failed to create RBF-compliant tx: %v", err)
			return nil, err
		}
	}
}

// createAndCheckTx creates a tx based on the given inputs, change output
// script, and the fee rate. In addition, it validates the tx's mempool
// acceptance before returning a tx that can be published directly, along with
// its fee.
func (t *TxPublisher) createAndCheckTx(r *monitorRecord) (*sweepTxCtx, error) {
	req := r.req
	f := r.feeFunction

	// Create the sweep tx with max fee rate of 0 as the fee function
	// guarantees the fee rate used here won't exceed the max fee rate.
	sweepCtx, err := t.createSweepTx(
		req.Inputs, req.DeliveryAddress, f.FeeRate(),
	)
	if err != nil {
		return sweepCtx, fmt.Errorf("create sweep tx: %w", err)
	}

	// Sanity check the budget still covers the fee.
	if sweepCtx.fee > req.Budget {
		return sweepCtx, fmt.Errorf("%w: budget=%v, fee=%v",
			ErrNotEnoughBudget, req.Budget, sweepCtx.fee)
	}

	// If we had an extra txOut, then we'll update the result to include
	// it.
	req.ExtraTxOut = sweepCtx.extraTxOut

	// Validate the tx's mempool acceptance.
	err = t.cfg.Wallet.CheckMempoolAcceptance(sweepCtx.tx)

	// Exit early if the tx is valid.
	if err == nil {
		return sweepCtx, nil
	}

	// Print an error log if the chain backend doesn't support the mempool
	// acceptance test RPC.
	if errors.Is(err, rpcclient.ErrBackendVersion) {
		log.Errorf("TestMempoolAccept not supported by backend, " +
			"consider upgrading it to a newer version")
		return sweepCtx, nil
	}

	// We are running on a backend that doesn't implement the RPC
	// testmempoolaccept, eg, neutrino, so we'll skip the check.
	if errors.Is(err, chain.ErrUnimplemented) {
		log.Debug("Skipped testmempoolaccept due to not implemented")
		return sweepCtx, nil
	}

	// If the inputs are spent by another tx, we will exit with the latest
	// sweepCtx and an error.
	if errors.Is(err, chain.ErrMissingInputs) {
		log.Debugf("Tx %v missing inputs, it's likely the input has "+
			"been spent by others", sweepCtx.tx.TxHash())

		// Make sure to update the record with the latest attempt.
		t.updateRecord(r, sweepCtx)

		return sweepCtx, ErrInputMissing
	}

	return sweepCtx, fmt.Errorf("tx=%v failed mempool check: %w",
		sweepCtx.tx.TxHash(), err)
}

// handleMissingInputs handles the case when the chain backend reports back a
// missing inputs error, which could happen when one of the input has been spent
// in another tx, or the input is referencing an orphan. When the input is
// spent, it will be handled via the TxUnknownSpend flow by creating a
// TxUnknownSpend bump result, otherwise, a TxFatal bump result is returned.
func (t *TxPublisher) handleMissingInputs(r *monitorRecord) *BumpResult {
	// Get the spending txns.
	spends := t.getSpentInputs(r)

	// Attach the spending txns.
	r.spentInputs = spends

	// If there are no spending txns found and the input is missing, the
	// input is referencing an orphan tx that's no longer valid, e.g., the
	// spending the anchor output from the remote commitment after the local
	// commitment has confirmed. In this case we will mark it as fatal and
	// exit.
	if len(spends) == 0 {
		log.Warnf("Failing record=%v: found orphan inputs: %v\n",
			r.requestID, inputTypeSummary(r.req.Inputs))

		// Create a result that will be sent to the resultChan which is
		// listened by the caller.
		result := &BumpResult{
			Event:     TxFatal,
			Tx:        r.tx,
			requestID: r.requestID,
			Err:       ErrInputMissing,
		}

		return result
	}

	// Check that the spending tx matches the sweeping tx - given that the
	// current sweeping tx has been failed due to missing inputs, the
	// spending tx must be a different tx, thus it should NOT be matched. We
	// perform a sanity check here to catch the unexpected state.
	if !t.isUnknownSpent(r, spends) {
		log.Errorf("Sweeping tx %v has missing inputs, yet the "+
			"spending tx is the sweeping tx itself: %v",
			r.tx.TxHash(), r.spentInputs)
	}

	return t.createUnknownSpentBumpResult(r)
}

// broadcast takes a monitored tx and publishes it to the network. Prior to the
// broadcast, it will subscribe the tx's confirmation notification and attach
// the event channel to the record. Any broadcast-related errors will not be
// returned here, instead, they will be put inside the `BumpResult` and
// returned to the caller.
func (t *TxPublisher) broadcast(record *monitorRecord) (*BumpResult, error) {
	txid := record.tx.TxHash()

	tx := record.tx
	log.Debugf("Publishing sweep tx %v, num_inputs=%v, height=%v",
		txid, len(tx.TxIn), t.currentHeight.Load())

	// Before we go to broadcast, we'll notify the aux sweeper, if it's
	// present of this new broadcast attempt.
	err := fn.MapOptionZ(t.cfg.AuxSweeper, func(aux AuxSweeper) error {
		return aux.NotifyBroadcast(
			record.req, tx, record.fee, record.outpointToTxIndex,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("unable to notify aux sweeper: %w", err)
	}

	// Set the event, and change it to TxFailed if the wallet fails to
	// publish it.
	event := TxPublished

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
		requestID: record.requestID,
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

	log.Debugf("Sending result %v for requestID=%v", result, id)

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

	var txid chainhash.Hash
	if result.Tx != nil {
		txid = result.Tx.TxHash()
	}

	// Remove the record from the maps if there's an error or the tx is
	// confirmed. When there's an error, it means this tx has failed its
	// broadcast and cannot be retried. There are two cases it may fail,
	// - when the budget cannot cover the increased fee calculated by the
	//   fee function, hence the budget is used up.
	// - when a non-fee related error returned from PublishTransaction.
	switch result.Event {
	case TxFailed:
		log.Errorf("Removing monitor record=%v, tx=%v, due to err: %v",
			id, txid, result.Err)

	case TxConfirmed:
		// Remove the record if the tx is confirmed.
		log.Debugf("Removing confirmed monitor record=%v, tx=%v", id,
			txid)

	case TxFatal:
		// Remove the record if there's an error.
		log.Debugf("Removing monitor record=%v due to fatal err: %v",
			id, result.Err)

	case TxUnknownSpend:
		// Remove the record if there's an unknown spend.
		log.Debugf("Removing monitor record=%v due unknown spent: "+
			"%v", id, result.Err)

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
	// requestID is the ID of the request that created this record.
	requestID uint64

	// tx is the tx being monitored.
	tx *wire.MsgTx

	// req is the original request.
	req *BumpRequest

	// feeFunction is the fee bumping algorithm used by the publisher.
	feeFunction FeeFunction

	// fee is the fee paid by the tx.
	fee btcutil.Amount

	// outpointToTxIndex is a map of outpoint to tx index.
	outpointToTxIndex map[wire.OutPoint]int

	// spentInputs are the inputs spent by another tx which caused the
	// current tx failed.
	spentInputs map[wire.OutPoint]*wire.MsgTx
}

// Start starts the publisher by subscribing to block epoch updates and kicking
// off the monitor loop.
func (t *TxPublisher) Start(beat chainio.Blockbeat) error {
	log.Info("TxPublisher starting...")

	if t.started.Swap(true) {
		return fmt.Errorf("TxPublisher started more than once")
	}

	// Set the current height.
	t.currentHeight.Store(beat.Height())

	t.wg.Add(1)
	go t.monitor()

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
func (t *TxPublisher) monitor() {
	defer t.wg.Done()

	for {
		select {
		case beat := <-t.BlockbeatChan:
			height := beat.Height()
			log.Debugf("TxPublisher received new block: %v", height)

			// Update the best known height for the publisher.
			t.currentHeight.Store(height)

			// Check all monitored txns to see if any of them needs
			// to be bumped.
			t.processRecords()

			// Notify we've processed the block.
			t.NotifyBlockProcessed(beat, nil)

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

	// feeBumpRecords stores a map of records which need to be bumped.
	feeBumpRecords := make(map[uint64]*monitorRecord)

	// failedRecords stores a map of records which has inputs being spent
	// by a third party.
	failedRecords := make(map[uint64]*monitorRecord)

	// initialRecords stores a map of records which are being created and
	// published for the first time.
	initialRecords := make(map[uint64]*monitorRecord)

	// visitor is a helper closure that visits each record and divides them
	// into two groups.
	visitor := func(requestID uint64, r *monitorRecord) error {
		log.Tracef("Checking monitor recordID=%v", requestID)

		// Check whether the inputs have already been spent.
		spends := t.getSpentInputs(r)

		// If the any of the inputs has been spent, the record will be
		// marked as failed or confirmed.
		if len(spends) != 0 {
			// Attach the spending txns.
			r.spentInputs = spends

			// When tx is nil, it means we haven't tried the initial
			// broadcast yet the input is already spent. This could
			// happen when the node shuts down, a previous sweeping
			// tx confirmed, then the node comes back online and
			// reoffers the inputs. Another case is the remote node
			// spends the input quickly before we even attempt the
			// sweep. In either case we will fail the record and let
			// the sweeper handles it.
			if r.tx == nil {
				failedRecords[requestID] = r
				return nil
			}

			// Check whether the inputs has been spent by a unknown
			// tx.
			if t.isUnknownSpent(r, spends) {
				failedRecords[requestID] = r

				// Move to the next record.
				return nil
			}

			// The tx is ours, we can move it to the confirmed queue
			// and stop monitoring it.
			confirmedRecords[requestID] = r

			// Move to the next record.
			return nil
		}

		// This is the first time we see this record, so we put it in
		// the initial queue.
		if r.tx == nil {
			initialRecords[requestID] = r

			return nil
		}

		// We can only get here when the inputs are not spent and a
		// previous sweeping tx has been attempted. In this case we will
		// perform an RBF on it in the current block.
		feeBumpRecords[requestID] = r

		// Return nil to move to the next record.
		return nil
	}

	// Iterate through all the records and divide them into four groups.
	t.records.ForEach(visitor)

	// Handle the initial broadcast.
	for _, r := range initialRecords {
		t.handleInitialBroadcast(r)
	}

	// For records that are confirmed, we'll notify the caller about this
	// result.
	for _, r := range confirmedRecords {
		t.wg.Add(1)
		go t.handleTxConfirmed(r)
	}

	// Get the current height to be used in the following goroutines.
	currentHeight := t.currentHeight.Load()

	// For records that are not confirmed, we perform a fee bump if needed.
	for _, r := range feeBumpRecords {
		t.wg.Add(1)
		go t.handleFeeBumpTx(r, currentHeight)
	}

	// For records that are failed, we'll notify the caller about this
	// result.
	for _, r := range failedRecords {
		t.wg.Add(1)
		go t.handleUnknownSpent(r)
	}
}

// handleTxConfirmed is called when a monitored tx is confirmed. It will
// notify the subscriber then remove the record from the maps .
//
// NOTE: Must be run as a goroutine to avoid blocking on sending the result.
func (t *TxPublisher) handleTxConfirmed(r *monitorRecord) {
	defer t.wg.Done()

	log.Debugf("Record %v is spent in tx=%v", r.requestID, r.tx.TxHash())

	// Create a result that will be sent to the resultChan which is
	// listened by the caller.
	result := &BumpResult{
		Event:     TxConfirmed,
		Tx:        r.tx,
		requestID: r.requestID,
		Fee:       r.fee,
		FeeRate:   r.feeFunction.FeeRate(),
	}

	// Notify that this tx is confirmed and remove the record from the map.
	t.handleResult(result)
}

// handleInitialTxError takes the error from `initializeTx` and decides the
// bump event. It will construct a BumpResult and handles it.
func (t *TxPublisher) handleInitialTxError(r *monitorRecord, err error) {
	// Create a bump result to be sent to the sweeper.
	result := &BumpResult{
		Err:       err,
		requestID: r.requestID,
	}

	// We now decide what type of event to send.
	switch {
	// When the error is due to a dust output, we'll send a TxFailed so
	// these inputs can be retried with a different group in the next
	// block.
	case errors.Is(err, ErrTxNoOutput):
		result.Event = TxFailed

	// When the error is due to zero fee rate delta, we'll send a TxFailed
	// so these inputs can be retried in the next block.
	case errors.Is(err, ErrZeroFeeRateDelta):
		result.Event = TxFailed

	// When the error is due to budget being used up, we'll send a TxFailed
	// so these inputs can be retried with a different group in the next
	// block.
	case errors.Is(err, ErrMaxPosition):
		fallthrough

	// If the tx doesn't not have enough budget, or if the inputs amounts
	// are not sufficient to cover the budget, we will return a TxFailed
	// event so the sweeper can handle it by re-clustering the utxos.
	case errors.Is(err, ErrNotEnoughInputs),
		errors.Is(err, ErrNotEnoughBudget):

		result.Event = TxFailed

		// Calculate the starting fee rate to be used when retry
		// sweeping these inputs.
		feeRate, err := t.calculateRetryFeeRate(r)
		if err != nil {
			result.Event = TxFatal
			result.Err = err
		}

		// Attach the new fee rate.
		result.FeeRate = feeRate

	// When there are missing inputs, we'll create a TxUnknownSpend bump
	// result here so the rest of the inputs can be retried.
	case errors.Is(err, ErrInputMissing):
		result = t.handleMissingInputs(r)

	// Otherwise this is not a fee-related error and the tx cannot be
	// retried. In that case we will fail ALL the inputs in this tx, which
	// means they will be removed from the sweeper and never be tried
	// again.
	//
	// TODO(yy): Find out which input is causing the failure and fail that
	// one only.
	default:
		result.Event = TxFatal
	}

	t.handleResult(result)
}

// handleInitialBroadcast is called when a new request is received. It will
// handle the initial tx creation and broadcast. In details,
// 1. init a fee function based on the given strategy.
// 2. create an RBF-compliant tx and monitor it for confirmation.
// 3. notify the initial broadcast result back to the caller.
func (t *TxPublisher) handleInitialBroadcast(r *monitorRecord) {
	log.Debugf("Initial broadcast for requestID=%v", r.requestID)

	var (
		result *BumpResult
		err    error
	)

	// Attempt an initial broadcast which is guaranteed to comply with the
	// RBF rules.
	//
	// Create the initial tx to be broadcasted.
	record, err := t.initializeTx(r)
	if err != nil {
		log.Errorf("Initial broadcast failed: %v", err)

		// We now handle the initialization error and exit.
		t.handleInitialTxError(r, err)

		return
	}

	// Successfully created the first tx, now broadcast it.
	result, err = t.broadcast(record)
	if err != nil {
		// The broadcast failed, which can only happen if the tx record
		// cannot be found or the aux sweeper returns an error. In
		// either case, we will send back a TxFail event so these
		// inputs can be retried.
		result = &BumpResult{
			Event:     TxFailed,
			Err:       err,
			requestID: r.requestID,
		}
	}

	t.handleResult(result)
}

// handleFeeBumpTx checks if the tx needs to be bumped, and if so, it will
// attempt to bump the fee of the tx.
//
// NOTE: Must be run as a goroutine to avoid blocking on sending the result.
func (t *TxPublisher) handleFeeBumpTx(r *monitorRecord, currentHeight int32) {
	defer t.wg.Done()

	log.Debugf("Attempting to fee bump tx=%v in record %v", r.tx.TxHash(),
		r.requestID)

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
	resultOpt := t.createAndPublishTx(r)

	// If there's a result, we will notify the caller about the result.
	resultOpt.WhenSome(func(result BumpResult) {
		// Notify the new result.
		t.handleResult(&result)
	})
}

// handleUnknownSpent is called when the inputs are spent by a unknown tx. It
// will notify the subscriber then remove the record from the maps and send a
// TxUnknownSpend event to the subscriber.
//
// NOTE: Must be run as a goroutine to avoid blocking on sending the result.
func (t *TxPublisher) handleUnknownSpent(r *monitorRecord) {
	defer t.wg.Done()

	log.Debugf("Record %v has inputs spent by a tx unknown to the fee "+
		"bumper, failing it now:\n%v", r.requestID,
		inputTypeSummary(r.req.Inputs))

	// Create a result that will be sent to the resultChan which is listened
	// by the caller.
	result := t.createUnknownSpentBumpResult(r)

	// Notify the sweeper about this result in the end.
	t.handleResult(result)
}

// createUnknownSpentBumpResult creates and returns a BumpResult given the
// monitored record has unknown spends.
func (t *TxPublisher) createUnknownSpentBumpResult(
	r *monitorRecord) *BumpResult {

	// Create a result that will be sent to the resultChan which is listened
	// by the caller.
	result := &BumpResult{
		Event:       TxUnknownSpend,
		Tx:          r.tx,
		requestID:   r.requestID,
		Err:         ErrUnknownSpent,
		SpentInputs: r.spentInputs,
	}

	// Calculate the next fee rate for the retry.
	feeRate, err := t.calculateRetryFeeRate(r)
	if err != nil {
		// Overwrite the event and error so the sweeper will
		// remove this input.
		result.Event = TxFatal
		result.Err = err
	}

	// Attach the new fee rate to be used for the next sweeping attempt.
	result.FeeRate = feeRate

	return result
}

// createAndPublishTx creates a new tx with a higher fee rate and publishes it
// to the network. It will update the record with the new tx and fee rate if
// successfully created, and return the result when published successfully.
func (t *TxPublisher) createAndPublishTx(
	r *monitorRecord) fn.Option[BumpResult] {

	// Fetch the old tx.
	oldTx := r.tx

	// Create a new tx with the new fee rate.
	//
	// NOTE: The fee function is expected to have increased its returned
	// fee rate after calling the SkipFeeBump method. So we can use it
	// directly here.
	sweepCtx, err := t.createAndCheckTx(r)

	// If there's an error creating the replacement tx, we need to abort the
	// flow and handle it.
	if err != nil {
		return t.handleReplacementTxError(r, oldTx, err)
	}

	// The tx has been created without any errors, we now register a new
	// record by overwriting the same requestID.
	record := t.updateRecord(r, sweepCtx)

	// Attempt to broadcast this new tx.
	result, err := t.broadcast(record)
	if err != nil {
		log.Infof("Failed to broadcast replacement tx %v: %v",
			sweepCtx.tx.TxHash(), err)

		return fn.None[BumpResult]()
	}

	// If the result error is fee related, we will return no error and let
	// the fee bumper retry it at next block.
	//
	// NOTE: we may get this error if we've bypassed the mempool check,
	// which means we are using neutrino backend.
	if errors.Is(result.Err, chain.ErrInsufficientFee) ||
		errors.Is(result.Err, lnwallet.ErrMempoolFee) {

		log.Debugf("Failed to bump tx %v: %v", oldTx.TxHash(),
			result.Err)

		return fn.None[BumpResult]()
	}

	// A successful replacement tx is created, attach the old tx.
	result.ReplacedTx = oldTx

	// If the new tx failed to be published, we will return the result so
	// the caller can handle it.
	if result.Event == TxFailed {
		return fn.Some(*result)
	}

	log.Debugf("Replaced tx=%v with new tx=%v", oldTx.TxHash(),
		sweepCtx.tx.TxHash())

	// Otherwise, it's a successful RBF, set the event and return.
	result.Event = TxReplaced

	return fn.Some(*result)
}

// isUnknownSpent checks whether the inputs of the tx has already been spent by
// a tx not known to us. When a tx is not confirmed, yet its inputs has been
// spent, then it must be spent by a different tx other than the sweeping tx
// here.
func (t *TxPublisher) isUnknownSpent(r *monitorRecord,
	spends map[wire.OutPoint]*wire.MsgTx) bool {

	txid := r.tx.TxHash()

	// Iterate all the spending txns and check if they match the sweeping
	// tx.
	for op, spendingTx := range spends {
		spendingTxID := spendingTx.TxHash()

		// If the spending tx is the same as the sweeping tx then we are
		// good.
		if spendingTxID == txid {
			continue
		}

		log.Warnf("Detected unknown spend of input=%v in tx=%v", op,
			spendingTx.TxHash())

		return true
	}

	return false
}

// getSpentInputs performs a non-blocking read on the spending subscriptions to
// see whether any of the monitored inputs has been spent. A map of inputs with
// their spending txns are returned if found.
func (t *TxPublisher) getSpentInputs(
	r *monitorRecord) map[wire.OutPoint]*wire.MsgTx {

	// Create a slice to record the inputs spent.
	spentInputs := make(map[wire.OutPoint]*wire.MsgTx, len(r.req.Inputs))

	// Iterate all the inputs and check if they have been spent already.
	for _, inp := range r.req.Inputs {
		op := inp.OutPoint()

		// For wallet utxos, the height hint is not set - we don't need
		// to monitor them for third party spend.
		//
		// TODO(yy): We need to properly lock wallet utxos before
		// skipping this check as the same wallet utxo can be used by
		// different sweeping txns.
		heightHint := inp.HeightHint()
		if heightHint == 0 {
			heightHint = uint32(t.currentHeight.Load())
			log.Debugf("Checking wallet input %v using heightHint "+
				"%v", op, heightHint)
		}

		// If the input has already been spent after the height hint, a
		// spend event is sent back immediately.
		spendEvent, err := t.cfg.Notifier.RegisterSpendNtfn(
			&op, inp.SignDesc().Output.PkScript, heightHint,
		)
		if err != nil {
			log.Criticalf("Failed to register spend ntfn for "+
				"input=%v: %v", op, err)

			return nil
		}

		// Remove the subscription when exit.
		defer spendEvent.Cancel()

		// Do a non-blocking read to see if the output has been spent.
		select {
		case spend, ok := <-spendEvent.Spend:
			if !ok {
				log.Debugf("Spend ntfn for %v canceled", op)

				continue
			}

			spendingTx := spend.SpendingTx

			log.Debugf("Detected spent of input=%v in tx=%v", op,
				spendingTx.TxHash())

			spentInputs[op] = spendingTx

		// Move to the next input.
		default:
			log.Tracef("Input %v not spent yet", op)
		}
	}

	return spentInputs
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

// sweepTxCtx houses a sweep transaction with additional context.
type sweepTxCtx struct {
	tx *wire.MsgTx

	fee btcutil.Amount

	extraTxOut fn.Option[SweepOutput]

	// outpointToTxIndex maps the outpoint of the inputs to their index in
	// the sweep transaction.
	outpointToTxIndex map[wire.OutPoint]int
}

// createSweepTx creates a sweeping tx based on the given inputs, change
// address and fee rate.
func (t *TxPublisher) createSweepTx(inputs []input.Input,
	changePkScript lnwallet.AddrWithKey,
	feeRate chainfee.SatPerKWeight) (*sweepTxCtx, error) {

	// Validate and calculate the fee and change amount.
	txFee, changeOutputsOpt, locktimeOpt, err := prepareSweepTx(
		inputs, changePkScript, feeRate, t.currentHeight.Load(),
		t.cfg.AuxSweeper,
	)
	if err != nil {
		return nil, err
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
	outpointToTxIndex := make(map[wire.OutPoint]int)
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

		outpointToTxIndex[o.OutPoint()] = len(sweepTx.TxOut) - 1
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

	// If we have change outputs to add, then add it the sweep transaction
	// here.
	changeOutputsOpt.WhenSome(func(changeOuts []SweepOutput) {
		for i := range changeOuts {
			sweepTx.AddTxOut(&changeOuts[i].TxOut)
		}
	})

	// We'll default to using the current block height as locktime, if none
	// of the inputs commits to a different locktime.
	sweepTx.LockTime = uint32(locktimeOpt.UnwrapOr(t.currentHeight.Load()))

	prevInputFetcher, err := input.MultiPrevOutFetcher(inputs)
	if err != nil {
		return nil, fmt.Errorf("error creating prev input fetcher "+
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
			return nil, err
		}
	}

	log.Debugf("Created sweep tx %v for inputs:\n%v", sweepTx.TxHash(),
		inputTypeSummary(inputs))

	// Try to locate the extra change output, though there might be None.
	extraTxOut := fn.MapOption(
		func(sweepOuts []SweepOutput) fn.Option[SweepOutput] {
			for _, sweepOut := range sweepOuts {
				if !sweepOut.IsExtra {
					continue
				}

				// If we sweep outputs of a custom channel, the
				// custom leaves in those outputs will be merged
				// into a single output, even if we sweep
				// multiple outputs (e.g. to_remote and breached
				// to_local of a breached channel) at the same
				// time. So there will only ever be one extra
				// output.
				log.Debugf("Sweep produced extra_sweep_out=%v",
					lnutils.SpewLogClosure(sweepOut))

				return fn.Some(sweepOut)
			}

			return fn.None[SweepOutput]()
		},
	)(changeOutputsOpt)

	return &sweepTxCtx{
		tx:                sweepTx,
		fee:               txFee,
		extraTxOut:        fn.FlattenOption(extraTxOut),
		outpointToTxIndex: outpointToTxIndex,
	}, nil
}

// prepareSweepTx returns the tx fee, a set of optional change outputs and an
// optional locktime after a series of validations:
// 1. check the locktime has been reached.
// 2. check the locktimes are the same.
// 3. check the inputs cover the outputs.
//
// NOTE: if the change amount is below dust, it will be added to the tx fee.
func prepareSweepTx(inputs []input.Input, changePkScript lnwallet.AddrWithKey,
	feeRate chainfee.SatPerKWeight, currentHeight int32,
	auxSweeper fn.Option[AuxSweeper]) (
	btcutil.Amount, fn.Option[[]SweepOutput], fn.Option[int32], error) {

	noChange := fn.None[[]SweepOutput]()
	noLocktime := fn.None[int32]()

	// Given the set of inputs we have, if we have an aux sweeper, then
	// we'll attempt to see if we have any other change outputs we'll need
	// to add to the sweep transaction.
	changePkScripts := [][]byte{changePkScript.DeliveryAddress}

	var extraChangeOut fn.Option[SweepOutput]
	err := fn.MapOptionZ(
		auxSweeper, func(aux AuxSweeper) error {
			extraOut := aux.DeriveSweepAddr(inputs, changePkScript)
			if err := extraOut.Err(); err != nil {
				return err
			}

			extraChangeOut = extraOut.LeftToSome()

			return nil
		},
	)
	if err != nil {
		return 0, noChange, noLocktime, err
	}

	// We also add the extra change output to the change pk scripts.
	//
	// NOTE: The weight estimation will not be quite accurate because the
	// witness data is greater when overlay channels are used. But that
	// shouldn't be a problem since we will increase the fee rate
	// incrementally via the fee function.
	extraChangeOut.WhenSome(func(o SweepOutput) {
		changePkScripts = append(changePkScripts, o.TxOut.PkScript)
	})

	// Creating a weight estimator with nil outputs and zero max fee rate.
	// We don't allow adding customized outputs in the sweeping tx, and the
	// fee rate is already being managed before we get here.
	inputs, estimator, err := getWeightEstimate(
		inputs, nil, feeRate, 0, changePkScripts,
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

	// If we have an extra change output, then we'll add it as a required
	// output amt.
	extraChangeOut.WhenSome(func(o SweepOutput) {
		requiredOutput += btcutil.Amount(o.Value)
	})

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
			return 0, noChange, noLocktime,
				fmt.Errorf("%w: current height is %v, "+
					"locktime is %v", ErrLocktimeImmature,
					currentHeight, lt)
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
		log.Errorf("Insufficient input to create sweep tx: "+
			"input_sum=%v, output_sum=%v", totalInput,
			requiredOutput+txFee)

		return 0, noChange, noLocktime, ErrNotEnoughInputs
	}

	// The value remaining after the required output and fees is the
	// change output.
	changeAmt := totalInput - requiredOutput - txFee
	changeOuts := make([]SweepOutput, 0, 2)

	extraChangeOut.WhenSome(func(o SweepOutput) {
		changeOuts = append(changeOuts, o)
	})

	// We'll calculate the dust limit for the given changePkScript since it
	// is variable.
	changeFloor := lnwallet.DustLimitForSize(
		len(changePkScript.DeliveryAddress),
	)

	switch {
	// If the change amount is dust, we'll move it into the fees, and
	// ignore it.
	case changeAmt < changeFloor:
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

	// Otherwise, we'll actually recognize it as a change output.
	default:
		changeOuts = append(changeOuts, SweepOutput{
			TxOut: wire.TxOut{
				Value:    int64(changeAmt),
				PkScript: changePkScript.DeliveryAddress,
			},
			IsExtra:     false,
			InternalKey: changePkScript.InternalKey,
		})
	}

	// Optionally set the locktime.
	locktimeOpt := fn.Some(locktime)
	if locktime == -1 {
		locktimeOpt = noLocktime
	}

	var changeOutsOpt fn.Option[[]SweepOutput]
	if len(changeOuts) > 0 {
		changeOutsOpt = fn.Some(changeOuts)
	}

	log.Debugf("Creating sweep tx for %v inputs (%s) using %v, "+
		"tx_weight=%v, tx_fee=%v, locktime=%v, parents_count=%v, "+
		"parents_fee=%v, parents_weight=%v, current_height=%v",
		len(inputs), inputTypeSummary(inputs), feeRate,
		estimator.weight(), txFee, locktimeOpt, len(estimator.parents),
		estimator.parentsFee, estimator.parentsWeight, currentHeight)

	return txFee, changeOutsOpt, locktimeOpt, nil
}

// handleReplacementTxError handles the error returned from creating the
// replacement tx. It returns a BumpResult that should be notified to the
// sweeper.
func (t *TxPublisher) handleReplacementTxError(r *monitorRecord,
	oldTx *wire.MsgTx, err error) fn.Option[BumpResult] {

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

	// At least one of the inputs is missing, which means it has already
	// been spent by another tx and confirmed. In this case we will handle
	// it by returning a TxUnknownSpend bump result.
	if errors.Is(err, ErrInputMissing) {
		log.Warnf("Fail to fee bump tx %v: %v", oldTx.TxHash(), err)
		bumpResult := t.handleMissingInputs(r)

		return fn.Some(*bumpResult)
	}

	// Return a failed event to retry the sweep.
	event := TxFailed

	// Calculate the next fee rate for the retry.
	feeRate, ferr := t.calculateRetryFeeRate(r)
	if ferr != nil {
		// If there's an error with the fee calculation, we need to
		// abort the sweep.
		event = TxFatal
	}

	// If the error is not fee related, we will return a `TxFailed` event so
	// this input can be retried.
	result := fn.Some(BumpResult{
		Event:     event,
		Tx:        oldTx,
		Err:       err,
		requestID: r.requestID,
		FeeRate:   feeRate,
	})

	// If the tx doesn't not have enough budget, or if the inputs amounts
	// are not sufficient to cover the budget, we will return a result so
	// the sweeper can handle it by re-clustering the utxos.
	if errors.Is(err, ErrNotEnoughBudget) ||
		errors.Is(err, ErrNotEnoughInputs) {

		log.Warnf("Fail to fee bump tx %v: %v", oldTx.TxHash(), err)
		return result
	}

	// Otherwise, an unexpected error occurred, we will log an error and let
	// the sweeper retry the whole process.
	log.Errorf("Failed to bump tx %v: %v", oldTx.TxHash(), err)

	return result
}

// calculateRetryFeeRate calculates a new fee rate to be used as the starting
// fee rate for the next sweep attempt if the inputs are to be retried. When the
// fee function is nil it will be created here, and an error is returned if the
// fee func cannot be initialized.
func (t *TxPublisher) calculateRetryFeeRate(
	r *monitorRecord) (chainfee.SatPerKWeight, error) {

	// Get the fee function, which will be used to decided the next fee rate
	// to use if the sweeper decides to retry sweeping this input.
	feeFunc := r.feeFunction

	// When the record is failed before the initial broadcast is attempted,
	// it will have a nil fee func. In this case, we'll create the fee func
	// here.
	//
	// NOTE: Since the current record is failed and will be deleted, we
	// don't need to update the record on this fee function. We only need
	// the fee rate data so the sweeper can pick up where we left off.
	if feeFunc == nil {
		f, err := t.initializeFeeFunction(r.req)

		// TODO(yy): The only error we would receive here is when the
		// pkScript is not recognized by the weightEstimator. What we
		// should do instead is to check the pkScript immediately after
		// receiving a sweep request so we don't need to check it again,
		// which will also save us from error checking from several
		// callsites.
		if err != nil {
			log.Errorf("Failed to create fee func for record %v: "+
				"%v", r.requestID, err)

			return 0, err
		}

		feeFunc = f
	}

	// Since we failed to sweep the inputs, either the sweeping tx has been
	// replaced by another party's tx, or the current output values cannot
	// cover the budget, we missed this block window to increase its fee
	// rate. To make sure the fee rate stays in the initial line, we now ask
	// the fee function to give us the next fee rate as if the sweeping tx
	// were RBFed. This new fee rate will be used as the starting fee rate
	// if the upper system decides to continue sweeping the rest of the
	// inputs.
	_, err := feeFunc.Increment()
	if err != nil {
		// The fee function has reached its max position - nothing we
		// can do here other than letting the user increase the budget.
		log.Errorf("Failed to calculate the next fee rate for "+
			"Record(%v): %v", r.requestID, err)
	}

	return feeFunc.FeeRate(), nil
}
