package gossipsync

import (
	"errors"
	"fmt"
	"slices"
	"time"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// DefaultMaxRangeReplies is the default budget of reply_channel_range
	// messages we'll process for a single query_channel_range. Hitting the
	// budget ends the stream as if the final reply had arrived.
	DefaultMaxRangeReplies = 500

	// zlibReplyWeight is the budget charged for a zlib encoded reply. A
	// compressed reply can carry far more SCIDs per byte, so it is charged
	// more than a plain one.
	zlibReplyWeight = 4

	// DefaultMaxRangeReplySCIDs is the default limit on the total number of
	// SCIDs we'll accept across one reply stream. A peer that exceeds it is
	// treated as faulty.
	DefaultMaxRangeReplySCIDs = 100_000

	// DefaultFreshnessHorizon is how far a channel update timestamp may
	// sit in the past or the future before we consider it stale or skewed.
	// It matches the graph's default channel prune expiry.
	DefaultFreshnessHorizon = 14 * 24 * time.Hour
)

var (
	// ErrInvalidRangeReply is returned when a peer's reply_channel_range
	// does not match the query that prompted it, or uses an encoding we
	// don't support. It is always a peer fault.
	ErrInvalidRangeReply = errors.New("invalid channel range reply")

	// ErrRangeReplyTooLarge is returned when a peer's reply stream carries
	// more SCIDs than we'll accept for one query. It is always a peer
	// fault.
	ErrRangeReplyTooLarge = errors.New("channel range reply exceeds " +
		"maximum number of short channel IDs")
)

// RangeLimits bounds the resources a single reply stream may consume.
type RangeLimits struct {
	// MaxReplies is the reply budget for one query. Plain replies cost
	// one unit, and zlib replies cost zlibReplyWeight units.
	MaxReplies uint32

	// MaxSCIDs is the maximum number of SCIDs accepted across the whole
	// stream.
	MaxSCIDs uint32

	// FreshnessHorizon is how far into the past or future both of a
	// channel's update timestamps may be before we skip the channel.
	FreshnessHorizon time.Duration
}

// DefaultRangeLimits returns the limits lnd has always applied.
func DefaultRangeLimits() RangeLimits {
	return RangeLimits{
		MaxReplies:       DefaultMaxRangeReplies,
		MaxSCIDs:         DefaultMaxRangeReplySCIDs,
		FreshnessHorizon: DefaultFreshnessHorizon,
	}
}

// rangeAccumulator collects the replies to one query_channel_range. It
// validates each reply against the query and the previous reply, charges the
// reply and SCID budgets, and buffers the channels worth asking about.
//
// The accumulator is a value, and add returns an updated copy. The channel
// buffer is shared between the copies rather than cloned, since the state
// that owns the accumulator discards the old copy on every transition.
type rangeAccumulator struct {
	// query is the query the replies answer.
	query *lnwire.QueryChannelRange

	// prev is the last reply accepted, used to check continuity.
	prev *lnwire.ReplyChannelRange

	// chans are the buffered channels, after the freshness filter.
	chans []graphdb.ChannelUpdateInfo

	// replyBudgetUsed is the weighted number of replies accepted.
	replyBudgetUsed uint32

	// scids is the number of SCIDs accepted, before the freshness filter.
	scids uint32
}

// newRangeAccumulator returns an empty accumulator for query.
func newRangeAccumulator(query *lnwire.QueryChannelRange) rangeAccumulator {
	return rangeAccumulator{query: query}
}

// isLegacyReply reports whether reply echoes the whole query instead of the
// portion it covers. Old lnd nodes did this, and for them only the Complete
// flag tells us when the stream has ended.
func isLegacyReply(query *lnwire.QueryChannelRange,
	reply *lnwire.ReplyChannelRange) bool {

	return reply.ChainHash == query.ChainHash &&
		reply.FirstBlockHeight == query.FirstBlockHeight &&
		reply.NumBlocks == query.NumBlocks
}

// add validates reply and folds it into the accumulator. It returns the
// updated accumulator and whether the stream is now complete, either because
// the reply covers the query's last block (or sets Complete, for a legacy
// peer), or because the reply budget is exhausted. Any error wraps
// ErrInvalidRangeReply or ErrRangeReplyTooLarge, and means the peer broke
// the protocol.
func (a rangeAccumulator) add(reply *lnwire.ReplyChannelRange,
	limits RangeLimits, now time.Time) (rangeAccumulator, bool, error) {

	legacy := isLegacyReply(a.query, reply)

	// A non-legacy reply must describe a slice of our query, and must
	// continue from the previous reply.
	if !legacy {
		if err := a.checkRange(reply); err != nil {
			return a, false, err
		}
	}

	// Charge the reply budget using the encoding that was actually
	// received. Our configured encoding is a local preference and says
	// nothing about the responder's message.
	var weight uint32
	switch reply.EncodingType {
	case lnwire.EncodingSortedPlain:
		weight = 1

	case lnwire.EncodingSortedZlib:
		weight = zlibReplyWeight

	default:
		return a, false, fmt.Errorf("%w: unhandled encoding type %v",
			ErrInvalidRangeReply, reply.EncodingType)
	}

	// The subtraction form keeps the check free of overflow.
	numSCIDs := uint32(len(reply.ShortChanIDs))
	if a.scids > limits.MaxSCIDs || numSCIDs > limits.MaxSCIDs-a.scids {
		return a, false, fmt.Errorf("%w: max=%v", ErrRangeReplyTooLarge,
			limits.MaxSCIDs)
	}

	a.replyBudgetUsed += weight
	a.scids += numSCIDs
	a.prev = reply
	a.chans = slices.Grow(a.chans, int(numSCIDs))

	// The wire decoder guarantees that a non-empty timestamp list has one
	// entry per SCID.
	for i, scid := range reply.ShortChanIDs {
		info := graphdb.NewV1ChannelUpdateInfo(
			scid, time.Time{}, time.Time{},
		)

		if len(reply.Timestamps) != 0 {
			ts := reply.Timestamps[i]
			info.Node1Freshness = lnwire.UnixTimestamp(
				ts.Timestamp1,
			)
			info.Node2Freshness = lnwire.UnixTimestamp(
				ts.Timestamp2,
			)

			t1 := time.Unix(int64(ts.Timestamp1), 0)
			t2 := time.Unix(int64(ts.Timestamp2), 0)
			if bothOutOfBounds(t1, t2, now, limits) {
				continue
			}
		}

		a.chans = append(a.chans, info)
	}

	return a, a.complete(reply, legacy, limits), nil
}

// checkRange verifies that a non-legacy reply stays within the query and
// continues from the previous reply.
func (a rangeAccumulator) checkRange(reply *lnwire.ReplyChannelRange) error {
	if reply.FirstBlockHeight < a.query.FirstBlockHeight {
		return fmt.Errorf("%w: reply includes channels for height %v "+
			"prior to query %v", ErrInvalidRangeReply,
			reply.FirstBlockHeight, a.query.FirstBlockHeight)
	}

	// Checking the last block is enough, since the SCIDs within a reply
	// are sorted.
	replyLast := reply.LastBlockHeight()
	queryLast := a.query.LastBlockHeight()
	if replyLast > queryLast {
		return fmt.Errorf("%w: reply includes channels for height %v "+
			"after query %v", ErrInvalidRangeReply, replyLast,
			queryLast)
	}

	// BOLT 7 requires the first reply to start at or before the query's
	// first block, and we've just required it not to start before, so it
	// must start exactly there. Without this, the tail of an earlier
	// stream that arrives late would be accepted as a complete answer to
	// a new query, since it ends on the same last block.
	if a.prev == nil {
		if reply.FirstBlockHeight != a.query.FirstBlockHeight {
			return fmt.Errorf("%w: first reply starts at height "+
				"%v, not at query start %v",
				ErrInvalidRangeReply, reply.FirstBlockHeight,
				a.query.FirstBlockHeight)
		}

		return nil
	}

	// A reply either continues the previous reply's last block, when a
	// block's channels span replies, or starts on the block after it.
	prevLast := a.prev.LastBlockHeight()
	if reply.FirstBlockHeight != prevLast &&
		reply.FirstBlockHeight != prevLast+1 {

		return fmt.Errorf("%w: first block of reply %v does not "+
			"continue from last block of previous %v",
			ErrInvalidRangeReply, reply.FirstBlockHeight, prevLast)
	}

	return nil
}

// complete reports whether the stream ends with reply.
func (a rangeAccumulator) complete(reply *lnwire.ReplyChannelRange,
	legacy bool, limits RangeLimits) bool {

	if a.replyBudgetUsed >= limits.MaxReplies {
		return true
	}

	// A legacy peer echoes our whole query in every reply, so only its
	// Complete flag marks the end.
	if legacy {
		return reply.Complete != 0
	}

	return reply.LastBlockHeight() >= a.query.LastBlockHeight()
}

// bothOutOfBounds reports whether a channel's two update timestamps are both
// outside the freshness horizon, each either stale or skewed. A channel is
// only skipped if neither direction has a plausible timestamp.
func bothOutOfBounds(t1, t2, now time.Time, limits RangeLimits) bool {
	outOfBounds := func(ts time.Time) bool {
		return now.Sub(ts) > limits.FreshnessHorizon ||
			ts.Sub(now) > limits.FreshnessHorizon
	}

	return outOfBounds(t1) && outOfBounds(t2)
}
