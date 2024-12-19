package sweep

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrMaxPosition is returned when trying to increase the position of
	// the fee function while it's already at its max.
	ErrMaxPosition = errors.New("position already at max")

	// ErrZeroFeeRateDelta is returned when the fee rate delta is zero.
	ErrZeroFeeRateDelta = errors.New("fee rate delta is zero")
)

// mSatPerKWeight represents a fee rate in msat/kw.
//
// TODO(yy): unify all the units to be virtual bytes.
type mSatPerKWeight lnwire.MilliSatoshi

// String returns a human-readable string of the fee rate.
func (m mSatPerKWeight) String() string {
	s := lnwire.MilliSatoshi(m)
	return fmt.Sprintf("%v/kw", s)
}

// FeeFunction defines an interface that is used to calculate fee rates for
// transactions. It's expected the implementations use three params, the
// starting fee rate, the ending fee rate, and number of blocks till deadline
// block height, to build an algorithm to calculate the fee rate based on the
// current block height.
type FeeFunction interface {
	// FeeRate returns the current fee rate calculated by the fee function.
	FeeRate() chainfee.SatPerKWeight

	// Increment increases the fee rate by one step. The definition of one
	// step is up to the implementation. After calling this method, it's
	// expected to change the state of the fee function such that calling
	// `FeeRate` again will return the increased value.
	//
	// It returns a boolean to indicate whether the fee rate is increased,
	// as fee bump should not be attempted if the increased fee rate is not
	// greater than the current fee rate, which may happen if the algorithm
	// gives the same fee rates at two positions.
	//
	// An error is returned when the max fee rate is reached.
	//
	// NOTE: we intentionally don't return the new fee rate here, so both
	// the implementation and the caller are aware of the state change.
	Increment() (bool, error)

	// IncreaseFeeRate increases the fee rate to the new position
	// calculated using (width - confTarget). It returns a boolean to
	// indicate whether the fee rate is increased, and an error if the
	// position is greater than the width.
	//
	// NOTE: this method is provided to allow the caller to increase the
	// fee rate based on a conf target without taking care of the fee
	// function's current state (position).
	IncreaseFeeRate(confTarget uint32) (bool, error)
}

// LinearFeeFunction implements the FeeFunction interface with a linear
// function:
//
//	feeRate = startingFeeRate + position * delta.
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

	// currentFeeRate specifies the current calculated fee rate.
	currentFeeRate chainfee.SatPerKWeight

	// width is the number of blocks between the starting block height
	// and the deadline block height minus one.
	//
	// NOTE: We do minus one from the conf target here because we want to
	// max out the budget before the deadline height is reached.
	width uint32

	// position is the fee function's current position, given a width of w,
	// a valid position should lie in range [0, w].
	position uint32

	// deltaFeeRate is the fee rate (msat/kw) increase per block.
	//
	// NOTE: this is used to increase precision.
	deltaFeeRate mSatPerKWeight

	// estimator is the fee estimator used to estimate the fee rate. We use
	// it to get the initial fee rate and, use it as a benchmark to decide
	// whether we want to used the estimated fee rate or the calculated fee
	// rate based on different strategies.
	estimator chainfee.Estimator
}

// Compile-time check to ensure LinearFeeFunction satisfies the FeeFunction.
var _ FeeFunction = (*LinearFeeFunction)(nil)

// NewLinearFeeFunction creates a new linear fee function and initializes it
// with a starting fee rate which is an estimated value returned from the fee
// estimator using the initial conf target.
func NewLinearFeeFunction(maxFeeRate chainfee.SatPerKWeight,
	confTarget uint32, estimator chainfee.Estimator,
	startingFeeRate fn.Option[chainfee.SatPerKWeight]) (
	*LinearFeeFunction, error) {

	// If the deadline is one block away or has already been reached,
	// there's nothing the fee function can do. In this case, we'll use the
	// max fee rate immediately.
	if confTarget <= 1 {
		return &LinearFeeFunction{
			startingFeeRate: maxFeeRate,
			endingFeeRate:   maxFeeRate,
			currentFeeRate:  maxFeeRate,
		}, nil
	}

	l := &LinearFeeFunction{
		endingFeeRate: maxFeeRate,
		width:         confTarget - 1,
		estimator:     estimator,
	}

	// If the caller specifies the starting fee rate, we'll use it instead
	// of estimating it based on the deadline.
	start, err := startingFeeRate.UnwrapOrFuncErr(
		func() (chainfee.SatPerKWeight, error) {
			// Estimate the initial fee rate.
			//
			// NOTE: estimateFeeRate guarantees the returned fee
			// rate is capped by the ending fee rate, so we don't
			// need to worry about overpay.
			return l.estimateFeeRate(confTarget)
		})
	if err != nil {
		return nil, fmt.Errorf("estimate initial fee rate: %w", err)
	}

	// Calculate how much fee rate should be increased per block.
	end := l.endingFeeRate

	// The starting and ending fee rates are in sat/kw, so we need to
	// convert them to msat/kw by multiplying by 1000.
	delta := btcutil.Amount(end - start).MulF64(1000 / float64(l.width))
	l.deltaFeeRate = mSatPerKWeight(delta)

	// We only allow the delta to be zero if the width is one - when the
	// delta is zero, it means the starting and ending fee rates are the
	// same, which means there's nothing to increase, so any width greater
	// than 1 doesn't provide any utility. This could happen when the
	// budget is too small.
	if l.deltaFeeRate == 0 && l.width != 1 {
		log.Errorf("Failed to init fee function: startingFeeRate=%v, "+
			"endingFeeRate=%v, width=%v, delta=%v", start, end,
			l.width, l.deltaFeeRate)

		return nil, ErrZeroFeeRateDelta
	}

	// Attach the calculated values to the fee function.
	l.startingFeeRate = start
	l.currentFeeRate = start

	log.Debugf("Linear fee function initialized with startingFeeRate=%v, "+
		"endingFeeRate=%v, width=%v, delta=%v", start, end,
		l.width, l.deltaFeeRate)

	return l, nil
}

// FeeRate returns the current fee rate.
//
// NOTE: part of the FeeFunction interface.
func (l *LinearFeeFunction) FeeRate() chainfee.SatPerKWeight {
	return l.currentFeeRate
}

// Increment increases the fee rate by one position, returns a boolean to
// indicate whether the fee rate was increased, and an error if the position is
// greater than the width. The increased fee rate will be set as the current
// fee rate, and the internal position will be incremented.
//
// NOTE: this method will change the state of the fee function as it increases
// its current fee rate.
//
// NOTE: part of the FeeFunction interface.
func (l *LinearFeeFunction) Increment() (bool, error) {
	return l.increaseFeeRate(l.position + 1)
}

// IncreaseFeeRate calculate a new position using the given conf target, and
// increases the fee rate to the new position by calling the Increment method.
//
// NOTE: this method will change the state of the fee function as it increases
// its current fee rate.
//
// NOTE: part of the FeeFunction interface.
func (l *LinearFeeFunction) IncreaseFeeRate(confTarget uint32) (bool, error) {
	newPosition := uint32(0)

	// Only calculate the new position when the conf target is less than
	// the function's width - the width is the initial conf target-1, and
	// we expect the current conf target to decrease over time. However, we
	// still allow the supplied conf target to be greater than the width,
	// and we won't increase the fee rate in that case.
	if confTarget < l.width+1 {
		newPosition = l.width + 1 - confTarget
		log.Tracef("Increasing position from %v to %v", l.position,
			newPosition)
	}

	if newPosition <= l.position {
		log.Tracef("Skipped increase feerate: position=%v, "+
			"newPosition=%v ", l.position, newPosition)

		return false, nil
	}

	return l.increaseFeeRate(newPosition)
}

// increaseFeeRate increases the fee rate by the specified position, returns a
// boolean to indicate whether the fee rate was increased, and an error if the
// position is greater than the width. The increased fee rate will be set as
// the current fee rate, and the internal position will be set to the specified
// position.
//
// NOTE: this method will change the state of the fee function as it increases
// its current fee rate.
func (l *LinearFeeFunction) increaseFeeRate(position uint32) (bool, error) {
	// If the new position is already at the end, we return an error.
	if l.position >= l.width {
		return false, ErrMaxPosition
	}

	// Get the old fee rate.
	oldFeeRate := l.currentFeeRate

	// Update its internal state.
	l.position = position
	l.currentFeeRate = l.feeRateAtPosition(position)

	log.Tracef("Fee rate increased from %v to %v at position %v",
		oldFeeRate, l.currentFeeRate, l.position)

	return l.currentFeeRate > oldFeeRate, nil
}

// feeRateAtPosition calculates the fee rate at a given position and caps it at
// the ending fee rate.
func (l *LinearFeeFunction) feeRateAtPosition(p uint32) chainfee.SatPerKWeight {
	if p >= l.width {
		return l.endingFeeRate
	}

	// deltaFeeRate is in msat/kw, so we need to divide by 1000 to get the
	// fee rate in sat/kw.
	feeRateDelta := btcutil.Amount(l.deltaFeeRate).MulF64(float64(p) / 1000)

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
		ConfTarget: confTarget,
	}

	// If the conf target is greater or equal to the max allowed value
	// (1008), we will use the min relay fee instead.
	if confTarget >= chainfee.MaxBlockTarget {
		minFeeRate := l.estimator.RelayFeePerKW()
		log.Infof("Conf target %v is greater than max block target, "+
			"using min relay fee rate %v", confTarget, minFeeRate)

		return minFeeRate, nil
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
