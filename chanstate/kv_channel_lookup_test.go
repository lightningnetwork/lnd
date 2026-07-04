package chanstate

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestFetchClosedChannelForID tests that we are able to properly retrieve a
// ChannelCloseSummary from the store given a ChannelID.
func TestFetchClosedChannelForID(t *testing.T) {
	t.Parallel()

	const numChans = 101

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	channel := createTestChannelState(t, store)
	for i := uint32(0); i < numChans; i++ {
		chanPoint := channel.FundingOutpoint
		chanPoint.Index = i

		openChannel := createTestOpenChannel(
			t, store, fundingPointOption(chanPoint),
		)

		closeSummary := &ChannelCloseSummary{
			ChanPoint:      openChannel.FundingOutpoint,
			RemotePub:      openChannel.IdentityPub,
			SettledBalance: btcutil.Amount(500 + i),
		}
		require.NoError(t, openChannel.CloseChannel(closeSummary))
	}

	for i := uint32(0); i < numChans; i++ {
		chanPoint := channel.FundingOutpoint
		chanPoint.Index = i

		cid := lnwire.NewChanIDFromOutPoint(chanPoint)
		fetchedSummary, err := store.FetchClosedChannelForID(cid)
		require.NoError(t, err)
		require.Equal(
			t, btcutil.Amount(500+i),
			fetchedSummary.SettledBalance,
		)
	}

	badChanPoint := channel.FundingOutpoint
	badChanPoint.Index = numChans

	cid := lnwire.NewChanIDFromOutPoint(badChanPoint)
	_, err := store.FetchClosedChannelForID(cid)
	require.ErrorIs(t, err, ErrClosedChannelNotFound)
}

// TestFetchChannel tests that we're able to fetch an arbitrary channel from
// disk.
func TestFetchChannel(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	channelState := createTestOpenChannel(t, store)

	dbChannel, err := store.FetchChannel(channelState.FundingOutpoint)
	require.NoError(t, err, "unable to fetch channel")
	require.Equal(t, channelState, dbChannel)

	chanID := lnwire.NewChanIDFromOutPoint(channelState.FundingOutpoint)
	dbChannel, err = store.FetchChannelByID(chanID)
	require.NoError(t, err, "unable to fetch channel")
	require.Equal(t, channelState, dbChannel)

	channelState2 := createTestChannelState(t, store)
	uniqueOutputIndex.Add(1)
	channelState2.FundingOutpoint.Index = uniqueOutputIndex.Load()

	_, err = store.FetchChannel(channelState2.FundingOutpoint)
	require.ErrorIs(t, err, ErrChannelNotFound)

	chanID2 := lnwire.NewChanIDFromOutPoint(channelState2.FundingOutpoint)
	_, err = store.FetchChannelByID(chanID2)
	require.ErrorIs(t, err, ErrChannelNotFound)
}

// TestFetchHistoricalChannel tests lookup of historical channels.
func TestFetchHistoricalChannel(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	channel := createTestOpenChannel(t, store)

	_, err := store.FetchHistoricalChannel(&channel.FundingOutpoint)
	require.ErrorIs(t, err, ErrNoHistoricalBucket)

	err = channel.CloseChannel(&ChannelCloseSummary{
		ChanPoint:      channel.FundingOutpoint,
		RemotePub:      channel.IdentityPub,
		SettledBalance: btcutil.Amount(500),
	})
	require.NoError(t, err, "unexpected error closing channel")

	histChannel, err := store.FetchHistoricalChannel(
		&channel.FundingOutpoint,
	)
	require.NoError(t, err, "unexpected error getting channel")
	require.Equal(t, channel, histChannel)

	badOutpoint := &wire.OutPoint{
		Hash:  channel.FundingOutpoint.Hash,
		Index: channel.FundingOutpoint.Index + 1,
	}
	_, err = store.FetchHistoricalChannel(badOutpoint)
	require.ErrorIs(t, err, ErrChannelNotFound)
}
