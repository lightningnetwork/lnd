package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
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

// TestUnknownSpendRetriesWithEstimatedFee checks an infeasible starting hint
// does not prevent retrying the remaining inputs.
func TestUnknownSpendRetriesWithEstimatedFee(t *testing.T) {
	tp, mocks := createTestPublisher(t)
	first := createTestInput(1000000, input.WitnessKeyHash)
	second := createTestInput(1000000, input.WitnessKeyHash)
	start := chainfee.SatPerKWeight(1200)
	req := &BumpRequest{
		Inputs:          []input.Input{&first, &second},
		DeliveryAddress: changePkScript,
		Budget:          10000,
		MaxFeeRate:      1000,
		DeadlineHeight:  6,
		StartingFeeRate: fn.Some(start),
	}
	spend := &wire.MsgTx{}
	spent := map[wire.OutPoint]*wire.MsgTx{first.OutPoint(): spend}
	r := &monitorRecord{requestID: 1, req: req, spentInputs: spent}
	mocks.estimator.On("EstimateFeePerKW", uint32(6)).Return(
		chainfee.SatPerKWeight(700), nil,
	).Once()
	mocks.estimator.On("RelayFeePerKW").Return(
		chainfee.SatPerKWeight(250),
	).Once()

	result := tp.createUnknownSpentBumpResult(r)
	require.Equal(t, TxUnknownSpend, result.Event)
	require.ErrorIs(t, result.Err, ErrUnknownSpent)
	require.Equal(t, spent, result.SpentInputs)
	require.Equal(t, chainfee.SatPerKWeight(760), result.FeeRate)
	require.Equal(t, fn.Some(start), req.StartingFeeRate)
	require.Nil(t, r.feeFunction)

	// Other estimator failures remain fatal and are not retried here.
	tp2, mocks2 := createTestPublisher(t)
	otherReq := *req
	otherReq.StartingFeeRate = fn.None[chainfee.SatPerKWeight]()
	other := &monitorRecord{req: &otherReq, spentInputs: spent}
	mocks2.estimator.On("EstimateFeePerKW", uint32(6)).Return(
		chainfee.SatPerKWeight(0), errDummy,
	).Once()
	failure := tp2.createUnknownSpentBumpResult(other)
	require.Equal(t, TxFatal, failure.Event)
	require.ErrorIs(t, failure.Err, errDummy)
}
