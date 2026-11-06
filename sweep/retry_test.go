package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/chainntnfs"
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

func TestPublicationHistory(t *testing.T) {
	t.Parallel()

	store := NewMockSweeperStore()
	s := New(&UtxoSweeperConfig{Store: store})
	inp := createMockInput(t, s, PendingPublish)
	pi := s.inputs[inp.OutPoint()]
	pi.params.StartingFeeRate = fn.Some(chainfee.SatPerKWeight(250))
	set := &MockInputSet{}
	set.On("Inputs").Return([]input.Input{inp})

	for _, rate := range []uint64{500, 1000} {
		record := &TxRecord{FeeRate: rate, Fee: rate}
		store.On("StoreTx", record).Return(nil).Once()
		require.NoError(t, s.markInputsPublished(record, set))
		require.EqualValues(t, rate, pi.lastFeeRate)
		require.EqualValues(t, rate, pi.rbf.UnwrapOrFail(t).FeeRate)
		require.EqualValues(t, 250,
			pi.params.StartingFeeRate.UnwrapOrFail(t))
	}
	s.markInputsPublishFailed(set)
	require.EqualValues(t, 1000, pi.lastFeeRate)
	store.AssertExpectations(t)
}

// TestPublicationFailureReturnsToSweeper covers publication-time fee rejection
// with no preflight. Each invocation publishes once and releases the inputs for
// retry; elapsed time, rather than rejection count, advances their schedule.
func TestPublicationFailureReturnsToSweeper(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		immediate bool
		previous  chainfee.SatPerKWeight
	}{
		{"immediate", true, 0},
		{"deferred", false, 0},
		{"immediate with published floor", true, 8000},
		{"deferred with published floor", false, 8000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp, m := createTestPublisher(t)
			tp.cfg.AuxSweeper = fn.None[AuxSweeper]()
			s := New(&UtxoSweeperConfig{})
			s.currentHeight = 100
			if !tc.immediate {
				s.currentHeight--
			}
			inp := createTestInput(100000, input.WitnessKeyHash)
			params := Params{
				Budget: 10000, Immediate: tc.immediate,
				StartingFeeRate: fn.Some(
					chainfee.SatPerKWeight(1000),
				),
			}
			pi := &SweeperInput{
				Input:          &inp,
				params:         params,
				DeadlineHeight: 1101,
				lastFeeRate:    tc.previous,
			}
			if tc.previous != 0 {
				pi.rbf = fn.Some(RBFInfo{FeeRate: tc.previous})
			}
			history := pi.rbf
			s.inputs[inp.OutPoint()] = pi
			s.startFeeSchedule(pi)
			set := &MockInputSet{}
			set.On("Inputs").Return([]input.Input{&inp})
			m.signer.On("ComputeInputScript", mock.Anything,
				mock.Anything).Return(&input.Script{}, nil)
			m.wallet.On("CheckMempoolAcceptance", mock.Anything).
				Return(chain.ErrUnimplemented)
			if !tc.immediate {
				spends := make(chan *chainntnfs.SpendDetail)
				m.notifier.On(
					"RegisterSpendNtfn", mock.Anything,
					mock.Anything, mock.Anything,
				).Return(&chainntnfs.SpendEvent{
					Spend: spends, Cancel: func() {},
				}, nil)
			}

			attempts := []struct {
				height int32
				rate   chainfee.SatPerKWeight
			}{
				{100, 1000}, {100, 1000}, {101, 1010},
				{800, 8000}, {801, 8010}, {1100, 11000},
				{1101, 11000},
			}
			for i, attempt := range attempts {
				s.currentHeight = attempt.height
				tp.currentHeight.Store(attempt.height)
				pi.state = PendingPublish
				req := &BumpRequest{
					Inputs:          []input.Input{&inp},
					DeliveryAddress: changePkScript,
					Budget:          params.Budget,
					MaxFeeRate:      11000,
					DeadlineHeight:  pi.DeadlineHeight,
					Immediate:       tc.immediate,
				}
				require.NoError(t, s.attachFeeContext(req))
				var publishErr error = chain.ErrInsufficientFee
				if i == len(attempts)-1 {
					publishErr = nil
				}
				m.wallet.On("PublishTransaction", mock.Anything,
					mock.Anything).Return(publishErr).Once()
				ch := tp.Broadcast(req)
				if !tc.immediate {
					tp.processRecords()
				}
				result := <-ch
				m.wallet.AssertNumberOfCalls(t,
					"PublishTransaction", i+1)
				require.Equal(t, max(attempt.rate, tc.previous),
					result.FeeRate)
				require.ErrorIs(t, result.Err, publishErr)
				require.Equal(t, params, pi.params)
				require.Equal(t, params.StartingFeeRate,
					pi.feeSchedule.StartingFeeRate)
				require.EqualValues(t, 100,
					pi.feeSchedule.StartHeight)
				require.Equal(t, tc.previous, pi.lastFeeRate)
				require.Equal(t, history, pi.rbf)
				if publishErr == nil {
					require.Equal(
						t, TxPublished, result.Event,
					)

					continue
				}
				require.Equal(t, TxFailed, result.Event)
				require.Zero(t, tp.records.Len())
				require.Zero(t, tp.subscriberChans.Len())
				s.handleBumpEventTxFailed(&bumpResp{
					set: set, result: result,
				})
				require.Equal(t, PublishFailed, pi.state)
				require.Contains(t, s.updateSweeperInputs(),
					inp.OutPoint())
			}
		})
	}
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
