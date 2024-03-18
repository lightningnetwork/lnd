package sweep

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestLinearFeeFunctionNew tests the NewLinearFeeFunction function.
func TestLinearFeeFunctionNew(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}

	// Create testing params.
	maxFeeRate := chainfee.SatPerKWeight(10000)
	estimatedFeeRate := chainfee.SatPerKWeight(500)
	confTarget := uint32(6)

	// Assert init fee function with zero conf value returns an error.
	f, err := NewLinearFeeFunction(maxFeeRate, 0, estimator)
	rt.ErrorContains(err, "width must be greater than zero")
	rt.Nil(f)

	// When the fee estimator returns an error, it's returned.
	//
	// Mock the fee estimator to return an error.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		chainfee.SatPerKWeight(0), errDummy).Once()

	f, err = NewLinearFeeFunction(maxFeeRate, confTarget, estimator)
	rt.ErrorIs(err, errDummy)
	rt.Nil(f)

	// When the starting feerate is greater than the ending feerate, the
	// starting feerate is capped.
	//
	// Mock the fee estimator to return the fee rate.
	smallConf := uint32(1)
	estimator.On("EstimateFeePerKW", smallConf).Return(
		// The fee rate is greater than the max fee rate.
		maxFeeRate+1, nil).Once()
	estimator.On("RelayFeePerKW").Return(estimatedFeeRate).Once()

	f, err = NewLinearFeeFunction(maxFeeRate, smallConf, estimator)
	rt.NoError(err)
	rt.NotNil(f)

	// When the calculated fee rate delta is 0, an error should be returned.
	//
	// Mock the fee estimator to return the fee rate.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		// The starting fee rate is the max fee rate.
		maxFeeRate, nil).Once()
	estimator.On("RelayFeePerKW").Return(estimatedFeeRate).Once()

	f, err = NewLinearFeeFunction(maxFeeRate, confTarget, estimator)
	rt.ErrorContains(err, "fee rate delta is zero")
	rt.Nil(f)

	// Check a successfully created fee function.
	//
	// Mock the fee estimator to return the fee rate.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		estimatedFeeRate, nil).Once()
	estimator.On("RelayFeePerKW").Return(estimatedFeeRate).Once()

	f, err = NewLinearFeeFunction(maxFeeRate, confTarget, estimator)
	rt.NoError(err)
	rt.NotNil(f)

	// Assert the internal state.
	rt.Equal(estimatedFeeRate, f.startingFeeRate)
	rt.Equal(maxFeeRate, f.endingFeeRate)
	rt.Equal(estimatedFeeRate, f.currentFeeRate)
	rt.NotZero(f.deltaFeeRate)
	rt.Equal(confTarget, f.width)
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
		tc := tc

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

	// Create testing params. These params are chosen so the delta value is
	// 100.
	maxFeeRate := chainfee.SatPerKWeight(1000)
	estimatedFeeRate := chainfee.SatPerKWeight(100)
	confTarget := uint32(9)

	// Mock the fee estimator to return the fee rate.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		estimatedFeeRate, nil).Once()
	estimator.On("RelayFeePerKW").Return(estimatedFeeRate).Once()

	f, err := NewLinearFeeFunction(maxFeeRate, confTarget, estimator)
	rt.NoError(err)

	// We now increase the position from 1 to 9.
	for i := uint32(1); i <= confTarget; i++ {
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

	// Now the position is at 9th, increase it again should give us an
	// error.
	increased, err := f.Increment()
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

	// Create testing params. These params are chosen so the delta value is
	// 100.
	maxFeeRate := chainfee.SatPerKWeight(1000)
	estimatedFeeRate := chainfee.SatPerKWeight(100)
	confTarget := uint32(9)

	// Mock the fee estimator to return the fee rate.
	estimator.On("EstimateFeePerKW", confTarget).Return(
		estimatedFeeRate, nil).Once()
	estimator.On("RelayFeePerKW").Return(estimatedFeeRate).Once()

	f, err := NewLinearFeeFunction(maxFeeRate, confTarget, estimator)
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

	// Test that when we use a conf target of 0, we get an error.
	increased, err = f.IncreaseFeeRate(0)
	rt.ErrorIs(err, ErrMaxPosition)
	rt.False(increased)

	// We now increase the fee rate from conf target 8 to 1 and assert we
	// get no error and true.
	for i := uint32(1); i < confTarget; i++ {
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
}
