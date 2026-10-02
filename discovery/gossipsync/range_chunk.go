package gossipsync

import (
	"iter"
	"slices"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// DefaultRangeChunkSize is the most SCIDs we put in one plain encoded
	// reply_channel_range, which keeps the message under the transport
	// limit. It is halved when timestamps are included, since two
	// timestamps take as many bytes as an SCID.
	DefaultRangeChunkSize = 8000
)

// replyStream is a stream of reply_channel_range messages.
type replyStream = iter.Seq[*lnwire.ReplyChannelRange]

// rangeChunker splits the channels answering a query_channel_range into a
// stream of reply_channel_range messages that each fit in one wire message.
type rangeChunker struct {
	// query is the query being answered.
	query *lnwire.QueryChannelRange

	// encoding is the SCID encoding we reply with.
	encoding lnwire.QueryEncoding

	// chunkSize is the most SCIDs per reply, before halving for
	// timestamps.
	chunkSize int

	// withTimestamps is set if the replies carry update timestamps.
	withTimestamps bool

	// shuffle randomly permutes n elements with swap. It picks which
	// channels of an oversized block make it into the reply. Tests pass a
	// seeded shuffle, production passes rand.Shuffle.
	shuffle func(n int, swap func(i, j int))
}

// replies returns the reply stream for the given per-block channel ranges,
// which must be sorted by height and lie within the query.
//
// Every reply covers a contiguous block range, and the ranges of successive
// replies tile the query from its first block to its last, which the final
// reply marks with Complete. A block holding more channels than fit in one
// reply is truncated to a random subset, on the assumption that a later
// historical sync will pick up the rest.
func (c *rangeChunker) replies(
	ranges []graphdb.BlockChannelRange) replyStream {

	chunkSize := c.chunkSize
	if c.withTimestamps {
		chunkSize /= 2
	}

	return func(yield func(*lnwire.ReplyChannelRange) bool) {
		var (
			firstHeight = c.query.FirstBlockHeight
			chunk       []graphdb.ChannelUpdateInfo
		)

		for _, block := range ranges {
			channels := block.Channels

			// Add the whole block to the ongoing chunk if it
			// fits.
			if len(channels) <= chunkSize-len(chunk) {
				chunk = append(chunk, channels...)
				continue
			}

			// Otherwise send the pending chunk, which ends on the
			// block before this one. If this is the first block,
			// there is nothing pending to send.
			if block.Height > firstHeight {
				reply := c.reply(
					chunk, firstHeight, block.Height-1,
					false,
				)
				if !yield(reply) {
					return
				}
			}

			// Start a new chunk with this block, truncated to a
			// random subset if it is too large on its own.
			firstHeight = block.Height
			chunk = c.fitBlock(channels, chunkSize)
		}

		yield(c.reply(
			chunk, firstHeight, c.query.LastBlockHeight(), true,
		))
	}
}

// fitBlock returns the channels of a single block, truncated to a random,
// sorted subset if there are more than limit of them.
func (c *rangeChunker) fitBlock(channels []graphdb.ChannelUpdateInfo,
	limit int) []graphdb.ChannelUpdateInfo {

	if len(channels) <= limit {
		return slices.Clone(channels)
	}

	picked := slices.Clone(channels)
	c.shuffle(len(picked), func(i, j int) {
		picked[i], picked[j] = picked[j], picked[i]
	})
	picked = picked[:limit]

	slices.SortFunc(picked, func(a, b graphdb.ChannelUpdateInfo) int {
		x, y := a.ShortChannelID.ToUint64(), b.ShortChannelID.ToUint64()
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	})

	return picked
}

// reply builds one reply_channel_range covering [first, last].
func (c *rangeChunker) reply(chunk []graphdb.ChannelUpdateInfo, first,
	last uint32, final bool) *lnwire.ReplyChannelRange {

	reply := &lnwire.ReplyChannelRange{
		ChainHash:        c.query.ChainHash,
		FirstBlockHeight: first,
		NumBlocks:        last - first + 1,
		EncodingType:     c.encoding,
		ShortChanIDs:     make([]lnwire.ShortChannelID, len(chunk)),
	}
	if final {
		reply.Complete = 1
	}
	if c.withTimestamps {
		reply.Timestamps = make(lnwire.Timestamps, len(chunk))
	}

	for i, info := range chunk {
		reply.ShortChanIDs[i] = info.ShortChannelID

		if c.withTimestamps {
			reply.Timestamps[i] = lnwire.ChanUpdateTimestamps{
				Timestamp1: uint32(
					info.Node1FreshnessTime().Unix(),
				),
				Timestamp2: uint32(
					info.Node2FreshnessTime().Unix(),
				),
			}
		}
	}

	return reply
}
