package sweep

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// singleSchedule returns a schedule originating at height 100.
func singleSchedule(
	start fn.Option[chainfee.SatPerKWeight]) []InputSchedule {

	return []InputSchedule{{FeeSchedule: FeeSchedule{
		StartHeight: 100, StartingFeeRate: start,
	}}}
}

// TestPooledFeeCapsRequestedRate checks that an unaffordable starting rate
// produces a constant schedule, including at and beyond the deadline.
// Elapsed time reaches the endpoint without error; only an explicit search
// step past it is an exhaustion.
func TestPooledFeeCapsRequestedRate(t *testing.T) {
	t.Parallel()

	for _, target := range []int32{20, 2, 1, 0} {
		t.Run(fmt.Sprintf("target=%d", target), func(t *testing.T) {
			const capRate = chainfee.SatPerKWeight(1000)
			f, err := newPooledFeeFunction(
				capRate, 100+target, 100,
				singleSchedule(fn.Some(capRate+1)), nil,
			)
			require.NoError(t, err)
			require.Equal(t, capRate, f.FeeRate())

			increased, err := f.IncreaseFeeRate(0)
			require.NoError(t, err)
			require.False(t, increased)
			require.Equal(t, capRate, f.FeeRate())

			increased, err = f.Increment()
			require.False(t, increased)
			require.ErrorIs(t, err, ErrMaxPosition)
			require.Equal(t, capRate, f.FeeRate())
		})
	}
}

// TestPooledFeeEstimator checks that a schedule without a starting rate is
// estimated at its origin's conf target, and that the estimator's response or
// error is handled.
func TestPooledFeeEstimator(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	maxFeeRate := chainfee.SatPerKWeight(10000)
	noStart := fn.None[chainfee.SatPerKWeight]()

	// An estimator error is returned.
	estimator.On("EstimateFeePerKW", uint32(6)).Return(
		chainfee.SatPerKWeight(0), errDummy).Once()
	_, err := newPooledFeeFunction(
		maxFeeRate, 106, 100, singleSchedule(noStart), estimator,
	)
	rt.ErrorIs(err, errDummy)

	// An estimate above the maximum is capped.
	estimator.On("EstimateFeePerKW", uint32(6)).Return(
		maxFeeRate+1, nil).Once()
	estimator.On("RelayFeePerKW").Return(
		chainfee.SatPerKWeight(100)).Once()
	f, err := newPooledFeeFunction(
		maxFeeRate, 106, 100, singleSchedule(noStart), estimator,
	)
	rt.NoError(err)
	rt.Equal(maxFeeRate, f.FeeRate())

	// When the conf target is >= 1008, the min relay fee is used.
	estimator.On("RelayFeePerKW").Return(
		chainfee.SatPerKWeight(100)).Once()
	f, err = newPooledFeeFunction(
		maxFeeRate, 1108, 100, singleSchedule(noStart), estimator,
	)
	rt.NoError(err)
	rt.EqualValues(100, f.FeeRate())

	// When the deadline is within one block of the origin, nothing is
	// estimated and the maximum is used.
	for _, deadline := range []int32{100, 101} {
		f, err = newPooledFeeFunction(
			maxFeeRate, deadline, 100, singleSchedule(noStart),
			estimator,
		)
		rt.NoError(err)
		rt.Equal(maxFeeRate, f.FeeRate())
	}
}

// deferredBatch returns schedules for an immediate input with origin 100 and
// a deferred input with origin 101, sharing deadline 111 and equal budgets.
func deferredBatch() []InputSchedule {
	return []InputSchedule{{
		FeeSchedule: FeeSchedule{
			StartHeight:     100,
			StartingFeeRate: fn.Some(chainfee.SatPerKWeight(1000)),
		},
		Budget: 5000,
	}, {
		FeeSchedule: FeeSchedule{
			StartHeight:     101,
			StartingFeeRate: fn.Some(chainfee.SatPerKWeight(1500)),
		},
		Budget: 5000,
	}}
}

// TestPooledFeeSearch checks that a local search advances the evaluation
// height without elapsed time undoing it, and that a deferred schedule
// constrains the start but commits nothing before its origin.
func TestPooledFeeSearch(t *testing.T) {
	t.Parallel()

	f, err := newPooledFeeFunction(11000, 111, 100, deferredBatch(), nil)
	require.NoError(t, err)

	// The deferred input's starting rate binds the whole transaction.
	require.EqualValues(t, 1500, f.FeeRate())

	// A fee rejection searches one block ahead of the chain. Only the
	// immediate input's ramp has begun, committing its half of the
	// headroom: 1500 + 9500 * 0.5 * 1/10.
	increased, err := f.Increment()
	require.NoError(t, err)
	require.True(t, increased)
	require.EqualValues(t, 1975, f.FeeRate())

	// Elapsed time must not undo that search.
	increased, err = f.IncreaseFeeRate(10)
	require.NoError(t, err)
	require.False(t, increased)
	require.EqualValues(t, 1975, f.FeeRate())

	// A block later both ramps have begun:
	// 1500 + 9500 * 0.5 * (2/10 + 1/9).
	increased, err = f.IncreaseFeeRate(9)
	require.NoError(t, err)
	require.True(t, increased)
	require.EqualValues(t, 2978, f.FeeRate())

	// Time reaching the cap is normal; searching beyond it is exhaustion.
	increased, err = f.IncreaseFeeRate(1)
	require.NoError(t, err)
	require.True(t, increased)
	require.EqualValues(t, 11000, f.FeeRate())
	increased, err = f.Increment()
	require.ErrorIs(t, err, ErrMaxPosition)
	require.False(t, increased)
	increased, err = f.IncreaseFeeRate(0)
	require.NoError(t, err)
	require.False(t, increased)
}

// TestPooledFeeCommitsOwnBudget checks that elapsed urgency commits only the
// schedule's own budget. An old input with a small budget must not spend a
// new input's budget on its behalf.
func TestPooledFeeCommitsOwnBudget(t *testing.T) {
	t.Parallel()

	start := fn.Some(chainfee.SatPerKWeight(1000))
	old := InputSchedule{
		FeeSchedule: FeeSchedule{
			StartHeight: 100, StartingFeeRate: start,
		},
		Budget: 1000,
	}
	fresh := InputSchedule{
		FeeSchedule: FeeSchedule{
			StartHeight: 105, StartingFeeRate: start,
		},
		Budget: 9000,
	}

	// Alone, the old input is halfway along its ramp.
	f, err := newPooledFeeFunction(
		11000, 111, 105, []InputSchedule{old}, nil,
	)
	require.NoError(t, err)
	require.EqualValues(t, 6000, f.FeeRate())

	// Pooled, it commits half of its tenth of the headroom, and the fresh
	// input commits nothing: 1000 + 10000 * 0.1 * 0.5.
	f, err = newPooledFeeFunction(
		11000, 111, 105, []InputSchedule{old, fresh}, nil,
	)
	require.NoError(t, err)
	require.EqualValues(t, 1500, f.FeeRate())

	// Every budget is committed by the deadline.
	_, err = f.IncreaseFeeRate(1)
	require.NoError(t, err)
	require.EqualValues(t, 11000, f.FeeRate())
}

// TestPooledFeeMatchesLinear checks that schedules sharing an origin follow
// the linear ramp from their highest starting rate, whatever their budgets,
// through any interleaving of elapsed time and searches. Weights are summed
// in floating point, so a rate exactly between two integers may round either
// way.
func TestPooledFeeMatchesLinear(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		width := rapid.Int32Range(1, 1008).Draw(rt, "width")
		capRate := chainfee.SatPerKWeight(
			rapid.Int64Range(253, 1_000_000).Draw(rt, "cap"),
		)
		count := rapid.IntRange(1, 5).Draw(rt, "schedules")
		schedules := make([]InputSchedule, count)
		var start chainfee.SatPerKWeight
		for i := range schedules {
			rate := chainfee.SatPerKWeight(
				rapid.Int64Range(253, 1_000_000).
					Draw(rt, "start"),
			)
			start = max(start, min(rate, capRate))
			schedules[i] = InputSchedule{
				FeeSchedule: FeeSchedule{
					StartHeight:     100,
					StartingFeeRate: fn.Some(rate),
				},
				Budget: btcutil.Amount(
					rapid.Int64Range(0, 1_000_000).
						Draw(rt, "budget"),
				),
			}
		}
		deadline := 100 + width + 1
		f, err := newPooledFeeFunction(
			capRate, deadline, 100, schedules, nil,
		)
		require.NoError(rt, err)

		// Searches step the evaluation height ahead of the chain, and
		// elapsed time never moves it back.
		height, evaluated := int32(100), int32(100)
		ops := rapid.SliceOfN(rapid.Bool(), 0, 50).Draw(rt, "ops")
		for _, search := range ops {
			if search {
				_, err := f.Increment()
				if err != nil {
					require.ErrorIs(rt, err, ErrMaxPosition)
					require.Equal(rt, capRate, f.FeeRate())
				} else {
					evaluated++
				}
			} else {
				height++
				evaluated = max(evaluated, height)
				target := calcCurrentConfTarget(
					height, deadline,
				)
				_, err := f.IncreaseFeeRate(target)
				require.NoError(rt, err)
			}

			elapsed := min(1, float64(evaluated-100)/float64(width))
			want := float64(start) +
				float64(capRate-start)*elapsed
			require.InDelta(rt, want, float64(f.FeeRate()),
				0.5+1e-6)
		}
	})
}
