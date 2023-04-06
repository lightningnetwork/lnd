package sweep

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// NoHeightInfo is used when there's no height information available.
	NoHeightInfo = math.MaxInt32

	// defaultMaxFeeRatio is used when the max fee ratio is not set for the
	// requested input.
	defaultMaxFeeRatio = 0.1
)

var (
	// ErrConfTargetTooSmall is used when the requested conf target is less
	// than 1.
	ErrConfTargetTooSmall = errors.New("conf target is too small")

	// ErrInvalidStrategy specifies an invalid strategy.
	ErrInvalidStrategy = errors.New("invalid strategy")

	// ErrAlreadyMonitored is used when the requested tx has already been
	// monitored.
	ErrAlreadyMonitored = errors.New("record already monitored")

	// ErrConflictArgs is used when conflicting arguments are both
	// set/unset when making a request.
	ErrConflictArgs = errors.New("conflict arguments used")

	// ErrMissingParams is returned when the requested strategy needs
	// certain params but they don't exist.
	ErrMissingParams = errors.New("missing params")
)

// Bumper helps managing the fee bumping of a given outpoint.
type Bumper interface {
	// Start starts the fee bumper service.
	Start()

	// Stop stops the fee bumper service.
	Stop()

	// RequestMonitor sends a request to monitor a given outpoint for
	// potential fee bumping.
	RequestMonitor(req *MonitorRequest) error

	// NotifyConfirm specifies a previously requested outpoint has been
	// swept.
	NotifyConfirm(op *wire.OutPoint)
}

// Strategy specifies how the fee bump is performed.
type Strategy uint8

const (
	// StrategyNone defines a strategy that does not bump the fee.
	StrategyNone Strategy = iota

	// StrategyLinear defines a strategy that bumps the fee linearly
	// towards the deadline. It calcuates the fee rate using,
	// newFeeRate = startingFeeRate + feeRateIncreased, in which,
	// - feeRateIncreased = blocksPassed/blocksTotal * feeRateDelta
	// - feeRateDelta = maxFeeRate - startingFeeRate
	// - blocksTotal = deadlineHeight - monitorHeight
	// - blocksPassed = currentHeight - monitorHeight
	// This means to be able to use this strategy, the MaxFeeRate must be
	// set in the request. And either the DeadlineHeight or ConfTarget, or
	// both, must be set. If only ConfTarget is set, the DeadlineHeight
	// will be ConfTarget+MonitorHeight, which may not be as precise as
	// setting the DeadlineHeight directly but still gives us an implicit
	// deadline.
	StrategyLinear

	// strategySentinel specifies the total number of strategies and is
	// used only for validation.
	strategySentinel
)

// validateStrategy checks that a given strategy is defined.
func validateStrategy(s Strategy) error {
	if s < strategySentinel {
		return nil
	}

	return ErrInvalidStrategy
}

// String gives a friendly string out which can be used in logging.
func (s Strategy) String() string {
	switch s {
	case StrategyNone:
		return "none"

	case StrategyLinear:
		return "linear"

	default:
		return "unknown strategy"
	}
}

// MonitorRequest is used when a given outpoint needs to be monitored.
type MonitorRequest struct {
	// Input specifies the input to be monitored.
	Input input.Input

	// Height specifies the height at which the request was made. This
	// should be viewed as the input's sweeping transaction's broadcast
	// height.
	Height uint32

	// ConfTarget is the sweep transaction's confirmation target.
	ConfTarget uint32

	// FeeRate is the requested fee rate when the sweeping transaction is
	// created.
	FeeRate chainfee.SatPerKWeight

	// MaxFeeRatio specifies a portion of the input amount that can be used
	// as mining fee. If not set, defaultMaxFeeRatio will be used.
	MaxFeeRatio float64

	// Strategy specifies how the fee will be bumped.
	Strategy Strategy
}

// String gives a verbose string which can be used in logging.
func (m *MonitorRequest) String() string {
	s := fmt.Sprintf("outpoint %v: strategy=%s, fee rate=%v",
		m.Input.OutPoint(), m.Strategy, m.FeeRate)

	if m.ConfTarget != NoHeightInfo {
		s += fmt.Sprintf(", conf target=%v", m.ConfTarget)
	}

	if m.MaxFeeRatio != 0 {
		s += fmt.Sprintf(", max fee ratio=%v", m.MaxFeeRatio)
	} else {
		s += fmt.Sprintf(", max fee ratio=%v", defaultMaxFeeRatio)
	}

	return s
}

// estimateMaxFeeRate gives an estimated max fee rate based on the requested
// MaxFeeRatio. It assumes the input will be swept inside a transition with one
// input and one change output, and calcualtes the fee rate using
// (max fee) / (transaction weight). The changeOutputScript is specified when
// creating the fee bumper, so it's shared across all inputs. Since the actual
// sweeping transaction may have multiple inputs and one change output, the
// actual fee used will be smaller than the max allowed fee.
//
// TODO(yy): allow the weight or max fee rate to be specified via the request?
func (m *MonitorRequest) estimateMaxFeeRate(
	changeOutputScript []byte) (chainfee.SatPerKWeight, error) {

	// Get the max fee ratio. If not set, use the default.
	maxFeeRatio := m.MaxFeeRatio
	if maxFeeRatio == 0 {
		maxFeeRatio = defaultMaxFeeRatio
	}

	// Calculate the max fee.
	amount := btcutil.Amount(m.Input.SignDesc().Output.Value)
	maxFee := amount.MulF64(maxFeeRatio)

	// Create a new weight estimator. The fee rate we pass in here doesn't
	// matter as we only use it for calculating the weight.
	weightEstimate := newWeightEstimator(m.FeeRate)

	// Add the change output.
	switch {
	case txscript.IsPayToTaproot(changeOutputScript):
		weightEstimate.addP2TROutput()

	case txscript.IsPayToWitnessScriptHash(changeOutputScript):
		weightEstimate.addP2WSHOutput()

	case txscript.IsPayToWitnessPubKeyHash(changeOutputScript):
		weightEstimate.addP2WKHOutput()

	case txscript.IsPayToPubKeyHash(changeOutputScript):
		weightEstimate.estimator.AddP2PKHOutput()

	case txscript.IsPayToScriptHash(changeOutputScript):
		weightEstimate.estimator.AddP2SHOutput()

	default:
		// Unknown script type.
		return 0, errors.New("unknown script type")
	}

	// Add the input.
	if err := weightEstimate.add(m.Input); err != nil {
		return 0, err
	}

	// Get the total weight and calculate the fee rate.
	weight := weightEstimate.weight()
	maxFeeRate := maxFee.MulF64(1 / float64(weight))

	return chainfee.SatPerKWeight(maxFeeRate), nil
}

// monitoredInput is an internal representation of a MonitorRequest that has
// more info attached.
type monitoredInput struct {
	// outpoint is the outpoint that is being monitored.
	outpoint *wire.OutPoint

	// monitorHeight specifies the broadcast height when it's been
	// requested for monitoring.
	monitorHeight uint32

	// confTarget is the sweep transaction's confirmation target.
	confTarget uint32

	// deadlineHeight specifies the block height that the transaction must
	// be confirmed at. If empty, we will use the conf target + the above
	// height to calcuate one when the strategy is used.
	deadlineHeight uint32

	// strategy specifies how the fee will be bumped.
	strategy Strategy

	// maxFeeRate specifies the maximum fee rate that can be used when
	// sweeping this input.
	maxFeeRate chainfee.SatPerKWeight

	// initalFeeRate is the starting fee rate when the input was requested
	// for monitoring.
	initalFeeRate chainfee.SatPerKWeight

	// currentFeeRate specifies the latest fee rate to be used for fee
	// bumping.
	currentFeeRate chainfee.SatPerKWeight

	// succeedHeight specifies the height that the outpoint has been spent.
	succeedHeight uint32
}

// String gives a verbose string which can be used in logging.
func (m *monitoredInput) String() string {
	s := fmt.Sprintf("monitorHeight=%v, confTarget=%v, "+
		"deadlineHeight=%v, strategy=%s, maxFeeRate=%v, "+
		"initalFeeRate=%v, currentFeeRate=%v", m.monitorHeight,
		m.confTarget, m.deadlineHeight, m.strategy,
		m.maxFeeRate, m.initalFeeRate, m.currentFeeRate)

	// If succeedHeight is set, we'll also add it to the string.
	if m.succeedHeight != 0 {
		s += fmt.Sprintf(", succeedHeight=%v", m.succeedHeight)
	}

	return s
}

// paramsUpdater defines the type `UpdateParams` function.
type paramsUpdater func(op wire.OutPoint, p ParamsUpdate) (chan Result, error)

// feeBumper implements the Bumper interface. Once started, it will monitor
// each of the requested outpoints. Upon receiving a new block, the bumper
// checks the current fee rate given a conf target, and decides whether to
// perform fee bumping based on different strategies.
type feeBumper struct {
	wg sync.WaitGroup

	// estimator is used when deciding new fee rate based on current conf
	// target.
	estimator chainfee.Estimator

	// chainNotifier is an instance of a chain notifier we'll use to watch
	// for new blocks.
	chainNotifier chainntnfs.ChainNotifier

	// currentHeight stores the currently best known block height.
	currentHeight uint32

	// monitoredInputs is a map that stores the requested and unconfirmed
	// UTXOs.
	monitoredInputs lnutils.SyncMap[*wire.OutPoint, *monitoredInput]

	// updateParams mounts the function `UpdateParams` of the sweeper so we
	// can easily send a request to update the fee rate for a given
	// input.
	updateParams paramsUpdater

	changeOutputScript []byte

	// quit is closed when the fee bumper stops.
	quit chan struct{}
}

// A compile time check to ensure feeBumper implements Bumper interface.
var _ Bumper = (*feeBumper)(nil)

// newFeeBumper creates a new feeBumper.
func newFeeBumper(chainNotifier chainntnfs.ChainNotifier,
	updateParams paramsUpdater, pkScript []byte) *feeBumper {

	return &feeBumper{
		chainNotifier: chainNotifier,
		updateParams:  updateParams,
		monitoredInputs: lnutils.SyncMap[
			*wire.OutPoint, *monitoredInput,
		]{},
		changeOutputScript: pkScript,
		quit:               make(chan struct{}),
	}
}

// NotifyConfirm tells the bumper a transaction has been confirmed and needs to
// be removed from the monitor queue.
//
// NOTE: part of the Bumper interface.
func (f *feeBumper) NotifyConfirm(op *wire.OutPoint) {
	input, ok := f.monitoredInputs.LoadAndDelete(op)
	if !ok {
		feeLog.Infof("Notify the spend of outpoint: %v not found in "+
			"monitored txes, might have been deleted", op)
		return
	}

	feeLog.Infof("Notified the spent: outpoint=%v deleted", op)

	feeLog.Tracef("Outpoint confirmed: %s", input)
}

// validateRequest checks that the request has sane params used.
func (f *feeBumper) validateRequest(req *MonitorRequest) error {
	// Validate the strategy used.
	if err := validateStrategy(req.Strategy); err != nil {
		return err
	}

	// Validate conf target and deadline height.
	if err := validateConfTarget(req); err != nil {
		return err
	}

	feeLog.Debugf("Validated request for outpoint: %v",
		req.Input.OutPoint())

	return nil
}

// validateConfTarget validates the conf target is sane.
func validateConfTarget(req *MonitorRequest) error {
	// Check conf target is greater than 0.
	if req.ConfTarget < 1 {
		return ErrConfTargetTooSmall
	}

	if req.ConfTarget == NoHeightInfo && req.FeeRate == 0 {
		return fmt.Errorf("%w: must set conf target or fee rate",
			ErrConflictArgs)
	}

	// If strategy is linear, we need to make sure the conf target is set
	// so we can use it to find the deadline.
	if req.Strategy == StrategyLinear {
		if req.ConfTarget == NoHeightInfo {
			return fmt.Errorf("%w: must set ConfTarget or "+
				"DeadlineHeight", ErrMissingParams)
		}
	}

	return nil
}

// RequestMonitor registers an outpoint to be monitored for potential fee
// bumping. If the outpoint already exits, ErrAlreadyMonitored will be
// returned.
//
// NOTE: part of the Bumper interface.
func (f *feeBumper) RequestMonitor(req *MonitorRequest) error {
	feeLog.Debugf("Received request %s", req)

	op := req.Input.OutPoint()

	// Check if the request has already been processed.
	//
	// TODO(yy): allow overwrite?
	input, loaded := f.monitoredInputs.Load(op)
	if loaded {
		feeLog.Errorf("Already monitored for %s", input)

		return ErrAlreadyMonitored
	}

	// Validate the request params.
	err := f.validateRequest(req)
	if err != nil {
		return fmt.Errorf("invalid record: %w", err)
	}

	// If the request doesn't specify a fee rate used, we need to get the
	// current fee rate from our fee estimator.
	feeRateUsed := req.FeeRate
	if feeRateUsed == 0 {
		feeRate, err := f.estimator.EstimateFeePerKW(req.ConfTarget)
		if err != nil {
			return fmt.Errorf("failed to estimate fee: %w", err)
		}

		// Save the fee rate used for this request.
		feeRateUsed = feeRate
	}

	// If the requst doesn't specify a broadcast height, use the manager's
	// best known height.
	broadcastHeight := req.Height
	if broadcastHeight == 0 {
		// Save the current height used for this request.
		broadcastHeight = f.currentHeight
	}

	// Calculate the deadline height based on the broadcast height and the
	// conf target.
	var deadlineHeight uint32

	// Calculate the deadline height if the conf target is set.
	if req.ConfTarget != NoHeightInfo {
		deadlineHeight = broadcastHeight + req.ConfTarget
	}

	// Calculate the max fee rate allowed for this request.
	maxFeeRate, err := req.estimateMaxFeeRate(f.changeOutputScript)
	if err != nil {
		return fmt.Errorf("estimate max fee rate: %w", err)
	}

	// Create a new monitored input.
	input = &monitoredInput{
		outpoint:       op,
		monitorHeight:  broadcastHeight,
		confTarget:     req.ConfTarget,
		deadlineHeight: deadlineHeight,
		strategy:       req.Strategy,
		initalFeeRate:  feeRateUsed,
		currentFeeRate: feeRateUsed,
		maxFeeRate:     maxFeeRate,
	}

	// Add the request to our monitor map.
	f.monitoredInputs.Store(op, input)

	feeLog.Infof("Start monitoring %s", req)

	return nil
}

// calculateFeeRate calculates the fee rate to use for a given input based on
// the strategy in use.
//
// TODO(yy): implement more strategies.
func (f *feeBumper) calculateFeeRate(r *monitoredInput) chainfee.SatPerKWeight {
	switch r.strategy {
	// If no strategy is used, return the original fee rate.
	case StrategyNone:
		return r.currentFeeRate

	case StrategyLinear:
		return f.feeRateForStrategyLinear(r)

	default:
		feeLog.Errorf("Unknown strategy %v", r.strategy)
		return r.currentFeeRate
	}
}

// TODO(yy): cache the results?
func (f *feeBumper) feeRateForStrategyLinear(
	r *monitoredInput) chainfee.SatPerKWeight {

	startingFeeRate := r.initalFeeRate
	endingFeeRate := r.maxFeeRate

	// Calculate the fee rate delta.
	feeRateDelta := btcutil.Amount(endingFeeRate - startingFeeRate)

	// Calculate the height delta.
	heightDelta := r.deadlineHeight - r.monitorHeight
	heightsPassed := f.currentHeight - r.monitorHeight

	// Calculate the fee rate increased.
	ratio := float64(heightsPassed) / float64(heightDelta)
	feeRateIncreased := chainfee.SatPerKWeight(feeRateDelta.MulF64(ratio))

	// Calculate the fee rate to use.
	feeRate := startingFeeRate + feeRateIncreased

	return feeRate
}

// needFeeBump checks whether a fee bumping is needed for a given outpoint.  A
// fee bump is only needed when the new calculated fee rate is larger than the
// original fee rate used. It returns the new fee rate and bool.
func (f *feeBumper) needFeeBump(
	r *monitoredInput) (chainfee.SatPerKWeight, bool) {

	// Decide the latest fee rate.
	newFeeRate := f.calculateFeeRate(r)

	// If newly estimated fee rate is no greater than the fee rate in use,
	// there's no need to bump the fee rate.
	if newFeeRate <= r.currentFeeRate {
		return r.currentFeeRate, false
	}

	// Check the fee rate is sane and not exceeding the max.
	if r.maxFeeRate != 0 && newFeeRate > r.maxFeeRate {
		feeLog.Infof("Estimated fee rate: %v for outpoint(%v) "+
			"exceeds the max allowed fee rate: %v, using the max "+
			"fee rate instead", newFeeRate, r.outpoint,
			r.maxFeeRate)

		newFeeRate = r.maxFeeRate
	}

	return newFeeRate, true
}

// handleFeeBump handles the fee bumping by first checking whether it's needed.
// If so, it will bump the fee and updates the monitor record once succeeded.
//
// NOTE: must run as a goroutine.
func (f *feeBumper) handleFeeBump(r *monitoredInput) {
	defer f.wg.Done()

	op := r.outpoint
	feeLog.Debugf("Preparing fee bump for outpoint: %v", op)

	newFeeRate, needUpdate := f.needFeeBump(r)

	// Check if we need to do a fee bump for this tx.
	if !needUpdate {
		feeLog.Debugf("Skipped fee bump at height=%v for outpoint: "+
			"%v, current fee rate: %v, estimated new fee rate: %v",
			op, f.currentHeight, r.currentFeeRate, newFeeRate)

		return
	}

	// Send the bump request to the sweeper.
	err := f.sendFeeBump(r, newFeeRate)
	if err != nil {
		feeLog.Errorf("Fee bump for outpoint: %v failed, error from "+
			"update params: %v, record removed", op, err)

		f.monitoredInputs.Delete(op)

		return
	}

	feeLog.Infof("Fee bump for outpoint: %v succeeded, old fee rate: %v, "+
		"current fee rate: %v", op, r.currentFeeRate, newFeeRate)

	// Update the current fee rate on this record.
	r.currentFeeRate = newFeeRate

	// Update our monitored outpoints map.
	f.monitoredInputs.Store(op, r)
}

// sendFeeBump sends a params update request to the sweeper.
func (f *feeBumper) sendFeeBump(r *monitoredInput,
	newFeeRate chainfee.SatPerKWeight) error {

	op := r.outpoint
	feeLog.Debugf("Fee bumping at height=%v for outpoint: %v, current "+
		"fee rate: %v, estimated new fee rate: %v", op,
		f.currentHeight, r.currentFeeRate, newFeeRate)

	// Note that if we bump an already confirmed transaction, an error will
	// be returned.
	params := ParamsUpdate{
		Fee: FeePreference{
			FeeRate: newFeeRate,
		},
		Force: true,
	}
	resp, err := f.updateParams(*op, params)
	if err != nil {
		return err
	}

	select {
	case result := <-resp:
		switch {
		// We may get an ErrNotMine if the input has already been
		// swpet.
		case errors.Is(result.Err, lnwallet.ErrNotMine):
			feeLog.Debugf("Outpoint: %v not found in sweeper's "+
				"pendingInputs, assuming already spent", op)

			return nil

		case result.Err != nil:
			return err
		}

		return nil

	case <-f.quit:
		feeLog.Debugf("Fee bumper shutting down, exit sendFeeBump "+
			"for %v", op)
		return nil
	}
}

// monitor is the main goroutine that manages the fee bumping.
//
// NOTE: must run as a goroutine.
func (f *feeBumper) monitor() {
	defer f.wg.Done()

	// Subscribe to block height updates.
	blockEpochs, err := f.chainNotifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		feeLog.Errorf("register block epoch ntfn: %w", err)
		return
	}

	for {
		select {
		case epoch := <-blockEpochs.Epochs:
			height := epoch.Height
			feeLog.Debugf("Received new block height: %v", height)

			// Update our current block height.
			f.currentHeight = uint32(height)

			// Process the monitored outpoints.
			f.monitoredInputs.Range(func(_ *wire.OutPoint,
				r *monitoredInput) bool {

				f.wg.Add(1)
				go f.handleFeeBump(r)

				return true
			})

		case <-f.quit:
			feeLog.Debugf("Bumper shutting down, quit monitoring")

			return
		}
	}
}

// Start starts the manager's main goroutine monitor. It will spawn a child
// context from the passed one and cancel it when the function is returned.
func (f *feeBumper) Start() {
	feeLog.Infof("Starting fee bumper...")

	f.wg.Add(1)
	go f.monitor()
}

func (f *feeBumper) Stop() {
	feeLog.Infof("Stopping fee bumper...")

	close(f.quit)
	f.wg.Wait()
}
