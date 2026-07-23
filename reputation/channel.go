package reputation

// channelReputation holds all per-channel reputation state. A single channel
// plays both roles: as an outgoing link it accrues reputation and holds the
// pending HTLCs it is responsible for; as an incoming link it accrues the
// revenue that sets its reputation threshold.
type channelReputation struct {
	// outgoingReputation is the reputation this channel has accrued as an
	// outgoing link.
	outgoingReputation *decayingAverage

	// incomingRevenue is the revenue this channel has earned us as an
	// incoming link.
	incomingRevenue *aggregatedWindowAverage

	// pendingHTLCs tracks the in-flight HTLCs for which this channel is the
	// outgoing link, keyed by their incoming circuit.
	pendingHTLCs map[htlcRef]*pendingHTLC
}

// newChannelReputation builds empty reputation state for a channel as of the
// provided start time.
func newChannelReputation(cfg Config, start uint64) *channelReputation {
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

// sufficientReputation evaluates the per-HTLC isolation inequality
//
//	outgoing_rep - htlc_risk >= revenue_threshold
//
// on this (incoming) channel's revenue threshold. It answers the question "if
// this HTLC were forwarded in isolation, would its outgoing channel have
// sufficient reputation to be protected?". It returns the verdict and the
// threshold value used.
//
// Unlike the full subsystem, the in-flight risk of other pending HTLCs on the
// outgoing channel is deliberately NOT subtracted here: the decision is scored
// per-HTLC in isolation (see DESIGN.md).
func (c *channelReputation) sufficientReputation(inFlightHTLCRisk uint64,
	outgoingReputation int64, at uint64) (bool, int64, error) {

	threshold, err := c.incomingRevenue.valueAt(at)
	if err != nil {
		return false, 0, err
	}

	net := saturatingAddInt64(
		outgoingReputation, -saturatingI64FromU64(inFlightHTLCRisk),
	)

	return net >= threshold, threshold, nil
}
