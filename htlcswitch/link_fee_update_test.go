package htlcswitch

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// mailboxAdmissionPeer records disconnect requests made by a channel link.
type mailboxAdmissionPeer struct {
	*lnpeer.MockPeer

	disconnected chan error
}

// Disconnect records the error supplied by the channel link.
func (p *mailboxAdmissionPeer) Disconnect(err error) {
	p.disconnected <- err
}

// mailboxAdmissionTestBox fails its first message admission and records the
// number of admission attempts.
type mailboxAdmissionTestBox struct {
	MailBox

	mu       sync.Mutex
	addCalls int
}

// AddMessage records an admission attempt and fails the first one.
func (m *mailboxAdmissionTestBox) AddMessage(lnwire.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.addCalls++
	if m.addCalls == 1 {
		return errWireMessageQueueFull
	}

	return nil
}

// calls returns the number of message admission attempts.
func (m *mailboxAdmissionTestBox) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.addCalls
}

// newLinkCapturingLogger returns a logger backed by an in-memory buffer.
func newLinkCapturingLogger() (btclog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	handler := btclog.NewDefaultHandler(buf, btclog.WithNoTimestamp())

	return btclog.NewSLogger(handler), buf
}

// TestProcessRemoteUpdateFeeRoleValidation checks that fee update role
// validation is performed at the link boundary.
func TestProcessRemoteUpdateFeeRoleValidation(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	newLink := func(channel *lnwallet.LightningChannel) *channelLink {
		link, ok := NewChannelLink(ChannelLinkConfig{
			DisallowQuiescence: true,
			OnChannelFailure: func(lnwire.ChannelID,
				lnwire.ShortChannelID, LinkFailureError) {
			},
		}, channel).(*channelLink)
		require.True(t, ok)

		return link
	}

	t.Run("unauthorized sender", func(t *testing.T) {
		link := newLink(aliceChannel)

		err := link.processRemoteUpdateFee(&lnwire.UpdateFee{})
		require.EqualError(t, err, "received fee update as initiator")
		require.True(t, link.failed)
	})

	t.Run("authorized sender", func(t *testing.T) {
		link := newLink(bobChannel)
		mailbox := newMemoryMailBox(&mailBoxConfig{})
		link.mailBox = mailbox

		feeRate := bobChannel.CommitFeeRate() + 1
		err := link.processRemoteUpdateFee(&lnwire.UpdateFee{
			FeePerKw: uint32(feeRate),
		})
		require.NoError(t, err)
		require.False(t, link.failed)
		require.True(t, bobChannel.NeedCommitment())
		require.Equal(t, feeRate, mailbox.feeRate)
	})
}

// TestProcessRemoteUpdateFeeExposureError checks that exceeding the fee
// exposure limit returns the error used to fail the link.
func TestProcessRemoteUpdateFeeExposureError(t *testing.T) {
	t.Parallel()

	_, bobChannel, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	link, ok := NewChannelLink(ChannelLinkConfig{
		DisallowQuiescence: true,
		MaxFeeExposure:     1,
		OnChannelFailure: func(lnwire.ChannelID,
			lnwire.ShortChannelID, LinkFailureError) {
		},
	}, bobChannel).(*channelLink)
	require.True(t, ok)

	err = link.processRemoteUpdateFee(&lnwire.UpdateFee{
		FeePerKw: 1000,
	})
	require.EqualError(t, err, "fee threshold exceeded")
	require.True(t, link.failed)
}

// TestLinkLogDeduplication checks that repeated non-fatal message classes are
// only recorded once during a link lifetime.
func TestLinkLogDeduplication(t *testing.T) {
	t.Parallel()

	aliceChannel, _, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	link, ok := NewChannelLink(ChannelLinkConfig{
		DisallowQuiescence: true,
	}, aliceChannel).(*channelLink)
	require.True(t, ok)
	logger, logBuffer := newLinkCapturingLogger()
	link.log = logger

	for i := 0; i < 2; i++ {
		link.handleUpstreamMsg(t.Context(), &lnwire.Warning{})
		link.handleUpstreamMsg(
			t.Context(), &lnwire.ChannelReestablish{},
		)
	}

	warningCount := strings.Count(
		logBuffer.String(), "received warning message from peer",
	)
	require.Equal(t, 1, warningCount)
	require.Equal(
		t, 1, strings.Count(
			logBuffer.String(), "received unknown message of type",
		),
	)
}

// TestChannelMessageAdmissionError checks that an admission error reconnects
// the ordered channel message stream instead of omitting a message.
func TestChannelMessageAdmissionError(t *testing.T) {
	t.Parallel()

	aliceChannel, _, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	peer := &mailboxAdmissionPeer{
		MockPeer:     &lnpeer.MockPeer{},
		disconnected: make(chan error, 1),
	}
	link, ok := NewChannelLink(ChannelLinkConfig{
		Peer:               peer,
		DisallowQuiescence: true,
	}, aliceChannel).(*channelLink)
	require.True(t, ok)

	mailbox := newMemoryMailBox(&mailBoxConfig{})
	link.mailBox = mailbox
	for i := 0; i < maxWireMessages; i++ {
		require.NoError(t, mailbox.AddMessage(&lnwire.UpdateFee{}))
	}

	link.HandleChannelUpdate(&lnwire.UpdateFee{})

	select {
	case err := <-peer.disconnected:
		require.ErrorIs(t, err, errWireMessageQueueFull)

	case <-time.After(time.Second):
		t.Fatal("mailbox admission error did not disconnect peer")
	}
}

// TestChannelMessageAdmissionFailureLatch checks that a link stops admitting
// peer messages after its first mailbox admission failure.
func TestChannelMessageAdmissionFailureLatch(t *testing.T) {
	t.Parallel()

	aliceChannel, _, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	peer := &mailboxAdmissionPeer{
		MockPeer:     &lnpeer.MockPeer{},
		disconnected: make(chan error, 2),
	}
	link, ok := NewChannelLink(ChannelLinkConfig{
		Peer:               peer,
		DisallowQuiescence: true,
	}, aliceChannel).(*channelLink)
	require.True(t, ok)

	mailbox := &mailboxAdmissionTestBox{}
	link.mailBox = mailbox
	logger, logBuffer := newLinkCapturingLogger()
	link.log = logger

	link.HandleChannelUpdate(&lnwire.UpdateFee{})

	select {
	case err := <-peer.disconnected:
		require.ErrorIs(t, err, errWireMessageQueueFull)

	case <-time.After(time.Second):
		t.Fatal("mailbox admission error did not disconnect peer")
	}

	link.HandleChannelUpdate(&lnwire.CommitSig{})

	require.Equal(t, 1, mailbox.calls())
	require.Equal(
		t, 1, strings.Count(
			logBuffer.String(), "failed to add Message to mailbox",
		),
	)
	select {
	case err := <-peer.disconnected:
		t.Fatalf("unexpected second disconnect: %v", err)

	default:
	}
}

// TestChannelMessageSizeAdmissionError checks that a message-size admission
// error reconnects the ordered channel message stream.
func TestChannelMessageSizeAdmissionError(t *testing.T) {
	t.Parallel()

	aliceChannel, _, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	peer := &mailboxAdmissionPeer{
		MockPeer:     &lnpeer.MockPeer{},
		disconnected: make(chan error, 1),
	}
	link, ok := NewChannelLink(ChannelLinkConfig{
		Peer:               peer,
		DisallowQuiescence: true,
	}, aliceChannel).(*channelLink)
	require.True(t, ok)

	mailbox := newMemoryMailBox(&mailBoxConfig{})
	link.mailBox = mailbox
	msg := &lnwire.Warning{
		Data: make([]byte, lnwire.MaxMsgBody-40),
	}
	for {
		err := mailbox.AddMessage(msg)
		if errors.Is(err, errWireMessageQueueFull) {
			break
		}
		require.NoError(t, err)
	}

	link.HandleChannelUpdate(msg)

	select {
	case err := <-peer.disconnected:
		require.ErrorIs(t, err, errWireMessageQueueFull)

	case <-time.After(time.Second):
		t.Fatal("message-size admission error did not disconnect peer")
	}
}
