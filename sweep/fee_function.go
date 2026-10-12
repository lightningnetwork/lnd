package sweep

import (
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrMaxPosition is returned when trying to increase the position of
	// the fee function while it's already at its max.
	ErrMaxPosition = errors.New("position already at max")
)

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

	// IncreaseFeeRate advances the fee function to the given conf target.
	// It returns a boolean to indicate whether the fee rate is increased.
	// A function already at its endpoint reports no increase and no error:
	// the endpoint is a terminus for elapsed time, and only Increment
	// treats it as an exhausted search.
	//
	// NOTE: this method is provided to allow the caller to increase the
	// fee rate based on a conf target without taking care of the fee
	// function's current state.
	IncreaseFeeRate(confTarget uint32) (bool, error)
}

// estimateStartingFeeRate estimates the initial policy rate. A zero maximum
// leaves it uncapped so a pending input can retain a baseline independent of
// the size and budget of any particular batch.
func estimateStartingFeeRate(estimator chainfee.Estimator,
	confTarget uint32, maxFeeRate chainfee.SatPerKWeight) (
	chainfee.SatPerKWeight, error) {

	fee := FeeEstimateInfo{ConfTarget: confTarget}

	// If the conf target is greater or equal to the max allowed value
	// (1008), we will use the min relay fee instead.
	if confTarget >= chainfee.MaxBlockTarget {
		minFeeRate := estimator.RelayFeePerKW()
		log.Infof("Conf target %v is greater than max block target, "+
			"using min relay fee rate %v", confTarget, minFeeRate)

		if maxFeeRate != 0 {
			minFeeRate = min(minFeeRate, maxFeeRate)
		}

		return minFeeRate, nil
	}

	// Bound the estimate when a batch maximum is supplied; otherwise retain
	// the baseline and let the publisher apply its eventual batch maximum.
	estimatedFeeRate, err := fee.Estimate(estimator, maxFeeRate)
	if err != nil {
		return 0, err
	}

	return estimatedFeeRate, nil
}

// pooledFeeFunction ramps a batch toward its ending fee rate by a deadline
// that all of its schedules share. It composes two kinds of policy
// differently. A starting rate constrains the whole transaction, whose rate
// every input shares, so the batch starts at the highest of them. Elapsed
// urgency commits an input's own budget, so each schedule raises the rate
// only by its budget's share of the headroom above that start. An input with
// a small budget therefore cannot spend another input's budget on its behalf.
// When every schedule shares an origin, this is a linear ramp from the
// starting rate to the ending rate.
type pooledFeeFunction struct {
	// startingFeeRate is the highest starting rate among the schedules,
	// capped at the ending fee rate.
	startingFeeRate chainfee.SatPerKWeight

	// endingFeeRate is the rate reached at the deadline.
	endingFeeRate chainfee.SatPerKWeight

	// deadline is the deadline height shared by every schedule.
	deadline int32

	// origins and weights hold each schedule's start height and its share
	// of the pooled budget. The weights sum to one.
	origins []int32
	weights []float64

	// height is the height at which the schedules are evaluated. A fee
	// search advances it ahead of the chain within an attempt.
	height int32
}

// Compile-time check to ensure pooledFeeFunction satisfies FeeFunction.
var _ FeeFunction = (*pooledFeeFunction)(nil)

// newPooledFeeFunction evaluates a nonempty set of schedules at the given
// height. A schedule without a starting rate is estimated at its origin's
// conf target, unless its deadline was then within one block. Such a schedule
// imposes no starting rate, and calls for the ending fee rate throughout.
func newPooledFeeFunction(maxFeeRate chainfee.SatPerKWeight, deadline,
	height int32, schedules []InputSchedule,
	estimator chainfee.Estimator) (*pooledFeeFunction, error) {

	p := &pooledFeeFunction{
		endingFeeRate: maxFeeRate,
		deadline:      deadline,
		height:        height,
	}

	var total btcutil.Amount
	for _, schedule := range schedules {
		total += schedule.Budget
	}

	start := fn.None[chainfee.SatPerKWeight]()
	for _, schedule := range schedules {
		// Without any budget to apportion, schedules share equally.
		weight := 1 / float64(len(schedules))
		if total > 0 {
			weight = float64(schedule.Budget) / float64(total)
		}
		p.origins = append(p.origins, schedule.StartHeight)
		p.weights = append(p.weights, weight)

		rate := schedule.StartingFeeRate
		target := calcCurrentConfTarget(schedule.StartHeight, deadline)
		if rate.IsNone() && target > 1 {
			estimate, err := estimateStartingFeeRate(
				estimator, target, maxFeeRate,
			)
			if err != nil {
				return nil, fmt.Errorf("estimate initial fee "+
					"rate: %w", err)
			}
			rate = fn.Some(estimate)
		}
		rate.WhenSome(func(r chainfee.SatPerKWeight) {
			r = min(r, maxFeeRate)
			start = fn.Some(max(start.UnwrapOr(r), r))
		})
	}

	// If no schedule imposes a starting rate, every schedule is complete
	// and the batch is at its ending fee rate.
	p.startingFeeRate = start.UnwrapOr(maxFeeRate)

	log.Debugf("Pooled fee function initialized with startingFeeRate=%v, "+
		"endingFeeRate=%v, deadline=%v, height=%v, schedules=%v",
		p.startingFeeRate, p.endingFeeRate, deadline, height,
		len(schedules))

	return p, nil
}

// FeeRate returns the starting rate raised by each schedule's elapsed share
// of the headroom.
//
// NOTE: part of the FeeFunction interface.
func (p *pooledFeeFunction) FeeRate() chainfee.SatPerKWeight {
	// Every schedule is complete once the deadline is within one block.
	if p.height >= p.deadline-1 {
		return p.endingFeeRate
	}

	var urgency float64
	for i, origin := range p.origins {
		width := p.deadline - 1 - origin
		switch {
		case width <= 0:
			urgency += p.weights[i]

		case p.height > origin:
			elapsed := float64(p.height-origin) / float64(width)
			urgency += p.weights[i] * elapsed
		}
	}

	headroom := float64(p.endingFeeRate - p.startingFeeRate)
	increase := chainfee.SatPerKWeight(math.Round(headroom * urgency))

	return min(p.startingFeeRate+increase, p.endingFeeRate)
}

// Increment advances the evaluation height by one block for a candidate
// search. The search belongs to the attempt, so it never changes the origins
// retained by the sweeper. The deadline bounds the search.
//
// NOTE: part of the FeeFunction interface.
func (p *pooledFeeFunction) Increment() (bool, error) {
	oldFeeRate := p.FeeRate()
	if oldFeeRate >= p.endingFeeRate {
		return false, ErrMaxPosition
	}

	p.height++

	return p.FeeRate() > oldFeeRate, nil
}

// IncreaseFeeRate advances the evaluation height to the given conf target,
// never moving it back behind a search. Reaching the ending fee rate through
// elapsed time is a terminus, not an error.
//
// NOTE: part of the FeeFunction interface.
func (p *pooledFeeFunction) IncreaseFeeRate(confTarget uint32) (bool, error) {
	height := p.deadline - int32(confTarget)
	if height <= p.height {
		return false, nil
	}

	oldFeeRate := p.FeeRate()
	p.height = height

	return p.FeeRate() > oldFeeRate, nil
}
