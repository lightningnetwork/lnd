package reputation

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// HeightSource returns the current best block height. It is injected so the
// in-flight risk calculation can convert cltv deltas to durations without the
// reputation package depending on the chain subsystem directly. It may return
// an error (e.g. a failed RPC), in which case the manager keeps its last cached
// height rather than resetting to zero.
type HeightSource func() (uint32, error)

// gcInterval is how often the stale-pending garbage collector runs.
const gcInterval = 5 * time.Minute

// heightRefreshInterval is how often the cached best block height is refreshed
// from the (potentially RPC-backed) height source. Blocks arrive ~every 10
// minutes, so 30s is ample; crucially this keeps the height-source call OFF the
// forwarding hot path.
const heightRefreshInterval = 30 * time.Second

// eventQueueSize bounds the buffered event channel. HTLC events are tiny and
// the worker's per-event work is microseconds, so this absorbs large bursts
// while capping memory; on overflow events are dropped (log-only, so a dropped
// event only costs a little reputation fidelity, never a forward).
const eventQueueSize = 2048

// dropLogInterval rate-limits the "events dropped" warning so a sustained
// overload cannot itself spam the log.
const dropLogInterval = 30 * time.Second

// eventKind enumerates the reputation lifecycle events the worker processes.
type eventKind uint8

const (
	evForward eventKind = iota
	evSettle
	evFail

	// evSnapshot is a read-only request: the worker builds a snapshot of
	// the per-channel state it exclusively owns and sends it back on the
	// event's reply channel. Routing the read through the worker keeps the
	// channel-state maps single-owner and lock-free (see Snapshot).
	evSnapshot

	// evBarrier is a test-only sentinel: the worker closes its done channel
	// once it has processed the barrier, letting a test deterministically
	// wait for all prior events to be applied.
	evBarrier
)

// event is a single reputation observation, fully self-contained so the worker
// never needs to read shared switch state. The timestamp (at) and height are
// captured at the hook, so async processing cannot reorder them relative to the
// per-channel averages.
type event struct {
	kind eventKind

	incoming, outgoing models.CircuitKey
	incomingAmt        lnwire.MilliSatoshi
	outgoingAmt        lnwire.MilliSatoshi
	advertisedFee      lnwire.MilliSatoshi
	incomingCltv       uint32
	accountable        bool
	settled            bool

	// at is the unix-seconds timestamp captured at the hook (finding #6).
	at uint64

	// height is the cached best block height captured at the hook.
	height uint32

	// done, when non-nil, is closed by the worker after the event is
	// processed (test-only barrier).
	done chan struct{}

	// reply, when non-nil (evSnapshot), receives the snapshot the worker
	// builds. It is buffered by the requester so the worker never blocks
	// sending on it.
	reply chan []ChannelReputationSnapshot
}

// Manager is the local reputation subsystem. It observes forwarded HTLCs via
// its OnForward/OnSettle/OnFail hooks, maintains per-channel reputation state,
// and logs the isolation decision it would make — without ever affecting
// routing (log-only).
//
// The hooks are non-blocking: they capture the event (timestamp and height
// included) and do a best-effort send to a bounded channel, so they never block
// the switch's forwarding goroutine and never do map mutation, floating-point
// math or logging on the hot path. A single worker goroutine drains the channel
// and is the ONLY goroutine that touches the channel-state maps, so those maps
// need no mutex against the switch. Nothing is persisted, so reputation is
// re-accrued from live traffic after a restart (the self-bootstrapping model).
type Manager struct {
	cfg Config

	clock        Clock
	heightSource HeightSource

	// cachedHeight is the most recently observed best block height. It is
	// refreshed off the hot path (at Start and by the worker) and read by
	// the hooks, so it stays atomic.
	cachedHeight atomic.Uint32

	// dropped counts events discarded because the worker's queue was full.
	dropped atomic.Uint64

	// lastDropLog is the unix-nanos of the last emitted drop warning, used
	// to rate-limit the warning under sustained overload.
	lastDropLog atomic.Int64

	// events is the bounded worker queue. Only the hooks send and only the
	// worker receives.
	events chan event

	// channels holds per-scid reputation state. It is owned exclusively by
	// the worker goroutine; no other goroutine may touch it (tests
	// synchronise via sync()).
	channels map[uint64]*channelReputation

	wg        sync.WaitGroup
	quit      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// Option configures a Manager.
type Option func(*Manager)

// WithClock sets the time source (defaults to the system clock).
func WithClock(c Clock) Option {
	return func(m *Manager) { m.clock = c }
}

// WithHeightSource sets the best-block-height getter.
func WithHeightSource(h HeightSource) Option {
	return func(m *Manager) { m.heightSource = h }
}

// NewManager constructs a reputation Manager with the given config and options.
func NewManager(cfg Config, opts ...Option) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid reputation config: %w", err)
	}

	m := &Manager{
		cfg:          cfg,
		clock:        NewSystemClock(),
		heightSource: func() (uint32, error) { return 0, nil },
		events:       make(chan event, eventQueueSize),
		channels:     make(map[uint64]*channelReputation),
		quit:         make(chan struct{}),
	}

	for _, opt := range opts {
		opt(m)
	}

	// Seed the cached height once so the hot path has a value even before
	// the maintenance loop runs (and for callers that never Start, e.g.
	// tests). This call is off the hot path.
	m.refreshHeight()

	return m, nil
}

// refreshHeight updates the cached best block height from the height source.
// It must NOT be called while holding mu (the source may be an RPC). On a
// source error it logs and keeps the last cached value rather than clobbering
// it with zero (which would make every in-flight risk calculation assume a
// worst-case hold from genesis).
func (m *Manager) refreshHeight() {
	height, err := m.heightSource()
	if err != nil {
		log.Warnf("Reputation height refresh failed, keeping cached "+
			"height %d: %v", m.cachedHeight.Load(), err)

		return
	}

	m.cachedHeight.Store(height)
}

// Start launches the single event worker (which also drives the periodic GC and
// height refresh) and refreshes the cached height before serving traffic.
// Per-channel state is created lazily on the first HTLC event, so there is
// nothing to load here (no persistence).
func (m *Manager) Start() error {
	m.startOnce.Do(func() {
		log.Infof("Reputation manager starting (log-only): "+
			"resolution_period=%v revenue_window=%v "+
			"reputation_multiplier=%d revenue_window_count=%d",
			m.cfg.ResolutionPeriod, m.cfg.RevenueWindow,
			m.cfg.ReputationMultiplier, m.cfg.RevenueWindowCount)

		// Refresh the cached height before serving traffic.
		m.refreshHeight()

		m.wg.Add(1)
		go m.worker()
	})

	return nil
}

// Stop tears down the subsystem, draining any queued events first so no
// observed HTLC is silently lost on a clean shutdown.
func (m *Manager) Stop() error {
	m.stopOnce.Do(func() {
		close(m.quit)
		m.wg.Wait()

		if dropped := m.dropped.Load(); dropped > 0 {
			log.Warnf("Reputation manager stopped: %d events were "+
				"dropped due to a full queue over its lifetime",
				dropped)
		} else {
			log.Infof("Reputation manager stopped")
		}
	})

	return nil
}

// now returns the current time in unix seconds.
func (m *Manager) now() uint64 {
	return unixSeconds(m.clock.Now())
}

// OnForward observes a forwarded HTLC at add time. It is non-blocking: it
// captures the event (timestamp and cached height included) and best-effort
// enqueues it for the worker, never touching shared state on the switch's
// forwarding goroutine. incoming/outgoing are the circuit keys, the amounts are
// the incoming and outgoing HTLC values, advertisedFee is the fee the node
// advertised on the outgoing link for this forward (used for both
// outgoing-reputation and incoming-revenue, per the proposal, to prevent
// over/under-payment inflation attacks), incomingCltv is the incoming cltv
// expiry, and accountable is the outgoing accountable signal.
func (m *Manager) OnForward(incoming, outgoing models.CircuitKey,
	incomingAmt, outgoingAmt, advertisedFee lnwire.MilliSatoshi,
	incomingCltv uint32, accountable bool) {

	m.enqueue(event{
		kind:          evForward,
		incoming:      incoming,
		outgoing:      outgoing,
		incomingAmt:   incomingAmt,
		outgoingAmt:   outgoingAmt,
		advertisedFee: advertisedFee,
		incomingCltv:  incomingCltv,
		accountable:   accountable,
		at:            m.now(),
		height:        m.cachedHeight.Load(),
	})
}

// OnSettle observes the successful resolution of a forwarded HTLC.
func (m *Manager) OnSettle(incoming, outgoing models.CircuitKey) {
	m.enqueue(event{
		kind:     evSettle,
		incoming: incoming,
		outgoing: outgoing,
		settled:  true,
		at:       m.now(),
	})
}

// OnFail observes the failed resolution of a forwarded HTLC.
func (m *Manager) OnFail(incoming, outgoing models.CircuitKey) {
	m.enqueue(event{
		kind:     evFail,
		incoming: incoming,
		outgoing: outgoing,
		settled:  false,
		at:       m.now(),
	})
}

// enqueue does a non-blocking send of an event to the worker queue. On a full
// queue it drops the event, bumps the dropped counter and emits a rate-limited
// warning — it must NEVER block the caller (the switch's forwarding goroutine).
func (m *Manager) enqueue(ev event) {
	select {
	case m.events <- ev:

	default:
		dropped := m.dropped.Add(1)
		m.logDrop(dropped)
	}
}

// logDrop emits a warning about dropped events, rate-limited to at most once
// per dropLogInterval so a sustained overload cannot itself spam the log.
func (m *Manager) logDrop(total uint64) {
	now := m.clock.Now().UnixNano()
	last := m.lastDropLog.Load()

	if now-last < int64(dropLogInterval) {
		return
	}

	if !m.lastDropLog.CompareAndSwap(last, now) {
		return
	}

	log.Warnf("Reputation event queue full; dropped event (%d total "+
		"dropped). Reputation fidelity is reduced but forwarding is "+
		"unaffected (log-only).", total)
}

// worker is the single goroutine that owns the channel-state maps. It drains
// the event queue and drives the periodic GC and height refresh. On quit it
// drains any already-queued events so a clean shutdown does not silently lose
// observations.
func (m *Manager) worker() {
	defer m.wg.Done()

	gcTicker := time.NewTicker(gcInterval)
	defer gcTicker.Stop()

	heightTicker := time.NewTicker(heightRefreshInterval)
	defer heightTicker.Stop()

	for {
		select {
		case <-m.quit:
			m.drainRemaining()

			return

		case ev := <-m.events:
			m.process(ev)

		case <-heightTicker.C:
			m.refreshHeight()

		case <-gcTicker.C:
			m.gcStalePendings()
		}
	}
}

// drainRemaining processes any events already sitting in the queue at shutdown,
// without blocking for new ones.
func (m *Manager) drainRemaining() {
	for {
		select {
		case ev := <-m.events:
			m.process(ev)

		default:
			return
		}
	}
}

// process applies a single event. It runs only on the worker goroutine, so it
// mutates the channel-state maps without any lock.
func (m *Manager) process(ev event) {
	switch ev.kind {
	case evForward:
		m.processForward(ev)

	case evSettle, evFail:
		m.processResolve(ev)

	case evSnapshot:
		ev.reply <- m.buildSnapshot()

	case evBarrier:
		// Test-only sentinel: signal that all prior events are applied.
	}

	if ev.done != nil {
		close(ev.done)
	}
}

// processForward records the pending HTLC and logs the (log-only) isolation
// decision. Worker-only.
func (m *Manager) processForward(ev event) {
	d, err := m.addHTLC(ev)
	if err != nil {
		log.Warnf("Reputation OnForward(in=%v out=%v) error: %v",
			ev.incoming, ev.outgoing, err)

		return
	}

	// Emit the greppable isolation decision line: if this HTLC were
	// forwarded in isolation, would its outgoing channel have sufficient
	// reputation to be protected? This is log-only and never affects
	// forwarding.
	log.Infof("reputation decision: chan=%v htlc=%v sufficient=%v (would "+
		"be protected in isolation)", ev.outgoing.ChanID.ToUint64(),
		ev.incoming, d.sufficient)

	log.Debugf("Reputation forward in=%v out=%v amt_in=%v amt_out=%v "+
		"advertised_fee=%v accountable=%v height=%d => %s", ev.incoming,
		ev.outgoing, ev.incomingAmt, ev.outgoingAmt, ev.advertisedFee,
		ev.accountable, ev.height, d)
}

// processResolve applies an HTLC resolution. Worker-only.
func (m *Manager) processResolve(ev event) {
	if err := m.resolveHTLC(ev); err != nil {
		log.Warnf("Reputation resolve(in=%v out=%v settled=%v) error: "+
			"%v", ev.incoming, ev.outgoing, ev.settled, err)
	}
}

// getOrCreateChannel returns the reputation state for an scid, creating it
// lazily (zero reputation, initialised as of the event timestamp) if it does
// not yet exist. Worker-only.
func (m *Manager) getOrCreateChannel(scid uint64,
	at uint64) *channelReputation {

	if c, ok := m.channels[scid]; ok {
		return c
	}

	c := newChannelReputation(m.cfg, at)
	m.channels[scid] = c

	return c
}

// addHTLC records the pending HTLC and computes the (log-only) isolation
// decision for it. Worker-only.
func (m *Manager) addHTLC(ev event) (isolationDecision, error) {
	incoming, outgoing := ev.incoming, ev.outgoing
	incomingAmt, outgoingAmt := ev.incomingAmt, ev.outgoingAmt
	advertisedFee := ev.advertisedFee
	incomingCltv, accountable := ev.incomingCltv, ev.accountable
	at, height := ev.at, ev.height

	if outgoingAmt > incomingAmt {
		return isolationDecision{}, fmt.Errorf("outgoing amount %v "+
			"exceeds incoming %v", outgoingAmt, incomingAmt)
	}

	outScid := outgoing.ChanID.ToUint64()
	inScid := incoming.ChanID.ToUint64()

	// Initialise any lazily-created channel state as of the event
	// timestamp (finding #6): using a fresh m.now() here could initialise
	// an average a moment AFTER `at`, causing this very event to be
	// rejected as backwards time.
	outChan := m.getOrCreateChannel(outScid, at)
	inChan := m.getOrCreateChannel(inScid, at)

	ref := incoming
	if _, ok := outChan.pendingHTLCs[ref]; ok {
		return isolationDecision{}, fmt.Errorf("duplicate htlc %v", ref)
	}

	// Use the fee the node advertised for this forward rather than the
	// offered fee (incoming - outgoing). Since an accepted HTLC always pays
	// at least the policy fee, this equals in-out when there is no
	// overpayment and excludes overpayment otherwise, preventing a sender
	// from inflating (or, via the offered-fee delta, destroying) its
	// reputation.
	fee := uint64(advertisedFee)
	inFlightHTLCRisk := m.cfg.inFlightRisk(fee, incomingCltv, height)

	outReputation, err := outChan.outgoingReputation.valueAt(at)
	if err != nil {
		return isolationDecision{}, err
	}

	// Score the HTLC in isolation against the incoming channel's revenue
	// threshold.
	sufficient, threshold, err := inChan.sufficientReputation(
		inFlightHTLCRisk, outReputation, at,
	)
	if err != nil {
		return isolationDecision{}, err
	}

	outChan.pendingHTLCs[ref] = &pendingHTLC{
		fee:            fee,
		accountable:    accountable,
		addedAt:        at,
		incomingCltv:   incomingCltv,
		inFlightRisk:   inFlightHTLCRisk,
		maxHoldSeconds: maxHoldSeconds(incomingCltv, height),
	}

	return isolationDecision{
		sufficient:         sufficient,
		outgoingReputation: outReputation,
		htlcRisk:           inFlightHTLCRisk,
		threshold:          threshold,
	}, nil
}

// resolveHTLC applies an HTLC resolution to reputation and revenue.
// Worker-only. Mirrors the LDK reference resolve_htlc.
func (m *Manager) resolveHTLC(ev event) error {
	incoming, outgoing := ev.incoming, ev.outgoing
	settled, at := ev.settled, ev.at

	inScid := incoming.ChanID.ToUint64()
	outScid := outgoing.ChanID.ToUint64()

	outChan, ok := m.channels[outScid]
	if !ok {
		// Tolerate: we never saw the forward (e.g. enabled mid-flight,
		// or the add was lost across a restart).
		log.Debugf("Reputation resolve for unknown outgoing channel "+
			"%d (htlc %v); ignoring", outScid, incoming)

		return nil
	}

	ref := incoming
	pending, ok := outChan.pendingHTLCs[ref]
	if !ok {
		log.Debugf("Reputation resolve for unmatched htlc %v; ignoring",
			incoming)

		return nil
	}

	// Validate the timestamp BEFORE mutating any state (finding #6): if the
	// resolution predates the add we must leave the pending entry and the
	// averages untouched rather than deleting the pending and then failing,
	// which would leave partial state (a lost pending that can never
	// resolve).
	if at < pending.addedAt {
		return errBackwardsTime
	}
	resolutionTime := time.Duration(at-pending.addedAt) * time.Second

	effFee := m.cfg.effectiveFee(
		pending.fee, resolutionTime, pending.accountable, settled,
	)

	// The reputation average errors only on a backwards timestamp; we know
	// at >= pending.addedAt >= the average's last update, so this cannot
	// fail here, but we still check before committing the delete.
	newRep, err := outChan.outgoingReputation.add(effFee, at)
	if err != nil {
		return err
	}

	inChan := m.getOrCreateChannel(inScid, at)

	if settled {
		fee := saturatingI64FromU64(pending.fee)
		if _, err := inChan.incomingRevenue.add(fee, at); err != nil {
			return err
		}
	}

	// All updates succeeded; now it is safe to drop the pending entry.
	delete(outChan.pendingHTLCs, ref)

	// Emit a distinctive, greppable summary whenever a resolution moves an
	// outgoing channel's reputation. Observability is the whole point of
	// this (log-only, off-by-default) subsystem, so a per-resolution line
	// is acceptable. The phrasing ("Reputation gained"/"Reputation lost")
	// is intentionally stable: integration tests match on it to confirm
	// reputation is actually being computed.
	switch {
	case effFee > 0:
		log.Infof("Reputation gained: outgoing=%v eff_fee=%d "+
			"new_outgoing_reputation=%d", outScid, effFee, newRep)

	case effFee < 0:
		log.Infof("Reputation lost: outgoing=%v eff_fee=%d "+
			"new_outgoing_reputation=%d", outScid, effFee, newRep)

	default:
		log.Debugf("Reputation resolve in=%v out=%v settled=%v "+
			"eff_fee=0 (neutral)", incoming, outgoing, settled)
	}

	return nil
}

// gcStalePendings evicts pending HTLCs that have outlived their worst-case hold
// time, guarding against a leaked pending from a missed resolution.
// Worker-only.
func (m *Manager) gcStalePendings() {
	at := m.now()
	var evicted int

	for _, ch := range m.channels {
		for ref, p := range ch.pendingHTLCs {
			if at <= p.addedAt+p.maxHoldSeconds {
				continue
			}

			delete(ch.pendingHTLCs, ref)
			evicted++
		}
	}

	if evicted > 0 {
		log.Debugf("Reputation GC evicted %d stale pending HTLCs",
			evicted)
	}
}
