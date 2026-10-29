package sweep

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestSpendCheckRetryPreservesStartingFeeRate ensures an inconclusive spend
// check does not overwrite the input's fee-rate preference.
func TestSpendCheckRetryPreservesStartingFeeRate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		starting fn.Option[chainfee.SatPerKWeight]
	}{
		{
			"explicit starting rate",
			fn.Some(chainfee.SatPerKWeight(1200)),
		},
		{
			"estimated starting rate",
			fn.None[chainfee.SatPerKWeight](),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp, mocks := createTestPublisher(t)
			req := createTestBumpRequest()
			req.StartingFeeRate = tc.starting
			r := &monitorRecord{requestID: 1, req: req}

			op := req.Inputs[0].OutPoint()
			mocks.notifier.On(
				"RegisterSpendNtfn", &op, mock.Anything,
				mock.Anything,
			).Return(nil, errors.New("unavailable")).Once()

			result := tp.handleMissingInputs(r)
			require.Equal(t, TxFailed, result.Event)

			s := New(&UtxoSweeperConfig{})
			inp := req.Inputs[0]
			s.inputs[op] = &SweeperInput{
				Input: inp, state: PendingPublish,
				params: Params{StartingFeeRate: tc.starting},
			}
			set := &MockInputSet{}
			set.On("Inputs").Return(req.Inputs).Once()

			err := s.handleBumpEvent(&bumpResp{
				set: set, result: result,
			})
			require.NoError(t, err)
			require.Equal(t, PublishFailed, s.inputs[op].state)
			require.Equal(
				t, tc.starting,
				s.inputs[op].params.StartingFeeRate,
			)
			set.AssertExpectations(t)
		})
	}
}

// TestRetryUpdatesSpecifiedFeeRate ensures ordinary retry results still update
// the starting fee rate supplied by the fee bumper.
func TestRetryUpdatesSpecifiedFeeRate(t *testing.T) {
	s := New(&UtxoSweeperConfig{})
	inp := createMockInput(t, s, PendingPublish)
	op := inp.OutPoint()
	s.inputs[op].params.StartingFeeRate = fn.Some(
		chainfee.SatPerKWeight(500),
	)
	set := &MockInputSet{}
	set.On("Inputs").Return([]input.Input{inp}).Once()

	err := s.handleBumpEvent(&bumpResp{
		set: set, result: &BumpResult{
			Event: TxFailed, Err: errDummy, FeeRate: 900,
		},
	})
	require.NoError(t, err)
	require.Equal(
		t, fn.Some(chainfee.SatPerKWeight(900)),
		s.inputs[op].params.StartingFeeRate,
	)
	set.AssertExpectations(t)
}
