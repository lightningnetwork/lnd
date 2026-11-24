package lnd

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockClosedChannelSource is a closedChannelSource backed by testify/mock.
type mockClosedChannelSource struct {
	mock.Mock
}

// FetchClosedChannel returns the mocked close summary for the outpoint.
func (m *mockClosedChannelSource) FetchClosedChannel(chanID *wire.OutPoint) (
	*channeldb.ChannelCloseSummary, error) {

	args := m.Called(*chanID)

	summary, _ := args.Get(0).(*channeldb.ChannelCloseSummary)

	return summary, args.Error(1)
}

// FetchClosedChannels returns the mocked close summaries.
func (m *mockClosedChannelSource) FetchClosedChannels(pendingOnly bool) (
	[]*channeldb.ChannelCloseSummary, error) {

	args := m.Called(pendingOnly)

	summaries, _ := args.Get(0).([]*channeldb.ChannelCloseSummary)

	return summaries, args.Error(1)
}

// TestIsClosedChannel checks that isClosedChannel looks a channel up by its
// funding outpoint when it has one, and falls back to matching its short
// channel ID against all close summaries when it doesn't.
func TestIsClosedChannel(t *testing.T) {
	t.Parallel()

	var (
		chanPoint = wire.OutPoint{Hash: [32]byte{1}, Index: 1}
		scid      = lnwire.NewShortChanIDFromInt(1234)
		otherSCID = lnwire.NewShortChanIDFromInt(5678)
		errDB     = errors.New("db error")
	)

	closedSummaries := []*channeldb.ChannelCloseSummary{
		{ShortChanID: otherSCID},
		{ShortChanID: scid},
	}

	tests := []struct {
		name      string
		chanPoint wire.OutPoint
		scid      lnwire.ShortChannelID
		setup     func(m *mockClosedChannelSource)
		closed    bool
		err       error
	}{
		{
			name:      "outpoint closed",
			chanPoint: chanPoint,
			scid:      scid,
			setup: func(m *mockClosedChannelSource) {
				m.On("FetchClosedChannel", chanPoint).Return(
					&channeldb.ChannelCloseSummary{}, nil,
				)
			},
			closed: true,
		},
		{
			name:      "outpoint not closed",
			chanPoint: chanPoint,
			scid:      scid,
			setup: func(m *mockClosedChannelSource) {
				m.On("FetchClosedChannel", chanPoint).Return(
					nil, channeldb.ErrClosedChannelNotFound,
				)
			},
		},
		{
			name:      "outpoint lookup error",
			chanPoint: chanPoint,
			scid:      scid,
			setup: func(m *mockClosedChannelSource) {
				m.On("FetchClosedChannel", chanPoint).Return(
					nil, errDB,
				)
			},
			err: errDB,
		},
		{
			name: "no outpoint, scid closed",
			scid: scid,
			setup: func(m *mockClosedChannelSource) {
				m.On("FetchClosedChannels", false).Return(
					closedSummaries, nil,
				)
			},
			closed: true,
		},
		{
			name: "no outpoint, scid not closed",
			scid: lnwire.NewShortChanIDFromInt(9999),
			setup: func(m *mockClosedChannelSource) {
				m.On("FetchClosedChannels", false).Return(
					closedSummaries, nil,
				)
			},
		},
		{
			name: "no outpoint, no closed channels",
			scid: scid,
			setup: func(m *mockClosedChannelSource) {
				m.On("FetchClosedChannels", false).Return(
					nil, channeldb.ErrNoClosedChannels,
				)
			},
		},
		{
			name: "no outpoint, scan error",
			scid: scid,
			setup: func(m *mockClosedChannelSource) {
				m.On("FetchClosedChannels", false).Return(
					nil, errDB,
				)
			},
			err: errDB,
		},
		{
			// A zero short channel ID must never match, as a
			// channel whose funding never confirmed can be
			// archived with an all-zero ID, so the db isn't
			// consulted.
			name:  "no outpoint, zero scid",
			setup: func(m *mockClosedChannelSource) {},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := &mockClosedChannelSource{}
			tc.setup(db)

			closed, err := isClosedChannel(
				db, tc.chanPoint, tc.scid,
			)
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.closed, closed)

			db.AssertExpectations(t)
		})
	}
}
