package gossipsync

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/actor/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// DefaultBacklogPageSize is how many backlog messages the responder
	// sends before it yields to any queries the peer sent meanwhile.
	DefaultBacklogPageSize = 64

	// backlogRetryDelay is how long the responder waits before trying
	// again to start a backlog replay when every filter token is taken.
	backlogRetryDelay = time.Second
)

// ChannelGraph is the part of the channel graph the responder reads.
type ChannelGraph interface {
	// FilterChannelRange returns the channels we know in the block range
	// [startHeight, endHeight], grouped by block and sorted by height.
	FilterChannelRange(chain chainhash.Hash, startHeight,
		endHeight uint32, withTimestamps bool) (
		[]graphdb.BlockChannelRange, error)

	// FetchChanAnns returns the announcements, updates and node
	// announcements for the given channels.
	FetchChanAnns(chain chainhash.Hash,
		shortChanIDs []lnwire.ShortChannelID) ([]lnwire.Message, error)

	// FetchChanUpdates returns the updates we know for a channel.
	FetchChanUpdates(chain chainhash.Hash,
		shortChanID lnwire.ShortChannelID) (
		[]*lnwire.ChannelUpdate1, error)

	// UpdatesInHorizon returns every announcement and update with a
	// timestamp in [startTime, endTime].
	UpdatesInHorizon(ctx context.Context, startTime,
		endTime time.Time) iter.Seq2[lnwire.Message, error]
}

// window is the time range of a peer's gossip filter.
type window struct {
	start, end time.Time
}

// newWindow returns the window a gossip_timestamp_filter describes.
func newWindow(f *lnwire.GossipTimestampRange) window {
	start := time.Unix(int64(f.FirstTimestamp), 0)
	end := start.Add(time.Duration(f.TimestampRange) * time.Second)

	return window{start: start, end: end}
}

// admits reports whether a message with the given timestamp passes the
// filter: at or after the start, and before the end.
func (w window) admits(ts uint32) bool {
	t := time.Unix(int64(ts), 0)
	return !t.Before(w.start) && t.Before(w.end)
}

// backlog is a replay of the gossip in a peer's new filter window.
type backlog struct {
	// gen identifies the replay.
	gen uint64

	// win is the window being replayed.
	win window

	// token is set while the replay holds a filter concurrency token,
	// which it does only while it sends a page.
	token bool

	// next and stop are the pull iterator over the window's gossip,
	// opened on the first page.
	next func() (lnwire.Message, error, bool)
	stop func()
}

// ResponderConfig configures the responder actors.
type ResponderConfig struct {
	// ChainHash is the chain we serve.
	ChainHash chainhash.Hash

	// Graph is the graph we serve from.
	Graph ChannelGraph

	// Encoding is the SCID encoding of our replies.
	Encoding lnwire.QueryEncoding

	// ChunkSize is the most SCIDs per reply_channel_range.
	ChunkSize int

	// NoTimestampQueries disables timestamps in our replies.
	NoTimestampQueries bool

	// IgnoreHistoricalFilters disables backlog replays: a new filter
	// only affects live forwarding.
	IgnoreHistoricalFilters bool

	// FilterTokens is the pool of tokens shared by every responder. A
	// replay holds one token while it runs, which bounds how many run at
	// once across all peers.
	FilterTokens chan struct{}

	// PageSize is how many backlog messages are sent per page.
	PageSize int
}

// responderActor answers one peer's queries, replays the backlog of its
// gossip filter, and forwards live gossip its filter admits.
type responderActor struct {
	cfg *ResponderConfig

	// peer is the peer served.
	peer route.Vertex

	// send sends to the peer.
	send *sender

	// timeouts schedules backlog retries.
	timeouts actor.TellOnlyRef[timeout.Msg]

	// self is this actor's own ref, for backlog pages. It is set right
	// after the actor is registered.
	self actor.TellOnlyRef[responderMsg]

	// courier delivers backlog pages to self without blocking.
	courier courier

	// horizon is the peer's filter window, if it has sent one.
	horizon fn.Option[window]

	// replay is the backlog replay in progress, if any.
	replay *backlog

	// nextGen numbers backlog replays.
	nextGen uint64

	// shuffle picks which channels of an oversized block make it into a
	// reply. Each responder has its own source, so responders never share
	// random state.
	shuffle func(n int, swap func(i, j int))
}

// newResponderActor returns the behavior of a new responder.
func newResponderActor(cfg *ResponderConfig, peer route.Vertex,
	session SessionID, send *sender,
	timeouts actor.TellOnlyRef[timeout.Msg],
	shuffle func(n int, swap func(i, j int))) *responderActor {

	return &responderActor{
		cfg:      cfg,
		peer:     peer,
		send:     send,
		timeouts: timeouts,
		shuffle:  shuffle,
		courier: courier{
			timeouts: timeouts,
			prefix: fmt.Sprintf(
				"gossipsync/%v/%d/backlog", peer, session,
			),
		},
	}
}

// Receive handles one message for the responder.
//
// NOTE: This implements the actor.ActorBehavior interface.
func (r *responderActor) Receive(ctx context.Context,
	msg responderMsg) fn.Result[Ack] {

	var err error
	switch m := msg.(type) {
	case *peerQueryMsg:
		switch q := m.query.(type) {
		case *lnwire.QueryChannelRange:
			err = r.replyRange(ctx, q)

		case *lnwire.QueryShortChanIDs:
			err = r.replySCIDs(ctx, q)

		default:
			err = fmt.Errorf("unknown query %T", q)
		}

	case *peerFilterMsg:
		r.setFilter(ctx, m.filter)

	case *forwardMsg:
		err = r.forward(ctx, m.batch)

	case *backlogPageMsg:
		r.page(ctx, m.gen)

	default:
		err = fmt.Errorf("unknown message %T", msg)
	}

	if err != nil {
		log.Debugf("GossipResponder(%v): %v", r.peer, err)

		return fn.Err[Ack](err)
	}

	return fn.Ok(Ack{})
}

// replyRange answers a query_channel_range with a stream of replies that
// tile the query.
func (r *responderActor) replyRange(ctx context.Context,
	q *lnwire.QueryChannelRange) error {

	// A query for another chain gets a single empty reply that says we
	// don't have that chain.
	if q.ChainHash != r.cfg.ChainHash {
		return r.send.send(ctx, true, &lnwire.ReplyChannelRange{
			ChainHash:        q.ChainHash,
			FirstBlockHeight: q.FirstBlockHeight,
			NumBlocks:        q.NumBlocks,
			EncodingType:     r.cfg.Encoding,
		})
	}

	withTimestamps := q.WithTimestamps() && !r.cfg.NoTimestampQueries

	ranges, err := r.cfg.Graph.FilterChannelRange(
		q.ChainHash, q.FirstBlockHeight, q.LastBlockHeight(),
		withTimestamps,
	)
	if err != nil {
		return fmt.Errorf("unable to filter channel range: %w", err)
	}

	chunker := &rangeChunker{
		query:          q,
		encoding:       r.cfg.Encoding,
		chunkSize:      r.cfg.ChunkSize,
		withTimestamps: withTimestamps,
		shuffle:        r.shuffle,
	}
	for reply := range chunker.replies(ranges) {
		if err := r.send.send(ctx, true, reply); err != nil {
			return err
		}
	}

	return nil
}

// replySCIDs answers a query_short_channel_ids with everything we know about
// the channels, then the end marker.
func (r *responderActor) replySCIDs(ctx context.Context,
	q *lnwire.QueryShortChanIDs) error {

	if q.ChainHash != r.cfg.ChainHash {
		return r.send.send(ctx, true, &lnwire.ReplyShortChanIDsEnd{
			ChainHash: q.ChainHash,
			Complete:  0,
		})
	}

	// An empty query needs no answer.
	if len(q.ShortChanIDs) == 0 {
		return nil
	}

	msgs, err := r.cfg.Graph.FetchChanAnns(q.ChainHash, q.ShortChanIDs)
	if err != nil {
		return fmt.Errorf("unable to fetch channel announcements: %w",
			err)
	}

	// Each message is written before the next is sent, which throttles
	// the reply here rather than buffering it in the peer.
	for _, msg := range msgs {
		if err := r.send.send(ctx, true, msg); err != nil {
			return err
		}
	}

	return r.send.send(ctx, true, &lnwire.ReplyShortChanIDsEnd{
		ChainHash: q.ChainHash,
		Complete:  1,
	})
}

// setFilter applies the peer's new gossip filter, and starts replaying the
// backlog in its window. A newer filter replaces a replay in progress.
func (r *responderActor) setFilter(ctx context.Context,
	f *lnwire.GossipTimestampRange) {

	win := newWindow(f)
	r.horizon = fn.Some(win)

	if r.cfg.IgnoreHistoricalFilters {
		return
	}

	r.endReplay()

	r.nextGen++
	r.replay = &backlog{gen: r.nextGen, win: win}
	r.page(ctx, r.replay.gen)
}

// page sends the next page of the backlog replay gen, then schedules the
// page after it. A page for a replay that has since ended is ignored.
func (r *responderActor) page(ctx context.Context, gen uint64) {
	b := r.replay
	if b == nil || b.gen != gen {
		return
	}

	// The replay needs a token before it touches the database. If none
	// is free, we try again shortly rather than block this peer's
	// queries.
	if !b.token {
		select {
		case <-r.cfg.FilterTokens:
			b.token = true

		default:
			r.retryPage(ctx, gen)
			return
		}
	}

	if b.next == nil {
		b.next, b.stop = iter.Pull2(r.cfg.Graph.UpdatesInHorizon(
			ctx, b.win.start, b.win.end,
		))
	}

	for range r.cfg.PageSize {
		msg, err, ok := b.next()
		if !ok {
			r.endReplay()
			return
		}
		if err != nil {
			log.Errorf("GossipResponder(%v): unable to read "+
				"backlog: %v", r.peer, err)

			continue
		}

		if err := r.send.send(ctx, true, msg); err != nil {
			log.Debugf("GossipResponder(%v): unable to send "+
				"backlog: %v", r.peer, err)

			r.endReplay()

			return
		}
	}

	// Return the token between pages. The peer's queries are answered
	// before the next page, and a whole-chain reply behind a rate limit
	// can take minutes, so holding the token across them would let a few
	// chatty peers stop every other peer's replay. The token bounds how
	// many pages run at once, not how many replays are open.
	r.releaseToken()

	deliver[responderMsg](ctx, &r.courier, r.self, &backlogPageMsg{
		gen: gen,
	})
}

// releaseToken returns the replay's filter token to the pool, if it holds
// one.
func (r *responderActor) releaseToken() {
	if r.replay != nil && r.replay.token {
		r.cfg.FilterTokens <- struct{}{}
		r.replay.token = false
	}
}

// retryPage schedules another attempt at page gen after a short delay.
func (r *responderActor) retryPage(ctx context.Context, gen uint64) {
	r.timeouts.Tell(ctx, &timeout.ScheduleTimeoutRequest{
		ID:       timeout.ID(r.courier.prefix + "/retry"),
		Duration: backlogRetryDelay,
		Callback: timeout.MapTimeoutExpired(
			r.self, func(timeout.ExpiredMsg) responderMsg {
				return &backlogPageMsg{gen: gen}
			},
		),
	})
}

// endReplay ends the backlog replay in progress, closing its iterator and
// returning its token.
func (r *responderActor) endReplay() {
	b := r.replay
	if b == nil {
		return
	}

	if b.stop != nil {
		b.stop()
	}
	r.releaseToken()

	r.replay = nil
}

// forward sends the peer the live gossip its filter admits, skipping what it
// sent us itself.
func (r *responderActor) forward(ctx context.Context,
	batch []ForwardMsg) error {

	if r.horizon.IsNone() {
		return nil
	}
	win := r.horizon.UnwrapOr(window{})

	// Index the batch's channel updates, so a channel announcement can
	// be judged by its updates without a database lookup.
	updates := make(map[lnwire.ShortChannelID][]*lnwire.ChannelUpdate1)
	for _, f := range batch {
		if u, ok := f.Msg.(*lnwire.ChannelUpdate1); ok {
			updates[u.ShortChannelID] = append(
				updates[u.ShortChannelID], u,
			)
		}
	}

	var out []lnwire.Message
	for _, f := range batch {
		if _, ok := f.Senders[r.peer]; ok {
			continue
		}

		switch m := f.Msg.(type) {
		// An announcement is sent if any of its updates is in the
		// window, or if it has no updates yet.
		case *lnwire.ChannelAnnouncement1:
			chanUpdates, ok := updates[m.ShortChannelID]
			if !ok {
				var err error
				chanUpdates, err = r.cfg.Graph.FetchChanUpdates(
					r.cfg.ChainHash, m.ShortChannelID,
				)
				if err != nil {
					continue
				}
			}

			admit := len(chanUpdates) == 0
			for _, u := range chanUpdates {
				admit = admit || win.admits(u.Timestamp)
			}
			if admit {
				out = append(out, m)
			}

		case *lnwire.ChannelUpdate1:
			if win.admits(m.Timestamp) {
				out = append(out, m)
			}

		case *lnwire.NodeAnnouncement1:
			if win.admits(m.Timestamp) {
				out = append(out, m)
			}
		}
	}

	if len(out) == 0 {
		return nil
	}

	return r.send.send(ctx, false, out...)
}

// OnStop ends any backlog replay, so its iterator is closed and its token
// returned to the pool.
//
// NOTE: This implements the actor.Stoppable interface.
func (r *responderActor) OnStop(context.Context) error {
	r.endReplay()

	return nil
}
