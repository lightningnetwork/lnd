package chanstate

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestFetchChannels tests the filtering of open channels exposed by the
// public fetch methods.
func TestFetchChannels(t *testing.T) {
	var (
		pendingChan        = lnwire.NewShortChanIDFromInt(1)
		pendingWaitingChan = lnwire.NewShortChanIDFromInt(2)
		openChan           = lnwire.NewShortChanIDFromInt(3)
		openWaitingChan    = lnwire.NewShortChanIDFromInt(4)
	)

	tests := []struct {
		name             string
		fetch            func(*KVStore) ([]*OpenChannel, error)
		expectedChannels map[lnwire.ShortChannelID]bool
	}{
		{
			name:  "get all channels",
			fetch: (*KVStore).FetchAllChannels,
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingChan:        true,
				pendingWaitingChan: true,
				openChan:           true,
				openWaitingChan:    true,
			},
		},
		{
			name:  "pending channels",
			fetch: (*KVStore).FetchPendingChannels,
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingChan: true,
			},
		},
		{
			name:  "open channels",
			fetch: (*KVStore).FetchAllOpenChannels,
			expectedChannels: map[lnwire.ShortChannelID]bool{
				openChan: true,
			},
		},
		{
			name:  "waiting close channels",
			fetch: (*KVStore).FetchWaitingCloseChannels,
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingWaitingChan: true,
				openWaitingChan:    true,
			},
		},
		{
			name: "not waiting close channels",
			fetch: func(store *KVStore) ([]*OpenChannel, error) {
				pendingChans, err := store.
					FetchPendingChannels()
				if err != nil {
					return nil, err
				}

				openChannels, err := store.
					FetchAllOpenChannels()
				if err != nil {
					return nil, err
				}

				pendingChans = append(
					pendingChans, openChannels...,
				)

				return pendingChans, nil
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingChan: true,
				openChan:    true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			backend, cleanup := makeTestBackend(t)
			t.Cleanup(cleanup)

			store := NewKVStore(backend)

			createTestChannel(
				t, store, channelIDOption(pendingChan),
			)

			pendingClosing := createTestChannel(
				t, store, channelIDOption(pendingWaitingChan),
			)
			err := pendingClosing.MarkCoopBroadcasted(
				wire.NewMsgTx(2), lntypes.Local,
			)
			require.NoError(t, err)

			createTestOpenChannel(
				t, store, channelIDOption(openChan),
			)

			openClosing := createTestOpenChannel(
				t, store, channelIDOption(openWaitingChan),
			)
			err = openClosing.MarkCoopBroadcasted(
				wire.NewMsgTx(2), lntypes.Local,
			)
			require.NoError(t, err)

			channels, err := test.fetch(store)
			require.NoError(t, err)
			require.Len(t, channels, len(test.expectedChannels))

			for _, channel := range channels {
				shortChanID := channel.ShortChannelID
				_, ok := test.expectedChannels[shortChanID]
				require.Truef(
					t, ok, "unexpected channel: %v",
					shortChanID,
				)
			}
		})
	}
}

// TestFetchPermTempPeer tests that we're able to call FetchPermAndTempPeers
// successfully.
func TestFetchPermTempPeer(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	privKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate new private key")

	pubKey1 := privKey1.PubKey()
	channelState1 := createTestOpenChannel(t, store, pubKeyOption(pubKey1))

	_, err = store.FetchChannel(channelState1.FundingOutpoint)
	require.NoError(t, err, "unable to fetch channel")

	privKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate private key")

	pubKey2 := privKey2.PubKey()
	channelState2 := createTestChannel(t, store, pubKeyOption(pubKey2))

	_, err = store.FetchChannel(channelState2.FundingOutpoint)
	require.NoError(t, err, "unable to fetch channel")

	privKey3, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate new private key")

	pubKey3 := privKey3.PubKey()
	closedChannel := createTestOpenChannel(t, store, pubKeyOption(pubKey3))
	err = closedChannel.CloseChannel(&ChannelCloseSummary{
		ChanPoint:      closedChannel.FundingOutpoint,
		RemotePub:      closedChannel.IdentityPub,
		SettledBalance: btcutil.Amount(500),
	})
	require.NoError(t, err, "unable to close channel")

	peerChanInfo, err := store.FetchPermAndTempPeers(key[:])
	require.NoError(t, err, "unable to fetch perm and temp peers")
	require.Len(t, peerChanInfo, 3)

	count1, found := peerChanInfo[string(pubKey1.SerializeCompressed())]
	require.True(t, found, "unable to find peer 1 in peerChanInfo")
	require.True(t, count1.HasOpenOrClosedChan)
	require.Zero(t, count1.PendingOpenCount)

	count2, found := peerChanInfo[string(pubKey2.SerializeCompressed())]
	require.True(t, found, "unable to find peer 2 in peerChanInfo")
	require.False(t, count2.HasOpenOrClosedChan)
	require.Equal(t, uint64(1), count2.PendingOpenCount)

	count3, found := peerChanInfo[string(pubKey3.SerializeCompressed())]
	require.True(t, found, "unable to find peer 3 in peerChanInfo")
	require.True(t, count3.HasOpenOrClosedChan)
	require.Zero(t, count3.PendingOpenCount)
}
