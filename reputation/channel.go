package reputation

import "time"

// twoWeeksSeconds is the congestion-misuse penalty window (2016 blocks at 10
// minutes per block), matching the LDK reference.
const twoWeeksSeconds = 2016 * 10 * 60

// channelReputation holds all per-channel reputation state. A single channel
// plays both roles: as an outgoing link it accrues reputation and holds the
// pending HTLCs it is responsible for; as an incoming link it accrues revenue
// and owns the resource buckets that limit how much can pile up on it.
type channelReputation struct {
	maxInFlightMsat  uint64
	maxAcceptedHTLCs uint16

	// outgoingReputation is the reputation this channel has accrued as an
	// outgoing link.
	outgoingReputation *decayingAverage

	// incomingRevenue is the revenue this channel has earned us as an
	// incoming link.
	incomingRevenue *aggregatedWindowAverage

	// pendingHTLCs tracks the in-flight HTLCs for which this channel is the
	// outgoing link, keyed by their incoming circuit.
	pendingHTLCs map[htlcRef]*pendingHTLC

	generalBucket    *generalBucket
	congestionBucket *bucketResources
	protectedBucket  *bucketResources

	// lastCongestionMisuse maps an outgoing scid to the unix-seconds time
	// it last misused this channel's congestion bucket.
	lastCongestionMisuse map[uint64]uint64
}

// newChannelReputation builds empty reputation state for a channel with the
// given capacity limits, as of the provided start time.
func newChannelReputation(scid uint64, maxInFlightMsat uint64,
	maxAcceptedHTLCs uint16, cfg Config, start uint64) *channelReputation {

	// Clamp to the protocol maximum so an over-large (mis)reported limit
	// can't size pathological buckets (review finding B4; mirrors the LDK
	// upper-bound guard). Real channels are already capped at this value.
	if maxAcceptedHTLCs > protocolMaxAcceptedHTLCs {
		log.Warnf("Reputation: channel %d reports max_accepted_htlcs="+
			"%d > %d; clamping", scid, maxAcceptedHTLCs,
			protocolMaxAcceptedHTLCs)
		maxAcceptedHTLCs = protocolMaxAcceptedHTLCs
	}

	alloc := computeBucketAllocations(
		maxAcceptedHTLCs, maxInFlightMsat, cfg.GeneralPct,
		cfg.CongestionPct,
	)

	return &channelReputation{
		maxInFlightMsat:  maxInFlightMsat,
		maxAcceptedHTLCs: maxAcceptedHTLCs,
		outgoingReputation: newDecayingAverage(
			start, cfg.reputationWindow(),
		),
		incomingRevenue: newAggregatedWindowAverage(
			cfg.RevenueWindow, cfg.ReputationMultiplier, start,
		),
		pendingHTLCs: make(map[htlcRef]*pendingHTLC),
		generalBucket: newGeneralBucket(
			scid, alloc.generalSlots, alloc.generalLiquidity,
		),
		congestionBucket: newBucketResources(
			alloc.congestionSlots, alloc.congestionLiquidity,
		),
		protectedBucket: newBucketResources(
			alloc.protectedSlots, alloc.protectedLiquidity,
		),
		lastCongestionMisuse: make(map[uint64]uint64),
	}
}

// outgoingInFlightRisk sums the in-flight risk of this channel's pending
// accountable HTLCs (only accountable HTLCs count, per the spec).
func (c *channelReputation) outgoingInFlightRisk() uint64 {
	var total uint64
	for _, htlc := range c.pendingHTLCs {
		if htlc.outgoingAccountable {
			total += htlc.inFlightRisk
		}
	}

	return total
}

// sufficientReputation evaluates the core inequality
//
//	outgoing_rep - outgoing_inflight_risk - htlc_risk >= revenue_threshold
//
// on this (incoming) channel's revenue threshold. It returns the verdict and
// the threshold value used.
func (c *channelReputation) sufficientReputation(inFlightHTLCRisk uint64,
	outgoingReputation int64, outgoingInFlightRisk uint64,
	at uint64) (bool, int64, error) {

	threshold, err := c.incomingRevenue.valueAt(at)
	if err != nil {
		return false, 0, err
	}

	net := saturatingAddInt64(
		outgoingReputation,
		-saturatingI64FromU64(outgoingInFlightRisk),
	)
	net = saturatingAddInt64(net, -saturatingI64FromU64(inFlightHTLCRisk))

	return net >= threshold, threshold, nil
}

// pendingHTLCsInCongestion reports whether the given incoming channel already
// has an in-flight HTLC occupying this channel's congestion bucket.
func (c *channelReputation) pendingHTLCsInCongestion(
	incomingChan uint64) bool {

	for ref, htlc := range c.pendingHTLCs {
		if htlc.bucket == bucketCongestion &&
			ref.ChanID.ToUint64() == incomingChan {

			return true
		}
	}

	return false
}

// misusedCongestion records that an outgoing scid held a congestion-bucket
// HTLC for too long, blocking it from the congestion bucket for ~2 weeks.
func (c *channelReputation) misusedCongestion(outgoingScid, at uint64) {
	c.lastCongestionMisuse[outgoingScid] = at
}

// hasMisusedCongestion reports whether the outgoing scid has misused the
// congestion bucket within the last two weeks. Expired entries are pruned.
func (c *channelReputation) hasMisusedCongestion(outgoingScid,
	at uint64) (bool, error) {

	last, ok := c.lastCongestionMisuse[outgoingScid]
	if !ok {
		return false, nil
	}

	if at < last {
		return false, errBackwardsTime
	}

	if at-last < twoWeeksSeconds {
		return true, nil
	}

	delete(c.lastCongestionMisuse, outgoingScid)

	return false, nil
}

// canAddHTLCCongestion reports whether an HTLC of the given amount is eligible
// for this channel's congestion bucket from the outgoing scid.
func (c *channelReputation) canAddHTLCCongestion(outgoingScid, amtMsat,
	at uint64) (bool, error) {

	available := c.congestionBucket.resourcesAvailable(amtMsat)

	misused, err := c.hasMisusedCongestion(outgoingScid, at)
	if err != nil {
		return false, err
	}

	var belowLimit bool
	if c.congestionBucket.slotsAllocated > 0 {
		belowLimit = amtMsat <= c.congestionBucket.liquidityAllocated/
			uint64(c.congestionBucket.slotsAllocated)
	}

	return available && !misused && belowLimit, nil
}

// congestionEligible combines the per-incoming-channel one-in-flight rule with
// the congestion bucket availability check.
func (c *channelReputation) congestionEligible(pendingInCongestion bool,
	amtMsat, outgoingScid, at uint64) (bool, error) {

	if pendingInCongestion {
		return false, nil
	}

	return c.canAddHTLCCongestion(outgoingScid, amtMsat, at)
}

// resolutionPeriodExceeded reports whether a resolution time was slow enough to
// count as congestion misuse.
func resolutionPeriodExceeded(resolutionTime, period time.Duration) bool {
	return resolutionTime > period
}
