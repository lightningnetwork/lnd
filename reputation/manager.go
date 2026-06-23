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
// reputation package depending on the chain subsystem directly.
type HeightSource func() uint32

// fallbackMaxInFlightMsat is used when an HTLC references a channel we have no
// learned limits for (a safety net for log-only mode; should be rare since both
// legs of a forward are our own channels).
const fallbackMaxInFlightMsat = 483_000_000

// gcInterval is how often the stale-pending garbage collector runs.
const gcInterval = 5 * time.Minute

// flushInterval is how often dirty channel state is flushed to the store.
const flushInterval = time.Minute

// heightRefreshInterval is how often the cached best block height is refreshed
// from the (potentially RPC-backed) height source. Blocks arrive ~every 10
// minutes, so 30s is ample; crucially this keeps the height-source call OFF the
// forwarding hot path and out from under the manager mutex.
const heightRefreshInterval = 30 * time.Second

// Manager is the local reputation subsystem. It observes forwarded HTLCs via
// its OnForward/OnSettle/OnFail hooks, maintains per-channel reputation and
// resource-bucket state, and logs the decisions it would make — without ever
// affecting routing (log-only).
//
// All event processing is synchronous and in-memory under mu; only persistence
// and GC run in background goroutines started by Start.
type Manager struct {
	cfg Config

	clock        Clock
	heightSource HeightSource
	channelSrc   ChannelSource
	store        Store

	// cachedHeight is the most recently observed best block height. It is
	// refreshed off the hot path (at Start and by the maintenance loop) so
	// the forwarding hooks never call the (possibly RPC-backed) height
	// source while holding mu. See review finding C1.
	cachedHeight atomic.Uint32

	mu       sync.Mutex
	channels map[uint64]*channelReputation
	limits   map[uint64]ChannelInfo
	dirty    map[uint64]struct{}

	wg          sync.WaitGroup
	quit        chan struct{}
	startOnce   sync.Once
	stopOnce    sync.Once
	cancelSubFn func()
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

// WithChannelSource sets the channel-lifecycle read seam.
func WithChannelSource(s ChannelSource) Option {
	return func(m *Manager) { m.channelSrc = s }
}

// WithStore sets the persistence backend (defaults to no persistence).
func WithStore(s Store) Option {
	return func(m *Manager) { m.store = s }
}

// NewManager constructs a reputation Manager with the given config and options.
func NewManager(cfg Config, opts ...Option) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid reputation config: %w", err)
	}

	m := &Manager{
		cfg:          cfg,
		clock:        NewSystemClock(),
		heightSource: func() uint32 { return 0 },
		channels:     make(map[uint64]*channelReputation),
		limits:       make(map[uint64]ChannelInfo),
		dirty:        make(map[uint64]struct{}),
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
// It must NOT be called while holding mu (the source may be an RPC).
func (m *Manager) refreshHeight() {
	m.cachedHeight.Store(m.heightSource())
}

// Start initializes channel state from the store and channel source, replays
// in-flight HTLCs, and launches background GC/flush loops.
func (m *Manager) Start() error {
	var startErr error
	m.startOnce.Do(func() {
		log.Infof("Reputation manager starting (log-only): "+
			"general=%d%% congestion=%d%% resolution_period=%v "+
			"revenue_window=%v reputation_multiplier=%d",
			m.cfg.GeneralPct, m.cfg.CongestionPct,
			m.cfg.ResolutionPeriod, m.cfg.RevenueWindow,
			m.cfg.ReputationMultiplier)

		// Load persisted state (warm restart) if a store is configured.
		if m.store != nil {
			if err := m.loadFromStore(); err != nil {
				startErr = fmt.Errorf("load reputation "+
					"state: %w", err)

				return
			}
		}

		// Seed/refresh channel limits + create any missing channels
		// (cold start builds empty per-channel state here).
		if m.channelSrc != nil {
			if err := m.seedChannels(); err != nil {
				startErr = fmt.Errorf("seed channels: %w", err)

				return
			}

			if err := m.subscribeChannels(); err != nil {
				startErr = fmt.Errorf("subscribe channels: "+
					"%w", err)

				return
			}
		}

		// Refresh the cached height before serving traffic.
		m.refreshHeight()

		m.wg.Add(1)
		go m.maintenanceLoop()
	})

	return startErr
}

// Stop tears down the subsystem, flushing any dirty state.
func (m *Manager) Stop() error {
	m.stopOnce.Do(func() {
		close(m.quit)
		if m.cancelSubFn != nil {
			m.cancelSubFn()
		}
		m.wg.Wait()

		if m.store != nil {
			if err := m.flush(); err != nil {
				log.Errorf("Reputation final flush failed: %v",
					err)
			}
		}

		log.Infof("Reputation manager stopped")
	})

	return nil
}

// now returns the current time in unix seconds.
func (m *Manager) now() uint64 {
	return unixSeconds(m.clock.Now())
}

// OnForward observes a forwarded HTLC at add time and records the (log-only)
// forwarding decision. incoming/outgoing are the circuit keys, the amounts are
// the incoming and outgoing HTLC values, incomingCltv is the incoming cltv
// expiry, and accountable is the observed accountable signal.
func (m *Manager) OnForward(incoming, outgoing models.CircuitKey,
	incomingAmt, outgoingAmt lnwire.MilliSatoshi, incomingCltv uint32,
	accountable bool) {

	// Read the cached height BEFORE taking the lock so the (possibly
	// RPC-backed) height source is never called under mu on the hot path.
	height := m.cachedHeight.Load()

	m.mu.Lock()
	defer m.mu.Unlock()

	at := m.now()

	o, err := m.addHTLCLocked(
		incoming, outgoing, incomingAmt, outgoingAmt, incomingCltv,
		accountable, at, height,
	)
	if err != nil {
		log.Warnf("Reputation OnForward(in=%v out=%v) error: %v",
			incoming, outgoing, err)

		return
	}

	log.Debugf("Reputation forward in=%v out=%v amt_in=%v amt_out=%v "+
		"accountable=%v height=%d => %s", incoming, outgoing,
		incomingAmt, outgoingAmt, accountable, height, o)
}

// OnSettle observes the successful resolution of a forwarded HTLC.
func (m *Manager) OnSettle(incoming, outgoing models.CircuitKey) {
	m.onResolve(incoming, outgoing, true)
}

// OnFail observes the failed resolution of a forwarded HTLC.
func (m *Manager) OnFail(incoming, outgoing models.CircuitKey) {
	m.onResolve(incoming, outgoing, false)
}

// onResolve is the shared settle/fail resolution path.
func (m *Manager) onResolve(incoming, outgoing models.CircuitKey,
	settled bool) {

	m.mu.Lock()
	defer m.mu.Unlock()

	at := m.now()
	err := m.resolveHTLCLocked(incoming, outgoing, settled, at)
	if err != nil {
		log.Warnf("Reputation resolve(in=%v out=%v settled=%v) error: "+
			"%v", incoming, outgoing, settled, err)
	}
}

// getOrCreateChannelLocked returns the reputation state for an scid, creating
// it lazily (zero reputation) from learned limits — or fallback defaults — if
// it does not yet exist. Caller must hold mu.
func (m *Manager) getOrCreateChannelLocked(scid uint64) *channelReputation {
	if c, ok := m.channels[scid]; ok {
		return c
	}

	info, ok := m.limits[scid]
	maxHTLCs := uint16(protocolMaxAcceptedHTLCs)
	maxInFlight := uint64(fallbackMaxInFlightMsat)
	if ok {
		maxHTLCs = info.MaxAcceptedHTLCs
		maxInFlight = uint64(info.MaxInFlightMsat)
	} else {
		log.Warnf("Reputation: creating channel %d with fallback "+
			"limits (no learned capacity)", scid)
	}

	c := newChannelReputation(scid, maxInFlight, maxHTLCs, m.cfg, m.now())
	m.channels[scid] = c

	return c
}

// addHTLCLocked implements the forwarding decision tree and records the pending
// HTLC. Caller must hold mu. Mirrors the LDK reference add_htlc.
func (m *Manager) addHTLCLocked(incoming, outgoing models.CircuitKey,
	incomingAmt, outgoingAmt lnwire.MilliSatoshi, incomingCltv uint32,
	accountable bool, at uint64, height uint32) (outcome, error) {

	if outgoingAmt > incomingAmt {
		return outcome{}, fmt.Errorf("outgoing amount %v exceeds "+
			"incoming %v", outgoingAmt, incomingAmt)
	}

	inScid := incoming.ChanID.ToUint64()
	outScid := outgoing.ChanID.ToUint64()

	outChan := m.getOrCreateChannelLocked(outScid)
	inChan := m.getOrCreateChannelLocked(inScid)

	ref := incoming
	if _, ok := outChan.pendingHTLCs[ref]; ok {
		return outcome{}, fmt.Errorf("duplicate htlc %v", ref)
	}

	fee := uint64(incomingAmt - outgoingAmt)
	inFlightHTLCRisk := m.cfg.inFlightRisk(fee, incomingCltv, height)

	outReputation, err := outChan.outgoingReputation.valueAt(at)
	if err != nil {
		return outcome{}, err
	}
	outInFlightRisk := outChan.outgoingInFlightRisk()
	pendingInCongestion := outChan.pendingHTLCsInCongestion(inScid)

	o, err := m.decideLocked(
		inChan, outScid, uint64(incomingAmt), accountable,
		inFlightHTLCRisk, outReputation, outInFlightRisk,
		pendingInCongestion, at,
	)
	if err != nil {
		return outcome{}, err
	}

	if !o.forward {
		return o, nil
	}

	// Reserve the chosen bucket on the incoming channel.
	switch o.bucket {
	case bucketGeneral:
		err = inChan.generalBucket.addHTLC(outScid, uint64(incomingAmt))
	case bucketCongestion:
		err = inChan.congestionBucket.addHTLC(uint64(incomingAmt))
	case bucketProtected:
		err = inChan.protectedBucket.addHTLC(uint64(incomingAmt))
	}
	if err != nil {
		return outcome{}, err
	}

	outChan.pendingHTLCs[ref] = &pendingHTLC{
		incomingChan:        incoming.ChanID,
		incomingAmount:      incomingAmt,
		fee:                 fee,
		outgoingAccountable: o.accountable,
		addedAt:             at,
		inFlightRisk:        inFlightHTLCRisk,
		bucket:              o.bucket,
		maxHoldSeconds:      maxHoldSeconds(incomingCltv, height),
	}

	m.markDirtyLocked(inScid)
	m.markDirtyLocked(outScid)

	return o, nil
}

// decideLocked runs the bucket decision tree. Caller must hold mu.
func (m *Manager) decideLocked(inChan *channelReputation, outScid uint64,
	incomingAmt uint64, accountable bool, inFlightHTLCRisk uint64,
	outReputation int64, outInFlightRisk uint64, pendingInCongestion bool,
	at uint64) (outcome, error) {

	if !accountable {
		// Unaccountable: prefer the general bucket; fall back to
		// protected (if reputation suffices) then congestion.
		ok, err := inChan.generalBucket.canAddHTLC(outScid, incomingAmt)
		if err != nil {
			return outcome{}, err
		}
		if ok {
			return outcome{
				forward: true, accountable: false,
				bucket: bucketGeneral,
			}, nil
		}

		sufficient, _, err := inChan.sufficientReputation(
			inFlightHTLCRisk, outReputation, outInFlightRisk, at,
		)
		if err != nil {
			return outcome{}, err
		}

		if sufficient &&
			inChan.protectedBucket.resourcesAvailable(incomingAmt) {

			return outcome{
				forward: true, accountable: true,
				bucket: bucketProtected,
			}, nil
		}

		eligible, err := inChan.congestionEligible(
			pendingInCongestion, incomingAmt, outScid, at,
		)
		if err != nil {
			return outcome{}, err
		}
		if eligible {
			return outcome{
				forward: true, accountable: true,
				bucket: bucketCongestion,
			}, nil
		}

		return outcome{forward: false}, nil
	}

	// Accountable: only forward with sufficient reputation, preferring
	// protected then general.
	sufficient, _, err := inChan.sufficientReputation(
		inFlightHTLCRisk, outReputation, outInFlightRisk, at,
	)
	if err != nil {
		return outcome{}, err
	}
	if !sufficient {
		return outcome{forward: false}, nil
	}

	if inChan.protectedBucket.resourcesAvailable(incomingAmt) {
		return outcome{
			forward:     true,
			accountable: true,
			bucket:      bucketProtected,
		}, nil
	}

	ok, err := inChan.generalBucket.canAddHTLC(outScid, incomingAmt)
	if err != nil {
		return outcome{}, err
	}
	if ok {
		return outcome{
			forward: true, accountable: true, bucket: bucketGeneral,
		}, nil
	}

	return outcome{forward: false}, nil
}

// resolveHTLCLocked applies an HTLC resolution to reputation/revenue/buckets.
// Caller must hold mu. Mirrors the LDK reference resolve_htlc.
func (m *Manager) resolveHTLCLocked(incoming, outgoing models.CircuitKey,
	settled bool, at uint64) error {

	inScid := incoming.ChanID.ToUint64()
	outScid := outgoing.ChanID.ToUint64()

	outChan, ok := m.channels[outScid]
	if !ok {
		// Tolerate: we never saw the forward (e.g. enabled mid-flight).
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
	delete(outChan.pendingHTLCs, ref)

	if at < pending.addedAt {
		return errBackwardsTime
	}
	resolutionTime := time.Duration(at-pending.addedAt) * time.Second

	effFee := m.cfg.effectiveFee(
		pending.fee, resolutionTime, pending.outgoingAccountable,
		settled,
	)
	newRep, err := outChan.outgoingReputation.add(effFee, at)
	if err != nil {
		return err
	}

	inChan, ok := m.channels[inScid]
	if !ok {
		inChan = m.getOrCreateChannelLocked(inScid)
	}

	// A congestion HTLC held past the resolution period blocks the outgoing
	// channel from the congestion bucket for ~2 weeks.
	exceeded := resolutionPeriodExceeded(
		resolutionTime, m.cfg.ResolutionPeriod,
	)
	if pending.bucket == bucketCongestion && exceeded {
		inChan.misusedCongestion(outScid, at)
	}

	m.releaseBucketLocked(inChan, outScid, pending)

	if settled {
		fee := saturatingI64FromU64(pending.fee)
		if _, err := inChan.incomingRevenue.add(fee, at); err != nil {
			return err
		}
	}

	m.markDirtyLocked(inScid)
	m.markDirtyLocked(outScid)

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

// releaseBucketLocked frees the bucket resources a pending HTLC reserved on its
// incoming channel. Caller must hold mu. Used by both the resolution path and
// the stale-pending GC so neither can leak occupancy.
func (m *Manager) releaseBucketLocked(inChan *channelReputation, outScid uint64,
	p *pendingHTLC) {

	var err error
	switch p.bucket {
	case bucketGeneral:
		err = inChan.generalBucket.removeHTLC(
			outScid, uint64(p.incomingAmount),
		)
	case bucketCongestion:
		err = inChan.congestionBucket.removeHTLC(
			uint64(p.incomingAmount),
		)
	case bucketProtected:
		err = inChan.protectedBucket.removeHTLC(
			uint64(p.incomingAmount),
		)
	}
	if err != nil {
		log.Warnf("Reputation bucket release error (in=%d out=%d): %v",
			p.incomingChan.ToUint64(), outScid, err)
	}
}

// maintenanceLoop runs the periodic GC and persistence flush.
func (m *Manager) maintenanceLoop() {
	defer m.wg.Done()

	gcTicker := time.NewTicker(gcInterval)
	defer gcTicker.Stop()

	flushTicker := time.NewTicker(flushInterval)
	defer flushTicker.Stop()

	heightTicker := time.NewTicker(heightRefreshInterval)
	defer heightTicker.Stop()

	for {
		select {
		case <-m.quit:
			return

		case <-heightTicker.C:
			m.refreshHeight()

		case <-gcTicker.C:
			m.gcStalePendings()

		case <-flushTicker.C:
			if m.store != nil {
				if err := m.flush(); err != nil {
					log.Errorf("Reputation flush: %v", err)
				}
			}
		}
	}
}

// gcStalePendings evicts pending HTLCs that have outlived their worst-case hold
// time, guarding against a leaked pending from a missed resolution.
func (m *Manager) gcStalePendings() {
	m.mu.Lock()
	defer m.mu.Unlock()

	at := m.now()
	var evicted int

	// Each channel's pendingHTLCs are the HTLCs going OUT on it, so the map
	// key (outScid) is this channel's scid and p.incomingChan is where the
	// bucket resources were reserved.
	for outScid, ch := range m.channels {
		for ref, p := range ch.pendingHTLCs {
			if at <= p.addedAt+p.maxHoldSeconds {
				continue
			}

			delete(ch.pendingHTLCs, ref)
			evicted++

			// Release the bucket resources this HTLC reserved
			// on its incoming channel; otherwise a missed
			// resolution would leak occupancy (review finding
			// B2).
			inScid := p.incomingChan.ToUint64()
			if inChan, ok := m.channels[inScid]; ok {
				// A congestion HTLC evicted by GC was held
				// past its worst-case hold time, the
				// strongest misuse signal, so stamp the
				// congestion-misuse block like the resolve
				// path (review finding m-1).
				if p.bucket == bucketCongestion {
					inChan.misusedCongestion(outScid, at)
				}
				m.releaseBucketLocked(inChan, outScid, p)
				m.markDirtyLocked(inScid)
			}
			m.markDirtyLocked(outScid)
		}
	}

	if evicted > 0 {
		log.Debugf("Reputation GC evicted %d stale pending HTLCs",
			evicted)
	}
}

// markDirtyLocked flags a channel as needing persistence. Caller must hold mu.
func (m *Manager) markDirtyLocked(scid uint64) {
	if m.store == nil {
		return
	}

	m.dirty[scid] = struct{}{}
}
