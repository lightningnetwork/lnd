package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestFailedAttemptPreservesPolicy exercises the publisher/sweeper boundary:
// errors from candidate construction must not become input fee requirements.
func TestFailedAttemptPreservesPolicy(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		tp, _ := createTestPublisher(t)
		s := New(&UtxoSweeperConfig{})
		inp := createMockInput(t, s, PendingPublish)
		pi := s.inputs[inp.OutPoint()]
		prior := fn.None[chainfee.SatPerKWeight]()
		if rapid.Bool().Draw(rt, "requested") {
			prior = fn.Some(chainfee.SatPerKWeight(
				rapid.Int64Range(1, 100000).Draw(rt, "rate"),
			))
		}
		pi.params.StartingFeeRate = prior
		published := chainfee.SatPerKWeight(
			rapid.Int64Range(0, 100000).Draw(rt, "published"),
		)
		pi.lastFeeRate = published
		set := &MockInputSet{}
		set.On("Inputs").Return([]input.Input{inp})

		for _, failure := range []error{
			ErrNotEnoughInputs, ErrNotEnoughBudget, ErrTxNoOutput,
			ErrMaxPosition,
		} {
			pi.state = PendingPublish
			ch := make(chan *BumpResult, 1)
			tp.subscriberChans.Store(uint64(1), ch)
			tp.handleInitialTxError(&monitorRecord{
				requestID: 1,
			}, failure)
			result := <-ch
			require.Equal(rt, TxFailed, result.Event)
			s.handleBumpEventTxFailed(&bumpResp{
				set: set, result: result,
			})
			require.Equal(rt, PublishFailed, pi.state)
			require.Equal(rt, prior, pi.params.StartingFeeRate)
			require.Equal(rt, published, pi.lastFeeRate)
		}
	})
}

// TestUnpublishedReplacementAtCap verifies that exhausting the fee schedule
// does not prevent retrying a higher candidate that has not been published.
func TestUnpublishedReplacementAtCap(t *testing.T) {
	t.Parallel()

	tp, m := createTestPublisher(t)
	inp := createTestInput(100000, input.WitnessKeyHash)
	req := &BumpRequest{
		Inputs:          []input.Input{&inp},
		DeliveryAddress: changePkScript,
		Budget:          10000,
		MaxFeeRate:      3000,
		DeadlineHeight:  3,
	}
	f, err := newPooledFeeFunction(
		3000, 3, 0, []InputSchedule{{FeeSchedule: FeeSchedule{
			StartingFeeRate: fn.Some(chainfee.SatPerKWeight(1000)),
		}}}, m.estimator,
	)
	require.NoError(t, err)
	_, err = f.IncreaseFeeRate(1)
	require.NoError(t, err)
	record := &monitorRecord{
		requestID: 1, req: req, feeFunction: f,
		tx: &wire.MsgTx{Version: 2}, feeRate: 1000,
	}
	tp.records.Store(uint64(1), record)
	ch := make(chan *BumpResult, 1)
	tp.subscriberChans.Store(uint64(1), ch)
	m.signer.On("ComputeInputScript", mock.Anything, mock.Anything).
		Return(&input.Script{}, nil)
	m.wallet.On("CheckMempoolAcceptance", mock.Anything).Return(nil).Once()
	m.wallet.On("PublishTransaction", mock.Anything, mock.Anything).
		Return(nil).Once()
	tp.wg.Add(1)
	tp.handleFeeBumpTx(record, 2)
	result := <-ch
	require.Equal(t, TxReplaced, result.Event)
	require.EqualValues(t, 3000, result.FeeRate)
	require.Same(t, record.tx, result.ReplacedTx)
}
