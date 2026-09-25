package gossipsync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/actor/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/routing/route"
	"golang.org/x/time/rate"
)

const (
	// DefaultRotateInterval is how often an active syncer is swapped for
	// a passive one.
	DefaultRotateInterval = 20 * time.Minute

	// DefaultHistoricalSyncInterval is how often a historical sync runs
	// with a random peer.
	DefaultHistoricalSyncInterval = time.Hour

	// DefaultPeerMailboxSize is the mailbox size of each per-peer actor.
	// A slot can hold a wire message of up to 64 KiB, and a peer can fill
	// both of its actors' mailboxes, so this bounds the memory one peer
	// can pin at about 6.5 MB, the same as the legacy syncer's buffers. A
	// full mailbox blocks only that peer's own message stream, so an
	// honest peer's replies simply wait their turn.
	DefaultPeerMailboxSize = 50

	// DefaultManagerMailboxSize is the manager's mailbox size.
	DefaultManagerMailboxSize = 1024
)

var (
	// ErrSyncerNotFound is returned when a message arrives for a peer
	// we have no syncer for.
	ErrSyncerNotFound = errors.New("gossip syncer not found")

	// ErrManagerStopped is returned by calls made after Stop.
	ErrManagerStopped = errors.New("gossip sync manager stopped")
)

// Graph is everything the syncer reads from the channel graph.
type Graph interface {
	KnownChannels
	ChannelGraph
}

// Config configures a Manager.
type Config struct {
	// ChainHash is the chain we sync.
	ChainHash chainhash.Hash

	// Graph is the channel graph.
	Graph Graph

	// IsStillZombie reports whether a zombie channel should stay one.
	IsStillZombie func(graphdb.ChannelUpdateInfo) bool

	// BestHeight returns our best block height.
	BestHeight func() uint32

	// NumActiveSyncers is how many non-pinned peers we keep active.
	NumActiveSyncers int

	// PinnedSyncers are the peers that are always active.
	PinnedSyncers map[route.Vertex]struct{}

	// RotateInterval is how often an active peer is rotated.
	RotateInterval time.Duration

	// HistoricalSyncInterval is how often a historical sync runs.
	HistoricalSyncInterval time.Duration

	// NoTimestampQueries disables channel update timestamps in queries
	// and replies.
	NoTimestampQueries bool

	// IgnoreHistoricalFilters disables backlog replays.
	IgnoreHistoricalFilters bool

	// PeerMsgBytesPerSecond limits the bytes per second we send to one
	// peer. Its burst is twice the rate.
	PeerMsgBytesPerSecond uint64

	// MsgBytesPerSecond and MsgBurstBytes limit the bytes per second we
	// send to all peers together.
	MsgBytesPerSecond uint64
	MsgBurstBytes     uint64

	// FilterConcurrency is how many backlog replays may run at once.
	FilterConcurrency int

	// Limits bounds each reply stream we receive.
	Limits RangeLimits

	// BatchSize is the most SCIDs per query we send.
	BatchSize int

	// ReplyTimeout and DrainTimeout bound how long we wait on a peer.
	ReplyTimeout time.Duration
	DrainTimeout time.Duration

	// ChunkSize is the most SCIDs per reply we send.
	ChunkSize int

	// BacklogPageSize is how many backlog messages are sent per page.
	BacklogPageSize int

	// Now returns the current time.
	Now func() time.Time

	// Rand is the source of every random pick. It is only used from the
	// manager actor's goroutine. Tests pass a seeded one.
	Rand *rand.Rand

	// System is the actor system the syncer's actors run in.
	System *actor.ActorSystem

	// Timeouts is the timeout actor that runs every timer.
	Timeouts actor.TellOnlyRef[timeout.Msg]

	// OnSyncOutcome, if set, is called from the manager actor with the
	// outcome of every historical sync attempt the manager accepts. It is
	// meant for metrics and tests, and must not block.
	OnSyncOutcome func(peer route.Vertex, outcome SyncOutcome)
}

// withDefaults returns a copy of cfg with zero fields set to defaults.
func (cfg Config) withDefaults() Config {
	orDefault := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	orDefault(&cfg.RotateInterval, DefaultRotateInterval)
	orDefault(&cfg.HistoricalSyncInterval, DefaultHistoricalSyncInterval)
	orDefault(&cfg.ReplyTimeout, DefaultReplyTimeout)
	orDefault(&cfg.DrainTimeout, DefaultDrainTimeout)

	if cfg.Limits == (RangeLimits{}) {
		cfg.Limits = DefaultRangeLimits()
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = DefaultSCIDBatchSize
	}
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = DefaultRangeChunkSize
	}
	if cfg.BacklogPageSize == 0 {
		cfg.BacklogPageSize = DefaultBacklogPageSize
	}
	if cfg.FilterConcurrency == 0 {
		cfg.FilterConcurrency = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		//nolint:gosec
		cfg.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if cfg.IsStillZombie == nil {
		cfg.IsStillZombie = func(graphdb.ChannelUpdateInfo) bool {
			return false
		}
	}

	return cfg
}

// peerRefs are the actors of one peer session.
type peerRefs struct {
	pub       route.Vertex
	syncer    actor.ActorRef[syncerMsg, Ack]
	responder actor.ActorRef[responderMsg, Ack]
}

// Manager is the gossip sync manager. It is a thin handle around the manager
// actor, which owns every decision; the handle only routes calls into the
// actor system, and publishes the two facts the rest of lnd reads without
// waiting: whether the graph is synced, and which peers have syncers.
type Manager struct {
	cfg Config

	ref actor.ActorRef[managerMsg, ManagerResp]

	// graphSynced is written by the manager actor only.
	graphSynced atomic.Bool

	// peers is the set of peers with running actors, written by the
	// manager actor only.
	peers atomic.Pointer[map[route.Vertex]peerRefs]

	// forwardDrops counts gossip batches dropped because a peer's
	// responder mailbox was full.
	forwardDrops atomic.Uint64

	stopOnce sync.Once

	// stopped lets calls made after Stop return at once, without a
	// message to a stopped actor. The manager actor keeps its own flag,
	// which is the one that closes the race with a call already in
	// flight when Stop runs.
	stopped atomic.Bool
}

// NewManager creates a manager and starts its actor in cfg.System.
func NewManager(cfg Config) (*Manager, error) {
	cfg = cfg.withDefaults()

	m := &Manager{cfg: cfg}
	empty := make(map[route.Vertex]peerRefs)
	m.peers.Store(&empty)

	behavior := &managerActor{
		m:     m,
		state: NewManagerState(),
		env: &ManagerEnv{
			NumActiveSyncers: cfg.NumActiveSyncers,
			Rand:             cfg.Rand.IntN,
		},
		peers: make(map[SessionID]peerRefs),
		responderCfg: &ResponderConfig{
			ChainHash:               cfg.ChainHash,
			Graph:                   cfg.Graph,
			Encoding:                lnwire.EncodingSortedPlain,
			ChunkSize:               cfg.ChunkSize,
			NoTimestampQueries:      cfg.NoTimestampQueries,
			IgnoreHistoricalFilters: cfg.IgnoreHistoricalFilters,
			FilterTokens: make(
				chan struct{}, cfg.FilterConcurrency,
			),
			PageSize: cfg.BacklogPageSize,
		},
		global: rate.NewLimiter(
			rate.Limit(cfg.MsgBytesPerSecond),
			int(cfg.MsgBurstBytes),
		),
	}
	for range cfg.FilterConcurrency {
		behavior.responderCfg.FilterTokens <- struct{}{}
	}
	if cfg.MsgBytesPerSecond == 0 {
		behavior.global = nil
	}

	ref, err := managerKey.Spawn(
		cfg.System, "gossipsync-manager", behavior,
		actor.WithMailboxSize[managerMsg, ManagerResp](
			DefaultManagerMailboxSize,
		),
	)
	if err != nil {
		return nil, err
	}
	m.ref = ref
	behavior.self = ref

	return m, nil
}

// Start arms the manager's recurring timers.
func (m *Manager) Start(ctx context.Context) {
	m.ref.Tell(ctx, &lifecycleMsg{start: true})
}

// Stop stops every peer's actors and the manager itself. The manager actor
// does the teardown, since it is the only owner of the peer actors.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		m.stopped.Store(true)

		ctx := context.Background()
		m.ref.Ask(ctx, &lifecycleMsg{start: false}).Await(ctx)
		managerKey.UnregisterAll(m.cfg.System)
	})
}

// InitSyncState registers a new peer connection, and returns once its
// actors are running, so gossip from the peer can be routed at once.
func (m *Manager) InitSyncState(ctx context.Context, conn PeerConn) error {
	if m.stopped.Load() {
		return ErrManagerStopped
	}

	pub := route.Vertex(conn.PubKey())
	_, pinned := m.cfg.PinnedSyncers[pub]

	res := m.ref.Ask(ctx, &connectMsg{conn: conn, pinned: pinned}).Await(
		ctx,
	)
	_, err := res.Unpack()

	return err
}

// PruneSyncState tears down a peer's actors after it disconnects.
func (m *Manager) PruneSyncState(pub route.Vertex) {
	if m.stopped.Load() {
		return
	}

	m.ref.Tell(context.Background(), &managerEventMsg{
		event: &PeerDisconnected{Peer: pub},
	})
}

// DeliverPeerMsg routes a gossip query message from a peer to its actors. It
// blocks while the target mailbox is full, which pushes back on that peer's
// own message stream only.
func (m *Manager) DeliverPeerMsg(ctx context.Context, pub route.Vertex,
	msg lnwire.Message) error {

	p, ok := (*m.peers.Load())[pub]
	if !ok {
		return ErrSyncerNotFound
	}

	switch msg := msg.(type) {
	case *lnwire.ReplyChannelRange:
		p.syncer.Tell(ctx, &syncerEventMsg{
			event: &RangeReplyReceived{Reply: msg},
		})

	case *lnwire.ReplyShortChanIDsEnd:
		p.syncer.Tell(ctx, &syncerEventMsg{
			event: &SCIDsEndReceived{End: msg},
		})

	case *lnwire.QueryChannelRange, *lnwire.QueryShortChanIDs:
		p.responder.Tell(ctx, &peerQueryMsg{query: msg})

	case *lnwire.GossipTimestampRange:
		p.responder.Tell(ctx, &peerFilterMsg{filter: msg})

	default:
		return fmt.Errorf("not a gossip query message: %T", msg)
	}

	return nil
}

// Forward hands a batch of live gossip to every peer's responder, which
// sends the peer what its filter admits. It never blocks: a peer whose
// responder is backed up misses the batch, and the drop is counted. It
// returns the peers the batch was offered to.
//
// The responders filter the batch later, on their own goroutines, so Forward
// copies each message's senders. The caller may modify its maps as soon as
// Forward returns, as the gossiper does to mark the covered peers.
func (m *Manager) Forward(ctx context.Context,
	batch []ForwardMsg) map[route.Vertex]struct{} {

	owned := make([]ForwardMsg, len(batch))
	for i, f := range batch {
		owned[i] = ForwardMsg{
			Msg:     f.Msg,
			Senders: maps.Clone(f.Senders),
		}
	}
	batch = owned

	peers := *m.peers.Load()
	covered := make(map[route.Vertex]struct{}, len(peers))
	for pub, p := range peers {
		covered[pub] = struct{}{}

		err := p.responder.TryTell(ctx, &forwardMsg{batch: batch})
		if errors.Is(err, actor.ErrMailboxFull) {
			m.forwardDrops.Add(1)
			log.Debugf("GossipResponder(%v): mailbox full, "+
				"dropped gossip batch", pub)
		}
	}

	return covered
}

// ForwardDrops returns how many gossip batches were dropped for a full
// responder mailbox.
func (m *Manager) ForwardDrops() uint64 {
	return m.forwardDrops.Load()
}

// IsGraphSynced reports whether a historical sync has completed.
func (m *Manager) IsGraphSynced() bool {
	return m.graphSynced.Load()
}

// SyncType returns a connected peer's role.
func (m *Manager) SyncType(ctx context.Context,
	pub route.Vertex) (SyncType, bool) {

	res := m.ref.Ask(ctx, &roleQuery{peer: pub}).Await(ctx)
	resp, err := res.Unpack()
	if err != nil {
		return 0, false
	}

	role := resp.Role
	return role.UnwrapOr(0), role.IsSome()
}

// Timer IDs of the manager's recurring ticks.
const (
	rotateTimerID     timeout.ID = "gossipsync/rotate"
	historicalTimerID timeout.ID = "gossipsync/historical"
)

// managerActor owns the manager state machine, and the actors of every
// peer session.
type managerActor struct {
	m *Manager

	// state is the machine's current state.
	state *ManagerState

	// env is the machine's environment.
	env *ManagerEnv

	// self is this actor's own ref, for timer callbacks.
	self actor.TellOnlyRef[managerMsg]

	// peers are the actors of each live session.
	peers map[SessionID]peerRefs

	// pending is the connection being registered by the connectMsg in
	// progress, which SpawnPeer needs.
	pending PeerConn

	// responderCfg is shared by every responder.
	responderCfg *ResponderConfig

	// global is the rate limiter shared by every peer.
	global *rate.Limiter

	// stopped is set once shutdown has run. A connection that reaches the
	// actor after that is refused, since nothing would ever stop its
	// actors.
	stopped bool
}

// Receive handles one message for the manager.
//
// NOTE: This implements the actor.ActorBehavior interface.
func (a *managerActor) Receive(ctx context.Context,
	msg managerMsg) fn.Result[ManagerResp] {

	switch m := msg.(type) {
	case *connectMsg:
		if a.stopped {
			return fn.Err[ManagerResp](ErrManagerStopped)
		}

		a.pending = m.conn
		defer func() { a.pending = nil }()

		err := a.apply(ctx, &PeerConnected{
			Peer:   route.Vertex(m.conn.PubKey()),
			Pinned: m.pinned,
		})

		return fn.NewResult(ManagerResp{}, err)

	case *managerEventMsg:
		a.observeOutcome(m.event)

		return fn.NewResult(ManagerResp{}, a.apply(ctx, m.event))

	case *lifecycleMsg:
		if m.start {
			a.armTicks(ctx, true, true)
		} else {
			a.shutdown(ctx)
		}

		return fn.Ok(ManagerResp{})

	case *roleQuery:
		return fn.Ok(ManagerResp{Role: a.state.Role(m.peer)})
	}

	return fn.Err[ManagerResp](fmt.Errorf("unknown message %T", msg))
}

// observeOutcome reports an attempt's outcome to the observer, if there is
// one and the manager will accept the outcome.
func (a *managerActor) observeOutcome(ev ManagerEvent) {
	o, ok := ev.(*HistoricalSyncOutcome)
	if !ok || a.m.cfg.OnSyncOutcome == nil {
		return
	}

	info, ok := a.state.inflight[o.Attempt]
	if !ok || info.session != o.Session {
		return
	}

	a.m.cfg.OnSyncOutcome(a.peers[o.Session].pub, o.Outcome)
}

// apply applies an event to the manager machine, commits the new state, and
// carries out the outbox.
func (a *managerActor) apply(ctx context.Context, ev ManagerEvent) error {
	next, outbox, err := protofsm.ApplyEvents(
		ctx, ManagerMachineState(a.state), ev, a.env,
	)
	if err != nil {
		log.Errorf("GossipSyncManager: %v", err)

		return err
	}

	a.state = next.(*ManagerState)

	for _, out := range outbox {
		a.dispatch(ctx, out)
	}

	return nil
}

// dispatch carries out one outbox event.
func (a *managerActor) dispatch(ctx context.Context, out ManagerOutbox) {
	switch o := out.(type) {
	case *SpawnPeer:
		if err := a.spawn(o.Session, o.Peer); err != nil {
			log.Errorf("GossipSyncManager: unable to start actors "+
				"for %v: %v", o.Peer, err)
		}

	case *StopPeer:
		p, ok := a.peers[o.Session]
		if !ok {
			return
		}
		delete(a.peers, o.Session)
		a.publishPeers()

		a.stopPeer(p)

	case *TellSyncer:
		p, ok := a.peers[o.Session]
		if !ok {
			return
		}

		// This may wait for a slot in the syncer's mailbox, but only
		// briefly: blocked senders are served first, and a syncer
		// never blocks on the manager.
		p.syncer.Tell(ctx, &syncerEventMsg{event: o.Event})

	case *PublishGraphSynced:
		a.m.graphSynced.Store(true)

	case *ResetHistoricalTimer:
		a.armTicks(ctx, false, true)

	default:
		log.Errorf("GossipSyncManager: unknown outbox event %T", out)
	}
}

// stopPeer unregisters and stops a session's actors. Unregister only stops an
// actor the receptionist still holds, so a false return means an actor was
// left running, which should never happen.
func (a *managerActor) stopPeer(p peerRefs) {
	sys := a.m.cfg.System
	if !syncerKey(p.pub).Unregister(sys, p.syncer) {
		log.Errorf("GossipSyncManager: syncer %v was not stopped",
			p.syncer.ID())
	}
	if !responderKey(p.pub).Unregister(sys, p.responder) {
		log.Errorf("GossipSyncManager: responder %v was not stopped",
			p.responder.ID())
	}
}

// shutdown cancels the recurring ticks and stops every peer's actors.
func (a *managerActor) shutdown(ctx context.Context) {
	a.stopped = true

	for _, id := range []timeout.ID{rotateTimerID, historicalTimerID} {
		a.m.cfg.Timeouts.Tell(ctx, &timeout.CancelTimeoutRequest{
			ID: id,
		})
	}

	for session, p := range a.peers {
		a.stopPeer(p)
		delete(a.peers, session)
	}
	a.publishPeers()
}

// spawn starts the syncer and responder actors of a new session.
func (a *managerActor) spawn(session SessionID, pub route.Vertex) error {
	conn := a.pending
	if conn == nil {
		return errors.New("no connection for new session")
	}

	cfg := a.m.cfg
	var peerLimiter *rate.Limiter
	if cfg.PeerMsgBytesPerSecond > 0 {
		peerLimiter = rate.NewLimiter(
			rate.Limit(cfg.PeerMsgBytesPerSecond),
			int(2*cfg.PeerMsgBytesPerSecond),
		)
	}
	send := &sender{conn: conn, peer: peerLimiter, global: a.global}

	env := &SyncerEnv{
		Peer:               pub,
		ChainHash:          cfg.ChainHash,
		Graph:              cfg.Graph,
		IsStillZombie:      cfg.IsStillZombie,
		BestHeight:         cfg.BestHeight,
		Now:                cfg.Now,
		Limits:             cfg.Limits,
		BatchSize:          cfg.BatchSize,
		ReplyTimeout:       cfg.ReplyTimeout,
		DrainTimeout:       cfg.DrainTimeout,
		NoTimestampQueries: cfg.NoTimestampQueries,
	}

	syncer := newSyncerActor(session, env, send, a.self, cfg.Timeouts)
	syncerRef, err := syncerKey(pub).Spawn(
		cfg.System, fmt.Sprintf("gossipsync-syncer-%v-%d", pub,
			session),
		syncer, actor.WithMailboxSize[syncerMsg, Ack](
			DefaultPeerMailboxSize,
		),
	)
	if err != nil {
		return err
	}
	syncer.self = syncerRef

	// The responder gets its own random source, seeded from ours. The
	// seed is drawn here on the manager's goroutine, in session order, so
	// a seeded run stays reproducible.
	seed := cfg.Rand.Uint64()
	rng := rand.New(rand.NewPCG(seed, seed))
	responder := newResponderActor(
		a.responderCfg, pub, session, send, cfg.Timeouts, rng.Shuffle,
	)
	responderRef, err := responderKey(pub).Spawn(
		cfg.System, fmt.Sprintf("gossipsync-responder-%v-%d", pub,
			session),
		responder, actor.WithMailboxSize[responderMsg, Ack](
			DefaultPeerMailboxSize,
		),
	)
	if err != nil {
		syncerKey(pub).Unregister(cfg.System, syncerRef)

		return err
	}
	responder.self = responderRef

	a.peers[session] = peerRefs{
		pub:       pub,
		syncer:    syncerRef,
		responder: responderRef,
	}
	a.publishPeers()

	return nil
}

// publishPeers publishes a fresh copy of the peer set for the handle.
func (a *managerActor) publishPeers() {
	peers := make(map[route.Vertex]peerRefs, len(a.peers))
	for _, p := range a.peers {
		peers[p.pub] = p
	}
	a.m.peers.Store(&peers)
}

// armTicks schedules the recurring rotate and historical ticks. Scheduling
// a tick that is already armed restarts its period.
func (a *managerActor) armTicks(ctx context.Context, rotate,
	historical bool) {

	tick := func(id timeout.ID, interval time.Duration,
		ev ManagerEvent) {

		callback := timeout.MapTickFired(
			a.self, func(timeout.TickFiredMsg) managerMsg {
				return &managerEventMsg{event: ev}
			},
		)
		req := &timeout.ScheduleRecurringTickRequest{
			ID:       id,
			Interval: interval,
			Callback: callback,
		}
		a.m.cfg.Timeouts.Tell(ctx, req)
	}

	if rotate {
		tick(rotateTimerID, a.m.cfg.RotateInterval, &RotateTick{})
	}
	if historical {
		tick(
			historicalTimerID, a.m.cfg.HistoricalSyncInterval,
			&HistoricalTick{},
		)
	}
}
