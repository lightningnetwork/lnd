package invoicesrpc

import (
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type hopHintsConfigMock struct {
	mock.Mock
}

// IsPublicNode mocks node public state lookup.
func (h *hopHintsConfigMock) IsPublicNode(pubKey [33]byte) (bool, error) {
	args := h.Mock.Called(pubKey)
	return args.Bool(0), args.Error(1)
}

// FetchChannelEdgesByID mocks channel edge lookup.
func (h *hopHintsConfigMock) FetchChannelEdgesByID(chanID uint64) (
	*channeldb.ChannelEdgeInfo, *channeldb.ChannelEdgePolicy,
	*channeldb.ChannelEdgePolicy, error) {

	args := h.Mock.Called(chanID)

	// If our error is non-nil, we expect nil responses otherwise. Our
	// casts below will fail with nil values, so we check our error and
	// return early on failure first.
	err := args.Error(3)
	if err != nil {
		return nil, nil, nil, err
	}

	edgeInfo := args.Get(0).(*channeldb.ChannelEdgeInfo)
	policy1 := args.Get(1).(*channeldb.ChannelEdgePolicy)
	policy2 := args.Get(2).(*channeldb.ChannelEdgePolicy)

	return edgeInfo, policy1, policy2, err
}

// TestSelectHopHints tests selection of hop hints for a node with private
// channels.
func TestSelectHopHints(t *testing.T) {
	var (
		// We need to serialize our pubkey in SelectHopHints so it
		// needs to be valid.
		pubkeyBytes, _ = hex.DecodeString(
			"598ec453728e0ffe0ae2f5e174243cf58f2" +
				"a3f2c83d2457b43036db568b11093",
		)
		pubkey = &btcec.PublicKey{
			X:     big.NewInt(4),
			Y:     new(big.Int).SetBytes(pubkeyBytes),
			Curve: btcec.S256(),
		}
		compressed = pubkey.SerializeCompressed()

		publicChannel = &HopHintInfo{
			IsPublic: true,
			IsActive: true,
			FundingOutpoint: wire.OutPoint{
				Index: 0,
			},
			RemoteBalance:  10,
			ShortChannelID: 0,
		}

		inactiveChannel = &HopHintInfo{
			IsPublic: false,
			IsActive: false,
		}

		// Create a private channel that we'll generate hints from.
		private1ShortID uint64 = 1
		privateChannel1        = &HopHintInfo{
			IsPublic: false,
			IsActive: true,
			FundingOutpoint: wire.OutPoint{
				Index: 1,
			},
			RemotePubkey:   pubkey,
			RemoteBalance:  100,
			ShortChannelID: private1ShortID,
		}

		// Create a edge policy for private channel 1.
		privateChan1Policy = &channeldb.ChannelEdgePolicy{
			FeeBaseMSat:               10,
			FeeProportionalMillionths: 100,
			TimeLockDelta:             1000,
		}

		// Create an edge policy different to ours which we'll use for
		// the other direction
		otherChanPolicy = &channeldb.ChannelEdgePolicy{
			FeeBaseMSat:               90,
			FeeProportionalMillionths: 900,
			TimeLockDelta:             9000,
		}

		// Create a hop hint based on privateChan1Policy.
		privateChannel1Hint = zpay32.HopHint{
			NodeID:      privateChannel1.RemotePubkey,
			ChannelID:   private1ShortID,
			FeeBaseMSat: uint32(privateChan1Policy.FeeBaseMSat),
			FeeProportionalMillionths: uint32(
				privateChan1Policy.FeeProportionalMillionths,
			),
			CLTVExpiryDelta: privateChan1Policy.TimeLockDelta,
		}

		// Create a second private channel that we'll use for hints.
		private2ShortID uint64 = 2
		privateChannel2        = &HopHintInfo{
			IsPublic: false,
			IsActive: true,
			FundingOutpoint: wire.OutPoint{
				Index: 2,
			},
			RemotePubkey:   pubkey,
			RemoteBalance:  100,
			ShortChannelID: private2ShortID,
		}

		// Create a edge policy for private channel 1.
		privateChan2Policy = &channeldb.ChannelEdgePolicy{
			FeeBaseMSat:               20,
			FeeProportionalMillionths: 200,
			TimeLockDelta:             2000,
		}

		// Create a hop hint based on privateChan2Policy.
		privateChannel2Hint = zpay32.HopHint{
			NodeID:      privateChannel2.RemotePubkey,
			ChannelID:   private2ShortID,
			FeeBaseMSat: uint32(privateChan2Policy.FeeBaseMSat),
			FeeProportionalMillionths: uint32(
				privateChan2Policy.FeeProportionalMillionths,
			),
			CLTVExpiryDelta: privateChan2Policy.TimeLockDelta,
		}

		// Create a third private channel that we'll use for hints.
		private3ShortID uint64 = 3
		privateChannel3        = &HopHintInfo{
			IsPublic: false,
			IsActive: true,
			FundingOutpoint: wire.OutPoint{
				Index: 3,
			},
			RemotePubkey:   pubkey,
			RemoteBalance:  100,
			ShortChannelID: private3ShortID,
		}

		// Create a edge policy for private channel 1.
		privateChan3Policy = &channeldb.ChannelEdgePolicy{
			FeeBaseMSat:               30,
			FeeProportionalMillionths: 300,
			TimeLockDelta:             3000,
		}

		// Create a hop hint based on privateChan2Policy.
		privateChannel3Hint = zpay32.HopHint{
			NodeID:      privateChannel3.RemotePubkey,
			ChannelID:   private3ShortID,
			FeeBaseMSat: uint32(privateChan3Policy.FeeBaseMSat),
			FeeProportionalMillionths: uint32(
				privateChan3Policy.FeeProportionalMillionths,
			),
			CLTVExpiryDelta: privateChan3Policy.TimeLockDelta,
		}
	)

	// We can't copy in the above var decls, so we copy in our pubkey here.
	var peer [33]byte
	copy(peer[:], compressed)

	var (
		// We pick our policy based on which node (1 or 2) the remote
		// peer is. Here we create two different sets of edge
		// information. One where our peer is node 1, the other where
		// our peer is edge 2. This ensures that we always pick the
		// right edge policy for our hint.
		infoNode1 = &channeldb.ChannelEdgeInfo{
			NodeKey1Bytes: peer,
		}

		infoNode2 = &channeldb.ChannelEdgeInfo{
			NodeKey1Bytes: [33]byte{9, 9, 9},
			NodeKey2Bytes: peer,
		}

		// setMockChannelUsed preps our mock for the case where we
		// want our private channel to be used for a hop hint.
		setMockChannelUsed = func(h *hopHintsConfigMock,
			shortID uint64,
			policy *channeldb.ChannelEdgePolicy) {

			// Return public node = true so that we'll consider
			// this node for our hop hints.
			h.Mock.On(
				"IsPublicNode", peer,
			).Once().Return(true, nil)

			// When it gets time to find an edge policy for this
			// node, fail it. We won't use it as a hop hint.
			h.Mock.On(
				"FetchChannelEdgesByID",
				shortID,
			).Once().Return(
				infoNode1, policy, otherChanPolicy, nil,
			)
		}
	)

	tests := []struct {
		name      string
		setupMock func(*hopHintsConfigMock)
		amount    lnwire.MilliSatoshi
		channels  []*HopHintInfo
		numHints  int

		// expectedHints is the set of hop hints that we expect. We
		// initialize this slice with our max hop hints length, so this
		// value won't be nil even if its empty.
		expectedHints [][]zpay32.HopHint
	}{
		{
			// We don't need hop hints for public channels.
			name: "channel is public",
			// When a channel is public, we exit before we make any
			// calls.
			setupMock: func(h *hopHintsConfigMock) {
			},
			amount: 100,
			channels: []*HopHintInfo{
				publicChannel,
			},
			numHints:      2,
			expectedHints: nil,
		},
		{
			name:      "channel is inactive",
			setupMock: func(h *hopHintsConfigMock) {},
			amount:    100,
			channels: []*HopHintInfo{
				inactiveChannel,
			},
			numHints:      2,
			expectedHints: nil,
		},
		{
			// If we can't lookup an edge policy, we skip channels.
			name: "no edge policy",
			setupMock: func(h *hopHintsConfigMock) {
				// Return public node = true so that we'll
				// consider this node for our hop hints.
				h.Mock.On(
					"IsPublicNode", peer,
				).Return(true, nil)

				// When it gets time to find an edge policy for
				// this node, fail it. We won't use it as a
				// hop hint.
				h.Mock.On(
					"FetchChannelEdgesByID",
					private1ShortID,
				).Return(
					nil, nil, nil,
					errors.New("no edge"),
				)
			},
			amount: 100,
			channels: []*HopHintInfo{
				privateChannel1,
			},
			numHints:      3,
			expectedHints: nil,
		},
		{
			// If one of our private channels belongs to a node
			// that is otherwise not announced to the network, we're
			// polite and don't include them (they can't be routed
			// through anyway).
			name: "node is private",
			setupMock: func(h *hopHintsConfigMock) {
				// Return public node = false so that we'll
				// give up on this node.
				h.Mock.On(
					"IsPublicNode", peer,
				).Return(false, nil)
			},
			amount: 100,
			channels: []*HopHintInfo{
				privateChannel1,
			},
			numHints:      1,
			expectedHints: nil,
		},
		{
			// This test case reproduces a bug where we have too
			// many hop hints for our maximum hint number.
			name: "too many hints",
			setupMock: func(h *hopHintsConfigMock) {
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)

				setMockChannelUsed(
					h, private2ShortID, privateChan2Policy,
				)

			},
			// Set our amount to less than our channel balance of
			// 100.
			amount: 30,
			channels: []*HopHintInfo{
				privateChannel1, privateChannel2,
			},
			numHints: 1,
			expectedHints: [][]zpay32.HopHint{
				{
					privateChannel1Hint,
				},
				{
					privateChannel2Hint,
				},
			},
		},
		{
			// If a channel has more balance than the amount we're
			// looking for, it'll be added in our first pass. We
			// can be sure we're adding it in our first pass because
			// we assert that there are no additional calls to our
			// mock (which would happen if we ran a second pass).
			//
			// We set our peer to be node 1 in our policy ordering.
			name: "balance > total amount, node 1",
			setupMock: func(h *hopHintsConfigMock) {
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)
			},
			// Our channel has balance of 100 (> 50).
			amount: 50,
			channels: []*HopHintInfo{
				privateChannel1,
			},
			numHints: 2,
			expectedHints: [][]zpay32.HopHint{
				{
					privateChannel1Hint,
				},
			},
		},
		{
			// As above, but we set our peer to be node 2 in our
			// policy ordering.
			name: "balance > total amount, node 2",
			setupMock: func(h *hopHintsConfigMock) {
				// Return public node = true so that we'll
				// consider this node for our hop hints.
				h.Mock.On(
					"IsPublicNode", peer,
				).Return(true, nil)

				// When it gets time to find an edge policy for
				// this node, fail it. We won't use it as a
				// hop hint.
				h.Mock.On(
					"FetchChannelEdgesByID",
					private1ShortID,
				).Return(
					infoNode2, otherChanPolicy,
					privateChan1Policy, nil,
				)
			},
			// Our channel has balance of 100 (> 50).
			amount: 50,
			channels: []*HopHintInfo{
				privateChannel1,
			},
			numHints: 2,
			expectedHints: [][]zpay32.HopHint{
				{
					privateChannel1Hint,
				},
			},
		},
		{
			// Since our balance is less than the amount we're
			// looking to route, we expect this hint to be picked
			// up in our second pass on the channel set.
			name: "balance < total amount",
			setupMock: func(h *hopHintsConfigMock) {
				// We expect to call all our checks twice
				// because we pick up this channel in the
				// second round.
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)
			},
			// Our channel has balance of 100 (< 150).
			amount: 150,
			channels: []*HopHintInfo{
				privateChannel1,
			},
			numHints: 2,
			expectedHints: [][]zpay32.HopHint{
				{
					privateChannel1Hint,
				},
			},
		},
		{
			// Test the case where we hit our total amount of
			// required liquidity in our first pass.
			name: "first pass sufficient balance",
			setupMock: func(h *hopHintsConfigMock) {
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)
			},
			// Divide our balance by hop hint factor so that the
			// channel balance will always reach our factored up
			// amount, even if we change this value.
			amount: privateChannel1.RemoteBalance / hopHintFactor,
			channels: []*HopHintInfo{
				privateChannel1,
			},
			numHints: 2,
			expectedHints: [][]zpay32.HopHint{
				{
					privateChannel1Hint,
				},
			},
		},
		{
			// Setup our amount so that we don't have enough
			// inbound total for our amount, but we hit our
			// desired hint limit.
			name: "second pass sufficient hint count",
			setupMock: func(h *hopHintsConfigMock) {
				// We expect all of our channels to be passed
				// on in the first pass.
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)

				setMockChannelUsed(
					h, private2ShortID, privateChan2Policy,
				)

				// In the second pass, our first two channels
				// should be added before we hit our hint count.
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)

			},
			// Add two channels that we'd want to use, but the
			// second one will be cut off due to our hop hint count
			// limit.
			channels: []*HopHintInfo{
				privateChannel1, privateChannel2,
			},
			// Set the amount we need to more than our two channels
			// can provide us.
			amount: privateChannel1.RemoteBalance +
				privateChannel2.RemoteBalance,
			numHints: 1,
			expectedHints: [][]zpay32.HopHint{
				{
					privateChannel1Hint,
				},
			},
		},
		{
			// Add three channels that are all less than the amount
			// we wish to receive, but collectively will reach the
			// total amount that we need.
			name: "second pass reaches bandwidth requirement",
			setupMock: func(h *hopHintsConfigMock) {
				// In the first round, all channels should be
				// passed on.
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)

				setMockChannelUsed(
					h, private2ShortID, privateChan2Policy,
				)

				setMockChannelUsed(
					h, private3ShortID, privateChan3Policy,
				)

				// In the second round, we'll pick up all of
				// our hop hints.
				setMockChannelUsed(
					h, private1ShortID, privateChan1Policy,
				)

				setMockChannelUsed(
					h, private2ShortID, privateChan2Policy,
				)

				setMockChannelUsed(
					h, private3ShortID, privateChan3Policy,
				)
			},
			channels: []*HopHintInfo{
				privateChannel1, privateChannel2,
				privateChannel3,
			},

			// All of our channels have 100 inbound, so none will
			// be picked up in the first round.
			amount:   110,
			numHints: 5,
			expectedHints: [][]zpay32.HopHint{
				{
					privateChannel1Hint,
				},
				{
					privateChannel2Hint,
				},
				{
					privateChannel3Hint,
				},
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Create mock and prime it for the test case.
			mock := &hopHintsConfigMock{}
			test.setupMock(mock)
			defer mock.AssertExpectations(t)

			cfg := &SelectHopHintsCfg{
				IsPublicNode:          mock.IsPublicNode,
				FetchChannelEdgesByID: mock.FetchChannelEdgesByID,
			}

			hints := SelectHopHints(
				test.amount, cfg, test.channels, test.numHints,
			)

			// SelectHopHints preallocates its hop hint slice, so
			// we check that it is empty if we don't expect any
			// hints, and otherwise assert that the two slices are
			// equal. This allows tests to set their expected value
			// to nil, rather than providing a preallocated empty
			// slice.
			if len(test.expectedHints) == 0 {
				require.Zero(t, len(hints))
			} else {
				require.Equal(t, test.expectedHints, hints)
			}
		})
	}
}
