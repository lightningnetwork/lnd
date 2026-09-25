package routing

import (
	"math"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// The constants below parameterize the liquidity interval belief model. They
// were selected by an evolutionary search against a payment simulator rather
// than derived from first principles, so each one is named and documented here
// for what it does rather than for why that particular number is right. Where a
// constant expresses a fraction of a channel's capacity it is written as a
// fraction rather than an absolute amount, which is what lets the model carry
// across networks whose channels differ in size by orders of magnitude.
const (
	// intervalPriorFloor is the probability floor of the bimodal prior. It
	// keeps an amount in the middle of a channel's range from being ruled
	// out entirely when we have no evidence at all.
	intervalPriorFloor = 0.005

	// intervalPriorMass is the probability mass assigned to each of the two
	// modes of the prior, the depleted one near zero and the saturated one
	// near capacity.
	intervalPriorMass = 0.495

	// intervalPriorScale is the width of both modes of the prior, as a
	// fraction of the channel's capacity.
	intervalPriorScale = 0.018

	// intervalPriorCliff is the point, as a fraction of capacity, at which
	// the saturated mode falls off. Above it a channel is assumed unable to
	// forward even if it has never failed.
	intervalPriorCliff = 0.965

	// intervalPriorMax and intervalPriorMin clamp the prior away from
	// certainty in either direction.
	intervalPriorMax = 0.999
	intervalPriorMin = 0.0005

	// intervalProvenProbability is the probability assigned to an amount at
	// or below an amount this channel has already forwarded. It is not one,
	// because liquidity can move underneath us between attempts.
	intervalProvenProbability = 0.9985

	// intervalLocalProbability is the probability assigned to one of our
	// own channels that the bandwidth hints say can carry the amount. The
	// small haircut below one is what makes the search prefer shorter
	// routes without a separate hop count term.
	intervalLocalProbability = 0.9995

	// intervalRichBase, intervalRichConfidence and intervalRichMargin
	// combine into the probability of a channel we have classified as
	// saturated and whose estimate covers the amount.
	intervalRichBase       = 0.975
	intervalRichConfidence = 0.022
	intervalRichMargin     = 0.002

	// intervalEstimateBase, intervalEstimateConfidence and
	// intervalEstimateMargin combine into the probability of an
	// unclassified channel whose estimate covers the amount.
	intervalEstimateBase       = 0.90
	intervalEstimateConfidence = 0.075
	intervalEstimateMargin     = 0.02

	// intervalPositionFloor, intervalPositionMass and
	// intervalPositionExponent shape the interpolation across a known
	// interval. At the lower bound the probability is near certainty, at
	// the upper bound it is near zero, and the exponent controls how fast
	// it falls off in between.
	intervalPositionFloor    = 0.01
	intervalPositionMass     = 0.94
	intervalPositionExponent = 2.8

	// intervalPriorBlend is the weight the prior keeps when an interval is
	// known on both sides. Holding a little prior back stops a single pair
	// of observations from speaking with more authority than it has.
	intervalPriorBlend = 0.10

	// intervalOverBase and intervalOverScale price an amount that runs past
	// what we estimate the channel holds, without a hard upper bound to
	// rule it out.
	intervalOverBase  = 0.12
	intervalOverScale = 0.035

	// intervalLowModeFloor and intervalLowModeMass shape the exponential
	// tail used for a channel classified as depleted.
	intervalLowModeFloor = 0.006
	intervalLowModeMass  = 0.78

	// intervalLowModeProven is the probability of an amount at or below the
	// proven lower bound of a depleted channel.
	intervalLowModeProven = 0.998

	// intervalLowModeEstimate is the probability floor applied to a
	// depleted channel whose estimate still covers the amount.
	intervalLowModeEstimate = 0.82

	// intervalUnknownCapacity is the probability used for a channel whose
	// capacity we do not know, which happens for light clients and for hop
	// hints. Without a capacity none of the fractions above mean anything,
	// so we fall back to a flat guess.
	intervalUnknownCapacity = 0.6

	// intervalMaxProbability and intervalMinProbability clamp the output of
	// the model. The minimum is deliberately not zero: only a proven upper
	// bound is allowed to say impossible.
	intervalMaxProbability = 0.999
	intervalMinProbability = 0.000001

	// intervalRestoredFloor and intervalRestoredCeiling clamp the output of
	// the model for a belief that was restored from disk rather than
	// gathered in this process. Neither certainty is available to a belief
	// that has been asleep: the floor keeps a stale upper bound from ruling
	// an amount out for good, and the ceiling keeps a stale lower bound from
	// being trusted as if we had just watched it hold.
	intervalRestoredFloor   = 0.012
	intervalRestoredCeiling = 0.95

	// intervalRestoredConfidence is the factor applied to the confidence of
	// a belief when it is restored, since whatever evidence stood behind it
	// is now at least one restart old.
	intervalRestoredConfidence = 0.5

	// intervalSuspectPromoteWeight is the corroboration a quarantined
	// observation needs before it becomes a bound. A failure that names two
	// suspects contributes about 0.7 to each of them, so roughly three such
	// failures agreeing on the same channel promote; one naming five
	// suspects contributes 0.45, so five are needed. The more ambiguous a
	// failure is, the more of them it takes to convict.
	intervalSuspectPromoteWeight = 2.05

	// intervalSuspectPenalty is how hard a quarantined observation prices,
	// per unit of the weight standing behind it. A single ambiguous failure
	// discounts an amount noticeably without ruling it out, which is the
	// whole point of holding it apart from the bounds.
	intervalSuspectPenalty = 0.55

	// intervalSuspectPenaltyCap bounds the discount, so that a channel
	// which keeps turning up in ambiguous failures without ever being
	// convicted cannot be priced out of the graph entirely.
	intervalSuspectPenaltyCap = 3.0

	// intervalSuspectEstimateNumerator and
	// intervalSuspectEstimateDenominator give the estimate a promoted
	// suspicion leaves behind, as a fraction of the amount it named.
	intervalSuspectEstimateNumerator   = 68
	intervalSuspectEstimateDenominator = 100
)

// Liquidity mode classifications. The model does not just carry a probability
// curve, it commits to a hypothesis about which side of the bimodal
// distribution a channel sits on and then reasons inside that hypothesis.
const (
	// intervalModeDepleted means the channel appears to be nearly empty in
	// this direction.
	intervalModeDepleted int8 = -1

	// intervalModeUnknown means we have not classified the channel.
	intervalModeUnknown int8 = 0

	// intervalModeRich means the channel appears to hold nearly its whole
	// capacity in this direction.
	intervalModeRich int8 = 1
)

// Thresholds that gate the classification and the estimate updates. These are
// written as integer divisors of capacity so that they can be applied without
// leaving millisatoshi arithmetic.
const (
	// intervalStrongObservationDivisor sets how large an observation has to
	// be, relative to capacity, before it is allowed to move the mode
	// latch. A dust sized probe proves almost nothing about which mode a
	// channel is in, so it moves the bounds but not the classification.
	intervalStrongObservationDivisor = 200

	// intervalDepletedDivisor is the fraction of capacity below which an
	// estimate classifies the channel as depleted.
	intervalDepletedDivisor = 50

	// intervalRichNumerator and intervalRichDenominator give the fraction
	// of capacity above which an estimate classifies the channel as rich.
	intervalRichNumerator   = 49
	intervalRichDenominator = 50

	// intervalProbeEstimateNumerator and intervalProbeEstimateDenominator
	// give the estimate we jump to when a strong observation proves a
	// channel forwarded an amount: we assume it is near the top of its
	// range rather than exactly at the amount we saw.
	intervalProbeEstimateNumerator   = 97
	intervalProbeEstimateDenominator = 100

	// intervalFailureEstimateDivisor is the divisor applied to a failing
	// amount to get the collapsed estimate after a failure.
	intervalFailureEstimateDivisor = 32

	// intervalFailureFloorDivisor caps the collapsed estimate of a strongly
	// observed failure at this fraction of capacity.
	intervalFailureFloorDivisor = 1000
)

// Confidence levels latched by each kind of observation. Confidence here is a
// saturating latch on how much evidence we have seen rather than a posterior
// width, and it only ever feeds the probability model as a small additive term.
const (
	intervalProbeConfidence          = 0.94
	intervalProbeReverseConfidence   = 0.86
	intervalFailureConfidence        = 0.99
	intervalFailureReverseConfidence = 0.97
	intervalSettleConfidence         = 0.96
)

// intervalRetryLadder prices the question "this channel just refused X, what do
// I believe about a smaller amount?". Each rung is a ratio of the new amount to
// the failed amount, paired with the factor the probability is multiplied by.
// Retrying at three quarters of a failed amount is nearly hopeless; retrying at
// a hundredth of it is nearly fine. This ladder is what replaces blacklisting a
// channel and waiting for a penalty to decay.
var intervalRetryLadder = []struct {
	ratio  float64
	factor float64
}{
	{ratio: 0.75, factor: 0.004},
	{ratio: 0.40, factor: 0.018},
	{ratio: 0.15, factor: 0.075},
	{ratio: 0.04, factor: 0.30},
	{ratio: 0.01, factor: 0.62},
}

// intervalRetryFloor is the factor used below the last rung of the ladder.
const intervalRetryFloor = 0.88

// IntervalKey identifies one direction of one channel. Unlike mission control,
// which keys its history on a node pair, the interval model keys on the
// directed channel, because the quantity it tracks is the balance sitting on
// one side of one funding output.
//
// NOTE: under non-strict forwarding a node may forward over a sibling channel
// to the same peer, in which case an observation can land on the wrong key. The
// pair keyed model does not have that problem, and this is the price the
// directed key pays for being able to hold an amount interval that means
// something physical.
type IntervalKey struct {
	// ChanID is the short channel id of the channel.
	ChanID uint64

	// From is the node the liquidity is flowing away from.
	From route.Vertex

	// To is the node the liquidity is flowing towards.
	To route.Vertex
}

// intervalPairScopeChanID marks a belief held about a node pair as a whole
// rather than about one channel between them. Zero is safe to use for this
// because it is not a short channel id any real channel can have: it would name
// the first output of the first transaction of the genesis block.
const intervalPairScopeChanID = 0

// Reverse returns the key for the opposite direction of the same channel.
func (k IntervalKey) Reverse() IntervalKey {
	return IntervalKey{
		ChanID: k.ChanID,
		From:   k.To,
		To:     k.From,
	}
}

// PairScope returns the key describing the node pair this channel connects,
// which is the granularity an observation has to fall back to when it cannot
// name a channel.
func (k IntervalKey) PairScope() IntervalKey {
	return IntervalKey{
		ChanID: intervalPairScopeChanID,
		From:   k.From,
		To:     k.To,
	}
}

// IsPairScoped reports whether this key describes a node pair rather than a
// channel.
func (k IntervalKey) IsPairScoped() bool {
	return k.ChanID == intervalPairScopeChanID
}

// intervalScopeKey returns the key an observation about a hop should be written
// under, given how many channels connect the pair the hop crosses.
//
// A channel is the granularity this model wants, because the quantity it tracks
// is the balance sitting on one side of one funding output. It is not always
// the granularity the evidence supports. Under non-strict forwarding a node
// asked to forward over one channel may use any channel it has to the same
// peer, and an onion failure names neither. So when a pair has more than one
// channel, an observation is written about the pair instead.
//
// The alternative, writing the same observation onto every channel of the pair,
// is worse rather than merely coarser. This model's upper bound is hard: an
// amount at or above it is impossible, and no amount of reduced confidence
// softens that, because confidence enters the model as a small additive term
// and never as a multiplier on the bound. Spreading a failure across siblings
// would therefore assert something false and unrecoverable about every channel
// that was not the one to refuse. Pair scope asserts only what was observed,
// which is that this peer could not move this amount to that node. It is also
// the granularity mission control has always used, so it is a loss of
// resolution rather than a loss of correctness.
func intervalScopeKey(key IntervalKey, siblings int) IntervalKey {
	if siblings > 1 {
		return key.PairScope()
	}

	return key
}

// LiquidityInterval is what we believe about the liquidity available in one
// direction of one channel. It is an interval rather than a point estimate:
// LowerOK is the largest amount we have proven can pass, UpperFail the smallest
// amount we have proven cannot, and Estimate our best guess in between.
//
// There is deliberately no clock in this structure. A bound moves when new
// evidence arrives or when a settlement displaces liquidity, and never merely
// because time has passed.
type LiquidityInterval struct {
	// LowerOK is the largest amount this direction has been proven to
	// carry. Anything at or below it is treated as near certain.
	LowerOK lnwire.MilliSatoshi

	// UpperFail is the smallest amount this direction has been proven not
	// to carry. Zero means no failure has been observed. Anything at or
	// above it is treated as impossible.
	UpperFail lnwire.MilliSatoshi

	// Estimate is our best guess at the balance currently available.
	Estimate lnwire.MilliSatoshi

	// Confidence is a saturating measure of how much evidence stands behind
	// the estimate, in the range [0, 1].
	Confidence float64

	// Failures and Successes count the observations that have landed here.
	Failures  uint32
	Successes uint32

	// Mode is the classification latch, one of the intervalMode constants.
	Mode int8

	// Known is set once any observation has been recorded.
	Known bool

	// Restored marks a belief that came back from disk rather than from an
	// attempt this process made. It is cleared by the first fresh
	// observation, because from that point the bounds describe evidence we
	// gathered ourselves.
	Restored bool

	// ProvenOK is the largest amount this direction has been watched
	// actually move, which is to say the largest amount a payment settled
	// over it. It is the only evidence class strong enough to clear a
	// suspicion, and nothing but a settlement ever writes it.
	//
	// LowerOK is not that, which is the distinction this field exists to
	// draw. LowerOK also rises when a failure reported by some hop implies
	// that the hops before it forwarded, and under misattribution that
	// implication is exactly what breaks: blame shifted downstream puts the
	// guilty channel before the reported index, so it collects a lower bound
	// claiming it carried the amount it had in fact just refused. Reading
	// that as proof of innocence lets the culprit walk out of every
	// suspicion it should have been held for.
	//
	// A settlement proves the forward direction and only the forward
	// direction. It does move balance to the other side, which is why the
	// reverse interval slides up, but sliding an interval is an inference
	// about a balance and this field is a record of something watched. So
	// the reverse direction is left alone.
	//
	// NOTE: this is not persisted. It says a settlement was watched by this
	// process, and a settlement from before a restart is evidence about a
	// network that has had the restart to move on. A failure observed now
	// outranks it, so a restored belief starts with nothing here and earns
	// it back with the first settlement.
	ProvenOK lnwire.MilliSatoshi

	// SuspectAmt is the smallest amount that a failure we could not
	// attribute has named for this channel, and SuspectWeight is how much
	// corroboration those failures carry between them. Zero means nothing
	// is under suspicion.
	//
	// This pair is the quarantine. An observation whose attribution we do
	// not trust is held here rather than written into the bounds above,
	// because a bound is a claim of certainty and an ambiguous failure is
	// not one. Quarantined evidence prices as a discount and never as an
	// impossibility. It is promoted into a real upper bound once enough
	// independent failures agree on it, and it is cleared the moment the
	// channel proves it can carry the amount after all.
	SuspectAmt    lnwire.MilliSatoshi
	SuspectWeight float64
}

// markRestored turns a belief loaded from disk into soft evidence. The bounds
// are kept, since they are still the best guess anybody has about a channel we
// have not touched yet, but the confidence behind them is cut and the
// probability model is told to stop short of certainty in either direction.
func (l *LiquidityInterval) markRestored() {
	l.Restored = true
	l.Confidence *= intervalRestoredConfidence

	// Proof of a settlement does not survive a restart. It is never written
	// down, and a belief being seeded in must not carry one regardless of
	// what the caller handed us, because a settlement from before the
	// restart says nothing about a failure observed after it.
	l.ProvenOK = 0
}

// normalize restores the invariant 0 <= LowerOK <= Estimate < UpperFail <=
// capacity and re-runs the mode classification. It is applied on every read and
// every write, so that no caller ever sees an interval that contradicts itself.
// When an upper bound contradicts a lower bound it is the upper bound that is
// dropped, because the lower bound records something we watched succeed.
func (l *LiquidityInterval) normalize(capacity lnwire.MilliSatoshi) {
	if l.LowerOK > capacity {
		l.LowerOK = capacity
	}

	if l.UpperFail > capacity {
		l.UpperFail = 0
	}
	if l.UpperFail != 0 && l.LowerOK >= l.UpperFail {
		l.UpperFail = 0
	}

	if l.Estimate < l.LowerOK {
		l.Estimate = l.LowerOK
	}
	if l.Estimate > capacity {
		l.Estimate = capacity
	}
	if l.UpperFail != 0 && l.Estimate >= l.UpperFail {
		l.Estimate = l.UpperFail - 1
		if l.Estimate < l.LowerOK {
			l.Estimate = l.LowerOK
		}
	}

	if l.ProvenOK > capacity {
		l.ProvenOK = capacity
	}

	if l.SuspectAmt > capacity {
		l.SuspectAmt = capacity
	}

	// A channel we have watched settle the suspected amount is a channel the
	// suspicion was wrong about. This is the contradiction rule, and putting
	// it here means it fires no matter which settlement moved the bound.
	//
	// It reads ProvenOK rather than LowerOK on purpose. See ProvenOK.
	if l.SuspectAmt != 0 && l.ProvenOK >= l.SuspectAmt {
		l.clearSuspect()
	}

	// A bound we do trust says everything the suspicion was reaching for.
	if l.SuspectAmt != 0 && l.UpperFail != 0 &&
		l.UpperFail <= l.SuspectAmt {

		l.clearSuspect()
	}

	if capacity == 0 {
		return
	}

	switch {
	case l.Estimate <= capacity/intervalDepletedDivisor:
		l.Mode = intervalModeDepleted

	case l.Estimate >= capacity*intervalRichNumerator/
		intervalRichDenominator:

		l.Mode = intervalModeRich
	}
}

// clearSuspect empties the quarantine.
func (l *LiquidityInterval) clearSuspect() {
	l.SuspectAmt = 0
	l.SuspectWeight = 0
}

// recordSuspect quarantines a failure we cannot attribute with confidence. The
// weight says how much this one failure implicates this channel, which is a
// question of how many other channels it implicated equally.
//
// Nothing is written to the bounds until the weight standing behind the
// quarantine crosses the promotion threshold. At that point enough independent
// failures have agreed on the same channel and the same amount that treating it
// as proven is the better bet than continuing to guess.
func (l *LiquidityInterval) recordSuspect(amt, capacity lnwire.MilliSatoshi,
	weight float64) {

	// An amount we have watched settle over this channel is not a suspicion
	// worth holding. Only a settlement counts here; see ProvenOK.
	if l.ProvenOK != 0 && l.ProvenOK >= amt {
		return
	}

	if l.SuspectAmt == 0 || amt < l.SuspectAmt {
		l.SuspectAmt = amt
	}
	l.SuspectWeight += weight

	if l.SuspectWeight < intervalSuspectPromoteWeight {
		// Deliberately not marked as known. Known says the bounds hold
		// evidence, and a suspicion is held apart from them precisely
		// because we cannot say that. A channel under suspicion is still
		// priced off the prior, discounted at the amount named.
		l.normalize(capacity)

		return
	}

	// Convicted. The suspicion becomes an ordinary upper bound, and the
	// quarantine that held it is emptied, since from here it is the bound
	// that speaks.
	suspect := l.SuspectAmt
	if l.UpperFail == 0 || suspect < l.UpperFail {
		l.UpperFail = suspect
	}

	failed := suspect * intervalSuspectEstimateNumerator /
		intervalSuspectEstimateDenominator
	if l.Estimate == 0 || failed < l.Estimate {
		l.Estimate = failed
	}

	l.Confidence = math.Max(l.Confidence, intervalFailureConfidence)
	l.Failures++
	l.Known = true
	l.Restored = false
	l.clearSuspect()
	l.normalize(capacity)
}

// suspectFactor returns the discount a quarantined observation applies to the
// given amount. It is always above zero: a suspicion we have not convicted must
// never say impossible, because an impossible amount is never attempted and the
// attempt is the only thing that could clear the suspicion.
func (l *LiquidityInterval) suspectFactor(amt lnwire.MilliSatoshi) float64 {
	if l.SuspectAmt == 0 || amt < l.SuspectAmt {
		return 1
	}

	weight := math.Min(l.SuspectWeight, intervalSuspectPenaltyCap)

	return math.Exp(-intervalSuspectPenalty * weight)
}

// intervalStrongObservation reports whether an observation of the given amount
// is large enough to be allowed to move the mode latch.
func intervalStrongObservation(amt, capacity lnwire.MilliSatoshi) bool {
	if capacity == 0 {
		return false
	}

	threshold := capacity / intervalStrongObservationDivisor
	if threshold < 1 {
		threshold = 1
	}

	return amt >= threshold
}

// intervalPrior returns the success probability of forwarding the given amount
// over a channel of the given capacity, in the absence of any observation. It
// is the bimodal hypothesis written directly as a probability: channels are
// assumed to sit near one end of their range or the other, so a small amount is
// near certain to pass and an amount close to the whole capacity is near
// certain to fail, with a narrow transition between the two.
//
// Both the width of the modes and the position of the cliff are fractions of
// capacity, which is what makes this prior mean the same thing on a channel of
// any size.
func intervalPrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity == 0 || amt == 0 || amt > capacity {
		return 0
	}

	ratio := float64(amt) / float64(capacity)

	lowSide := intervalPriorMass * math.Exp(-ratio/intervalPriorScale)
	highSide := intervalPriorMass /
		(1 + math.Exp((ratio-intervalPriorCliff)/intervalPriorScale))

	probability := intervalPriorFloor + lowSide + highSide

	return math.Min(
		math.Max(probability, intervalPriorMin), intervalPriorMax,
	)
}

// intervalRetryFactor returns the multiplier to apply to the probability of an
// amount when this payment has already watched the same channel refuse a larger
// amount. An amount at or above the failed one is hopeless; below it the ladder
// gives back belief in proportion to how much smaller the retry is.
func intervalRetryFactor(amt, failedAt lnwire.MilliSatoshi) float64 {
	if failedAt == 0 {
		return 1
	}
	if amt >= failedAt {
		return 0
	}

	ratio := float64(amt) / float64(failedAt)
	for _, rung := range intervalRetryLadder {
		if ratio > rung.ratio {
			return rung.factor
		}
	}

	return intervalRetryFloor
}

// lowModeProbability returns the probability of an amount over a channel we
// have classified as depleted. The available balance is modelled as an
// exponential tail rising from the proven lower bound, truncated and
// renormalized at the proven upper bound when we have one.
func (l *LiquidityInterval) lowModeProbability(amt,
	capacity lnwire.MilliSatoshi) float64 {

	if l.LowerOK >= amt {
		return intervalLowModeProven
	}
	if l.UpperFail != 0 && amt >= l.UpperFail {
		return 0
	}

	scale := math.Max(float64(capacity)*intervalPriorScale, 1)
	tail := math.Exp(-float64(amt-l.LowerOK) / scale)
	probability := intervalLowModeFloor + intervalLowModeMass*tail

	if l.Estimate >= amt {
		probability = math.Max(probability, intervalLowModeEstimate)
	}

	// If we know where the channel stops, the tail cannot run past that
	// point, so we cut it there and renormalize what is left.
	if l.UpperFail != 0 {
		upperTail := math.Exp(
			-float64(l.UpperFail-l.LowerOK) / scale,
		)
		if upperTail < 0.999 {
			tail = math.Max((tail-upperTail)/(1-upperTail), 0)
			probability = intervalLowModeFloor +
				intervalLowModeMass*tail
		}
	}

	return probability
}

// Probability returns the success probability of forwarding the given amount
// over the channel this interval describes.
func (l *LiquidityInterval) Probability(amt,
	capacity lnwire.MilliSatoshi) float64 {

	// Without a capacity none of the fractions in the model mean anything,
	// so fall back to a flat guess rather than pretending to know.
	if capacity == 0 {
		return intervalUnknownCapacity
	}

	// An amount larger than the channel itself is impossible whatever we
	// remember about it, so this one zero is never softened below.
	prior := intervalPrior(amt, capacity)
	if prior == 0 {
		return 0
	}

	probability := l.rawProbability(amt, capacity, prior)

	// A quarantined failure discounts the amount it named without ruling it
	// out. Multiplying leaves a proven zero at zero and leaves a restored
	// belief above its floor, so neither of those rules is disturbed.
	probability *= l.suspectFactor(amt)

	// A belief we restored from disk describes a network that has had every
	// chance to move on since we wrote it down. The bounds are still worth
	// something, which is why we keep them, but they are no longer allowed
	// to speak with certainty in either direction: a restored upper bound
	// says unlikely rather than impossible, and a restored lower bound says
	// likely rather than proven. Without the floor the model has no way back
	// from a bound that has gone stale, because nothing but an attempt can
	// revise one and an impossible amount is never attempted.
	if l.Restored {
		return math.Min(
			math.Max(probability, intervalRestoredFloor),
			intervalRestoredCeiling,
		)
	}

	// A bound this process watched hold is the one thing the model is
	// allowed to call impossible, so it is not floored.
	if probability == 0 {
		return 0
	}

	return math.Min(
		math.Max(probability, intervalMinProbability),
		intervalMaxProbability,
	)
}

// rawProbability runs the branch table of the model. The branches are ordered
// by how much the evidence proves, from a bound we watched hold to a guess we
// have nothing behind. A zero here means the evidence rules the amount out; it
// is the caller that decides whether the evidence is fresh enough to be
// believed that far.
func (l *LiquidityInterval) rawProbability(amt, capacity lnwire.MilliSatoshi,
	prior float64) float64 {

	var probability float64

	switch {
	// We have watched this channel carry at least this much.
	case l.LowerOK >= amt:
		probability = intervalProvenProbability

	// We have watched this channel refuse at most this much.
	case l.UpperFail != 0 && amt >= l.UpperFail:
		return 0

	// Nothing has ever been observed here.
	case !l.Known:
		probability = prior

	// The channel looks empty in this direction.
	case l.Mode == intervalModeDepleted:
		probability = l.lowModeProbability(amt, capacity)

	// The channel looks full in this direction and our estimate covers the
	// amount.
	case l.Mode == intervalModeRich && l.Estimate >= amt:
		margin := float64(l.Estimate-amt+1) / float64(capacity)
		probability = intervalRichBase +
			intervalRichConfidence*l.Confidence +
			intervalRichMargin*math.Min(margin*8, 1)

	// We have bounds on both sides, so interpolate across them. The
	// position of the amount inside the interval is what decides, which
	// makes this a smooth walk between the two certainties above.
	case l.UpperFail != 0:
		lower := float64(l.LowerOK)
		upper := float64(l.UpperFail)
		position := (float64(amt) - lower) /
			math.Max(upper-lower, 1)
		position = math.Min(math.Max(position, 0), 1)

		probability = intervalPositionFloor + intervalPositionMass*
			math.Pow(1-position, intervalPositionExponent)
		probability = (1-intervalPriorBlend)*probability +
			intervalPriorBlend*prior

	// No upper bound, but our estimate covers the amount.
	case l.Estimate >= amt:
		margin := float64(l.Estimate-amt+1) / float64(capacity)
		probability = intervalEstimateBase +
			intervalEstimateConfidence*l.Confidence +
			intervalEstimateMargin*math.Min(margin*5, 1)

	// The amount runs past our estimate but nothing has proven it cannot
	// pass, so discount the prior by how far past it runs.
	default:
		over := float64(amt-l.Estimate) / float64(capacity)
		probability = prior * intervalOverBase *
			math.Exp(-over/intervalOverScale)
	}

	return probability
}

// recordProbe records that this direction forwarded the given amount, which we
// learn whenever a failure comes back from a node further along the route than
// this hop. The forward lower bound rises to the amount, and the reverse
// direction's upper bound drops, because liquidity sitting on this side of the
// channel cannot also be sitting on the other side.
func (l *LiquidityInterval) recordProbe(reverse *LiquidityInterval,
	amt, capacity lnwire.MilliSatoshi) {

	if amt > l.LowerOK {
		l.LowerOK = amt
	}
	if l.UpperFail != 0 && amt >= l.UpperFail {
		l.UpperFail = 0
	}

	inferred := amt
	strong := intervalStrongObservation(amt, capacity)
	if strong {
		high := capacity * intervalProbeEstimateNumerator /
			intervalProbeEstimateDenominator
		if high > inferred {
			inferred = high
		}

		l.Mode = intervalModeRich
	}

	if !l.Known || inferred > l.Estimate {
		l.Estimate = inferred
	}

	l.Known = true
	l.Restored = false
	l.Confidence = math.Max(l.Confidence, intervalProbeConfidence)
	l.Successes++
	if l.Failures > 0 {
		l.Failures--
	}
	l.normalize(capacity)

	// Whatever sits on this side of the channel is not on the other side,
	// so an amount that passed here bounds what can pass back.
	reverseUpper := lnwire.MilliSatoshi(1)
	if capacity >= amt {
		reverseUpper = capacity - amt + 1
	}
	if reverse.UpperFail == 0 || reverseUpper < reverse.UpperFail {
		reverse.UpperFail = reverseUpper
	}
	if reverse.LowerOK >= reverse.UpperFail {
		reverse.LowerOK = reverse.UpperFail - 1
	}

	if strong {
		reverseEstimate := capacity - l.Estimate
		if reverseEstimate < reverse.LowerOK {
			reverseEstimate = reverse.LowerOK
		}
		if !reverse.Known || reverseEstimate < reverse.Estimate {
			reverse.Estimate = reverseEstimate
		}

		reverse.Mode = intervalModeDepleted
	}

	reverse.Known = true
	reverse.Restored = false
	reverse.Confidence = math.Max(
		reverse.Confidence, intervalProbeReverseConfidence,
	)
	reverse.normalize(capacity)
}

// recordFailure records that this direction could not carry the given amount.
// The forward upper bound drops to the failing amount, and the reverse
// direction gains a lower bound and counts a success, because a failure in one
// direction is evidence of available liquidity in the other.
func (l *LiquidityInterval) recordFailure(reverse *LiquidityInterval,
	amt, capacity lnwire.MilliSatoshi) {

	if l.UpperFail == 0 || amt < l.UpperFail {
		l.UpperFail = amt
	}
	if l.LowerOK >= amt {
		l.LowerOK = amt - 1
	}

	strong := intervalStrongObservation(amt, capacity)
	depleted := amt / intervalFailureEstimateDivisor
	if strong {
		floor := capacity / intervalFailureFloorDivisor
		if floor < 1 {
			floor = 1
		}
		if depleted > floor {
			depleted = floor
		}

		l.Mode = intervalModeDepleted
	}

	if depleted < l.LowerOK {
		depleted = l.LowerOK
	}
	if !l.Known || depleted < l.Estimate {
		l.Estimate = depleted
	}

	l.Known = true
	l.Restored = false
	l.Confidence = math.Max(l.Confidence, intervalFailureConfidence)
	l.Failures++
	l.normalize(capacity)

	reverseLower := lnwire.MilliSatoshi(0)
	if capacity >= amt {
		reverseLower = capacity - amt + 1
	}
	if reverseLower > reverse.LowerOK {
		reverse.LowerOK = reverseLower
	}
	if reverse.UpperFail != 0 && reverse.LowerOK >= reverse.UpperFail {
		reverse.UpperFail = 0
	}

	reverseEstimate := capacity - l.Estimate
	if reverseEstimate < reverse.LowerOK {
		reverseEstimate = reverse.LowerOK
	}
	if !reverse.Known || reverseEstimate > reverse.Estimate {
		reverse.Estimate = reverseEstimate
	}
	if strong {
		reverse.Mode = intervalModeRich
	}

	reverse.Known = true
	reverse.Restored = false
	reverse.Confidence = math.Max(
		reverse.Confidence, intervalFailureReverseConfidence,
	)
	reverse.Successes++
	reverse.normalize(capacity)
}

// recordSettlement records that this direction actually moved the given amount.
// Unlike the other two observations this one does not just narrow an interval,
// it shifts it: the balance really has moved across the channel, so the forward
// interval slides down by the settled amount and the reverse interval slides up
// by the same.
func (l *LiquidityInterval) recordSettlement(reverse *LiquidityInterval,
	amt, capacity lnwire.MilliSatoshi) {

	// Work out what the balance must have been before the settlement, then
	// subtract what just left.
	before := l.Estimate
	if !l.Known || before < amt {
		before = amt
		if intervalStrongObservation(amt, capacity) {
			high := capacity * intervalProbeEstimateNumerator /
				intervalProbeEstimateDenominator
			if high > before {
				before = high
			}

			l.Mode = intervalModeRich
		}
	}
	if before > capacity {
		before = capacity
	}

	l.Estimate = before - amt
	if l.LowerOK > amt {
		l.LowerOK -= amt
	} else {
		l.LowerOK = 0
	}
	if l.UpperFail > amt {
		l.UpperFail -= amt
	} else {
		l.UpperFail = 0
	}

	// This is the only place ProvenOK is ever written. The amount really
	// moved over this channel in this direction, which is the one claim
	// strong enough to clear a suspicion.
	if amt > l.ProvenOK {
		l.ProvenOK = amt
	}

	l.Known = true
	l.Restored = false
	l.Confidence = math.Max(l.Confidence, intervalSettleConfidence)
	l.Successes++
	if l.Failures > 0 {
		l.Failures--
	}
	l.normalize(capacity)

	headroom := lnwire.MilliSatoshi(0)
	if capacity >= amt {
		headroom = capacity - amt
	}

	if reverse.LowerOK > headroom {
		reverse.LowerOK = capacity
	} else {
		reverse.LowerOK += amt
	}

	if reverse.UpperFail != 0 {
		if reverse.UpperFail > headroom {
			reverse.UpperFail = 0
		} else {
			reverse.UpperFail += amt
		}
	}

	reverse.Estimate = capacity - l.Estimate
	if reverse.Estimate < reverse.LowerOK {
		reverse.Estimate = reverse.LowerOK
	}

	reverse.Known = true
	reverse.Restored = false
	reverse.Confidence = math.Max(
		reverse.Confidence, intervalSettleConfidence,
	)
	reverse.Successes++
	if reverse.Failures > 0 {
		reverse.Failures--
	}
	reverse.normalize(capacity)
}
