package routing

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
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
