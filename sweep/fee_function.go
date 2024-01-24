package sweep

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// FeeFunction defines an interface that is used to calculate fee rates for
// transactions. It's expected the implementations use three params, the
// starting fee rate, the ending fee rate, and number of blocks till deadline
// block height, to build an algorithm to calculate the fee rate based on the
// current block height.
type FeeFunction interface {
	// FeeRate returns the current fee rate calculated by the fee function.
	FeeRate() chainfee.SatPerKWeight

	// IncreaseFeeRate increases the fee rate by one step. The definition
	// of one step is up to the implementation. After calling this method,
	// it's expected to change the state of the fee function such that
	// calling `FeeRate` again will return the increased value.
	//
	// NOTE: we intentionally don't return the new fee rate here, so both
	// the implementation and the caller are aware of the state change.
	IncreaseFeeRate() error

	// SkipFeeBump returns true if a fee bump can be skipped. A fee bump
	// should not be attempted if the increased fee rate is not greater
	// than the current fee rate, which may happen if the algorithm gives
	// the same fee rates at two positions.
	SkipFeeBump(confTarget uint32) bool
}

// LinearFeeFunction implements the FeeFunction interface with a linear
// function:
//
//	feeRate = startingFeeRate + (1 + position/width) * delta.
//	     - width: deadlineBlockHeight - startingBlockHeight
//	     - delta: (endingFeeRate - startingFeeRate) / width
//	     - position: currentBlockHeight - startingBlockHeight
//
// The fee rate will be capped at endingFeeRate.
//
// TODO(yy): implement more functions specified here:
// - https://github.com/lightningnetwork/lnd/issues/4215
type LinearFeeFunction struct {
	// startingFeeRate specifies the initial fee rate to begin with.
	startingFeeRate chainfee.SatPerKWeight

	// endingFeeRate specifies the max allowed fee rate.
	endingFeeRate chainfee.SatPerKWeight

	// currentFeeRate specifies the current calcualted fee rate.
	currentFeeRate chainfee.SatPerKWeight

	// width is the number of blocks between the starting block height
	// and the deadline block height.
	width uint32

	// position is the number of blocks between the starting block height
	// and the current block height.
	position uint32

	// delta is the fee rate increase per block.
	delta btcutil.Amount

	// estimator is the fee estimator used to estimate the fee rate. We use
	// it to get the initial fee rate and, use it as a benchmark to decide
	// whether we want to used the estimated fee rate or the calculated fee
	// rate based on different strategies.
	estimator chainfee.Estimator
}

// NewLinearFeeFunction creates a new linear fee function and initializes it
// with a starting fee rate which is an estimated value returned from the fee
// estimator using the initial conf target.
func NewLinearFeeFunction(maxFeeRate chainfee.SatPerKWeight, confTarget uint32,
	estimator chainfee.Estimator) (*LinearFeeFunction, error) {

	// Sanity check conf target.
	if confTarget == 0 {
		return nil, fmt.Errorf("width must be greater than zero")
	}

	l := &LinearFeeFunction{
		endingFeeRate: maxFeeRate,
		width:         confTarget,
		estimator:     estimator,
	}

	// Estimate the initial fee rate.
	start, err := l.estimateFeeRate(confTarget)
	if err != nil {
		return nil, fmt.Errorf("estimate initial fee rate: %v", err)
	}

	// Sanity check the starting fee rate is not greater than the max fee
	// rate.
	end := maxFeeRate
	if start > end {
		return nil, fmt.Errorf("start fee rate %v is greater than "+
			"end fee rate %v", start, maxFeeRate)
	}

	// Calculate how much fee rate should be increased per block.
	delta := btcutil.Amount(end - start).MulF64(1 / float64(confTarget))

	// Attach the calculated values to the fee function.
	l.startingFeeRate = start
	l.currentFeeRate = start
	l.delta = delta

	log.Debugf("Linear fee function initialized with startingFeeRate=%v, "+
		"endingFeeRate=%v, width=%v, delta=%v", start, end,
		confTarget, delta)

	return l, nil
}

// FeeRate returns the current fee rate.
//
// NOTE: part of the FeeFunction interface.
func (l *LinearFeeFunction) FeeRate() chainfee.SatPerKWeight {
	return l.currentFeeRate
}

// IncreaseFeeRate increases the fee rate by one position, returns an error if
// the position is greater than the width. The increased fee rate will be set as
// the current fee rate, and the internal position will be incremented.
//
// NOTE: part of the FeeFunction interface.
func (l *LinearFeeFunction) IncreaseFeeRate() error {
	if l.position == l.width {
		return fmt.Errorf("cannot increase fee rate, already at max")
	}

	l.position++
	l.currentFeeRate = l.feeRateAtPosition(l.position)

	return nil
}

// SkipFeeBump returns a boolean indicating whether a fee bump can be skipped
// based on the current conf target. The following cases will cause a fee bump
// to be skipped:
// - when the fee rate is already at the ending fee rate.
// - when the fee estimator returns an error.
// - when the new fee rate is not greater than the current fee rate.
//
// NOTE: this method will change the state of the fee function as it increases
// its current fee rate.
//
// NOTE: part of the FeeFunction interface.
func (l *LinearFeeFunction) SkipFeeBump(confTarget uint32) bool {
	// Get the current fee rate of the tx.
	currentFeeRate := l.FeeRate()

	// Ask the fee function to increase the fee rate.
	//
	// If there's an error, it's likely the fee rate is already at its max.
	// We have two options here,
	// 1. skip fee bumping this tx here, which will make it sitting
	//    in the mempool and may be confirmed when next block arrives.
	// 2. return an error here, which will fail this monitored record and
	//    let the sweeper re-process these inputs and retry sweeping again
	//    after budge is increased.
	// We start with option 1 and see its effects.
	err := l.IncreaseFeeRate()
	if err != nil {
		log.Errorf("Failed to increase fee rate: %v", err)
		return true
	}

	// Get the new fee rate.
	newFeeRate := l.FeeRate()

	// Get the fee rate from the fee estimator(bitcoind/btcd/webAPI).
	estimatedFeeRate, err := l.estimateFeeRate(confTarget)
	if err != nil {
		log.Errorf("Fail to estimate fee: %v", err)
		return true
	}

	log.Debugf("Current fee rate: %v, new fee rate: %v, estimated fee "+
		"rate: %v", currentFeeRate, newFeeRate, estimatedFeeRate)

	// If the fee estimator gives a smaller fee rate, use it instead.
	//
	// TODO(yy): too conservative? switch based on fee strategy?
	if estimatedFeeRate < newFeeRate {
		log.Debugf("Use estimated fee rate %v instead of newFeeRate %v",
			estimatedFeeRate, newFeeRate)

		newFeeRate = estimatedFeeRate
		l.currentFeeRate = newFeeRate
	}

	// If the new fee rate is no greater, there's no need to perform a fee
	// bump as it won't pass the RBF constraints.
	if newFeeRate <= currentFeeRate {
		return true
	}

	return false
}

// feeRateAtPosition calculates the fee rate at a given position and caps it at
// the ending fee rate.
func (l *LinearFeeFunction) feeRateAtPosition(p uint32) chainfee.SatPerKWeight {
	if p >= l.width {
		return l.endingFeeRate
	}

	feeRateDelta := l.delta.MulF64(float64(p))

	feeRate := l.startingFeeRate + chainfee.SatPerKWeight(feeRateDelta)
	if feeRate > l.endingFeeRate {
		return l.endingFeeRate
	}

	return feeRate
}

// estimateFeeRate asks the fee estimator to estimate the fee rate based on its
// conf target.
func (l *LinearFeeFunction) estimateFeeRate(
	confTarget uint32) (chainfee.SatPerKWeight, error) {

	fee := FeeEstimateInfo{
		ConfTarget: uint32(confTarget),
	}

	// endingFeeRate comes from budget/txWeight, which means the returned
	// fee rate will always be capped by this value, hence we don't need to
	// worry about overpay.
	estimatedFeeRate, err := fee.Estimate(l.estimator, l.endingFeeRate)
	if err != nil {
		return 0, err
	}

	return estimatedFeeRate, nil
}
