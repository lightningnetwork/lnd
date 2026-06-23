package reputation

import (
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// loadFromStore reconstructs channel reputation state from the persistence
// backend (warm restart). Pending HTLCs / bucket occupancy are not restored
// here — they are rebuilt by ReplayInFlight.
func (m *Manager) loadFromStore() error {
	snaps, err := m.store.LoadChannels()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range snaps {
		snap := snaps[i]
		m.channels[snap.SCID] = m.restoreChannel(snap)
		m.limits[snap.SCID] = ChannelInfo{
			SCID: lnwire.NewShortChanIDFromInt(
				snap.SCID,
			),
			MaxAcceptedHTLCs: snap.MaxAcceptedHTLCs,
			MaxInFlightMsat: lnwire.MilliSatoshi(
				snap.MaxInFlightMsat,
			),
		}
	}

	log.Infof("Reputation loaded %d channels from store", len(snaps))

	return nil
}

// restoreChannel rebuilds a channelReputation from a persisted snapshot.
func (m *Manager) restoreChannel(snap ChannelSnapshot) *channelReputation {
	c := newChannelReputation(
		snap.SCID, snap.MaxInFlightMsat, snap.MaxAcceptedHTLCs, m.cfg,
		snap.reputation.lastUpdated,
	)

	c.outgoingReputation = &decayingAverage{
		value:       snap.reputation.value,
		lastUpdated: snap.reputation.lastUpdated,
		windowSecs:  snap.reputation.windowSecs,
		decayRate:   decayRateForWindow(snap.reputation.windowSecs),
	}

	innerSecs := snap.revenue.inner.windowSecs
	revWindow := time.Duration(snap.revenue.windowDuration) * time.Second
	c.incomingRevenue = &aggregatedWindowAverage{
		start:          snap.revenue.start,
		windowCount:    snap.revenue.windowCount,
		windowDuration: revWindow,
		inner: &decayingAverage{
			value:       snap.revenue.inner.value,
			lastUpdated: snap.revenue.inner.lastUpdated,
			windowSecs:  innerSecs,
			decayRate:   decayRateForWindow(innerSecs),
		},
	}

	// Regenerate the deterministic slot assignments from persisted salts.
	for outScid, salt := range snap.salts {
		saltCopy := salt
		if _, err := c.generalBucket.assignSlotsForChannel(
			outScid, &saltCopy,
		); err != nil {
			log.Warnf("Reputation: could not restore slots for "+
				"%d->%d: %v", snap.SCID, outScid, err)
		}
	}

	for outScid, ts := range snap.misuse {
		c.lastCongestionMisuse[outScid] = ts
	}

	return c
}

// snapshotChannelLocked builds a persistable snapshot of a channel. Caller must
// hold mu.
func (m *Manager) snapshotChannelLocked(scid uint64,
	c *channelReputation) ChannelSnapshot {

	salts := make(map[uint64][32]byte, len(c.generalBucket.channelsSlots))
	for outScid, cs := range c.generalBucket.channelsSlots {
		salts[outScid] = cs.salt
	}

	misuse := make(map[uint64]uint64, len(c.lastCongestionMisuse))
	for outScid, ts := range c.lastCongestionMisuse {
		misuse[outScid] = ts
	}

	revWindowSecs := uint64(c.incomingRevenue.windowDuration.Seconds())
	innerAvg := c.incomingRevenue.inner

	return ChannelSnapshot{
		SCID:             scid,
		MaxInFlightMsat:  c.maxInFlightMsat,
		MaxAcceptedHTLCs: c.maxAcceptedHTLCs,
		reputation: avgSnapshot{
			value:       c.outgoingReputation.value,
			lastUpdated: c.outgoingReputation.lastUpdated,
			windowSecs:  c.outgoingReputation.windowSecs,
		},
		revenue: revenueSnapshot{
			start:          c.incomingRevenue.start,
			windowCount:    c.incomingRevenue.windowCount,
			windowDuration: revWindowSecs,
			inner: avgSnapshot{
				value:       innerAvg.value,
				lastUpdated: innerAvg.lastUpdated,
				windowSecs:  innerAvg.windowSecs,
			},
		},
		salts:  salts,
		misuse: misuse,
	}
}

// flush persists all dirty channels and clears the dirty set.
func (m *Manager) flush() error {
	m.mu.Lock()
	if len(m.dirty) == 0 {
		m.mu.Unlock()

		return nil
	}

	snaps := make([]ChannelSnapshot, 0, len(m.dirty))
	scids := make([]uint64, 0, len(m.dirty))
	for scid := range m.dirty {
		if ch, ok := m.channels[scid]; ok {
			snaps = append(snaps, m.snapshotChannelLocked(scid, ch))
			scids = append(scids, scid)
		}
	}

	// Optimistically clear the dirty set; if the persist fails we re-mark
	// the channels we attempted so they are retried on the next flush
	// (review finding B3; a failed flush previously dropped them).
	m.dirty = make(map[uint64]struct{})
	m.mu.Unlock()

	if err := m.store.PersistChannels(snaps); err != nil {
		m.mu.Lock()
		for _, scid := range scids {
			m.dirty[scid] = struct{}{}
		}
		m.mu.Unlock()

		return err
	}

	return nil
}

// seedChannels reads the active channels from the channel source, recording
// their limits and creating empty per-channel state for any not already loaded
// (cold start).
func (m *Manager) seedChannels() error {
	infos, err := m.channelSrc.ActiveChannels()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, info := range infos {
		scid := info.SCID.ToUint64()
		m.limits[scid] = info
		if _, ok := m.channels[scid]; !ok {
			m.channels[scid] = newChannelReputation(
				scid, uint64(info.MaxInFlightMsat),
				info.MaxAcceptedHTLCs, m.cfg, m.now(),
			)
		}
	}

	log.Infof("Reputation seeded %d active channels", len(infos))

	return nil
}

// subscribeChannels starts a goroutine consuming channel open/close events.
func (m *Manager) subscribeChannels() error {
	events, cancel, err := m.channelSrc.SubscribeChannelEvents()
	if err != nil {
		return err
	}
	m.cancelSubFn = cancel

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-m.quit:
				return

			case ev, ok := <-events:
				if !ok {
					return
				}
				m.handleChannelEvent(ev)
			}
		}
	}()

	return nil
}

// handleChannelEvent applies a channel open/close to the reputation state.
func (m *Manager) handleChannelEvent(ev ChannelEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()

	scid := ev.Info.SCID.ToUint64()
	if ev.Open {
		m.limits[scid] = ev.Info
		if _, ok := m.channels[scid]; !ok {
			m.channels[scid] = newChannelReputation(
				scid, uint64(ev.Info.MaxInFlightMsat),
				ev.Info.MaxAcceptedHTLCs, m.cfg, m.now(),
			)
		}
		m.markDirtyLocked(scid)

		return
	}

	m.removeChannelLocked(scid)
}

// removeChannelLocked deletes a channel and frees the general-bucket slots it
// held on every other channel. Caller must hold mu.
func (m *Manager) removeChannelLocked(scid uint64) {
	delete(m.channels, scid)
	delete(m.limits, scid)
	delete(m.dirty, scid)

	for _, ch := range m.channels {
		ch.generalBucket.removeChannelSlots(scid)
	}
}

// InFlightHTLC describes an HTLC that is currently in flight at startup, used
// to rebuild pending state and bucket occupancy (P9). It is fed from the
// circuit map ∪ live channel HTLC set — never from history.
type InFlightHTLC struct {
	Incoming     models.CircuitKey
	Outgoing     models.CircuitKey
	IncomingAmt  lnwire.MilliSatoshi
	OutgoingAmt  lnwire.MilliSatoshi
	IncomingCltv uint32
	Accountable  bool
}

// ReplayInFlight rebuilds pending-HTLC and bucket-occupancy state from the
// currently in-flight HTLCs. Like LDK, replayed HTLCs are stamped with the
// current time and height (not their original add time), so derived resolution
// times differ from pre-restart — an accepted limitation.
//
// PARKED / not currently wired. In log-only mode we deliberately do NOT
// reconstruct in-flight HTLCs on restart: the pending set starts empty and
// resolutions of HTLCs that spanned the restart are tolerated as no-ops (see
// Manager.resolve). This is a bounded, self-healing calculational flaw that is
// acceptable while nothing routes on the result. This method (and its test) are
// retained on purpose: once enforcement is enabled, discarding in-flight risk
// across a restart becomes an attack surface (restart to wipe accountability),
// at which point the server must enumerate the live circuit map ∪ channel HTLC
// sets and call this. See reputation/DESIGN.md §6/§9.
func (m *Manager) ReplayInFlight(htlcs []InFlightHTLC) {
	height := m.cachedHeight.Load()

	m.mu.Lock()
	defer m.mu.Unlock()

	at := m.now()

	var replayed int
	for _, h := range htlcs {
		_, err := m.addHTLCLocked(
			h.Incoming, h.Outgoing, h.IncomingAmt, h.OutgoingAmt,
			h.IncomingCltv, h.Accountable, at, height,
		)
		if err != nil {
			log.Warnf("Reputation replay htlc %v error: %v",
				h.Incoming, err)

			continue
		}
		replayed++
	}

	log.Infof("Reputation replayed %d in-flight HTLCs", replayed)
}
