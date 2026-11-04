package chanlifecycle_test

import (
	"math"
	"math/rand"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

func genRandomKVChannelShell() (*chanstate.ChannelShell, error) {
	var testPriv [32]byte
	if _, err := rand.Read(testPriv[:]); err != nil {
		return nil, err
	}

	_, pub := btcec.PrivKeyFromBytes(testPriv[:])

	var chanPoint wire.OutPoint
	if _, err := rand.Read(chanPoint.Hash[:]); err != nil {
		return nil, err
	}

	chanPoint.Index = uint32(rand.Intn(math.MaxUint16))

	var shaChainPriv [32]byte
	if _, err := rand.Read(shaChainPriv[:]); err != nil {
		return nil, err
	}

	revRoot, err := chainhash.NewHash(shaChainPriv[:])
	if err != nil {
		return nil, err
	}
	shaChainProducer := shachain.NewRevocationProducer(*revRoot)

	commitParams := chanstate.CommitmentParams{
		CsvDelay: uint16(rand.Int63()),
	}

	channel := &chanstate.OpenChannel{
		ChainHash:       *revRoot,
		FundingOutpoint: chanPoint,
		ShortChannelID: lnwire.NewShortChanIDFromInt(
			uint64(rand.Int63()),
		),
		IdentityPub: pub,
		LocalChanCfg: chanstate.ChannelConfig{
			CommitmentParams: commitParams,
			PaymentBasePoint: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(
						rand.Int63(),
					),
					Index: uint32(rand.Int63()),
				},
			},
		},
		RemoteCurrentRevocation: pub,
		IsPending:               false,
		RevocationStore:         shachain.NewRevocationStore(),
		RevocationProducer:      shaChainProducer,
	}
	channel.SetChannelStatusForStore(
		chanstate.ChanStatusDefault | chanstate.ChanStatusRestored,
	)

	return &chanstate.ChannelShell{
		NodeAddrs: []net.Addr{&net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 18555,
		}},
		Chan: channel,
	}, nil
}

// TestRestoreChannelShells tests that we're able to insert a partially
// populated channel to disk and create its related link-node metadata.
func TestRestoreChannelShells(t *testing.T) {
	t.Parallel()

	fixture := newKVChannelTestContext(t)

	channelShell, err := genRandomKVChannelShell()
	require.NoError(t, err, "unable to gen channel shell")

	err = fixture.lifecycle.RestoreChannelShells(channelShell)
	require.NoError(t, err, "unable to restore channel shell")

	nodeChans, err := fixture.chanStore.FetchOpenChannels(
		channelShell.Chan.IdentityPub,
	)
	require.NoError(t, err, "unable find channel")
	require.Len(t, nodeChans, 1)

	channel := nodeChans[0]
	_, err = channel.UpdateCommitment(nil, nil)
	require.ErrorIs(t, err, chanstate.ErrNoRestoredChannelMutation)

	err = channel.AppendRemoteCommitChain(nil)
	require.ErrorIs(t, err, chanstate.ErrNoRestoredChannelMutation)

	err = channel.AdvanceCommitChainTail(nil, nil, 0, 1)
	require.ErrorIs(t, err, chanstate.ErrNoRestoredChannelMutation)

	require.Equal(
		t, channelShell.Chan.FundingOutpoint,
		channel.FundingOutpoint,
	)
	require.True(t, channel.HasChanStatus(chanstate.ChanStatusRestored))

	_, err = fixture.chanStore.FetchChannel(
		channelShell.Chan.FundingOutpoint,
	)
	require.NoError(t, err, "unable to fetch channel")

	linkNode, err := fixture.linkNodeStore.FetchLinkNode(
		channelShell.Chan.IdentityPub,
	)
	require.NoError(t, err, "unable to fetch link node")
	require.Len(t, linkNode.Addresses, len(channelShell.NodeAddrs))
	require.Equal(
		t, channelShell.NodeAddrs[0].String(),
		linkNode.Addresses[0].String(),
	)
}

// TestFetchClosedChannels tests that closed channel summaries can be fetched
// before and after the lifecycle implementation marks the channel fully
// closed.
func TestFetchClosedChannels(t *testing.T) {
	t.Parallel()

	fixture := newKVChannelTestContext(t)

	channel := newKVTestChannel(t, fixture.chanStore)
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}
	err := fixture.lifecycle.SyncPendingChannel(channel, addr, 100)
	require.NoError(t, err, "unable to sync pending channel")

	err = channel.MarkAsOpen(channel.ShortChannelID)
	require.NoError(t, err, "unable to mark channel open")

	summary := &chanstate.ChannelCloseSummary{
		ChanPoint:   channel.FundingOutpoint,
		ClosingTXID: channel.ChainHash,
		RemotePub:   channel.IdentityPub,
		Capacity:    channel.Capacity,
		SettledBalance: channel.LocalCommitment.LocalBalance.
			ToSatoshis(),
		TimeLockedBalance: channel.RemoteCommitment.LocalBalance.
			ToSatoshis() + 10000,
		CloseType:       chanstate.RemoteForceClose,
		IsPending:       true,
		LocalChanConfig: channel.LocalChanCfg,
	}
	err = channel.CloseChannel(summary)
	require.NoError(t, err, "unable to close channel")

	pendingClosed, err := fixture.chanStore.FetchClosedChannels(true)
	require.NoError(t, err, "failed fetching closed channels")
	require.Equal(
		t, []*chanstate.ChannelCloseSummary{summary}, pendingClosed,
	)

	closed, err := fixture.chanStore.FetchClosedChannels(false)
	require.NoError(t, err, "failed fetching all closed channels")
	require.Equal(t, []*chanstate.ChannelCloseSummary{summary}, closed)

	err = fixture.lifecycle.MarkChanFullyClosed(&channel.FundingOutpoint)
	require.NoError(t, err, "failed fully closing channel")

	closed, err = fixture.chanStore.FetchClosedChannels(false)
	require.NoError(t, err, "failed fetching closed channels")
	require.Len(t, closed, 1)
	require.False(t, closed[0].IsPending)

	pendingClose, err := fixture.chanStore.FetchClosedChannels(true)
	require.NoError(t, err, "failed fetching channels pending close")
	require.Empty(t, pendingClose)
}
