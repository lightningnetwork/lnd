package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestAppendFeeUpdateReplacementSequences checks replacement behavior across
// generated sequences of fee and commitment state transitions.
func TestAppendFeeUpdateReplacementSequences(t *testing.T) {
	t.Parallel()

	type feeAction struct {
		fee          uint32
		commitLocal  bool
		commitRemote bool
		interleave   bool
	}

	actions := rapid.SliceOfN(
		rapid.Custom(func(t *rapid.T) feeAction {
			return feeAction{
				fee: rapid.Uint32Range(1, 10_000_000).Draw(
					t, "fee",
				),
				commitLocal: rapid.Bool().Draw(
					t, "commit_local",
				),
				commitRemote: rapid.Bool().Draw(
					t, "commit_remote",
				),
				interleave: rapid.Bool().Draw(
					t, "interleave",
				),
			}
		}), 1, 200,
	)

	rapid.Check(t, func(t *rapid.T) {
		log := newUpdateLog(0, 0)
		committed := make(map[*paymentDescriptor]struct{})

		for i, action := range actions.Draw(t, "actions") {
			if action.interleave {
				log.appendUpdate(&paymentDescriptor{
					LogIndex:  log.logIndex,
					EntryType: Settle,
				})
			}

			feeUpdate := &paymentDescriptor{
				LogIndex: log.logIndex,
				Amount: lnwire.NewMSatFromSatoshis(
					btcutil.Amount(action.fee),
				),
				EntryType: FeeUpdate,
			}
			log.appendFeeUpdate(feeUpdate)

			var currentFee *paymentDescriptor
			entry := log.Back()
			for entry != nil {
				if entry.Value.EntryType == FeeUpdate {
					currentFee = entry.Value
					break
				}

				entry = entry.Prev()
			}
			if currentFee == nil {
				t.Fatal("fee update not retained")
			}
			if currentFee.Amount != feeUpdate.Amount {
				t.Fatalf("latest fee is %v, expected %v",
					currentFee.Amount, feeUpdate.Amount)
			}

			if action.commitLocal {
				currentFee.setCommitHeight(
					lntypes.Local, uint64(i+1),
				)
			}
			if action.commitRemote {
				currentFee.setCommitHeight(
					lntypes.Remote, uint64(i+1),
				)
			}
			if action.commitLocal || action.commitRemote {
				committed[currentFee] = struct{}{}
			}

			var uncommitted int
			resident := make(map[*paymentDescriptor]struct{})
			var nextLogIndex uint64
			entry = log.Front()
			for entry != nil {
				update := entry.Value
				resident[update] = struct{}{}
				if update.LogIndex != nextLogIndex {
					t.Fatalf(
						"non-contiguous log index: "+
							"got %d, want %d",
						update.LogIndex, nextLogIndex,
					)
				}
				nextLogIndex++

				if update.EntryType == FeeUpdate &&
					update.addCommitHeights.Local == 0 &&
					update.addCommitHeights.Remote == 0 {

					uncommitted++
				}

				entry = entry.Next()
			}
			if log.logIndex != nextLogIndex {
				t.Fatalf("log index is %d, expected %d",
					log.logIndex, nextLogIndex)
			}

			if uncommitted > 1 {
				t.Fatalf("retained %d uncommitted fee updates",
					uncommitted)
			}
			for update := range committed {
				if _, ok := resident[update]; !ok {
					t.Fatal("committed fee update removed")
				}
			}
		}
	})
}

// TestReceiveUpdateFeeReplacement checks that consecutive fee updates retain
// the latest value until a commitment chain observes the update.
func TestReceiveUpdateFeeReplacement(t *testing.T) {
	t.Parallel()

	_, bobChannel, err := CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	const numUpdates = 10_000
	for i := 1; i <= numUpdates; i++ {
		require.NoError(
			t, bobChannel.ReceiveUpdateFee(
				chainfee.SatPerKWeight(i),
			),
		)
	}

	require.Equal(t, uint64(1), bobChannel.updateLogs.Remote.logIndex)

	feeUpdates := make([]*paymentDescriptor, 0, 1)
	entry := bobChannel.updateLogs.Remote.Front()
	for entry != nil {
		if entry.Value.EntryType == FeeUpdate {
			feeUpdates = append(feeUpdates, entry.Value)
		}

		entry = entry.Next()
	}

	require.Len(t, feeUpdates, 1)
	require.Equal(
		t, int64(numUpdates), int64(feeUpdates[0].Amount.ToSatoshis()),
	)
	require.Zero(t, feeUpdates[0].LogIndex)
}

// TestAppendFeeUpdatePreservesCommitted checks that a fee update observed by
// either commitment chain isn't replaced by a later update.
func TestAppendFeeUpdatePreservesCommitted(t *testing.T) {
	t.Parallel()

	log := newUpdateLog(0, 0)
	first := &paymentDescriptor{
		LogIndex:  log.logIndex,
		EntryType: FeeUpdate,
	}
	log.appendFeeUpdate(first)
	first.setCommitHeight(lntypes.Remote, 1)

	second := &paymentDescriptor{
		LogIndex:  log.logIndex,
		Amount:    2,
		EntryType: FeeUpdate,
	}
	log.appendFeeUpdate(second)

	third := &paymentDescriptor{
		LogIndex:  log.logIndex,
		Amount:    3,
		EntryType: FeeUpdate,
	}
	log.appendFeeUpdate(third)

	require.Same(t, first, log.Front().Value)
	require.Same(t, second, log.Back().Value)
	require.Equal(t, third.Amount, second.Amount)
	require.Equal(t, uint64(2), log.logIndex)
	require.Contains(t, log.updateIndex, first.LogIndex)
	require.Contains(t, log.updateIndex, second.LogIndex)
	require.NotContains(t, log.updateIndex, third.LogIndex)
}
