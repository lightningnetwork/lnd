package routing

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestBandwidthManager tests getting of bandwidth hints from a bandwidth
// manager.
func TestBandwidthManager(t *testing.T) {
	var (
		chan1ID      uint64         = 101
		chan2ID      uint64         = 102
		chanCapacity btcutil.Amount = 100000
	)

	testCases := []struct {
		name              string
		channelID         uint64
		linkQuery         getLinkQuery
		expectedBandwidth lnwire.MilliSatoshi
		expectFound       bool
	}{
		{
			name:      "channel not ours",
			channelID: chan2ID,
			// Set a link query that will fail our test since we
			// don't expect to query the switch for a channel that
			// is not ours.
			linkQuery: func(id lnwire.ShortChannelID) (
				htlcswitch.ChannelLink, error) {

				require.Fail(t, "link query unexpected for: "+
					"%v", id)

				return nil, nil
			},
			expectedBandwidth: 0,
			expectFound:       false,
		},
		{
			name:      "channel ours, link not online",
			channelID: chan1ID,
			linkQuery: func(lnwire.ShortChannelID) (
				htlcswitch.ChannelLink, error) {

				return nil, htlcswitch.ErrChannelLinkNotFound
			},
			expectedBandwidth: 0,
			expectFound:       true,
		},
		{
			name:      "channel ours, link not eligible",
			channelID: chan1ID,
			linkQuery: func(lnwire.ShortChannelID) (
				htlcswitch.ChannelLink, error) {

				return &mockLink{
					ineligible: true,
				}, nil
			},
			expectedBandwidth: 0,
			expectFound:       true,
		},
		{
			name:      "channel ours, link can't add htlc",
			channelID: chan1ID,
			linkQuery: func(lnwire.ShortChannelID) (
				htlcswitch.ChannelLink, error) {

				return &mockLink{
					mayAddOutgoingErr: errors.New(
						"can't add htlc",
					),
				}, nil
			},
			expectedBandwidth: 0,
			expectFound:       true,
		},
		{
			name:      "channel ours, bandwidth available",
			channelID: chan1ID,
			linkQuery: func(lnwire.ShortChannelID) (
				htlcswitch.ChannelLink, error) {

				return &mockLink{
					bandwidth: 321,
				}, nil
			},
			expectedBandwidth: 321,
			expectFound:       true,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			g := newMockGraph(t)

			sourceNode := newMockNode(sourceNodeID)
			targetNode := newMockNode(targetNodeID)

			g.addNode(sourceNode)
			g.addNode(targetNode)
			g.addChannel(
				chan1ID, sourceNodeID, targetNodeID,
				chanCapacity,
			)

			m, err := newBandwidthManager(
				g, sourceNode.pubkey, testCase.linkQuery,
				fn.None[[]byte](),
				fn.Some[htlcswitch.AuxTrafficShaper](
					&mockTrafficShaper{},
				),
			)
			require.NoError(t, err)

			bandwidth, found := m.availableChanBandwidth(
				testCase.channelID, 10,
			)
			require.Equal(t, testCase.expectedBandwidth, bandwidth)
			require.Equal(t, testCase.expectFound, found)
		})
	}
}

type mockTrafficShaper struct{}

// ShouldHandleTraffic is called in order to check if the channel identified
// by the provided channel ID may have external mechanisms that would
// allow it to carry out the payment.
func (*mockTrafficShaper) ShouldHandleTraffic(_ lnwire.ShortChannelID,
	_, _ fn.Option[tlv.Blob]) (bool, error) {

	return true, nil
}

// PaymentBandwidth returns the available bandwidth for a custom channel decided
// by the given channel funding/commitment aux blob and HTLC blob. A return
// value of 0 means there is no bandwidth available. To find out if a channel is
// a custom channel that should be handled by the traffic shaper, the
// ShouldHandleTraffic method should be called first.
func (*mockTrafficShaper) PaymentBandwidth(_, _, _ fn.Option[tlv.Blob],
	linkBandwidth, _ lnwire.MilliSatoshi,
	_ lnwallet.AuxHtlcView, _ route.Vertex) (lnwire.MilliSatoshi, error) {

	return linkBandwidth, nil
}

// ProduceHtlcExtraData is a function that, based on the previous extra
// data blob of an HTLC, may produce a different blob or modify the
// amount of bitcoin this htlc should carry.
func (*mockTrafficShaper) ProduceHtlcExtraData(totalAmount lnwire.MilliSatoshi,
	_ lnwire.CustomRecords, _ route.Vertex) (lnwire.MilliSatoshi,
	lnwire.CustomRecords, error) {

	return totalAmount, nil, nil
}

func (*mockTrafficShaper) IsCustomHTLC(_ lnwire.CustomRecords) bool {
	return false
}
