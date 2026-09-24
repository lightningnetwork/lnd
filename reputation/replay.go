package reputation

import (
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// InFlightHTLC describes a forwarded HTLC that was still in flight when the
// node started, reconstructed from the switch's open circuits.
type InFlightHTLC struct {
	// Incoming identifies the HTLC by its incoming circuit key.
	Incoming models.CircuitKey

	// Outgoing is the channel the HTLC was forwarded on.
	Outgoing lnwire.ShortChannelID

	// Fee is the fee this node charges for the forward.
	Fee lnwire.MilliSatoshi

	// IncomingCltv is the cltv expiry of the incoming HTLC.
	IncomingCltv uint32

	// Accountable is the accountable signal the HTLC was forwarded with.
	Accountable bool
}

// ReplayInFlight rebuilds the pending state for HTLCs that were in flight
// across a restart, so that their in-flight risk is counted again and their
// eventual resolution is scored rather than ignored as unmatched. It returns
// the number of HTLCs replayed.
//
// The original forward time is not recoverable, so replayed HTLCs are stamped
// with the current time and height: the hold time charged when they resolve
// starts at the restart, and their remaining worst case hold is measured from
// the current height. HTLCs that cannot be tracked (expired or already known)
// are skipped with a warning.
func (m *Manager) ReplayInFlight(htlcs []InFlightHTLC, height uint32) int {
	at := m.clock.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	var replayed, accountable int
	for _, h := range htlcs {
		_, err := m.addHTLC(
			h.Incoming, h.Outgoing, h.Fee, h.IncomingCltv, height,
			h.Accountable, at,
		)
		if err != nil {
			log.Warnf("Reputation could not replay in-flight htlc "+
				"%v on outgoing channel %v: %v", h.Incoming,
				h.Outgoing, err)

			continue
		}

		replayed++
		if h.Accountable {
			accountable++
		}
	}

	// The phrasing is stable: integration tests match on it.
	log.Infof("Reputation replayed %d of %d in-flight HTLCs "+
		"(%d accountable)", replayed, len(htlcs), accountable)

	return replayed
}
