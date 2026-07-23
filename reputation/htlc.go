package reputation

import (
	"math"
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
)

// htlcRef uniquely identifies an in-flight forwarded HTLC by its incoming
// circuit key. This matches the LDK reference, which keys pending HTLCs by
// (incoming channel, incoming htlc id); the value is stored against the
// outgoing channel.
type htlcRef = models.CircuitKey

// pendingHTLC captures, at forward time, everything the resolution path needs
// to score the HTLC. Capturing the accountable bit, fee, add timestamp and
// in-flight risk here is essential because the settle/fail hooks only carry the
// circuit identity, not the original add details.
type pendingHTLC struct {
	// fee is the forwarding fee (incoming - outgoing) in millisatoshis.
	fee uint64

	// accountable is the accountable signal observed for this HTLC.
	accountable bool

	// addedAt is the unix-seconds timestamp at which the HTLC was
	// forwarded.
	addedAt uint64

	// incomingCltv is the incoming HTLC's cltv expiry height, retained for
	// reference/debugging.
	incomingCltv uint32

	// inFlightRisk is the precomputed worst-case opportunity cost of this
	// HTLC while it is in flight.
	inFlightRisk uint64

	// maxHoldSeconds is the worst-case time the HTLC can be held, used by
	// the stale-pending garbage collector.
	maxHoldSeconds uint64
}

// opportunityCost returns the opportunity cost of an HTLC that took
// resolutionTime to resolve while earning feeMsat. HTLCs resolving within the
// configured resolution period have zero opportunity cost; beyond that the cost
// grows linearly with the overrun.
//
// NOTE: the result is rounded to the nearest integer to match the LDK
// reference (which uses f64 .round(), not truncation).
func (c Config) opportunityCost(resolutionTime time.Duration,
	feeMsat uint64) uint64 {

	period := c.ResolutionPeriod.Seconds()
	overrun := (resolutionTime.Seconds() - period) / period
	if overrun < 0 {
		overrun = 0
	}

	return uint64(math.Round(overrun * float64(feeMsat)))
}

// effectiveFee returns the contribution this HTLC makes to the outgoing
// channel's reputation, given its fee, resolution time, accountable signal and
// outcome. Matches the BOLT/LDK matrix exactly.
func (c Config) effectiveFee(feeMsat uint64, resolutionTime time.Duration,
	accountable, settled bool) int64 {

	fee := saturatingI64FromU64(feeMsat)

	if accountable {
		oc := saturatingI64FromU64(
			c.opportunityCost(resolutionTime, feeMsat),
		)
		if settled {
			return fee - oc
		}

		return -oc
	}

	// Unaccountable HTLCs can only ever help reputation: they earn
	// their fee if they settle quickly, and contribute nothing
	// otherwise.
	if settled && resolutionTime <= c.ResolutionPeriod {
		return fee
	}

	return 0
}

// maxHoldSeconds returns the worst-case number of seconds an HTLC may be held,
// derived from how far its incoming cltv expiry is from the height it was added
// at (assuming 10-minute blocks).
func maxHoldSeconds(incomingCltv, heightAdded uint32) uint64 {
	var delta uint32
	if incomingCltv > heightAdded {
		delta = incomingCltv - heightAdded
	}

	return uint64(delta) * blockInterval
}

// inFlightRisk returns the worst-case opportunity cost of an in-flight HTLC,
// assuming it is held until just before its incoming cltv expiry.
func (c Config) inFlightRisk(feeMsat uint64, incomingCltv,
	heightAdded uint32) uint64 {

	hold := maxHoldSeconds(incomingCltv, heightAdded)

	return c.opportunityCost(time.Duration(hold)*time.Second, feeMsat)
}
