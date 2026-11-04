package reputation

import (
	"context"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// load restores the persisted channel state into the manager. Averages are
// restored with their timestamps verbatim so the first read decays them over
// the node's downtime. A timestamp in the future means the clock went
// backwards across the restart; it is clamped to now, so the value is kept
// without decay rather than every later read failing as backwards time.
//
// Rows whose averages have decayed to zero are restored too: the revenue start
// time still drives the warm-up factor, and the row is removed when the
// channel closes.
func (m *Manager) load() error {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	states, err := m.store.FetchChannels(ctx)
	if err != nil {
		return err
	}

	now := m.clock.Now()

	var clamped int

	m.mu.Lock()
	for _, state := range states {
		if clampFutureTimestamps(&state, now) {
			clamped++
		}

		m.channels[state.ChanID] = restoreChannelReputation(
			m.cfg, state,
		)
	}
	m.mu.Unlock()

	if clamped > 0 {
		log.Warnf("Reputation clamped %d channel timestamps that were "+
			"in the future (clock went backwards?)", clamped)
	}

	// The phrasing is stable: integration tests match on it.
	log.Infof("Reputation loaded %d channels from store", len(states))

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
	m.storeMu.Lock()
	defer m.storeMu.Unlock()

	m.mu.Lock()
	if len(m.dirty) == 0 {
		m.mu.Unlock()

		return nil
	}

	states := make([]ChannelState, 0, len(m.dirty))
	for chanID := range m.dirty {
		c, ok := m.channels[chanID]
		if !ok {
			// Removed since it was marked; nothing to write.
			delete(m.dirty, chanID)

			continue
		}

		states = append(states, c.state(chanID))
	}
	m.dirty = make(map[lnwire.ChannelID]struct{})
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	if err := m.store.UpsertChannels(ctx, states); err != nil {
		m.mu.Lock()
		for _, state := range states {
			m.dirty[state.ChanID] = struct{}{}
		}
		m.mu.Unlock()

		return err
	}

	log.Debugf("Reputation flushed %d channels to store", len(states))

	return nil
}

// RemoveChannel drops all state held for a channel, in memory and in the store.
// It is called when the channel closes. Any HTLC still pending with the channel
// as its outgoing link is forgotten with it, since the channel has no
// reputation left to score it against, and so is any HTLC that arrived on the
// channel, since there is no channel left to credit its fee to. Removing a
// channel that is not tracked only clears any stale row in the store.
func (m *Manager) RemoveChannel(chanID lnwire.ChannelID) error {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()

	m.mu.Lock()
	c, tracked := m.channels[chanID]
	if tracked {
		for ref := range c.pendingHTLCs {
			delete(m.htlcIndex, ref)
		}
	}
	for outChanID, outChan := range m.channels {
		if outChanID == chanID {
			continue
		}
		for ref, p := range outChan.pendingHTLCs {
			if p.incomingChan != chanID {
				continue
			}

			delete(outChan.pendingHTLCs, ref)
			delete(m.htlcIndex, ref)
		}
	}
	delete(m.channels, chanID)
	delete(m.dirty, chanID)
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	if err := m.store.DeleteChannel(ctx, chanID); err != nil {
		return err
	}

	if tracked {
		// The phrasing is stable: integration tests match on it.
		log.Infof("Reputation removed channel %v", chanID)
	} else {
		log.Debugf("Reputation removal of untracked channel %v",
			chanID)
	}

	return nil
}
