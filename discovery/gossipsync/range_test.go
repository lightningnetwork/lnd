package gossipsync

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// testNow is the fixed clock used by pure tests.
var testNow = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// genGraph draws a sorted set of per-block channel ranges between heights
// [0, maxHeight], with fresh timestamps so the freshness filter keeps every
// channel.
func genGraph(t *rapid.T, maxHeight uint32,
	maxPerBlock int) []graphdb.BlockChannelRange {

	numBlocks := rapid.IntRange(0, 30).Draw(t, "numBlocks")
	heights := rapid.SliceOfNDistinct(
		rapid.Uint32Range(0, maxHeight), numBlocks, numBlocks,
		func(h uint32) uint32 { return h },
	).Draw(t, "heights")
	slices.Sort(heights)

	fresh := testNow.Add(-time.Hour)
	blocks := make([]graphdb.BlockChannelRange, 0, len(heights))
	for _, h := range heights {
		n := rapid.IntRange(1, maxPerBlock).Draw(t, "perBlock")
		chans := make([]graphdb.ChannelUpdateInfo, n)
		for i := range chans {
			scid := lnwire.ShortChannelID{
				BlockHeight: h,
				TxIndex:     uint32(i),
			}
			chans[i] = graphdb.NewV1ChannelUpdateInfo(
				scid, fresh, fresh,
			)
		}
		blocks = append(blocks, graphdb.BlockChannelRange{
			Height:   h,
			Channels: chans,
		})
	}

	return blocks
}

// testChunker returns a chunker for query with a seeded shuffle.
func testChunker(query *lnwire.QueryChannelRange, chunkSize int,
	seed uint64) *rangeChunker {

	rng := rand.New(rand.NewPCG(seed, seed))

	return &rangeChunker{
		query:          query,
		encoding:       lnwire.EncodingSortedPlain,
		chunkSize:      chunkSize,
		withTimestamps: query.WithTimestamps(),
		shuffle:        rng.Shuffle,
	}
}

// TestHonestStreamAccepted asserts that for any graph and chunk size, the
// stream our responder produces is accepted in full by our own initiator:
// the replies tile the query, the accumulator completes on exactly the final
// reply, and every channel that fit in a reply is buffered.
func TestHonestStreamAccepted(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		const maxHeight = 2_000

		blocks := genGraph(rt, maxHeight, 12)
		chunkSize := rapid.IntRange(2, 40).Draw(rt, "chunkSize")
		withTS := rapid.Bool().Draw(rt, "withTimestamps")

		query := &lnwire.QueryChannelRange{
			ChainHash:        chainhash.Hash{1},
			FirstBlockHeight: 0,
			NumBlocks:        maxHeight + 1,
		}
		if withTS {
			query.QueryOptions = lnwire.NewTimestampQueryOption()
		}

		chunker := testChunker(query, chunkSize, 7)
		limit := chunkSize
		if withTS {
			limit /= 2
		}

		var (
			acc     = newRangeAccumulator(query)
			sent    = []lnwire.ShortChannelID{}
			next    = query.FirstBlockHeight
			done    bool
			replies int
		)
		limits := DefaultRangeLimits()
		for reply := range chunker.replies(blocks) {
			require.False(rt, done, "reply after completion")
			replies++

			// Replies tile the query without gaps or overlap,
			// and each fits the chunk limit.
			require.Equal(rt, next, reply.FirstBlockHeight)
			require.LessOrEqual(rt, len(reply.ShortChanIDs), limit)
			next = reply.LastBlockHeight() + 1

			sent = append(sent, reply.ShortChanIDs...)

			var err error
			acc, done, err = acc.add(reply, limits, testNow)
			require.NoError(rt, err)
		}

		require.True(rt, done, "stream never completed")
		require.Equal(rt, query.LastBlockHeight()+1, next)
		require.Positive(rt, replies)

		got := make([]lnwire.ShortChannelID, len(acc.chans))
		for i, c := range acc.chans {
			got[i] = c.ShortChannelID
		}
		require.Equal(rt, sent, got)

		// Every channel of a block that fits in one reply was sent.
		for _, b := range blocks {
			if len(b.Channels) > limit {
				continue
			}
			for _, c := range b.Channels {
				require.Contains(rt, sent, c.ShortChannelID)
			}
		}
	})
}

// TestRangeReplyRejectsOutOfQuery asserts that replies outside the query, or
// that don't continue from the previous reply, are peer faults.
func TestRangeReplyRejectsOutOfQuery(t *testing.T) {
	t.Parallel()

	query := &lnwire.QueryChannelRange{
		FirstBlockHeight: 100,
		NumBlocks:        100,
	}
	limits := DefaultRangeLimits()

	cases := []struct {
		name  string
		first uint32
		num   uint32
		prev  *lnwire.ReplyChannelRange
	}{{
		name:  "starts before query",
		first: 99,
		num:   10,
	}, {
		name:  "ends after query",
		first: 150,
		num:   51,
	}, {
		name:  "first reply starts after query start",
		first: 150,
		num:   50,
	}, {
		name:  "gap after previous",
		first: 130,
		num:   10,
		prev: &lnwire.ReplyChannelRange{
			FirstBlockHeight: 100,
			NumBlocks:        20,
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newRangeAccumulator(query)
			acc.prev = tc.prev

			_, _, err := acc.add(&lnwire.ReplyChannelRange{
				FirstBlockHeight: tc.first,
				NumBlocks:        tc.num,
				EncodingType:     lnwire.EncodingSortedPlain,
			}, limits, testNow)
			require.ErrorIs(t, err, ErrInvalidRangeReply)
		})
	}

	// An unknown encoding is a fault too.
	acc := newRangeAccumulator(query)
	_, _, err := acc.add(&lnwire.ReplyChannelRange{
		FirstBlockHeight: 100,
		NumBlocks:        1,
		EncodingType:     lnwire.QueryEncoding(99),
	}, limits, testNow)
	require.ErrorIs(t, err, ErrInvalidRangeReply)
}

// TestRangeReplyBudgets asserts that the SCID limit is a fault, that the
// reply budget ends the stream, and that zlib replies cost more.
func TestRangeReplyBudgets(t *testing.T) {
	t.Parallel()

	query := &lnwire.QueryChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        1_000,
	}

	// Two replies of 3 SCIDs overflow a limit of 5.
	limits := RangeLimits{MaxReplies: 100, MaxSCIDs: 5}
	scids := func(h uint32) []lnwire.ShortChannelID {
		return []lnwire.ShortChannelID{
			{BlockHeight: h}, {BlockHeight: h, TxIndex: 1},
			{BlockHeight: h, TxIndex: 2},
		}
	}
	acc := newRangeAccumulator(query)
	acc, done, err := acc.add(&lnwire.ReplyChannelRange{
		FirstBlockHeight: 0, NumBlocks: 10,
		EncodingType: lnwire.EncodingSortedPlain,
		ShortChanIDs: scids(5),
	}, limits, testNow)
	require.NoError(t, err)
	require.False(t, done)

	_, _, err = acc.add(&lnwire.ReplyChannelRange{
		FirstBlockHeight: 10, NumBlocks: 10,
		EncodingType: lnwire.EncodingSortedPlain,
		ShortChanIDs: scids(15),
	}, limits, testNow)
	require.ErrorIs(t, err, ErrRangeReplyTooLarge)

	// A zlib reply spends zlibReplyWeight units of a budget of 4, so the
	// first one already ends the stream.
	limits = RangeLimits{MaxReplies: zlibReplyWeight, MaxSCIDs: 100}
	acc = newRangeAccumulator(query)
	_, done, err = acc.add(&lnwire.ReplyChannelRange{
		FirstBlockHeight: 0, NumBlocks: 10,
		EncodingType: lnwire.EncodingSortedZlib,
	}, limits, testNow)
	require.NoError(t, err)
	require.True(t, done)
}

// TestRangeReplyLegacy asserts that a legacy reply, which echoes the whole
// query, is only complete once it sets Complete.
func TestRangeReplyLegacy(t *testing.T) {
	t.Parallel()

	query := &lnwire.QueryChannelRange{
		ChainHash:        chainhash.Hash{2},
		FirstBlockHeight: 0,
		NumBlocks:        500,
	}
	echo := func(complete uint8) *lnwire.ReplyChannelRange {
		return &lnwire.ReplyChannelRange{
			ChainHash:        query.ChainHash,
			FirstBlockHeight: query.FirstBlockHeight,
			NumBlocks:        query.NumBlocks,
			Complete:         complete,
			EncodingType:     lnwire.EncodingSortedPlain,
		}
	}

	acc := newRangeAccumulator(query)
	acc, done, err := acc.add(echo(0), DefaultRangeLimits(), testNow)
	require.NoError(t, err)
	require.False(t, done)

	_, done, err = acc.add(echo(1), DefaultRangeLimits(), testNow)
	require.NoError(t, err)
	require.True(t, done)
}

// TestRangeReplyFreshness asserts that a channel is only skipped when both of
// its timestamps are outside the freshness horizon.
func TestRangeReplyFreshness(t *testing.T) {
	t.Parallel()

	limits := DefaultRangeLimits()
	stale := testNow.Add(-limits.FreshnessHorizon - time.Hour)
	skewed := testNow.Add(limits.FreshnessHorizon + time.Hour)
	fresh := testNow.Add(-time.Hour)

	cases := []struct {
		name   string
		t1, t2 time.Time
		keep   bool
	}{
		{"both fresh", fresh, fresh, true},
		{"one stale", stale, fresh, true},
		{"one skewed", fresh, skewed, true},
		{"both stale", stale, stale, false},
		{"both skewed", skewed, skewed, false},
		{"stale and skewed", stale, skewed, false},
	}
	for _, tc := range cases {
		require.Equal(
			t, !tc.keep,
			bothOutOfBounds(tc.t1, tc.t2, testNow, limits),
			tc.name,
		)
	}
}
