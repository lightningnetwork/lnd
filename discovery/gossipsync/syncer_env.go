package gossipsync

import (
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// DefaultSCIDBatchSize is the most SCIDs we'll ask for in a single
	// query_short_channel_ids.
	DefaultSCIDBatchSize = 500

	// DefaultReplyTimeout is how long the syncer waits for the next reply
	// from a peer before it gives up on the attempt. It is an inactivity
	// timeout: every reply that arrives re-arms it. During the SCID phase
	// only the end marker of each batch counts as a reply, since the
	// announcements answering the batch go to the gossiper, so the
	// timeout has to cover a whole batch arriving from a peer whose
	// outbound gossip is heavily rate limited: 500 channels is about
	// 600 KB, which takes two minutes at 5 KB/s.
	DefaultReplyTimeout = 5 * time.Minute

	// DefaultDrainTimeout is how long the syncer waits for the next reply
	// of an abandoned stream before it is ready to start a new attempt.
	// Like the reply timeout, it is an inactivity timeout, and it matches
	// the reply timeout so that any peer slow enough to be accepted in a
	// live exchange also has its abandoned stream fully drained. A shorter
	// drain would let the rest of such a stream be read as replies to the
	// next attempt.
	DefaultDrainTimeout = DefaultReplyTimeout
)

// KnownChannels reports which of a peer's channels we're missing. It is the
// only graph lookup the peer syncer makes.
type KnownChannels interface {
	// FilterKnownChanIDs returns the channels in superSet that we don't
	// know, or that we know as zombies but that isStillZombie says have
	// been revived.
	FilterKnownChanIDs(chain chainhash.Hash,
		superSet []graphdb.ChannelUpdateInfo,
		isStillZombie func(graphdb.ChannelUpdateInfo) bool) (
		[]lnwire.ShortChannelID, error)
}

// SyncerEnv is the read-only environment of a peer syncer. Transitions may
// read from it but never write through it: every write and every message goes
// through the outbox.
type SyncerEnv struct {
	// Peer is the peer this syncer talks to.
	Peer route.Vertex

	// ChainHash is the chain we sync.
	ChainHash chainhash.Hash

	// Graph reports which channels we're missing.
	Graph KnownChannels

	// IsStillZombie reports whether a channel we know as a zombie should
	// stay one given the peer's timestamps.
	IsStillZombie func(graphdb.ChannelUpdateInfo) bool

	// BestHeight returns our current best block height.
	BestHeight func() uint32

	// Now returns the current time. It is read for freshness filtering
	// and timestamp filters, so tests can make both deterministic.
	Now func() time.Time

	// Limits bounds each reply stream.
	Limits RangeLimits

	// BatchSize is the most SCIDs per query_short_channel_ids.
	BatchSize int

	// ReplyTimeout is the inactivity timeout while waiting on the peer.
	ReplyTimeout time.Duration

	// DrainTimeout is the inactivity timeout while draining an abandoned
	// stream.
	DrainTimeout time.Duration

	// NoTimestampQueries disables asking for channel update timestamps in
	// our range queries.
	NoTimestampQueries bool
}

// Name returns a name for the environment, for logging.
//
// NOTE: This implements the protofsm.Environment interface.
func (e *SyncerEnv) Name() string {
	return "gossipsync-" + e.Peer.String()[:16]
}

// historicalQuery builds a query_channel_range covering the whole chain.
func (e *SyncerEnv) historicalQuery() *lnwire.QueryChannelRange {
	numBlocks := max(e.BestHeight(), 1)

	query := &lnwire.QueryChannelRange{
		ChainHash:        e.ChainHash,
		FirstBlockHeight: 0,
		NumBlocks:        numBlocks,
	}
	if !e.NoTimestampQueries {
		query.QueryOptions = lnwire.NewTimestampQueryOption()
	}

	return query
}

// timestampFilter builds the gossip_timestamp_filter that puts the peer in
// the given role. An active role asks for everything from now on, and the
// passive role asks for nothing.
func (e *SyncerEnv) timestampFilter(t SyncType) *lnwire.GossipTimestampRange {
	filter := &lnwire.GossipTimestampRange{
		ChainHash: e.ChainHash,
	}
	if t.wantsGossip() {
		filter.FirstTimestamp = uint32(e.Now().Unix())
		filter.TimestampRange = ^uint32(0)
	}

	return filter
}

// scidQuery builds a query_short_channel_ids for a batch.
func (e *SyncerEnv) scidQuery(
	batch []lnwire.ShortChannelID) *lnwire.QueryShortChanIDs {

	return &lnwire.QueryShortChanIDs{
		ChainHash:    e.ChainHash,
		EncodingType: lnwire.EncodingSortedPlain,
		ShortChanIDs: batch,
	}
}

// nextBatch splits the first batch off pending.
func (e *SyncerEnv) nextBatch(pending []lnwire.ShortChannelID) (
	[]lnwire.ShortChannelID, []lnwire.ShortChannelID) {

	n := min(len(pending), e.BatchSize)

	return pending[:n], pending[n:]
}
