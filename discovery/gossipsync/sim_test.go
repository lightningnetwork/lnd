package gossipsync

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"iter"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/actor/timeout"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// simChain is the chain every simulated node is on.
var simChain = chainhash.Hash{7}

// simBestHeight is the best height of every simulated node.
const simBestHeight = 10_000

// simChannel is a channel in a simulated graph.
type simChannel struct {
	ann     *lnwire.ChannelAnnouncement1
	updates [2]*lnwire.ChannelUpdate1
}

// simGraph is an in-memory channel graph implementing Graph. Every method
// takes a short lock, and none blocks while holding it.
type simGraph struct {
	mu    sync.Mutex
	chans map[lnwire.ShortChannelID]*simChannel
}

// newSimGraph returns an empty graph.
func newSimGraph() *simGraph {
	return &simGraph{chans: make(map[lnwire.ShortChannelID]*simChannel)}
}

// addChannel adds a channel with an update in each direction at ts.
func (g *simGraph) addChannel(scid lnwire.ShortChannelID, ts time.Time) {
	g.insert(&lnwire.ChannelAnnouncement1{
		ChainHash: simChain, ShortChannelID: scid,
	})
	for dir := range 2 {
		g.insert(&lnwire.ChannelUpdate1{
			ChainHash:      simChain,
			ShortChannelID: scid,
			Timestamp:      uint32(ts.Unix()),
			ChannelFlags:   lnwire.ChanUpdateChanFlags(dir),
		})
	}
}

// insert applies an announcement or update, as a gossiper that accepts
// everything would. It reports whether the graph changed.
func (g *simGraph) insert(msg lnwire.Message) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	switch m := msg.(type) {
	case *lnwire.ChannelAnnouncement1:
		if _, ok := g.chans[m.ShortChannelID]; ok {
			return false
		}
		g.chans[m.ShortChannelID] = &simChannel{ann: m}

		return true

	case *lnwire.ChannelUpdate1:
		c, ok := g.chans[m.ShortChannelID]
		if !ok {
			return false
		}
		dir := m.ChannelFlags & lnwire.ChanUpdateDirection
		old := c.updates[dir]
		if old != nil && old.Timestamp >= m.Timestamp {
			return false
		}
		c.updates[dir] = m

		return true
	}

	return false
}

// scids returns every channel in the graph, sorted.
func (g *simGraph) scids() []lnwire.ShortChannelID {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := slices.Collect(maps.Keys(g.chans))
	slices.SortFunc(out, func(a, b lnwire.ShortChannelID) int {
		return cmp.Compare(a.ToUint64(), b.ToUint64())
	})

	return out
}

// info returns a channel's update info, which carries its timestamps.
func (c *simChannel) info() graphdb.ChannelUpdateInfo {
	ts := func(u *lnwire.ChannelUpdate1) time.Time {
		if u == nil {
			return time.Time{}
		}

		return time.Unix(int64(u.Timestamp), 0)
	}

	return graphdb.NewV1ChannelUpdateInfo(
		c.ann.ShortChannelID, ts(c.updates[0]), ts(c.updates[1]),
	)
}

// FilterKnownChanIDs returns the channels in superSet we don't have.
func (g *simGraph) FilterKnownChanIDs(_ chainhash.Hash,
	superSet []graphdb.ChannelUpdateInfo,
	_ func(graphdb.ChannelUpdateInfo) bool) ([]lnwire.ShortChannelID,
	error) {

	g.mu.Lock()
	defer g.mu.Unlock()

	var missing []lnwire.ShortChannelID
	for _, info := range superSet {
		if _, ok := g.chans[info.ShortChannelID]; !ok {
			missing = append(missing, info.ShortChannelID)
		}
	}

	return missing, nil
}

// FilterChannelRange returns our channels in [start, end], by block.
func (g *simGraph) FilterChannelRange(_ chainhash.Hash, start, end uint32,
	_ bool) ([]graphdb.BlockChannelRange, error) {

	byHeight := make(map[uint32][]graphdb.ChannelUpdateInfo)
	for _, scid := range g.scids() {
		if scid.BlockHeight < start || scid.BlockHeight > end {
			continue
		}

		g.mu.Lock()
		info := g.chans[scid].info()
		g.mu.Unlock()

		byHeight[scid.BlockHeight] = append(
			byHeight[scid.BlockHeight], info,
		)
	}

	heights := slices.Sorted(maps.Keys(byHeight))
	out := make([]graphdb.BlockChannelRange, 0, len(heights))
	for _, h := range heights {
		out = append(out, graphdb.BlockChannelRange{
			Height: h, Channels: byHeight[h],
		})
	}

	return out, nil
}

// FetchChanAnns returns each channel's announcement and updates.
func (g *simGraph) FetchChanAnns(_ chainhash.Hash,
	scids []lnwire.ShortChannelID) ([]lnwire.Message, error) {

	g.mu.Lock()
	defer g.mu.Unlock()

	var out []lnwire.Message
	for _, scid := range scids {
		c, ok := g.chans[scid]
		if !ok {
			continue
		}
		out = append(out, c.ann)
		for _, u := range c.updates {
			if u != nil {
				out = append(out, u)
			}
		}
	}

	return out, nil
}

// FetchChanUpdates returns a channel's updates.
func (g *simGraph) FetchChanUpdates(_ chainhash.Hash,
	scid lnwire.ShortChannelID) ([]*lnwire.ChannelUpdate1, error) {

	g.mu.Lock()
	defer g.mu.Unlock()

	c, ok := g.chans[scid]
	if !ok {
		return nil, nil
	}

	var out []*lnwire.ChannelUpdate1
	for _, u := range c.updates {
		if u != nil {
			out = append(out, u)
		}
	}

	return out, nil
}

// UpdatesInHorizon returns every channel with an update in the window, as
// its announcement followed by those updates.
func (g *simGraph) UpdatesInHorizon(_ context.Context, start,
	end time.Time) iter.Seq2[lnwire.Message, error] {

	return func(yield func(lnwire.Message, error) bool) {
		for _, scid := range g.scids() {
			g.mu.Lock()
			c := g.chans[scid]
			var msgs []lnwire.Message
			for _, u := range c.updates {
				if u == nil {
					continue
				}
				t := time.Unix(int64(u.Timestamp), 0)
				if t.Before(start) || t.After(end) {
					continue
				}
				msgs = append(msgs, u)
			}
			if len(msgs) > 0 {
				msgs = append([]lnwire.Message{c.ann}, msgs...)
			}
			g.mu.Unlock()

			for _, m := range msgs {
				if !yield(m, nil) {
					return
				}
			}
		}
	}
}

// linkAction is what the bus does with one message.
type linkAction uint8

const (
	deliverMsg linkAction = iota
	dropMsg
	dupMsg
	delayMsg
)

// linkFaults decides what happens to each message on a link. The zero value
// delivers everything.
//
// A decision must depend only on the message, never on the order of calls:
// a node's syncer and responder send on the same link from two goroutines,
// and synctest fixes time but not goroutine scheduling, so an order based
// decision would change between runs of the same seed.
type linkFaults func(msg lnwire.Message) (linkAction, time.Duration)

// seededFaults returns faults that drop, duplicate or delay each message
// with the given percentages, decided by hashing the message with seed.
func seededFaults(seed uint64, dropPct, dupPct, delayPct int) linkFaults {
	return func(msg lnwire.Message) (linkAction, time.Duration) {
		h := fnv.New64a()
		_, _ = fmt.Fprintf(h, "%d/%T/%+v", seed, msg, msg)
		roll := int(h.Sum64() % 100)

		switch {
		case roll < dropPct:
			return dropMsg, 0
		case roll < dropPct+dupPct:
			return dupMsg, 0
		case roll < dropPct+dupPct+delayPct:
			return delayMsg, time.Duration(h.Sum64()%30) *
				time.Second
		default:
			return deliverMsg, 0
		}
	}
}

// link carries messages one way between two simulated nodes, in order,
// through an unbounded queue, like a network connection with buffers on both
// ends. It implements PeerConn for the sending node.
type link struct {
	from, to *simNode

	faults linkFaults

	mu     sync.Mutex
	queue  []lnwire.Message
	signal chan struct{}
	closed bool

	// sent counts messages by type, for DoS assertions.
	sent map[string]int

	// faulted counts messages the faults did not deliver as is.
	faulted int
}

// setFaults replaces the link's faults.
func (l *link) setFaults(f linkFaults) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.faults = f
}

// PubKey returns the public key of the node at the far end.
func (l *link) PubKey() [33]byte {
	return l.to.pub
}

// SendMessageLazy queues msgs on the link.
func (l *link) SendMessageLazy(_ bool, msgs ...lnwire.Message) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return errLinkClosed
	}

	for _, msg := range msgs {
		l.sent[msg.MsgType().String()]++

		action, delay := deliverMsg, time.Duration(0)
		if l.faults != nil {
			action, delay = l.faults(msg)
		}
		if action != deliverMsg {
			l.faulted++
		}

		switch action {
		case dropMsg:
			continue

		case dupMsg:
			l.queue = append(l.queue, msg, msg)

		case delayMsg:
			time.AfterFunc(delay, func() {
				l.mu.Lock()
				defer l.mu.Unlock()
				if !l.closed {
					l.queue = append(l.queue, msg)
					l.kick()
				}
			})

			continue

		default:
			l.queue = append(l.queue, msg)
		}
	}
	l.kick()

	return nil
}

// errLinkClosed is returned by a send on a closed link.
var errLinkClosed = errorString("link closed")

// errorString is a constant error type.
type errorString string

func (e errorString) Error() string { return string(e) }

// kick wakes the delivery goroutine. The caller holds mu.
func (l *link) kick() {
	select {
	case l.signal <- struct{}{}:
	default:
	}
}

// run delivers queued messages to the far node until the link closes.
func (l *link) run(ctx context.Context) {
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return
		}
		if len(l.queue) == 0 {
			l.mu.Unlock()
			select {
			case <-l.signal:
				continue
			case <-ctx.Done():
				return
			}
		}
		msg := l.queue[0]
		l.queue = l.queue[1:]
		l.mu.Unlock()

		l.to.receive(ctx, l.from.pub, msg)
	}
}

// close stops the link.
func (l *link) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.kick()
}

// simNode is one simulated lnd node: a gossip sync manager over an in-memory
// graph, in its own actor system.
type simNode struct {
	pub    route.Vertex
	graph  *simGraph
	system *actor.ActorSystem
	mgr    *Manager

	// byz is set for a byzantine peer, which has no manager and answers
	// queries however its mode says.
	byz *byzantine
}

// receive handles a message arriving from peer, as brontide and the gossiper
// would: sync messages go to the manager, and the rest to the graph.
func (n *simNode) receive(ctx context.Context, peer route.Vertex,
	msg lnwire.Message) {

	if n.byz != nil {
		n.byz.receive(msg)
		return
	}

	switch msg.(type) {
	case *lnwire.ReplyChannelRange, *lnwire.ReplyShortChanIDsEnd,
		*lnwire.QueryChannelRange, *lnwire.QueryShortChanIDs,
		*lnwire.GossipTimestampRange:

		_ = n.mgr.DeliverPeerMsg(ctx, peer, msg)

	default:
		if n.graph.insert(msg) {
			n.relay(ctx, msg, peer)
		}
	}
}

// relay forwards an announcement the node just learned from peer to its
// other peers, as the gossiper does once the graph is synced. Like the
// gossiper, it then marks every covered peer as a sender of the message, in
// the same map it passed to Forward.
func (n *simNode) relay(ctx context.Context, msg lnwire.Message,
	from route.Vertex) {

	if !n.mgr.IsGraphSynced() {
		return
	}

	batch := []ForwardMsg{{
		Msg:     msg,
		Senders: map[route.Vertex]struct{}{from: {}},
	}}
	covered := n.mgr.Forward(ctx, batch)
	for pub := range covered {
		batch[0].Senders[pub] = struct{}{}
	}
}

// announce adds a fresh channel to the node's graph and relays it to its
// peers, as a node does when one of its own channels is announced.
func (n *simNode) announce(ctx context.Context,
	scid lnwire.ShortChannelID) {

	n.graph.addChannel(scid, time.Now())

	var msgs []lnwire.Message
	n.graph.mu.Lock()
	c := n.graph.chans[scid]
	msgs = append(msgs, c.ann, c.updates[0], c.updates[1])
	n.graph.mu.Unlock()

	var batch []ForwardMsg
	for _, m := range msgs {
		batch = append(batch, ForwardMsg{
			Msg: m, Senders: map[route.Vertex]struct{}{},
		})
	}
	covered := n.mgr.Forward(ctx, batch)
	for _, f := range batch {
		for pub := range covered {
			f.Senders[pub] = struct{}{}
		}
	}
}

// simConfig tunes a simulated node.
type simConfig struct {
	numActive int
	pinned    map[route.Vertex]struct{}
	seed      uint64
	limits    RangeLimits

	// onOutcome observes the node's accepted attempt outcomes.
	onOutcome func(peer route.Vertex, outcome SyncOutcome)
}

// newSimNode starts a node. It must be called inside a synctest bubble.
func newSimNode(t *testing.T, id byte, cfg simConfig) *simNode {
	t.Helper()

	system := actor.NewActorSystem()
	timeouts := timeout.NewActor()
	timeoutRef, err := actor.RegisterWithSystem(
		system, "timeout",
		actor.NewServiceKey[timeout.Msg, timeout.Resp]("timeout"),
		timeouts,
	)
	require.NoError(t, err)
	timeouts.Start(timeoutRef)

	n := &simNode{
		pub:    route.Vertex{id},
		graph:  newSimGraph(),
		system: system,
	}

	seed := cfg.seed + uint64(id)
	n.mgr, err = NewManager(Config{
		ChainHash:         simChain,
		Graph:             n.graph,
		BestHeight:        func() uint32 { return simBestHeight },
		NumActiveSyncers:  cfg.numActive,
		PinnedSyncers:     cfg.pinned,
		FilterConcurrency: 2,
		Limits:            cfg.limits,
		OnSyncOutcome:     cfg.onOutcome,
		Rand:              rand.New(rand.NewPCG(seed, seed)),
		System:            system,
		Timeouts:          timeoutRef,
	})
	require.NoError(t, err)
	n.mgr.Start(t.Context())

	t.Cleanup(func() {
		n.mgr.Stop()
		require.NoError(t, system.Shutdown())
	})

	return n
}

// connect joins two nodes with a link in each direction, and registers each
// with the other's manager.
func connect(t *testing.T, a, b *simNode, ab, ba linkFaults) (*link,
	*link) {

	t.Helper()

	mk := func(from, to *simNode, f linkFaults) *link {
		return &link{
			from: from, to: to, faults: f,
			signal: make(chan struct{}, 1),
			sent:   make(map[string]int),
		}
	}
	toB, toA := mk(a, b, ab), mk(b, a, ba)

	// Like brontide, both sides register the peer before either starts
	// reading its messages, so no message reaches a node that doesn't
	// know its sender yet.
	require.NoError(t, a.mgr.InitSyncState(t.Context(), toB))
	require.NoError(t, b.mgr.InitSyncState(t.Context(), toA))

	for _, l := range []*link{toB, toA} {
		go l.run(t.Context())
		t.Cleanup(l.close)
	}

	return toB, toA
}

// seedGraph gives a node n channels spread over the chain, updated recently.
func seedGraph(n *simNode, rng *rand.Rand, count int) {
	fresh := time.Now().Add(-time.Hour)
	for i := range count {
		scid := lnwire.ShortChannelID{
			BlockHeight: uint32(rng.IntN(simBestHeight)),
			TxIndex:     uint32(i),
		}
		n.graph.addChannel(scid, fresh)
	}
}
