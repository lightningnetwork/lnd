package channeldb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// oldCountPublicAnchorChannels reproduces the pre-optimization
// CurrentNumAnchorChans computation: it fully reads every open channel via
// FetchAllChannels and every pending-close channel via FetchHistoricalChannel,
// then counts those that are both public (announced) and anchor channels. It
// is the oracle the new CountPublicAnchorChannels is compared against.
func oldCountPublicAnchorChannels(tb testing.TB, cdb *ChannelStateDB) int {
	tb.Helper()

	cnt := func(c *OpenChannel) int {
		// Skip private channels, as they aren't used for routing.
		if c.ChannelFlags&lnwire.FFAnnounceChannel == 0 {
			return 0
		}
		if c.ChanType.HasAnchors() {
			return 1
		}

		return 0
	}

	var numAnchors int

	// A database that never held a channel has no open-channel bucket, in
	// which case both the old and new code surface ErrNoActiveChannels;
	// treat it as zero open channels for the comparison.
	chans, err := cdb.FetchAllChannels()
	if err != nil && !errors.Is(err, ErrNoActiveChannels) {
		require.NoError(tb, err)
	}
	for _, c := range chans {
		numAnchors += cnt(c)
	}

	pendingClose, err := cdb.FetchClosedChannels(true)
	require.NoError(tb, err)
	for _, summary := range pendingClose {
		histChan, err := cdb.FetchHistoricalChannel(&summary.ChanPoint)
		if err != nil {
			// Not every channel is guaranteed to be in the
			// historical bucket, so skip the ones we can't read.
			continue
		}
		numAnchors += cnt(histChan)
	}

	return numAnchors
}

// makeCountAnchorChannel creates and persists a channel with the requested
// visibility and anchor type. When pendingClose is set, the opened channel is
// moved into the pending-close (historical) bucket, which is where
// FetchClosedChannels(true) and the historical lookup find it.
func makeCountAnchorChannel(tb testing.TB, cdb *ChannelStateDB,
	public, anchor, pendingClose bool) {

	tb.Helper()

	opts := []testChannelOption{openChannelOption()}
	if anchor {
		opts = append(opts, anchorOption())
	}
	if public {
		opts = append(opts, publicChannelOption())
	}

	channel := createTestChannel(tb, cdb, opts...)

	if !pendingClose {
		return
	}

	// Closing with IsPending set leaves a summary that
	// FetchClosedChannels(true) returns, and archives the channel state
	// under the historical bucket. The revocation fields are the minimum
	// required for the close to serialize without panicking.
	err := channel.CloseChannel(&ChannelCloseSummary{
		ChanPoint:               channel.FundingOutpoint,
		RemotePub:               channel.IdentityPub,
		RemoteCurrentRevocation: channel.IdentityPub,
		RemoteNextRevocation:    fn.Some(*channel.IdentityPub),
		SettledBalance:          btcutil.Amount(500),
		IsPending:               true,
	})
	require.NoError(tb, err)
}

// TestCountPublicAnchorChannels verifies that CountPublicAnchorChannels counts
// exactly the open and pending-close channels that are both public and anchor
// channels, and that it agrees with the full-read computation it replaced.
func TestCountPublicAnchorChannels(t *testing.T) {
	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Build a mix spanning every combination of visibility, anchor type,
	// and lifecycle state. Only public anchor channels -- whether open or
	// pending close -- should be counted.
	categories := []struct {
		public, anchor, pendingClose bool
		num                          int
		counted                      bool
	}{
		{true, true, false, 3, true},    // public anchor open
		{true, true, true, 2, true},     // public anchor closing
		{false, true, false, 4, false},  // private anchor open
		{true, false, false, 5, false},  // public non-anchor open
		{false, false, false, 2, false}, // private non-anchor open
		{false, true, true, 3, false},   // private anchor closing
		{true, false, true, 2, false},   // public non-anchor closing
	}

	var expected int
	for _, c := range categories {
		for range c.num {
			makeCountAnchorChannel(
				t, cdb, c.public, c.anchor, c.pendingClose,
			)
		}
		if c.counted {
			expected += c.num
		}
	}

	// The new count must match both the hand-computed expectation and the
	// original full-read computation it replaced.
	got, err := cdb.CountPublicAnchorChannels()
	require.NoError(t, err)
	require.Equal(t, expected, got, "unexpected public anchor count")

	require.Equal(
		t, oldCountPublicAnchorChannels(t, cdb), got,
		"new count disagrees with the full-read computation",
	)
}

// BenchmarkPostgresCountPublicAnchorChannels measures how counting the public
// anchor channels scales with the number of channels. The old path fully
// deserialized every channel (commitments and revocation state included); the
// new path reads only each channel's info record, so on a remote SQL backend
// it avoids the per-channel round trips the full read required.
func BenchmarkPostgresCountPublicAnchorChannels(b *testing.B) {
	if !kvdb.PostgresBackend {
		b.Skip("requires the kvdb_postgres build tag")
	}

	// The embedded Postgres instance is started once for the whole package
	// by TestMain (kvdb.RunTests), so GetTestBackend below just connects to
	// it; starting another here would collide on its port.
	for _, numChans := range []int{1, 10, 100, 1000} {
		backend, cleanup, err := kvdb.GetTestBackend(b.TempDir(), "cdb")
		require.NoError(b, err)

		fullDB, err := CreateWithBackend(backend)
		require.NoError(b, err)

		cdb := fullDB.ChannelStateDB()

		// Populate public anchor channels so both paths have the same
		// counting work to do; the difference is only how much of each
		// channel they read.
		for range numChans {
			makeCountAnchorChannel(b, cdb, true, true, false)
		}

		b.Run(fmt.Sprintf("chans=%d/old_fullRead", numChans),
			func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					oldCountPublicAnchorChannels(b, cdb)
				}
			},
		)

		b.Run(fmt.Sprintf("chans=%d/new_infoOnly", numChans),
			func(b *testing.B) {
				b.ReportAllocs()
				count := cdb.CountPublicAnchorChannels
				for range b.N {
					_, err := count()
					require.NoError(b, err)
				}
			},
		)

		require.NoError(b, fullDB.Close())
		cleanup()
	}
}
