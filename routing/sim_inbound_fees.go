package routing

import (
	"fmt"
	"math/rand"
	"sort"
)

// An inbound fee is the one piece of a real forwarding policy the simulator
// had no field for. Every generated tier therefore announces no inbound fee
// anywhere, and until stage B the describegraph loader parsed the real ones
// away, so lnd's inbound fee machinery ran against a hardcoded zero for the
// whole program.
//
// The families below put the real thing on generated tiers. As in stage A they
// draw from the marginals measured over the mainnet snapshot rather than from a
// shape we invented, which is the exp-017 lesson applied before the fact: a
// family we author is a family a router can only be overfit to by us. The one
// authored family here says so in its name and in every place it is reported.

// The measurements below were taken over the 62,798 announced directed
// policies of the 12,161 node describegraph snapshot at
// ~/codez/data/mainnet_graph.json on 2026-07-27, and they reproduce the design
// spec's census exactly: 4,783 policies carry a non-zero inbound fee (7.6%),
// 4,660 of them are discounts, 123 are surcharges, 284 distinct nodes
// advertise one, the median rate is -200 ppm, the 5th percentile is -2,000 ppm
// and the most negative is -18,800 ppm. The base component is almost always
// zero and never positive, reaching -10,000 msat at the tail.

const (
	// InboundFeeFamilyMainnet draws the inbound fee of every directed
	// policy from the mainnet marginals: whether the policy carries one at
	// all, its sign, and the magnitude of each component.
	InboundFeeFamilyMainnet = "mainnet_empirical"

	// InboundFeeFamilyHeavy is the AUTHORED stress rung, labelled as
	// authored wherever it is reported. It keeps the measured shape and
	// the measured sign split, so it stays a world of discounts rather
	// than becoming a world of surcharges, and turns up two dials the real
	// graph leaves low: every directed policy carries an inbound fee, and
	// every magnitude is multiplied. The result is a network where the
	// cheapest route is routinely not the one an outbound-only edge cost
	// picks.
	InboundFeeFamilyHeavy = "heavy"

	// InboundFeeFamilyLoaded draws nothing and enforces what the network
	// already announces. It is how the mainnet tier runs with its 4,783
	// real inbound fees priced, since drawing over a loaded snapshot would
	// throw away the very data this stage exists to recover.
	InboundFeeFamilyLoaded = "as_loaded"
)

// heavyInboundScale multiplies every drawn magnitude in the authored stress
// family. It is a round number chosen to move the median discount from -200
// ppm to -1,000, i.e. into the same range as the simulator's own synthetic
// outbound rates, so that ignoring an inbound fee costs about what ignoring an
// outbound one would.
const heavyInboundScale = 5

// SimInboundFeeParams parameterizes the inbound fees of every directed policy
// in the network, and switches the mechanism on. The zero value changes
// nothing at all: no fee is charged at forwarding time and none is shown in
// gossip, which is how every scenario file written before stage B behaves.
type SimInboundFeeParams struct {
	// Family names the family every directed policy's inbound fee is drawn
	// from. An empty string leaves the mechanism off entirely.
	Family string `json:"family"`

	// Seed seeds the draw. Zero derives one deterministically from the
	// scenario's liquidity seed, so omitting it is still reproducible.
	Seed int64 `json:"seed"`
}

// enabled reports whether this section asks for the mechanism at all.
func (p *SimInboundFeeParams) enabled() bool {
	return p != nil && p.Family != ""
}

// SimInboundFeeStats is the static census of a network's inbound fees, and the
// half of stage B's manipulation check that measures rather than alarms. A
// discount changes what a sender is willing to pay, not what a forwarding node
// does, so it leaves no trace on the wire at all; without these counts a tier
// whose inbound fees are everywhere would be indistinguishable in the output
// from one where the family drew nothing.
type SimInboundFeeStats struct {
	// Policies is how many directed policies the network carries.
	Policies int `json:"inbound_fee_policies,omitempty"`

	// Charging is how many of them announce a non-zero inbound fee.
	Charging int `json:"inbound_fee_charging,omitempty"`

	// Discounts is how many announce a negative one, which is what 97% of
	// the real ones are.
	Discounts int `json:"inbound_fee_discounts,omitempty"`

	// Surcharges is how many announce a positive one. These are the only
	// inbound fees that can refuse an htlc rather than merely reprice it,
	// so a tier with none of them cannot produce a wire refusal.
	Surcharges int `json:"inbound_fee_surcharges,omitempty"`
}

// inboundFeeFamily is the parsed form of a family name.
type inboundFeeFamily uint8

const (
	// inboundFeeFamilyNone leaves the mechanism off.
	inboundFeeFamilyNone inboundFeeFamily = iota

	// inboundFeeFamilyLoaded enforces what the network already announces.
	inboundFeeFamilyLoaded

	// inboundFeeFamilyEmpirical draws from the measured mainnet marginals.
	inboundFeeFamilyEmpirical

	// inboundFeeFamilyHeavy draws from the authored stress shape.
	inboundFeeFamilyHeavy
)

// parseInboundFeeFamily turns a family name into the family it names.
func parseInboundFeeFamily(name string) (inboundFeeFamily, error) {
	switch name {
	case "":
		return inboundFeeFamilyNone, nil

	case InboundFeeFamilyLoaded:
		return inboundFeeFamilyLoaded, nil

	case InboundFeeFamilyMainnet:
		return inboundFeeFamilyEmpirical, nil

	case InboundFeeFamilyHeavy:
		return inboundFeeFamilyHeavy, nil
	}

	return inboundFeeFamilyNone, fmt.Errorf("unknown inbound fee family "+
		"%q (want %q, %q or %q)", name, InboundFeeFamilyLoaded,
		InboundFeeFamilyMainnet, InboundFeeFamilyHeavy)
}

// Measured shares over the 4,783 policies that carry an inbound fee. The sign
// split is lopsided by design of the feature rather than by accident: lnd
// refuses to set a positive inbound fee unless the operator opts in with
// accept-positive-inbound-fees, because senders that predate the feature
// cannot pay one.
const (
	// inboundFeeShare is the share of all directed policies announcing a
	// non-zero inbound fee, 4,783 of 62,798.
	inboundFeeShare = 0.076165

	// inboundDiscountShare is the share of those whose rate is negative,
	// 4,601 of 4,783.
	inboundDiscountShare = 0.961949

	// inboundSurchargeShare is the share whose rate is positive, 123 of
	// 4,783. The remaining 1.2% announce a base component and no rate.
	inboundSurchargeShare = 0.025716

	// inboundBaseShare is the share of the fee-carrying policies that
	// announce a non-zero base component, 837 of 4,783. Every one of them
	// is negative.
	inboundBaseShare = 0.174995
)

// inboundKnot is one point of a measured magnitude distribution: the absolute
// value in msat or ppm at the given quantile of the policies concerned.
type inboundKnot struct {
	// quantile is the share of the policies at or below value.
	quantile float64

	// value is the magnitude, always positive; the sign is chosen by the
	// caller from the sign split above.
	value float64
}

// mainnetInboundDiscountRate is the measured quantile ladder of the discount
// rate's magnitude, over the 4,601 policies that announce a negative rate. The
// median is 235 ppm and the ladder is long tailed, reaching 18,800.
var mainnetInboundDiscountRate = []inboundKnot{
	{0.00, 1},
	{0.01, 5},
	{0.05, 15},
	{0.10, 29},
	{0.20, 58},
	{0.30, 100},
	{0.40, 174},
	{0.50, 235},
	{0.60, 316},
	{0.70, 500},
	{0.80, 750},
	{0.90, 1386},
	{0.95, 2000},
	{0.99, 3850},
	{1.00, 18800},
}

// mainnetInboundSurchargeRate is the measured quantile ladder of the surcharge
// rate, over the 123 policies that announce a positive one. The single largest
// is a 1,000,000 ppm policy, i.e. a node charging the amount again to receive
// it, and the ladder keeps it because a router that cannot survive one
// pathological node is worth knowing about.
var mainnetInboundSurchargeRate = []inboundKnot{
	{0.00, 12},
	{0.10, 25},
	{0.20, 49},
	{0.30, 90},
	{0.40, 173},
	{0.50, 278},
	{0.60, 388},
	{0.70, 688},
	{0.80, 1026},
	{0.90, 2814},
	{0.95, 8888},
	{1.00, 1000000},
}

// mainnetInboundBase is the measured quantile ladder of the base component's
// magnitude, over the 837 policies that announce one. It is dominated by a
// single point mass at 1,000 msat.
var mainnetInboundBase = []inboundKnot{
	{0.00, 1},
	{0.05, 10},
	{0.10, 16},
	{0.20, 128},
	{0.30, 250},
	{0.40, 999},
	{0.90, 1000},
	{0.95, 2000},
	{0.99, 5000},
	{1.00, 10000},
}

// sampleInboundKnots maps a uniform draw onto a measured ladder, interpolating
// linearly between knots. A flat stretch of the ladder reproduces a point mass
// and a steep one reproduces a tail, which is how the 1,000 msat base and the
// 18,800 ppm discount both come out of the same machinery.
func sampleInboundKnots(knots []inboundKnot, u float64) float64 {
	for i := 1; i < len(knots); i++ {
		if u > knots[i].quantile {
			continue
		}

		lo, hi := knots[i-1], knots[i]
		span := hi.quantile - lo.quantile
		if span <= 0 {
			return hi.value
		}

		return lo.value + (u-lo.quantile)/span*(hi.value-lo.value)
	}

	return knots[len(knots)-1].value
}

// simInboundFeeDraws is how many uniform draws each directed policy consumes,
// whatever the family asks for. Holding it fixed keeps a policy's stream
// independent of the branch its first draw took, so two families can be
// compared without one of them shifting the other's values downstream. The
// three are: presence and rate, base presence, base magnitude.
const simInboundFeeDraws = 3

// simInboundFeeSeed derives the inbound fee seed from the scenario's liquidity
// seed. It is a different mixer step from simHtlcLimitsSeed's and
// simAttributionSeed's, so a file that pins none of the three does not run
// them in lockstep.
func simInboundFeeSeed(liquiditySeed int64) int64 {
	return liquiditySeed*6364136223846793005 + 1442695040888963407
}

// drawInboundFee produces one directed policy's inbound fee from three uniform
// draws, under the given family. It returns the base in msat and the rate in
// ppm, both signed, and both zero for a policy that announces no inbound fee.
func drawInboundFee(family inboundFeeFamily,
	draws [simInboundFeeDraws]float64) (int32, int32) {

	share, scale := inboundFeeShare, 1.0
	if family == inboundFeeFamilyHeavy {
		share, scale = 1.0, heavyInboundScale
	}

	// The first draw decides both whether this policy carries an inbound
	// fee and, if it does, the sign of its rate. Rescaling the tail of the
	// same draw rather than spending a second one keeps the sign split
	// exact at every share.
	if draws[0] >= share {
		return 0, 0
	}

	sign := draws[0] / share

	var rate float64
	switch {
	case sign < inboundDiscountShare:
		u := sign / inboundDiscountShare
		rate = -scale * sampleInboundKnots(
			mainnetInboundDiscountRate, u,
		)

	case sign < inboundDiscountShare+inboundSurchargeShare:
		u := (sign - inboundDiscountShare) / inboundSurchargeShare
		rate = scale * sampleInboundKnots(
			mainnetInboundSurchargeRate, u,
		)
	}

	// The base component is announced by a minority of the policies that
	// announce anything, and it is negative without exception on the real
	// graph. The 1.2% of policies whose rate is zero are exactly the ones
	// that announce a base and nothing else, so there the base is certain
	// rather than drawn for.
	var base float64
	if rate == 0 || draws[1] < inboundBaseShare {
		base = -scale * sampleInboundKnots(
			mainnetInboundBase, draws[2],
		)
	}

	return int32(base), int32(rate)
}

// ApplyInboundFees redraws the inbound fee of every directed policy in the
// graph from the named family, deterministically from the seed, and switches
// the inbound fee mechanism on. Policies are visited in channel id order and
// node1 end first, so a given (family, seed) pair always produces the same
// network regardless of map iteration.
//
// The as_loaded family draws nothing and only switches the mechanism on, which
// is what the mainnet tier wants: the snapshot's own 4,783 inbound fees are
// the data, and a draw over them would replace measured values with modelled
// ones exactly where the measurement is the point.
//
// A nil or empty section is a no-op. That is what makes a corpus generated
// without the section byte identical to every corpus generated before the
// section existed, and it is also what keeps the mainnet tier reproducing its
// published numbers while its policies carry real inbound fees the loader now
// preserves.
func (g *SimGraph) ApplyInboundFees(params *SimInboundFeeParams,
	defaultSeed int64) error {

	if !params.enabled() {
		return nil
	}

	// Parse the family before touching a single policy, so that a
	// malformed section cannot leave the network half redrawn.
	family, err := parseInboundFeeFamily(params.Family)
	if err != nil {
		return err
	}

	g.inboundFees = true

	if family == inboundFeeFamilyLoaded {
		return nil
	}

	seed := params.Seed
	if seed == 0 {
		seed = simInboundFeeSeed(defaultSeed)
	}
	rng := rand.New(rand.NewSource(seed))

	ids := make([]uint64, 0, len(g.channels))
	for id := range g.channels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		channel := g.channels[id]

		for i := range channel.ends {
			var draws [simInboundFeeDraws]float64
			for j := range draws {
				draws[j] = rng.Float64()
			}

			base, rate := drawInboundFee(family, draws)

			policy := &channel.ends[i].policy
			policy.InboundBaseMsat = base
			policy.InboundRatePPM = rate
		}
	}

	return nil
}

// InboundFeeStats counts how many of the network's directed policies announce
// an inbound fee, and which way. It reads the graph as it stands, so it reports
// the real mainnet fees on a loaded snapshot just as it reports drawn ones on a
// generated tier.
func (g *SimGraph) InboundFeeStats() SimInboundFeeStats {
	var stats SimInboundFeeStats

	for _, channel := range g.channels {
		for i := range channel.ends {
			policy := &channel.ends[i].policy
			stats.Policies++

			if !policy.hasInboundFee() {
				continue
			}
			stats.Charging++

			// A policy is a discount when the fee it charges on any
			// amount is negative, which the rate decides wherever
			// it is non-zero and the base decides otherwise. No
			// real policy announces the two with opposite signs.
			switch {
			case policy.InboundRatePPM < 0:
				stats.Discounts++

			case policy.InboundRatePPM > 0:
				stats.Surcharges++

			case policy.InboundBaseMsat < 0:
				stats.Discounts++

			default:
				stats.Surcharges++
			}
		}
	}

	return stats
}
