package reputation

import (
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// staleCheckInterval is how often the manager checks for pending HTLCs that
// have outlived the worst case time they could be held for.
const staleCheckInterval = 5 * time.Minute

// Manager is the local reputation subsystem. It observes forwarded HTLCs via
// its OnForward/OnSettle/OnFail hooks, maintains per-channel reputation state,
// and logs the reputation decision it would make, without ever affecting
// routing (log-only).
//
// The hooks run synchronously on the caller's goroutine: they take the
// manager's lock, update the per-channel state, and return. The work per hook
// is a handful of map lookups and floating-point operations, so it is cheap
// enough to sit on the switch's forwarding path, and computing the decision
// inline (rather than on a background worker) is what a future enforcement step
// will require. Nothing is persisted, so reputation is re-accrued from live
// traffic after a restart.
type Manager struct {
	cfg   Config
	clock clock.Clock

	// mu guards channels. It is held for the duration of each hook.
	mu sync.Mutex

	// channels holds per-scid reputation state, created lazily on the first
	// HTLC event for a channel.
	channels map[uint64]*channelReputation

	// htlcIndex maps each pending HTLC's incoming circuit key to the scid
	// of the outgoing channel it is pending on. Resolutions are looked up
	// through this index because the switch's resolution paths do not
	// reliably know the outgoing channel (a mailbox-failed add, for
	// example, never had a keystone set).
	htlcIndex map[models.CircuitKey]uint64

	wg        sync.WaitGroup
	quit      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewManager constructs a reputation Manager with the given config and clock.
// The clock is mandatory (production passes clock.NewDefaultClock; tests pass a
// test clock).
func NewManager(cfg Config, clk clock.Clock) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid reputation config: %w", err)
	}

	if clk == nil {
		return nil, fmt.Errorf("reputation manager requires a clock")
	}

	return &Manager{
		cfg:       cfg,
		clock:     clk,
		channels:  make(map[uint64]*channelReputation),
		htlcIndex: make(map[models.CircuitKey]uint64),
		quit:      make(chan struct{}),
	}, nil
}

// Start launches the periodic stale-pending check. Per-channel state is created
// lazily on the first HTLC event, so there is nothing to load.
func (m *Manager) Start() error {
	m.startOnce.Do(func() {
		log.Infof("Reputation manager starting (log-only): "+
			"resolution_period=%v revenue_window=%v "+
			"reputation_multiplier=%d revenue_window_count=%d",
			m.cfg.ResolutionPeriod, m.cfg.RevenueWindow,
			m.cfg.ReputationMultiplier, m.cfg.RevenueWindowCount)

		m.wg.Add(1)
		go m.staleCheckLoop()
	})

	return nil
}

// Stop tears down the subsystem.
func (m *Manager) Stop() error {
	m.stopOnce.Do(func() {
		close(m.quit)
		m.wg.Wait()

		log.Infof("Reputation manager stopped")
	})

	return nil
}

// OnForward observes a forwarded HTLC at the point the switch commits to
// forwarding it. outgoing identifies the outgoing channel, advertisedFee is the
// total fee the node advertised for this forward (outbound plus inbound
// component, attributed to the outgoing channel), height is the current best
// block height, and accountable is the outgoing accountable signal.
func (m *Manager) OnForward(incoming models.CircuitKey,
	outgoing lnwire.ShortChannelID, incomingAmt, outgoingAmt,
	advertisedFee lnwire.MilliSatoshi, incomingCltv, height uint32,
	accountable bool) {

	at := m.clock.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	d, err := m.addHTLC(
		incoming, outgoing, advertisedFee, incomingCltv, height,
		accountable, at,
	)
	if err != nil {
		log.Warnf("Reputation OnForward(in=%v out=%v) error: %v",
			incoming, outgoing, err)

		return
	}

	// Emit the greppable decision line. This is log-only and never affects
	// forwarding.
	log.Infof("reputation decision: chan=%v htlc=%v in_isolation=%v "+
		"with_in_flight=%v", outgoing.ToUint64(), incoming,
		d.inIsolation, d.withInFlight)

	log.Debugf("Reputation forward in=%v out=%v amt_in=%v amt_out=%v "+
		"advertised_fee=%v accountable=%v height=%d => %s", incoming,
		outgoing, incomingAmt, outgoingAmt, advertisedFee, accountable,
		height, d)
}

// OnSettle observes the successful resolution of a forwarded HTLC, identified
// by its incoming circuit key.
func (m *Manager) OnSettle(incoming models.CircuitKey) {
	m.resolve(incoming, true)
}

// OnFail observes the failed resolution of a forwarded HTLC, identified by its
// incoming circuit key.
func (m *Manager) OnFail(incoming models.CircuitKey) {
	m.resolve(incoming, false)
}

// resolve applies an HTLC resolution under the lock.
func (m *Manager) resolve(incoming models.CircuitKey, settled bool) {
	at := m.clock.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.resolveHTLC(incoming, settled, at); err != nil {
		log.Warnf("Reputation resolve(in=%v settled=%v) error: %v",
			incoming, settled, err)
	}
}

// getOrCreateChannel returns the reputation state for an scid, creating it
// lazily (zero reputation, initialised as of at) if it does not yet exist.
// Caller must hold mu.
func (m *Manager) getOrCreateChannel(scid uint64,
	at time.Time) *channelReputation {

	if c, ok := m.channels[scid]; ok {
		return c
	}

	c := newChannelReputation(m.cfg, at)
	m.channels[scid] = c

	return c
}

// addHTLC records the pending HTLC and computes the (log-only) decision for it.
// Caller must hold mu.
func (m *Manager) addHTLC(incoming models.CircuitKey,
	outgoing lnwire.ShortChannelID, advertisedFee lnwire.MilliSatoshi,
	incomingCltv, height uint32, accountable bool,
	at time.Time) (decision, error) {

	// The incoming expiry must be in the future; if it is not, something
	// is badly wrong upstream (the HTLC should never have been accepted)
	// and we cannot bound its hold time, so we refuse to track it.
	if incomingCltv <= height {
		return decision{}, fmt.Errorf("incoming cltv %d not beyond "+
			"current height %d", incomingCltv, height)
	}

	outScid := outgoing.ToUint64()
	inScid := incoming.ChanID.ToUint64()

	// Initialise any lazily-created channel state as of the event timestamp
	// (a fresh clock read here could be a moment after `at`, causing this
	// event to be rejected as backwards time).
	outChan := m.getOrCreateChannel(outScid, at)
	inChan := m.getOrCreateChannel(inScid, at)

	if _, ok := m.htlcIndex[incoming]; ok {
		return decision{}, fmt.Errorf("duplicate htlc %v", incoming)
	}

	// BOLT #1280 scores reputation on the fee the node advertised, not the
	// offered fee, so a sender cannot inflate or destroy reputation by
	// over/under-paying.
	fee := uint64(advertisedFee)
	risk := m.cfg.inFlightRisk(fee, incomingCltv, height)
	htlcRisk := satFromUint(risk)

	// The total risk is this HTLC plus the accountable HTLCs already in
	// flight on the outgoing channel, which is the risk BOLT #1280 uses.
	totalRisk := htlcRisk.Add(outChan.inFlightRisk())

	outReputation, err := outChan.outgoingReputation.valueAt(at)
	if err != nil {
		return decision{}, err
	}

	// Score the HTLC both on its own risk and against the channel's total
	// in-flight risk.
	inIsolation, threshold, err := inChan.sufficientReputation(
		htlcRisk, outReputation, at,
	)
	if err != nil {
		return decision{}, err
	}

	withInFlight, _, err := inChan.sufficientReputation(
		totalRisk, outReputation, at,
	)
	if err != nil {
		return decision{}, err
	}

	outChan.pendingHTLCs[incoming] = &pendingHTLC{
		fee:         fee,
		accountable: accountable,
		addedAt:     at,
		maxHold:     maxHold(incomingCltv, height),
		risk:        risk,
	}
	m.htlcIndex[incoming] = outScid

	return decision{
		inIsolation:        inIsolation,
		withInFlight:       withInFlight,
		outgoingReputation: outReputation,
		htlcRisk:           uint64(htlcRisk.Int64()),
		totalRisk:          totalRisk.Int64(),
		threshold:          threshold,
	}, nil
}

// resolveHTLC applies an HTLC resolution to reputation and revenue. The pending
// HTLC is looked up by its incoming circuit key alone: the switch's resolution
// paths do not reliably know the outgoing channel, so it is recovered from the
// index recorded at forward time. Caller must hold mu.
func (m *Manager) resolveHTLC(incoming models.CircuitKey, settled bool,
	at time.Time) error {

	outScid, ok := m.htlcIndex[incoming]
	if !ok {
		// Tolerate: we never saw the forward (e.g. enabled mid-flight).
		log.Debugf("Reputation resolve for unmatched htlc %v; ignoring",
			incoming)

		return nil
	}

	// Drop the pending entry up front. The HTLC has resolved, so whatever
	// happens below it must not be left in our in-flight view.
	delete(m.htlcIndex, incoming)

	outChan, ok := m.channels[outScid]
	if !ok {
		return fmt.Errorf("htlc %v indexed to unknown outgoing "+
			"channel %d", incoming, outScid)
	}

	pending, ok := outChan.pendingHTLCs[incoming]
	if !ok {
		return fmt.Errorf("htlc %v indexed to outgoing channel %d "+
			"but not pending on it", incoming, outScid)
	}

	delete(outChan.pendingHTLCs, incoming)

	// The resolution cannot predate the add; if it does the clock went
	// backwards and we cannot score this HTLC, so leave the averages alone.
	if at.Before(pending.addedAt) {
		return errBackwardsTime
	}

	// Credit the incoming channel's revenue first: it does not depend on
	// the reputation update below, so an error there must not starve it.
	if settled {
		inScid := incoming.ChanID.ToUint64()
		inChan := m.getOrCreateChannel(inScid, at)
		fee := satFromUint(pending.fee).Int64()
		if _, err := inChan.incomingRevenue.add(fee, at); err != nil {
			return err
		}
	}

	effFee := m.cfg.effectiveFee(
		pending.fee, at.Sub(pending.addedAt), pending.accountable,
		settled,
	)

	newRep, err := outChan.outgoingReputation.add(effFee, at)
	if err != nil {
		return err
	}

	// Log a single greppable line per resolution reporting the reputation
	// change, so it can be tracked without matching several phrasings. The
	// phrasing is stable: integration tests match on it.
	log.Infof("Reputation change: outgoing=%v eff_fee=%d "+
		"new_outgoing_reputation=%d settled=%v", outScid, effFee,
		newRep, settled)

	return nil
}

// staleCheckLoop runs the periodic stale-pending check until the manager is
// stopped.
func (m *Manager) staleCheckLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(staleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.quit:
			return

		case <-ticker.C:
			m.reportStalePendings()
		}
	}
}

// reportStalePendings warns about pending HTLCs that have outlived the worst
// case time they could be held for, and returns how many it found.
//
// These entries are deliberately NOT evicted. Every resolution path removes its
// own pending, so a stale entry means the switch never reported a resolution to
// us, i.e. a bug on our side. Quietly sweeping it away would hide that bug,
// so instead it is reported and left in place, where it keeps contributing to
// the channel's in-flight risk and stays visible.
func (m *Manager) reportStalePendings() int {
	at := m.clock.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	var stale int
	for scid, ch := range m.channels {
		for ref, p := range ch.pendingHTLCs {
			if at.Before(p.addedAt.Add(p.maxHold)) {
				continue
			}

			stale++

			log.Warnf("Reputation has pending htlc %v on outgoing "+
				"channel %d that outlived its maximum hold "+
				"time (added_at=%v, max_hold=%v): its "+
				"resolution was never observed", ref, scid,
				p.addedAt, p.maxHold)
		}
	}

	if stale > 0 {
		log.Warnf("Reputation is tracking %d pending HTLC(s) past "+
			"their maximum hold time; the in-flight view has "+
			"diverged from the switch", stale)
	}

	return stale
}
