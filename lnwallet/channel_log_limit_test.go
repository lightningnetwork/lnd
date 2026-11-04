package lnwallet

import (
	"math"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func TestRevokeRejectsTooManyUnsignedAckedUpdates(t *testing.T) {
	channel, _, err := CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	for i := 0; i <= math.MaxUint16; i++ {
		channel.updateLogs.Remote.appendUpdate(&paymentDescriptor{
			EntryType: FeeUpdate,
			LogIndex:  uint64(i),
			Amount:    1000,
		})
	}

	channel.commitChains.Local.addCommitment(&commitment{
		messageIndices: lntypes.Dual[uint64]{
			Remote: math.MaxUint16 + 1,
		},
	})

	oldTail := channel.commitChains.Local.tail()
	oldHeight := channel.currentHeight
	_, _, _, err = channel.RevokeCurrentCommitment()
	require.ErrorContains(t, err, "too many unsigned acked updates")
	require.Same(t, oldTail, channel.commitChains.Local.tail())
	require.Equal(t, oldHeight, channel.currentHeight)
}

func TestRevokeAllowsMaxUnsignedAckedUpdates(t *testing.T) {
	channel, _, err := CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	// Fill the acked update window to the largest list the on-disk uint16
	// count can express. The revocation must still go through.
	for i := range math.MaxUint16 {
		channel.updateLogs.Remote.appendUpdate(&paymentDescriptor{
			EntryType: FeeUpdate,
			LogIndex:  uint64(i),
			Amount:    1000,
		})
	}

	channel.commitChains.Local.addCommitment(&commitment{
		txn: wire.NewMsgTx(2),
		messageIndices: lntypes.Dual[uint64]{
			Remote: math.MaxUint16,
		},
	})

	_, _, _, err = channel.RevokeCurrentCommitment()
	require.NoError(t, err)
}
