package reputation

import (
	"context"
	"time"
)

// load restores the persisted channel state into the manager. Averages are
// restored with their timestamps verbatim so the first read decays them over
// the node's downtime. Two kinds of rows are not restored as-is:
//
//   - A timestamp in the future means the clock went backwards across the
//     restart. It is clamped to now, so the value is kept without decay rather
//     than every later read failing as backwards time.
//   - A channel whose reputation and revenue have both decayed to zero carries
//     no information any more. Its row is dropped from the store and the
//     channel is recreated lazily if it forwards again.
func (m *Manager) load() error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	states, err := m.store.FetchChannels(ctx)
	if err != nil {
		return err
	}

	now := m.clock.Now()

	var loaded, clamped int
	var decayed []uint64

	m.mu.Lock()
	for _, state := range states {
		if clampFutureTimestamps(&state, now) {
			clamped++
		}

		c := restoreChannelReputation(m.cfg, state)

		rep, err := c.outgoingReputation.valueAt(now)
		if err != nil {
			m.mu.Unlock()

			return err
		}

		rev, err := c.incomingRevenue.valueAt(now)
		if err != nil {
			m.mu.Unlock()

			return err
		}

		if rep == 0 && rev == 0 {
			decayed = append(decayed, state.SCID)

			continue
		}

		m.channels[state.SCID] = c
		loaded++
	}
	m.mu.Unlock()

	if clamped > 0 {
		log.Warnf("Reputation clamped %d channel timestamps that were "+
			"in the future (clock went backwards?)", clamped)
	}

	for _, scid := range decayed {
		if err := m.store.DeleteChannel(ctx, scid); err != nil {
			return err
		}
	}

	log.Infof("Reputation loaded %d channels from store, dropped %d "+
		"that fully decayed", loaded, len(decayed))

	return nil
}

// clampFutureTimestamps clamps any timestamp of the state that lies after now
// to now, reporting whether it had to.
func clampFutureTimestamps(state *ChannelState, now time.Time) bool {
	var clamped bool

	clamp := func(ts *time.Time) {
		if ts.After(now) {
			*ts = now
			clamped = true
		}
	}

	clamp(&state.OutgoingReputationUpdatedAt)
	clamp(&state.IncomingRevenueUpdatedAt)
	clamp(&state.IncomingRevenueStartedAt)

	return clamped
}

// flush writes every channel whose state changed since the last flush to the
// store. If the write fails the channels stay marked so the next flush retries
// them.
func (m *Manager) flush() error {
	m.mu.Lock()
	if len(m.dirty) == 0 {
		m.mu.Unlock()

		return nil
	}

	states := make([]ChannelState, 0, len(m.dirty))
	for scid := range m.dirty {
		c, ok := m.channels[scid]
		if !ok {
			// Removed since it was marked; nothing to write.
			delete(m.dirty, scid)

			continue
		}

		states = append(states, c.state(scid))
	}
	m.dirty = make(map[uint64]struct{})
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	if err := m.store.UpsertChannels(ctx, states); err != nil {
		m.mu.Lock()
		for _, state := range states {
			m.dirty[state.SCID] = struct{}{}
		}
		m.mu.Unlock()

		return err
	}

	log.Debugf("Reputation flushed %d channels to store", len(states))

	return nil
}

// RemoveChannel drops all state held for a channel, in memory and in the store.
// It is called when the channel closes. Any HTLC still pending on the channel
// as its outgoing link is forgotten with it: the channel has no reputation left
// to score them against.
func (m *Manager) RemoveChannel(scid uint64) error {
	m.mu.Lock()
	if c, ok := m.channels[scid]; ok {
		for ref := range c.pendingHTLCs {
			delete(m.htlcIndex, ref)
		}
	}
	delete(m.channels, scid)
	delete(m.dirty, scid)
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	if err := m.store.DeleteChannel(ctx, scid); err != nil {
		return err
	}

	log.Infof("Reputation removed channel %d", scid)

	return nil
}
