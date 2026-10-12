package sweep

import (
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
