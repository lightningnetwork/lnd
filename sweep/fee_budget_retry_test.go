package sweep

import (
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestHighStartingFeeRateRetriesWithEstimator ensures an infeasible starting
// rate is cleared before the next sweep attempt.
func TestHighStartingFeeRateRetriesWithEstimator(t *testing.T) {
	tp, _ := createTestPublisher(t)
	req := createTestBumpRequest()
	start := chainfee.SatPerKWeight(1200)
	req.StartingFeeRate = fn.Some(start)
	r := &monitorRecord{requestID: 1, req: req}

	subscriber := make(chan *BumpResult, 1)
	tp.subscriberChans.Store(r.requestID, subscriber)
	tp.records.Store(r.requestID, r)
	_, err := NewLinearFeeFunction(
		1000, 6, &chainfee.MockEstimator{}, req.StartingFeeRate,
	)
	require.ErrorIs(t, err, errStartingFeeRateTooHigh)
	tp.handleInitialTxError(r, err)
	result := <-subscriber
	require.Equal(t, TxFailed, result.Event)

	s := New(&UtxoSweeperConfig{})
	inp := req.Inputs[0]
	op := inp.OutPoint()
	s.inputs[op] = &SweeperInput{
		Input: inp, state: PendingPublish,
		params: Params{StartingFeeRate: req.StartingFeeRate},
	}
	set := &MockInputSet{}
	set.On("Inputs").Return(req.Inputs).Once()

	err = s.handleBumpEvent(&bumpResp{set: set, result: result})
	require.NoError(t, err)
	require.Equal(t, PublishFailed, s.inputs[op].state)
	require.Equal(
		t, fn.None[chainfee.SatPerKWeight](),
		s.inputs[op].params.StartingFeeRate,
	)

	estimator := &chainfee.MockEstimator{}
	estimator.On(
		"EstimateFeePerKW", uint32(6),
	).Return(chainfee.SatPerKWeight(700), nil).Once()
	estimator.On("RelayFeePerKW").Return(
		chainfee.SatPerKWeight(250),
	).Once()
	fee, err := NewLinearFeeFunction(
		1000, 6, estimator, s.inputs[op].params.StartingFeeRate,
	)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(700), fee.FeeRate())
	estimator.AssertExpectations(t)
	set.AssertExpectations(t)
}
