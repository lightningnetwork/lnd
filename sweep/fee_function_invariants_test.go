package sweep

import (
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

func TestLinearFeeFunctionBudgetInvariant(t *testing.T) {
	maxRate := chainfee.SatPerKWeight(1000)
	estimator := &chainfee.MockEstimator{}
	_, err := NewLinearFeeFunction(
		maxRate, 3, estimator, fn.Some(maxRate+1),
	)
	require.ErrorIs(t, err, errStartingFeeRateTooHigh)

	f, err := NewLinearFeeFunction(
		maxRate, 5, estimator, fn.Some(chainfee.SatPerKWeight(500)),
	)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(500), f.FeeRate())
	for range 4 {
		previous := f.FeeRate()
		_, err := f.Increment()
		require.NoError(t, err)
		require.GreaterOrEqual(t, f.FeeRate(), previous)
		require.LessOrEqual(t, f.FeeRate(), maxRate)
	}
	require.Equal(t, maxRate, f.FeeRate())

	// At the deadline the budget maximum is used even with a higher hint.
	f, err = NewLinearFeeFunction(
		maxRate, 1, estimator, fn.Some(maxRate+1),
	)
	require.NoError(t, err)
	require.Equal(t, maxRate, f.FeeRate())
}

func TestStartingFeeRateTooHighIsRetryable(t *testing.T) {
	tp, _ := createTestPublisher(t)
	r := &monitorRecord{requestID: 1}
	subscriber := make(chan *BumpResult, 1)
	tp.subscriberChans.Store(r.requestID, subscriber)
	tp.records.Store(r.requestID, r)
	tp.handleInitialTxError(r, errStartingFeeRateTooHigh)
	result := <-subscriber
	require.Equal(t, TxFailed, result.Event)
	require.ErrorIs(t, result.Err, errStartingFeeRateTooHigh)
}

func TestLinearFeeFunctionRelayAboveBudget(t *testing.T) {
	estimator := &chainfee.MockEstimator{}
	estimator.On("RelayFeePerKW").Return(
		chainfee.SatPerKWeight(1000),
	).Once()
	_, err := NewLinearFeeFunction(
		500, chainfee.MaxBlockTarget, estimator,
		fn.None[chainfee.SatPerKWeight](),
	)
	require.ErrorIs(t, err, ErrZeroFeeRateDelta)
	estimator.AssertExpectations(t)
}
