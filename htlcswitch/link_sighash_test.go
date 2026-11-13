package htlcswitch

import (
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// negotiatedSigHashSigner is an aux signer whose answer about the HTLC
// sighash type can change over time, the way the real one does once the
// peer has (re)established the channel and the negotiated features are
// known.
type negotiatedSigHashSigner struct {
	*lnwallet.MockAuxSigner

	mu      sync.Mutex
	sigHash fn.Option[txscript.SigHashType]
}

// negotiate sets the sighash type the signer reports from now on. None
// stands for a peer without the DeterministicHTLCs feature.
func (s *negotiatedSigHashSigner) negotiate(
	sigHash fn.Option[txscript.SigHashType]) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sigHash = sigHash
}

// HtlcSigHashType returns the currently negotiated sighash type.
func (s *negotiatedSigHashSigner) HtlcSigHashType(
	lnwallet.HtlcSigHashReq) fn.Option[txscript.SigHashType] {

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.sigHash
}

// TestLinkRefreshHtlcSigHashTypeMailboxDust asserts that when the channel
// picks up a new HTLC sighash type after a reestablish, the mailbox's dust
// closure is refreshed along with it. The mailbox captures the closure when
// it is attached, which happens before the reestablish, so an HTLC that is
// dust under one sighash type and not under the other must be classified
// the way the channel now classifies it, not the way it did at attach time.
func TestLinkRefreshHtlcSigHashTypeMailboxDust(t *testing.T) {
	t.Parallel()

	customChanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SimpleTaprootFeatureBit |
		channeldb.TapscriptRootBit

	aliceChannel, _, err := lnwallet.CreateTestChannels(
		t, customChanType,
	)
	require.NoError(t, err)

	// At construction the peer has not negotiated anything yet, so the
	// channel uses the standard sighash flags.
	signer := &negotiatedSigHashSigner{
		MockAuxSigner: lnwallet.NewAuxSignerMock(nil),
		sigHash:       fn.None[txscript.SigHashType](),
	}
	channel, err := lnwallet.NewLightningChannel(
		aliceChannel.Signer, aliceChannel.State(),
		lnwallet.NewSigPool(1, aliceChannel.Signer),
		lnwallet.WithAuxSigner(signer),
	)
	require.NoError(t, err)
	require.False(t, channel.IsChanSigHashDefault())

	link, ok := NewChannelLink(ChannelLinkConfig{
		DisallowQuiescence: true,
		OnChannelFailure: func(lnwire.ChannelID,
			lnwire.ShortChannelID, LinkFailureError) {
		},
	}, channel).(*channelLink)
	require.True(t, ok)

	// The mailbox is attached before the channel is reestablished, so it
	// captures the dust closure of the standard sighash flags.
	mailbox := newMemoryMailBox(&mailBoxConfig{
		clock:  clock.NewTestClock(time.Now()),
		expiry: time.Minute,
	})
	link.AttachMailBox(mailbox)

	// Pick an HTLC amount that is not dust on either commitment under
	// the standard flags but is dust on at least one of them under
	// SigHashDefault, where the on-chain value also has to cover the
	// baked-in second-level fee and the CPFP anchor.
	state := channel.State()
	feeRate := channel.CommitFeeRate()
	amt := max(
		state.LocalChanCfg.DustLimit, state.RemoteChanCfg.DustLimit,
	) + 100
	isDust := func(whoseCommit lntypes.ChannelParty,
		sigHashDefault bool) bool {

		dustLimit := state.LocalChanCfg.DustLimit
		if whoseCommit.IsRemote() {
			dustLimit = state.RemoteChanCfg.DustLimit
		}

		return lnwallet.HtlcIsDust(
			customChanType, false, whoseCommit, feeRate, amt,
			dustLimit, sigHashDefault,
		)
	}
	require.False(t, isDust(lntypes.Local, false))
	require.False(t, isDust(lntypes.Remote, false))
	require.True(t,
		isDust(lntypes.Local, true) || isDust(lntypes.Remote, true),
		"the amount must be dust under SigHashDefault somewhere")

	dustSum := func(whoseCommit lntypes.ChannelParty,
		sigHashDefault bool) lnwire.MilliSatoshi {

		if isDust(whoseCommit, sigHashDefault) {
			return lnwire.NewMSatFromSatoshis(amt)
		}

		return 0
	}

	pkt := &htlcPacket{
		incomingChanID: lnwire.NewShortChanIDFromInt(1),
		incomingHTLCID: 0,
		amount:         lnwire.NewMSatFromSatoshis(amt),
		htlc:           &lnwire.UpdateAddHTLC{},
	}
	require.NoError(t, mailbox.AddPacket(pkt))

	localSum, remoteSum := mailbox.DustPackets()
	require.Equal(t, dustSum(lntypes.Local, false), localSum)
	require.Equal(t, dustSum(lntypes.Remote, false), remoteSum)

	// The peer reestablishes the channel with DeterministicHTLCs
	// negotiated. Refreshing the channel alone moves its own sighash
	// type, but leaves the mailbox on the closure it captured: this is
	// the mismatch the link has to prevent.
	signer.negotiate(fn.Some(txscript.SigHashDefault))
	channel.RefreshHtlcSigHashType()
	require.True(t, channel.IsChanSigHashDefault())

	localSum, remoteSum = mailbox.DustPackets()
	require.Equal(t, dustSum(lntypes.Local, false), localSum)
	require.Equal(t, dustSum(lntypes.Remote, false), remoteSum)

	// The link's refresh, which is what the reestablish path calls,
	// updates both.
	link.refreshHtlcSigHashType()
	require.True(t, channel.IsChanSigHashDefault())

	localSum, remoteSum = mailbox.DustPackets()
	require.Equal(t, dustSum(lntypes.Local, true), localSum)
	require.Equal(t, dustSum(lntypes.Remote, true), remoteSum)

	// A peer that comes back without the feature takes the channel, and
	// the mailbox, back to the standard flags.
	signer.negotiate(fn.None[txscript.SigHashType]())
	link.refreshHtlcSigHashType()
	require.False(t, channel.IsChanSigHashDefault())

	localSum, remoteSum = mailbox.DustPackets()
	require.Equal(t, dustSum(lntypes.Local, false), localSum)
	require.Equal(t, dustSum(lntypes.Remote, false), remoteSum)

	// A refresh on a link that has no mailbox attached yet must not
	// panic.
	bare, ok := NewChannelLink(ChannelLinkConfig{
		DisallowQuiescence: true,
		OnChannelFailure: func(lnwire.ChannelID,
			lnwire.ShortChannelID, LinkFailureError) {
		},
	}, channel).(*channelLink)
	require.True(t, ok)
	require.Nil(t, bare.mailBox)
	require.NotPanics(t, bare.refreshHtlcSigHashType)
}

// TestLinkDustClosureIgnoresSigHashOnVanillaChannel asserts that on a channel
// without a tapscript root the sighash type never influences the dust
// closure: refreshing is a no-op for the mailbox, whatever the aux signer
// answers.
func TestLinkDustClosureIgnoresSigHashOnVanillaChannel(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SimpleTaprootFeatureBit

	aliceChannel, _, err := lnwallet.CreateTestChannels(t, chanType)
	require.NoError(t, err)

	signer := &negotiatedSigHashSigner{
		MockAuxSigner: lnwallet.NewAuxSignerMock(nil),
		sigHash:       fn.Some(txscript.SigHashDefault),
	}
	channel, err := lnwallet.NewLightningChannel(
		aliceChannel.Signer, aliceChannel.State(),
		lnwallet.NewSigPool(1, aliceChannel.Signer),
		lnwallet.WithAuxSigner(signer),
	)
	require.NoError(t, err)

	link, ok := NewChannelLink(ChannelLinkConfig{
		DisallowQuiescence: true,
		OnChannelFailure: func(lnwire.ChannelID,
			lnwire.ShortChannelID, LinkFailureError) {
		},
	}, channel).(*channelLink)
	require.True(t, ok)

	mailbox := newMemoryMailBox(&mailBoxConfig{
		clock:  clock.NewTestClock(time.Now()),
		expiry: time.Minute,
	})
	link.AttachMailBox(mailbox)

	state := channel.State()
	amt := max(
		state.LocalChanCfg.DustLimit, state.RemoteChanCfg.DustLimit,
	) + 100
	require.NoError(t, mailbox.AddPacket(&htlcPacket{
		incomingChanID: lnwire.NewShortChanIDFromInt(1),
		amount:         lnwire.NewMSatFromSatoshis(amt),
		htlc:           &lnwire.UpdateAddHTLC{},
	}))

	localSum, remoteSum := mailbox.DustPackets()
	require.Zero(t, localSum)
	require.Zero(t, remoteSum)

	require.False(t, channel.IsChanSigHashDefault())
	link.refreshHtlcSigHashType()
	require.False(t, channel.IsChanSigHashDefault())

	localSum, remoteSum = mailbox.DustPackets()
	require.Zero(t, localSum)
	require.Zero(t, remoteSum)
}
