package reputation

import "time"

// channelReputation holds all per-channel reputation state. A single channel
// plays both roles: as an outgoing link it accrues reputation and holds the
// pending HTLCs it is responsible for; as an incoming link it accrues the
// revenue that sets its reputation threshold.
type channelReputation struct {
	// outgoingReputation is the reputation this channel has accrued as an
	// outgoing link.
	outgoingReputation *decayingAverage

	// incomingRevenue is the revenue this channel has earned us as an
	// incoming link. It is aggregated over several windows so that a peer
	// cannot cheaply move its own threshold by manipulating recent
	// forwarding.
	incomingRevenue *aggregatedWindowAverage

	// pendingHTLCs tracks the in-flight HTLCs for which this channel is the
	// outgoing link, keyed by their incoming circuit.
	pendingHTLCs map[htlcRef]*pendingHTLC
}

// newChannelReputation builds empty reputation state for a channel as of the
// provided start time.
func newChannelReputation(cfg Config,
	start time.Time) *channelReputation {

	return &channelReputation{
		outgoingReputation: newDecayingAverage(
			start, cfg.reputationWindow(),
		),
		incomingRevenue: newAggregatedWindowAverage(
			cfg.RevenueWindow, cfg.RevenueWindowCount, start,
		),
		pendingHTLCs: make(map[htlcRef]*pendingHTLC),
	}
}

// restoreChannelReputation rebuilds channel state from its persisted form, with
// no pending HTLCs: those are replayed separately from the switch's in-flight
// circuits.
func restoreChannelReputation(cfg Config,
	state ChannelState) *channelReputation {

	return &channelReputation{
		outgoingReputation: restoreDecayingAverage(
			state.OutgoingReputation,
			state.OutgoingReputationUpdatedAt,
			cfg.reputationWindow(),
		),
		incomingRevenue: restoreAggregatedWindowAverage(
			cfg.RevenueWindow, cfg.RevenueWindowCount,
			state.IncomingRevenueStartedAt, state.IncomingRevenue,
			state.IncomingRevenueUpdatedAt,
		),
		pendingHTLCs: make(map[htlcRef]*pendingHTLC),
	}
}

// state returns the persistable form of this channel's averages.
func (c *channelReputation) state(scid uint64) ChannelState {
	rep, rev := c.outgoingReputation, c.incomingRevenue

	return ChannelState{
		SCID:                        scid,
		OutgoingReputation:          rep.value.Int64(),
		OutgoingReputationUpdatedAt: rep.lastUpdated,
		IncomingRevenue:             rev.inner.value.Int64(),
		IncomingRevenueUpdatedAt:    rev.inner.lastUpdated,
		IncomingRevenueStartedAt:    rev.start,
	}
}

// inFlightRisk returns the total worst-case opportunity cost of the HTLCs
// already in flight on this channel as an outgoing link. Per BOLT #1280 only
// accountable HTLCs contribute: an unaccountable HTLC was never told it would
// be held liable, so it cannot dock reputation.
func (c *channelReputation) inFlightRisk() saturatedI64 {
	var total saturatedI64

	for _, p := range c.pendingHTLCs {
		if !p.accountable {
			continue
		}

		total = total.Add(satFromUint(p.risk))
	}

	return total
}

// sufficientReputation evaluates the reputation inequality
//
//	outgoing_reputation - risk >= revenue_threshold
//
// against this (incoming) channel's revenue threshold, returning the verdict
// and the threshold value used. The caller chooses which risk to pass in.
func (c *channelReputation) sufficientReputation(risk saturatedI64,
	outgoingReputation int64, at time.Time) (bool, int64, error) {

	threshold, err := c.incomingRevenue.valueAt(at)
	if err != nil {
		return false, 0, err
	}

	net := satFromInt(outgoingReputation).Sub(risk)

	return net.Int64() >= threshold, threshold, nil
}
