package reputation

import (
	"math"
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
)

// htlcRef uniquely identifies an in-flight forwarded HTLC by its incoming
// circuit key. The pending HTLC is stored against its outgoing channel.
type htlcRef = models.CircuitKey

// pendingHTLC captures, at forward time, the data the resolution path needs to
// score the HTLC that is not carried by the settle/fail hooks (which only
// identify the circuit).
type pendingHTLC struct {
	// fee is the fee in millisatoshis that our policy requires to forward
	// this HTLC. It is deliberately not the fee the sender chose to pay,
	// which may be inflated: scoring on the required fee means an attacker
	// has to make multiple payments rather than one over-paying payment to
	// move a channel's reputation.
	fee uint64

	// accountable is the accountable signal as this node would forward it
	// on the outgoing link, i.e. the bit received on the incoming link and
	// only if this node forwards the experimental accountability signal at
	// all.
	accountable bool

	// addedAt is the time at which the HTLC was forwarded.
	addedAt time.Time

	// maxHold is the worst-case duration for which the HTLC can be held,
	// derived from its incoming cltv expiry.
	maxHold time.Duration
}

// opportunityCost implements the BOLT #1280 opportunity_cost:
//
//	max(0, (resolution_time - resolution_period)/resolution_period) * fees
//
// The spec value is real-valued; since reputation is tracked in integer
// millisatoshis we round to the nearest integer.
func (c Config) opportunityCost(resolutionTime time.Duration,
	feeMsat uint64) uint64 {

	period := c.ResolutionPeriod.Seconds()
	overrun := (resolutionTime.Seconds() - period) / period
	if overrun < 0 {
		overrun = 0
	}

	// overrun and feeMsat are both non-negative, so the product cannot be
	// negative; clamp the high end where a very long hold on a large fee
	// would exceed uint64 (an out-of-range float->uint conversion is
	// undefined in Go).
	cost := math.Round(overrun * float64(feeMsat))
	if cost >= float64(math.MaxUint64) {
		return math.MaxUint64
	}

	return uint64(cost)
}

// effectiveFee returns the contribution this HTLC makes to the outgoing
// channel's reputation, given its fee, resolution time, accountable signal and
// outcome.
func (c Config) effectiveFee(feeMsat uint64, resolutionTime time.Duration,
	accountable, settled bool) int64 {

	fee := satFromUint(feeMsat)

	if accountable {
		oc := satFromUint(c.opportunityCost(resolutionTime, feeMsat))
		if settled {
			return fee.Sub(oc).Int64()
		}

		return satFromInt(0).Sub(oc).Int64()
	}

	// Unaccountable HTLCs can only ever help reputation: they earn their
	// fee if they settle quickly, and contribute nothing otherwise.
	if settled && resolutionTime <= c.ResolutionPeriod {
		return fee.Int64()
	}

	return 0
}

// maxHold returns the worst-case duration for which an HTLC may be held,
// derived from how far its incoming cltv expiry is from the height it was added
// at. A non-positive delta yields zero; callers validate that the incoming
// expiry is in the future before adding an HTLC.
func maxHold(incomingCltv, heightAdded uint32) time.Duration {
	var delta uint32
	if incomingCltv > heightAdded {
		delta = incomingCltv - heightAdded
	}

	return time.Duration(delta) * blockInterval
}

// inFlightRisk returns the worst-case opportunity cost of an in-flight HTLC,
// assuming it is held until just before its incoming cltv expiry.
func (c Config) inFlightRisk(feeMsat uint64, incomingCltv,
	heightAdded uint32) uint64 {

	return c.opportunityCost(maxHold(incomingCltv, heightAdded), feeMsat)
}
