package htlcswitch

import (
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

// feeMsgPeer records the types of messages a link sends to its peer.
type feeMsgPeer struct {
	*lnpeer.MockPeer

	mu    sync.Mutex
	types []lnwire.MessageType
}

// SendMessage records the message types and reports success.
func (p *feeMsgPeer) SendMessage(_ bool, msgs ...lnwire.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, m := range msgs {
		p.types = append(p.types, m.MsgType())
	}

	return nil
}

// SendMessageLazy records the message types and reports success.
func (p *feeMsgPeer) SendMessageLazy(sync bool, msgs ...lnwire.Message) error {
	return p.SendMessage(sync, msgs...)
}

// sentTypes returns a copy of the recorded message types.
func (p *feeMsgPeer) sentTypes() []lnwire.MessageType {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]lnwire.MessageType(nil), p.types...)
}

// TestUpdateChannelFeeShutdownCases checks when updateChannelFee sends an
// update_fee after the link's own or the remote's shutdown.
func TestUpdateChannelFeeShutdownCases(t *testing.T) {
	t.Parallel()

	shutdown := lnwire.Shutdown{
		Address: append([]byte{0x00, 0x14}, make([]byte, 20)...),
	}

	cases := []struct {
		name string

		// ownShutdown marks that we have sent our own shutdown.
		ownShutdown bool

		// remoteShutdown marks that the remote has sent a shutdown,
		// which disables outgoing adds.
		remoteShutdown bool

		// addHtlc adds an HTLC that is committed on both sides.
		addHtlc bool

		wantFee bool
	}{
		{
			name:        "own shutdown no htlcs",
			ownShutdown: true,
			wantFee:     false,
		},
		{
			name:           "remote shutdown only no htlcs",
			remoteShutdown: true,
			wantFee:        true,
		},
		{
			name:        "own shutdown with htlc",
			ownShutdown: true,
			addHtlc:     true,
			wantFee:     true,
		},
		{
			name:    "no shutdown no htlcs",
			wantFee: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob, err := lnwallet.CreateTestChannels(
				t, channeldb.SingleFunderTweaklessBit,
			)
			require.NoError(t, err)

			if tc.addHtlc {
				htlc := &lnwire.UpdateAddHTLC{
					ID:          0,
					Amount:      lnwire.NewMSatFromSatoshis(100_000),
					Expiry:      1_000,
					PaymentHash: [32]byte{1},
				}
				_, err := alice.AddHTLC(htlc, nil)
				require.NoError(t, err)
				_, err = bob.ReceiveHTLC(htlc)
				require.NoError(t, err)
				require.NoError(t, lnwallet.ForceStateTransition(
					alice, bob,
				))
			}

			peer := &feeMsgPeer{MockPeer: &lnpeer.MockPeer{}}
			link, ok := NewChannelLink(ChannelLinkConfig{
				Peer:                peer,
				Circuits:            &mockCircuitMap{},
				PendingCommitTicker: ticker.New(time.Minute),
				NotifyContractUpdate: func(
					*contractcourt.ContractUpdate) error {

					return nil
				},
				DisallowQuiescence: true,
				OnChannelFailure: func(lnwire.ChannelID,
					lnwire.ShortChannelID, LinkFailureError) {
				},
			}, alice).(*channelLink)
			require.True(t, ok)
			link.mailBox = newMemoryMailBox(&mailBoxConfig{})
			link.markReestablished()

			if tc.ownShutdown {
				s := shutdown
				s.ChannelID = link.ChanID()
				link.cfg.PreviouslySentShutdown = fn.Some(s)
			}
			if tc.remoteShutdown {
				link.DisableAdds(Outgoing)
			}

			feeRate := alice.CommitFeeRate() + 1000
			require.NoError(t, link.updateChannelFee(
				t.Context(), feeRate,
			))

			sent := peer.sentTypes()
			var updateFee lnwire.MessageType = lnwire.MsgUpdateFee
			if tc.wantFee {
				require.Contains(t, sent, updateFee)
			} else {
				require.NotContains(t, sent, updateFee)
			}
		})
	}
}

// TestHandleUpdateFeeNonFunderNoSend checks that a non-funding link never
// sends an update_fee, regardless of shutdown state.
func TestHandleUpdateFeeNonFunderNoSend(t *testing.T) {
	t.Parallel()

	_, bob, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	peer := &feeMsgPeer{MockPeer: &lnpeer.MockPeer{}}
	link, ok := NewChannelLink(ChannelLinkConfig{
		Peer:               peer,
		DisallowQuiescence: true,
		OnChannelFailure: func(lnwire.ChannelID,
			lnwire.ShortChannelID, LinkFailureError) {
		},
	}, bob).(*channelLink)
	require.True(t, ok)
	link.mailBox = newMemoryMailBox(&mailBoxConfig{})
	link.markReestablished()
	link.cfg.PreviouslySentShutdown = fn.Some(lnwire.Shutdown{
		ChannelID: link.ChanID(),
		Address:   append([]byte{0x00, 0x14}, make([]byte, 20)...),
	})

	require.NoError(t, link.handleUpdateFee(t.Context()))
	var updateFee lnwire.MessageType = lnwire.MsgUpdateFee
	require.NotContains(t, peer.sentTypes(), updateFee)
}

// TestFeeTimerAfterOwnShutdown asserts that a funder link does not send an
// update_fee after its own shutdown once no HTLCs remain on either commitment.
func TestFeeTimerAfterOwnShutdown(t *testing.T) {
	t.Parallel()

	// No HTLCs on either commitment. The link resumes with a persisted
	// shutdown: it disables adds in both directions and sends the shutdown
	// again.
	h, err := newSingleLinkTestHarness(t, 1_000_000, 10_000)
	require.NoError(t, err)

	link := h.aliceLink.(*channelLink)
	link.cfg.PreviouslySentShutdown = fn.Some(lnwire.Shutdown{
		ChannelID: link.ChanID(),
		Address:   append([]byte{0x00, 0x14}, make([]byte, 20)...),
	})
	require.NoError(t, h.start())

	est := link.cfg.FeeEstimator.(*mockFeeEstimator)
	sent := link.cfg.Peer.(*mockPeer).sentMsgs
	var names []string
	drain := func() {
		for {
			select {
			case m := <-sent:
				names = append(names, m.MsgType().String())
			case <-time.After(2 * time.Second):
				return
			}
		}
	}

	// The shutdown goes out first.
	drain()
	require.True(t, link.IsFlushing(Incoming))
	require.True(t, link.IsFlushing(Outgoing))

	// The fee timer fires and the network feerate is lower than the
	// channel's.
	names = nil
	link.updateFeeTimer.Reset(time.Millisecond)
	select {
	case est.byteFeeIn <- 1000:
	case <-time.After(5 * time.Second):
		t.Fatal("link did not sample the network fee")
	}
	select {
	case est.relayFee <- chainfee.FeePerKwFloor:
	case <-time.After(5 * time.Second):
		t.Fatal("link did not sample the relay fee")
	}
	drain()

	updateFee := lnwire.MessageType(lnwire.MsgUpdateFee).String()
	require.NotContains(t, names, updateFee,
		"link sent update_fee after its own shutdown with no HTLCs: %v",
		names)
}
