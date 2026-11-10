package sweep

import (
	"math"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestLinearFeeFunctionNewMaxFeeRateUsed tests when the conf target is <= 1,
// the max fee rate is used.
func TestLinearFeeFunctionNewMaxFeeRateUsed(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create testing params.
	maxFeeRate := chainfee.SatPerKWeight(10000)
	noStartFeeRate := fn.None[chainfee.SatPerKWeight]()

	// Assert init fee function with zero conf value will end up using the
	// max fee rate.
	f, err := NewLinearFeeFunction(maxFeeRate, 0, estimator, noStartFeeRate)
	rt.NoError(err)
	rt.NotNil(f)

	// Assert the internal state.
	rt.Equal(maxFeeRate, f.startingFeeRate)
	rt.Equal(maxFeeRate, f.endingFeeRate)
	rt.Equal(maxFeeRate, f.currentFeeRate)

	// Assert init fee function with conf of one will end up using the max
	// fee rate.
	f, err = NewLinearFeeFunction(maxFeeRate, 1, estimator, noStartFeeRate)
	rt.NoError(err)
	rt.NotNil(f)

	// Assert the internal state.
	rt.Equal(maxFeeRate, f.startingFeeRate)
	rt.Equal(maxFeeRate, f.endingFeeRate)
	rt.Equal(maxFeeRate, f.currentFeeRate)
}

// TestLinearFeeFunctionNewStartAtMaxFeeRate tests that a fee function whose
// starting fee rate is at or above the max fee rate starts at the max fee rate
// and stays there, for any width.
func TestLinearFeeFunctionNewStartAtMaxFeeRate(t *testing.T) {
	t.Parallel()

	// Create testing params.
	maxFeeRate := chainfee.SatPerKWeight(10000)
	minRelayFeeRate := chainfee.SatPerKWeight(500)

	testCases := []struct {
		name       string
		confTarget uint32

		// start is the starting fee rate passed in by the caller.
		start fn.Option[chainfee.SatPerKWeight]

		// estimate and relayFee are the fee rates the fee estimator
		// returns for the conf target and as the min relay fee rate.
		// Only the ones that are set are mocked, so the others assert
		// that the fee estimator isn't asked for them.
		estimate fn.Option[chainfee.SatPerKWeight]
		relayFee fn.Option[chainfee.SatPerKWeight]
	}{
		{
			// When the budget can't pay more than the estimate for
			// the deadline, the estimate is capped at the max fee
			// rate, which leaves nothing to increase.
			name:       "estimate at max fee rate",
			confTarget: 6,
			estimate:   fn.Some(maxFeeRate),
			relayFee:   fn.Some(minRelayFeeRate),
		},
		{
			name:       "estimate above max fee rate",
			confTarget: 6,
			estimate:   fn.Some(maxFeeRate + 1),
			relayFee:   fn.Some(minRelayFeeRate),
		},
		{
			name:       "estimate above max fee rate, width one",
			confTarget: 2,
			estimate:   fn.Some(maxFeeRate + 1),
			relayFee:   fn.Some(minRelayFeeRate),
		},
		{
			// The sweeper passes in the max fee rate as the
			// starting fee rate when it retries inputs whose
			// budget has been used up.
			name:       "start at max fee rate",
			confTarget: 6,
			start:      fn.Some(maxFeeRate),
		},
		{
			// This holds up to the widest window a CSV delay
			// allows.
			name:       "start at max fee rate, widest window",
			confTarget: math.MaxUint16,
			start:      fn.Some(maxFeeRate),
		},
		{
			// A starting fee rate above the max fee rate is capped.
			name:       "start above max fee rate",
			confTarget: 6,
			start:      fn.Some(maxFeeRate + 1),
		},
		{
			// So is the min relay fee rate, which is used for conf
			// targets of at least 1008 blocks.
			name:       "min relay fee rate above max fee rate",
			confTarget: 1008,
			relayFee:   fn.Some(maxFeeRate + 1),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a mock fee estimator.
			estimator := &chainfee.MockEstimator{}
			defer estimator.AssertExpectations(t)

			tc.estimate.WhenSome(func(r chainfee.SatPerKWeight) {
				estimator.On(
					"EstimateFeePerKW", tc.confTarget,
				).Return(r, nil).Once()
			})
			tc.relayFee.WhenSome(func(r chainfee.SatPerKWeight) {
				estimator.On("RelayFeePerKW").Return(r).Once()
			})

			f, err := NewLinearFeeFunction(
				maxFeeRate, tc.confTarget, estimator, tc.start,
			)
			require.NoError(t, err)

			// Assert the internal state: the fee function starts
			// at the max fee rate, with nothing to increase.
			width := tc.confTarget - 1
			require.Equal(t, maxFeeRate, f.startingFeeRate)
			require.Equal(t, maxFeeRate, f.endingFeeRate)
			require.Equal(t, maxFeeRate, f.currentFeeRate)
			require.Zero(t, f.deltaFeeRate)
			require.Equal(t, width, f.width)

			// Raising the fee rate as the deadline approaches
			// doesn't change it, up to the last position.
			increased, err := f.IncreaseFeeRate(width)
			require.NoError(t, err)
			require.False(t, increased)
			require.Equal(t, maxFeeRate, f.FeeRate())

			increased, err = f.IncreaseFeeRate(1)
			require.NoError(t, err)
			require.False(t, increased)
			require.Equal(t, maxFeeRate, f.FeeRate())

			// From the last position, the fee rate can't be
			// incremented.
			increased, err = f.Increment()
			require.ErrorIs(t, err, ErrMaxPosition)
			require.False(t, increased)
		})
	}
}

// TestLinearFeeFunctionNewDeltaRoundedToZero tests that a difference between
// the starting and max fee rates that is too small to be spread over the width
// keeps the starting fee rate until the last position, which uses the max fee
// rate.
func TestLinearFeeFunctionNewDeltaRoundedToZero(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create testing params. An increase of one sat/kw over a width of
	// 3000 blocks is a third of a msat/kw per block, which is rounded to
	// zero.
	maxFeeRate := chainfee.SatPerKWeight(10000)
	startFeeRate := maxFeeRate - 1
	confTarget := uint32(3001)

	f, err := NewLinearFeeFunction(
		maxFeeRate, confTarget, estimator, fn.Some(startFeeRate),
	)
	rt.NoError(err)
	rt.Zero(f.deltaFeeRate)
	rt.Equal(startFeeRate, f.FeeRate())

	// The fee rate stays at the starting fee rate up to the last
	// position.
	increased, err := f.IncreaseFeeRate(2)
	rt.NoError(err)
	rt.False(increased)
	rt.Equal(startFeeRate, f.FeeRate())

	// The last position uses the max fee rate.
	increased, err = f.IncreaseFeeRate(1)
	rt.NoError(err)
	rt.True(increased)
	rt.Equal(maxFeeRate, f.FeeRate())
}

// TestLinearFeeFunctionNewEsimator tests the NewLinearFeeFunction function
// properly reacts to the response or error returned from the fee estimator.
func TestLinearFeeFunctionNewEsimator(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create testing params.
	maxFeeRate := chainfee.SatPerKWeight(10000)
	minRelayFeeRate := chainfee.SatPerKWeight(100)
	confTarget := uint32(6)
	noStartFeeRate := fn.None[chainfee.SatPerKWeight]()

	// When the fee estimator returns an error, it's returned.
	//
	// Mock the fee estimator to return an error.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		chainfee.SatPerKWeight(0), errDummy).Once()

	f, err := NewLinearFeeFunction(
		maxFeeRate, confTarget, estimator, noStartFeeRate,
	)
	rt.ErrorIs(err, errDummy)
	rt.Nil(f)

	// When the conf target is >= 1008, the min relay fee should be used.
	//
	// Mock the fee estimator to reutrn the fee rate.
	estimator.On("RelayFeePerKW").Return(minRelayFeeRate).Once()

	largeConf := uint32(1008)
	f, err = NewLinearFeeFunction(
		maxFeeRate, largeConf, estimator, noStartFeeRate,
	)
	rt.NoError(err)
	rt.NotNil(f)

	// Assert the internal state.
	rt.Equal(minRelayFeeRate, f.startingFeeRate)
	rt.Equal(maxFeeRate, f.endingFeeRate)
	rt.Equal(minRelayFeeRate, f.currentFeeRate)
	rt.NotZero(f.deltaFeeRate)
	rt.Equal(largeConf-1, f.width)
}

// TestLinearFeeFunctionNewSuccess tests we can create the fee function
// successfully.
func TestLinearFeeFunctionNewSuccess(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create testing params.
	maxFeeRate := chainfee.SatPerKWeight(10000)
	estimatedFeeRate := chainfee.SatPerKWeight(500)
	minRelayFeeRate := chainfee.SatPerKWeight(100)
	confTarget := uint32(6)
	noStartFeeRate := fn.None[chainfee.SatPerKWeight]()
	startFeeRate := chainfee.SatPerKWeight(1000)

	// Check a successfully created fee function.
	//
	// Mock the fee estimator to return the fee rate.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		estimatedFeeRate, nil).Once()
	estimator.On("RelayFeePerKW").Return(minRelayFeeRate).Once()

	f, err := NewLinearFeeFunction(
		maxFeeRate, confTarget, estimator, noStartFeeRate,
	)
	rt.NoError(err)
	rt.NotNil(f)

	// Assert the internal state.
	rt.Equal(estimatedFeeRate, f.startingFeeRate)
	rt.Equal(maxFeeRate, f.endingFeeRate)
	rt.Equal(estimatedFeeRate, f.currentFeeRate)
	rt.NotZero(f.deltaFeeRate)
	rt.Equal(confTarget-1, f.width)

	// Check a successfully created fee function using the specified
	// starting fee rate.
	//
	// NOTE: by NOT mocking the fee estimator, we assert the
	// estimateFeeRate is NOT called.
	f, err = NewLinearFeeFunction(
		maxFeeRate, confTarget, estimator, fn.Some(startFeeRate),
	)

	rt.NoError(err)
	rt.NotNil(f)

	// Assert the customized starting fee rate is used.
	rt.Equal(startFeeRate, f.startingFeeRate)
	rt.Equal(maxFeeRate, f.endingFeeRate)
	rt.Equal(startFeeRate, f.currentFeeRate)
	rt.NotZero(f.deltaFeeRate)
	rt.Equal(confTarget-1, f.width)
}

// TestLinearFeeFunctionFeeRateAtPosition checks the expected feerate is
// calculated and returned.
func TestLinearFeeFunctionFeeRateAtPosition(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a fee func which has three positions:
	// - position 0: 1000
	// - position 1: 2000
	// - position 2: 3000
	f := &LinearFeeFunction{
		startingFeeRate: 1000,
		endingFeeRate:   3000,
		position:        0,
		deltaFeeRate:    1_000_000,
		width:           3,
	}

	testCases := []struct {
		name            string
		pos             uint32
		expectedFeerate chainfee.SatPerKWeight
	}{
		{
			name:            "position 0",
			pos:             0,
			expectedFeerate: 1000,
		},
		{
			name:            "position 1",
			pos:             1,
			expectedFeerate: 2000,
		},
		{
			name:            "position 2",
			pos:             2,
			expectedFeerate: 3000,
		},
		{
			name:            "position 3",
			pos:             3,
			expectedFeerate: 3000,
		},
	}

	for _, tc := range testCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := f.feeRateAtPosition(tc.pos)
			rt.Equal(tc.expectedFeerate, result)
		})
	}
}

// TestLinearFeeFunctionIncrement checks the internal state is updated
// correctly when the fee rate is incremented.
func TestLinearFeeFunctionIncrement(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create testing params. These params are chosen so the delta value is
	// 100.
	maxFeeRate := chainfee.SatPerKWeight(900)
	estimatedFeeRate := chainfee.SatPerKWeight(100)
	confTarget := uint32(9) // This means the width is 8.

	// Mock the fee estimator to return the fee rate.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		estimatedFeeRate, nil).Once()
	estimator.On("RelayFeePerKW").Return(estimatedFeeRate).Once()

	f, err := NewLinearFeeFunction(
		maxFeeRate, confTarget, estimator,
		fn.None[chainfee.SatPerKWeight](),
	)
	rt.NoError(err)

	// We now increase the position from 1 to 8.
	for i := uint32(1); i <= confTarget-1; i++ {
		// Increase the fee rate.
		increased, err := f.Increment()
		rt.NoError(err)
		rt.True(increased)

		// Assert the internal state.
		rt.Equal(i, f.position)

		delta := chainfee.SatPerKWeight(i * 100)
		rt.Equal(estimatedFeeRate+delta, f.currentFeeRate)

		// Check public method returns the expected fee rate.
		rt.Equal(estimatedFeeRate+delta, f.FeeRate())
	}

	// Now the position is at 8th, increase it again should give us an
	// error.
	increased, err := f.Increment()
	rt.ErrorIs(err, ErrMaxPosition)
	rt.False(increased)
}

// TestLinearFeeFunctionIncrementSameFeeRate checks that incrementing a fee
// function skips the positions that can't change its fee rate, however far
// away its deadline is.
func TestLinearFeeFunctionIncrementSameFeeRate(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create testing params, with a deadline as far away as a deadline
	// height allows.
	maxFeeRate := chainfee.SatPerKWeight(10000)
	confTarget := uint32(math.MaxInt32)
	width := confTarget - 1

	// A fee function that starts at the max fee rate stays there, so the
	// first increment moves it to its last position, and the next one
	// fails.
	//
	// NOTE: by NOT mocking the fee estimator, we assert the
	// estimateFeeRate is NOT called.
	f, err := NewLinearFeeFunction(
		maxFeeRate, confTarget, estimator, fn.Some(maxFeeRate),
	)
	rt.NoError(err)

	increased, err := f.Increment()
	rt.NoError(err)
	rt.False(increased)
	rt.Equal(width, f.position)
	rt.Equal(maxFeeRate, f.FeeRate())

	increased, err = f.Increment()
	rt.ErrorIs(err, ErrMaxPosition)
	rt.False(increased)

	// The min relay fee rate used for such a deadline is too close to the
	// max fee rate to be spread over the width, so the delta is zero, and
	// the fee rate stays at the starting fee rate until the last position.
	// The first increment keeps it, the second one reaches the max fee
	// rate, and the third one fails.
	estimator.On("RelayFeePerKW").Return(chainfee.FeePerKwFloor).Once()

	f, err = NewLinearFeeFunction(
		maxFeeRate, confTarget, estimator,
		fn.None[chainfee.SatPerKWeight](),
	)
	rt.NoError(err)
	rt.Zero(f.deltaFeeRate)

	increased, err = f.Increment()
	rt.NoError(err)
	rt.False(increased)
	rt.Equal(width-1, f.position)
	rt.Equal(chainfee.FeePerKwFloor, f.FeeRate())

	increased, err = f.Increment()
	rt.NoError(err)
	rt.True(increased)
	rt.Equal(width, f.position)
	rt.Equal(maxFeeRate, f.FeeRate())

	increased, err = f.Increment()
	rt.ErrorIs(err, ErrMaxPosition)
	rt.False(increased)
}

// TestLinearFeeFunctionIncreaseFeeRate checks the internal state is updated
// correctly when the fee rate is increased using conf targets.
func TestLinearFeeFunctionIncreaseFeeRate(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create testing params. These params are chosen so the delta value is
	// 100.
	maxFeeRate := chainfee.SatPerKWeight(900)
	estimatedFeeRate := chainfee.SatPerKWeight(100)
	confTarget := uint32(9) // This means the width is 8.

	// Mock the fee estimator to return the fee rate.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		estimatedFeeRate, nil).Once()
	estimator.On("RelayFeePerKW").Return(estimatedFeeRate).Once()

	f, err := NewLinearFeeFunction(
		maxFeeRate, confTarget, estimator,
		fn.None[chainfee.SatPerKWeight](),
	)
	rt.NoError(err)

	// If we are increasing the fee rate using the initial conf target, we
	// should get a nil error and false.
	increased, err := f.IncreaseFeeRate(confTarget)
	rt.NoError(err)
	rt.False(increased)

	// Test that we are allowed to use a larger conf target.
	increased, err = f.IncreaseFeeRate(confTarget + 1)
	rt.NoError(err)
	rt.False(increased)

	// We now increase the fee rate from conf target 8 to 2 and assert we
	// get no error and true.
	for i := uint32(1); i < confTarget-1; i++ {
		// Increase the fee rate.
		increased, err := f.IncreaseFeeRate(confTarget - i)
		rt.NoError(err)
		rt.True(increased)

		// Assert the internal state.
		rt.Equal(i, f.position)

		delta := chainfee.SatPerKWeight(i * 100)
		rt.Equal(estimatedFeeRate+delta, f.currentFeeRate)

		// Check public method returns the expected fee rate.
		rt.Equal(estimatedFeeRate+delta, f.FeeRate())
	}

	// Test that when we use a conf target of 1, we get the ending fee
	// rate.
	increased, err = f.IncreaseFeeRate(1)
	rt.NoError(err)
	rt.True(increased)
	rt.Equal(confTarget-1, f.position)
	rt.Equal(maxFeeRate, f.currentFeeRate)

	// Test that when we use a conf target of 0, ErrMaxPosition is
	// returned.
	increased, err = f.IncreaseFeeRate(0)
	rt.ErrorIs(err, ErrMaxPosition)
	rt.False(increased)
}
