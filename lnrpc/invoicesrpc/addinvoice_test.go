package invoicesrpc

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	pubkeyBytes, _ = hex.DecodeString(
		"598ec453728e0ffe0ae2f5e174243cf58f2" +
			"a3f2c83d2457b43036db568b11093",
	)
	pubKeyY = new(btcec.FieldVal)
	_       = pubKeyY.SetByteSlice(pubkeyBytes)
	pubkey  = btcec.NewPublicKey(new(btcec.FieldVal).SetInt(4), pubKeyY)
)

type hopHintsConfigMock struct {
	t *testing.T
	mock.Mock
}

func newHopHintsConfigMock(t *testing.T) *hopHintsConfigMock {
	return &hopHintsConfigMock{t: t}
}

// IsPublicNode mocks node public state lookup.
func (h *hopHintsConfigMock) IsPublicNode(pubKey [33]byte) (bool, error) {
	args := h.Mock.Called(pubKey)
	return args.Bool(0), args.Error(1)
}

// IsChannelActive is used to generate valid hop hints.
func (h *hopHintsConfigMock) IsChannelActive(chanID lnwire.ChannelID) bool {
	args := h.Mock.Called(chanID)
	return args.Bool(0)
}

// GetAlias allows the peer's alias SCID to be retrieved for private
// option_scid_alias channels.
func (h *hopHintsConfigMock) GetAlias(
	chanID lnwire.ChannelID) (lnwire.ShortChannelID, error) {

	args := h.Mock.Called(chanID)
	return args.Get(0).(lnwire.ShortChannelID), args.Error(1)
}

// FetchAllChannels retrieves all open channels currently stored
// within the database.
func (h *hopHintsConfigMock) FetchAllChannels() ([]*channeldb.OpenChannel,
	error) {

	args := h.Mock.Called()
	return args.Get(0).([]*channeldb.OpenChannel), args.Error(1)
}

// FetchChannelEdgesByID attempts to lookup the two directed edges for
// the channel identified by the channel ID.
func (h *hopHintsConfigMock) FetchChannelEdgesByID(chanID uint64) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	args := h.Mock.Called(chanID)

	// If our error is non-nil, we expect nil responses otherwise. Our
	// casts below will fail with nil values, so we check our error and
	// return early on failure first.
	err := args.Error(3)
	if err != nil {
		return nil, nil, nil, err
	}

	edgeInfo, ok := args.Get(0).(*models.ChannelEdgeInfo)
	require.True(h.t, ok)

	policy1, ok := args.Get(1).(*models.ChannelEdgePolicy)
	require.True(h.t, ok)

	policy2, ok := args.Get(2).(*models.ChannelEdgePolicy)
	require.True(h.t, ok)

	return edgeInfo, policy1, policy2, nil
}

// getTestPubKey returns a valid parsed pub key to be used in our tests.
func getTestPubKey() *btcec.PublicKey {
	return pubkey
}

var shouldIncludeChannelTestCases = []struct {
	name            string
	setupMock       func(*hopHintsConfigMock)
	channel         *channeldb.OpenChannel
	alreadyIncluded map[uint64]bool
	cfg             *SelectHopHintsCfg
	hopHint         zpay32.HopHint
	remoteBalance   lnwire.MilliSatoshi
	include         bool
}{{
	name: "already included channels should not be included " +
		"again",
	alreadyIncluded: map[uint64]bool{1: true},
	channel: &channeldb.OpenChannel{
		ShortChannelID: lnwire.NewShortChanIDFromInt(1),
	},
	include: false,
}, {
	name: "public channels should not be included",
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 0,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)
		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 0,
		},
		ChannelFlags: lnwire.FFAnnounceChannel,
	},
}, {
	name: "not active channels should not be included",
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 0,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)
		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(false)
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 0,
		},
	},
	include: false,
}, {
	name: "a channel with a not public peer should not be included",
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 0,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(false, nil)
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 0,
		},
		IdentityPub: getTestPubKey(),
	},
	include: false,
}, {
	name: "if we are unable to fetch the edge policy for the channel it " +
		"should not be included",
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 0,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(nil, nil, nil, fmt.Errorf("no edge"))

		// TODO(positiveblue): check that the func is called with the
		// right scid when we have access to the `confirmedscid` form
		// here.
		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(nil, nil, nil, fmt.Errorf("no edge"))
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 0,
		},
		IdentityPub: getTestPubKey(),
	},
	include: false,
}, {
	name: "channels with the option-scid-alias but not assigned alias " +
		"yet should not be included",
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 0,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)

		h.Mock.On(
			"GetAlias", mock.Anything,
		).Once().Return(lnwire.ShortChannelID{}, nil)
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 0,
		},
		IdentityPub: getTestPubKey(),
		ChanType:    channeldb.ScidAliasFeatureBit,
	},
	include: false,
}, {
	name: "channels with the option-scid-alias and an alias that has " +
		"already been included should not be included again",
	alreadyIncluded: map[uint64]bool{5: true},
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 0,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)
		alias := lnwire.ShortChannelID{TxPosition: 5}
		h.Mock.On(
			"GetAlias", mock.Anything,
		).Once().Return(alias, nil)
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 0,
		},
		IdentityPub: getTestPubKey(),
		ChanType:    channeldb.ScidAliasFeatureBit,
	},
	include: false,
}, {
	name: "channels that pass all the checks should be " +
		"included, using policy 1",
	alreadyIncluded: map[uint64]bool{5: true},
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 1,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		var selectedPolicy [33]byte
		copy(selectedPolicy[:], getTestPubKey().SerializeCompressed())

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{
				NodeKey1Bytes: selectedPolicy,
			},
			&models.ChannelEdgePolicy{
				FeeBaseMSat:               1000,
				FeeProportionalMillionths: 20,
				TimeLockDelta:             13,
			},
			&models.ChannelEdgePolicy{},
			nil,
		)
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 1,
		},
		IdentityPub:    getTestPubKey(),
		ShortChannelID: lnwire.NewShortChanIDFromInt(12),
	},
	hopHint: zpay32.HopHint{
		NodeID:                    getTestPubKey(),
		FeeBaseMSat:               1000,
		FeeProportionalMillionths: 20,
		ChannelID:                 12,
		CLTVExpiryDelta:           13,
	},
	include: true,
}, {
	name: "channels that pass all the checks should be " +
		"included, using policy 2",
	alreadyIncluded: map[uint64]bool{5: true},
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 1,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{
				FeeBaseMSat:               1000,
				FeeProportionalMillionths: 20,
				TimeLockDelta:             13,
			}, nil,
		)
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 1,
		},
		IdentityPub:    getTestPubKey(),
		ShortChannelID: lnwire.NewShortChanIDFromInt(12),
	},
	hopHint: zpay32.HopHint{
		NodeID:                    getTestPubKey(),
		FeeBaseMSat:               1000,
		FeeProportionalMillionths: 20,
		ChannelID:                 12,
		CLTVExpiryDelta:           13,
	},
	include: true,
}, {
	name: "channels that pass all the checks and have an alias " +
		"should be included with the alias",
	alreadyIncluded: map[uint64]bool{5: true},
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{
			Index: 1,
		}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{
				FeeBaseMSat:               1000,
				FeeProportionalMillionths: 20,
				TimeLockDelta:             13,
			}, nil,
		)

		aliasSCID := lnwire.NewShortChanIDFromInt(15)

		h.Mock.On(
			"GetAlias", mock.Anything,
		).Once().Return(aliasSCID, nil)
	},
	channel: &channeldb.OpenChannel{
		FundingOutpoint: wire.OutPoint{
			Index: 1,
		},
		IdentityPub:    getTestPubKey(),
		ShortChannelID: lnwire.NewShortChanIDFromInt(12),
		ChanType:       channeldb.ScidAliasFeatureBit,
	},
	hopHint: zpay32.HopHint{
		NodeID:                    getTestPubKey(),
		FeeBaseMSat:               1000,
		FeeProportionalMillionths: 20,
		ChannelID:                 15,
		CLTVExpiryDelta:           13,
	},
	include: true,
}}

func TestShouldIncludeChannel(t *testing.T) {
	for _, tc := range shouldIncludeChannelTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create mock and prime it for the test case.
			mock := newHopHintsConfigMock(t)
			if tc.setupMock != nil {
				tc.setupMock(mock)
			}
			defer mock.AssertExpectations(t)

			cfg := &SelectHopHintsCfg{
				IsPublicNode:          mock.IsPublicNode,
				IsChannelActive:       mock.IsChannelActive,
				FetchChannelEdgesByID: mock.FetchChannelEdgesByID,
				GetAlias:              mock.GetAlias,
			}

			hopHint, remoteBalance, include := shouldIncludeChannel(
				cfg, tc.channel, tc.alreadyIncluded,
			)

			require.Equal(t, tc.include, include)
			if include {
				require.Equal(t, tc.hopHint, hopHint)
				require.Equal(
					t, tc.remoteBalance, remoteBalance,
				)
			}
		})
	}
}

var sufficientHintsTestCases = []struct {
	name          string
	nHintsLeft    int
	currentAmount lnwire.MilliSatoshi
	targetAmount  lnwire.MilliSatoshi
	done          bool
}{{
	name:          "not enough hints neither bandwidth",
	nHintsLeft:    3,
	currentAmount: 100,
	targetAmount:  200,
	done:          false,
}, {
	name:       "enough hints",
	nHintsLeft: 0,
	done:       true,
}, {
	name:          "enough bandwidth",
	nHintsLeft:    1,
	currentAmount: 200,
	targetAmount:  200,
	done:          true,
}, {
	name:          "no amount provided",
	nHintsLeft:    1,
	currentAmount: 100,
	targetAmount:  0,
	done:          false,
}}

func TestSufficientHints(t *testing.T) {
	for _, tc := range sufficientHintsTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			enoughHints := sufficientHints(
				tc.nHintsLeft, tc.currentAmount,
				tc.targetAmount,
			)
			require.Equal(t, tc.done, enoughHints)
		})
	}
}

var populateHopHintsTestCases = []struct {
	name             string
	setupMock        func(*hopHintsConfigMock)
	amount           lnwire.MilliSatoshi
	maxHopHints      int
	forcedHints      [][]zpay32.HopHint
	expectedHopHints [][]zpay32.HopHint
}{{
	name:        "populate hop hints with forced hints",
	maxHopHints: 1,
	forcedHints: [][]zpay32.HopHint{
		{
			{ChannelID: 12},
		},
	},
	expectedHopHints: [][]zpay32.HopHint{
		{
			{ChannelID: 12},
		},
	},
}, {
	name: "populate hop hints stops when we reached the max number of " +
		"hop hints allowed",
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{Index: 9}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)
		allChannels := []*channeldb.OpenChannel{
			{
				FundingOutpoint: fundingOutpoint,
				ShortChannelID:  lnwire.NewShortChanIDFromInt(9),
				IdentityPub:     getTestPubKey(),
			},
			// Have one empty channel that we should not process
			// because we have already finished.
			{},
		}

		h.Mock.On(
			"FetchAllChannels",
		).Once().Return(allChannels, nil)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)
	},
	maxHopHints: 1,
	amount:      1_000_000,
	expectedHopHints: [][]zpay32.HopHint{
		{
			{
				NodeID:    getTestPubKey(),
				ChannelID: 9,
			},
		},
	},
}, {
	name: "populate hop hints stops when we reached the targeted bandwidth",
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{Index: 9}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)
		remoteBalance := lnwire.MilliSatoshi(10_000_000)
		allChannels := []*channeldb.OpenChannel{
			{
				LocalCommitment: channeldb.ChannelCommitment{
					RemoteBalance: remoteBalance,
				},
				FundingOutpoint: fundingOutpoint,
				ShortChannelID:  lnwire.NewShortChanIDFromInt(9),
				IdentityPub:     getTestPubKey(),
			},
			// Have one empty channel that we should not process
			// because we have already finished.
			{},
		}

		h.Mock.On(
			"FetchAllChannels",
		).Once().Return(allChannels, nil)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)
	},
	maxHopHints: 10,
	amount:      1_000_000,
	expectedHopHints: [][]zpay32.HopHint{
		{
			{
				NodeID:    getTestPubKey(),
				ChannelID: 9,
			},
		},
	},
}, {
	name: "populate hop hints tries to use the channels with higher " +
		"remote balance frist",
	setupMock: func(h *hopHintsConfigMock) {
		fundingOutpoint := wire.OutPoint{Index: 9}
		chanID := lnwire.NewChanIDFromOutPoint(fundingOutpoint)
		remoteBalance := lnwire.MilliSatoshi(10_000_000)
		allChannels := []*channeldb.OpenChannel{
			// Because the channels with higher remote balance have
			// enough bandwidth we should never use this one.
			{},
			{
				LocalCommitment: channeldb.ChannelCommitment{
					RemoteBalance: remoteBalance,
				},
				FundingOutpoint: fundingOutpoint,
				ShortChannelID:  lnwire.NewShortChanIDFromInt(9),
				IdentityPub:     getTestPubKey(),
			},
		}

		h.Mock.On(
			"FetchAllChannels",
		).Once().Return(allChannels, nil)

		h.Mock.On(
			"IsChannelActive", chanID,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)
	},
	maxHopHints: 1,
	amount:      1_000_000,
	expectedHopHints: [][]zpay32.HopHint{
		{
			{
				NodeID:    getTestPubKey(),
				ChannelID: 9,
			},
		},
	},
}, {
	name: "populate hop hints stops after having considered all the open " +
		"channels",
	setupMock: func(h *hopHintsConfigMock) {
		chanID1, chanID2 := setupMockTwoChannels(h)

		// Prepare the mock for the first channel.
		h.Mock.On(
			"IsChannelActive", chanID1,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)

		// Prepare the mock for the second channel.
		h.Mock.On(
			"IsChannelActive", chanID2,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)
	},
	maxHopHints: 10,
	amount:      100_000_000,
	expectedHopHints: [][]zpay32.HopHint{
		{
			{
				NodeID:    getTestPubKey(),
				ChannelID: 9,
			},
		}, {
			{
				NodeID:    getTestPubKey(),
				ChannelID: 2,
			},
		},
	},
}, {
	name: "consider all the open channels when amount is zero",
	setupMock: func(h *hopHintsConfigMock) {
		chanID1, chanID2 := setupMockTwoChannels(h)

		// Prepare the mock for the first channel.
		h.Mock.On(
			"IsChannelActive", chanID1,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)

		// Prepare the mock for the second channel.
		h.Mock.On(
			"IsChannelActive", chanID2,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)
	},
	maxHopHints: 10,
	amount:      0,
	expectedHopHints: [][]zpay32.HopHint{
		{
			{
				NodeID:    getTestPubKey(),
				ChannelID: 9,
			},
		}, {
			{
				NodeID:    getTestPubKey(),
				ChannelID: 2,
			},
		},
	},
}, {
	name: "consider all the open channels when amount is zero" +
		" up to maxHopHints",
	setupMock: func(h *hopHintsConfigMock) {
		chanID1, _ := setupMockTwoChannels(h)

		// Prepare the mock for the first channel.
		h.Mock.On(
			"IsChannelActive", chanID1,
		).Once().Return(true)

		h.Mock.On(
			"IsPublicNode", mock.Anything,
		).Once().Return(true, nil)

		h.Mock.On(
			"FetchChannelEdgesByID", mock.Anything,
		).Once().Return(
			&models.ChannelEdgeInfo{},
			&models.ChannelEdgePolicy{},
			&models.ChannelEdgePolicy{}, nil,
		)
	},
	maxHopHints: 1,
	amount:      0,
	expectedHopHints: [][]zpay32.HopHint{
		{
			{
				NodeID:    getTestPubKey(),
				ChannelID: 9,
			},
		},
	},
}}

func setupMockTwoChannels(h *hopHintsConfigMock) (lnwire.ChannelID,
	lnwire.ChannelID) {

	fundingOutpoint1 := wire.OutPoint{Index: 9}
	chanID1 := lnwire.NewChanIDFromOutPoint(fundingOutpoint1)
	remoteBalance1 := lnwire.MilliSatoshi(10_000_000)

	fundingOutpoint2 := wire.OutPoint{Index: 2}
	chanID2 := lnwire.NewChanIDFromOutPoint(fundingOutpoint2)
	remoteBalance2 := lnwire.MilliSatoshi(1_000_000)

	allChannels := []*channeldb.OpenChannel{
		// After sorting we will first process chanID1 and then
		// chanID2.
		{
			LocalCommitment: channeldb.ChannelCommitment{
				RemoteBalance: remoteBalance2,
			},
			FundingOutpoint: fundingOutpoint2,
			ShortChannelID:  lnwire.NewShortChanIDFromInt(2),
			IdentityPub:     getTestPubKey(),
		},
		{
			LocalCommitment: channeldb.ChannelCommitment{
				RemoteBalance: remoteBalance1,
			},
			FundingOutpoint: fundingOutpoint1,
			ShortChannelID:  lnwire.NewShortChanIDFromInt(9),
			IdentityPub:     getTestPubKey(),
		},
	}

	h.Mock.On(
		"FetchAllChannels",
	).Once().Return(allChannels, nil)

	return chanID1, chanID2
}

func TestPopulateHopHints(t *testing.T) {
	for _, tc := range populateHopHintsTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create mock and prime it for the test case.
			mock := newHopHintsConfigMock(t)
			if tc.setupMock != nil {
				tc.setupMock(mock)
			}
			defer mock.AssertExpectations(t)

			cfg := &SelectHopHintsCfg{
				IsPublicNode:          mock.IsPublicNode,
				IsChannelActive:       mock.IsChannelActive,
				FetchChannelEdgesByID: mock.FetchChannelEdgesByID,
				GetAlias:              mock.GetAlias,
				FetchAllChannels:      mock.FetchAllChannels,
				MaxHopHints:           tc.maxHopHints,
			}
			hopHints, err := PopulateHopHints(
				cfg, tc.amount, tc.forcedHints,
			)
			require.NoError(t, err)
			// We shuffle the elements in the hop hint list so we
			// need to compare the elements here.
			require.ElementsMatch(t, tc.expectedHopHints, hopHints)
		})
	}
}
