package chanstate

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func createTestChannelAtHeight(t *testing.T, store *KVStore,
	height uint32, opts ...testChannelOption) *OpenChannel {

	t.Helper()

	channel := createTestChannelState(t, store)
	for _, option := range opts {
		option(channel)
	}

	err := kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(outpointBucket)
		if err != nil {
			return err
		}

		_, err = tx.CreateTopLevelBucket(chanIDBucket)
		if err != nil {
			return err
		}

		return SyncPendingOpenChannel(tx, channel, height)
	}, func() {})
	require.NoError(t, err, "unable to save channel state")

	return channel
}

// TestOpeningChannelTxConfirmation verifies that calling
// MarkConfirmationHeight correctly updates the confirmed state. It also
// ensures that calling Refresh on a different OpenChannel updates its
// in-memory state to reflect the prior MarkConfirmationHeight call.
func TestOpeningChannelTxConfirmation(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	const broadcastHeight = uint32(99)
	channelState := createTestChannelAtHeight(
		t, store, broadcastHeight,
	)

	pendingChannels, err := store.FetchPendingChannels()
	require.NoError(t, err)
	require.Len(t, pendingChannels, 1)
	require.Equal(
		t, broadcastHeight, pendingChannels[0].FundingBroadcastHeight,
	)

	confirmationHeight := broadcastHeight + 1
	err = pendingChannels[0].MarkConfirmationHeight(confirmationHeight)
	require.NoError(t, err)
	require.Equal(
		t, confirmationHeight, pendingChannels[0].ConfirmationHeight,
	)

	pendingChannels, err = store.FetchPendingChannels()
	require.NoError(t, err)
	require.Len(t, pendingChannels, 1)
	require.Equal(
		t, confirmationHeight, pendingChannels[0].ConfirmationHeight,
	)
	require.Equal(
		t, broadcastHeight, pendingChannels[0].FundingBroadcastHeight,
	)

	require.Zero(t, channelState.ConfirmationHeight)

	err = channelState.Refresh()
	require.NoError(t, err)
	require.Equal(
		t, pendingChannels[0].ConfirmationHeight,
		channelState.ConfirmationHeight,
	)
}

func TestFetchPendingChannels(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	const broadcastHeight = uint32(99)
	createTestChannelAtHeight(t, store, broadcastHeight)

	pendingChannels, err := store.FetchPendingChannels()
	require.NoError(t, err, "unable to list pending channels")
	require.Len(t, pendingChannels, 1)
	require.Equal(
		t, broadcastHeight, pendingChannels[0].FundingBroadcastHeight,
	)

	chanOpenLoc := lnwire.ShortChannelID{
		BlockHeight: broadcastHeight + 1,
		TxIndex:     10,
		TxPosition:  15,
	}
	err = pendingChannels[0].MarkAsOpen(chanOpenLoc)
	require.NoError(t, err, "unable to mark channel as open")
	require.False(t, pendingChannels[0].IsPending)
	require.Equal(t, chanOpenLoc, pendingChannels[0].ShortChanID())

	openChans, err := store.FetchAllChannels()
	require.NoError(t, err, "unable to fetch channels")
	require.Equal(t, chanOpenLoc, openChans[0].ShortChanID())
	require.Equal(
		t, broadcastHeight, openChans[0].FundingBroadcastHeight,
	)

	pendingChannels, err = store.FetchPendingChannels()
	require.NoError(t, err, "unable to list pending channels")
	require.Empty(t, pendingChannels)
}

// TestFetchWaitingCloseChannels ensures that the correct channels that are
// waiting to be closed are returned.
func TestFetchWaitingCloseChannels(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	const (
		numChannels     = 2
		broadcastHeight = uint32(99)
	)

	channels := make([]*OpenChannel, numChannels)
	for i := range numChannels {
		channels[i] = createTestChannelAtHeight(
			t, store, broadcastHeight,
		)
	}

	channelConf := lnwire.ShortChannelID{
		BlockHeight: broadcastHeight + 1,
		TxIndex:     10,
		TxPosition:  15,
	}
	err := channels[0].MarkAsOpen(channelConf)
	require.NoError(t, err, "unable to mark channel as open")

	for _, channel := range channels {
		closeTx := wire.NewMsgTx(2)
		closeTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: channel.FundingOutpoint,
		})

		err := channel.MarkCommitmentBroadcasted(
			closeTx, lntypes.Local,
		)
		require.NoError(t, err, "unable to mark commitment broadcast")

		err = channel.MarkCoopBroadcasted(nil, lntypes.Local)
		require.Error(t, err, "nil tx should be rejected")

		closeTx.TxIn[0].PreviousOutPoint.Index ^= 1
		err = channel.MarkCoopBroadcasted(closeTx, lntypes.Local)
		require.NoError(t, err, "unable to mark coop broadcast")
	}

	waitingCloseChannels, err := store.FetchWaitingCloseChannels()
	require.NoError(t, err, "unable to fetch waiting close channels")
	require.Len(t, waitingCloseChannels, numChannels)

	expectedChannels := make(map[wire.OutPoint]struct{})
	for _, channel := range channels {
		expectedChannels[channel.FundingOutpoint] = struct{}{}
	}

	for _, channel := range waitingCloseChannels {
		_, ok := expectedChannels[channel.FundingOutpoint]
		require.Truef(
			t, ok, "expected channel %v to be waiting close",
			channel.FundingOutpoint,
		)

		chanPoint := channel.FundingOutpoint

		forceCloseTx, err := channel.BroadcastedCommitment()
		require.NoError(t, err, "unable to retrieve commitment")
		require.Equal(
			t, chanPoint,
			forceCloseTx.TxIn[0].PreviousOutPoint,
		)

		coopCloseTx, err := channel.BroadcastedCooperative()
		require.NoError(t, err, "unable to retrieve coop close")

		chanPoint.Index ^= 1
		require.Equal(
			t, chanPoint,
			coopCloseTx.TxIn[0].PreviousOutPoint,
		)
	}
}
