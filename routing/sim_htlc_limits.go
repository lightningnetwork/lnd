package routing

import (
	"fmt"
	"math/rand"
	"sort"

	"github.com/lightningnetwork/lnd/lnwire"
)

// Min and max htlc are the two announced constraints every routing candidate
// in this program already reads, and on every synthetic tier they have carried
// a constant. defaultSimPolicy stamps MinHTLCMsat: 1000 and leaves MaxHTLCMsat
// at zero, which means no maximum, so a generated network announces a floor
// below dust and no ceiling at all. The mainnet snapshot is the sole exception,
// and there the values are not trivial: one directed policy in eight announces
// a ceiling below half its channel's capacity, and one in seventeen announces a
// floor at or above 100 sats.
//
// The families below put that pressure back on the generated tiers. They draw
// from the marginals measured over the real graph rather than from a shape we
// invented, which is the exp-017 lesson applied before the fact: a family we
// author is a family a router can only be overfit to by us.

// The measurements below were taken over the 62,798 directed policies of the
// 12,161 node describegraph snapshot at ~/codez/data/mainnet_graph.json on
// 2026-07-27. Only one of those policies announces no maximum at all, so the
// empirical family gives every directed policy a maximum, which is itself a
// change of regime from the generated tiers where none of them has one.

const (
	// HtlcLimitFamilyMainnet draws both limits from the mainnet marginals:
	// the maximum from the measured max_htlc/capacity quantile ladder, the
	// minimum from the measured value ladder.
	HtlcLimitFamilyMainnet = "mainnet_empirical"

	// HtlcLimitFamilyTight is the AUTHORED stress rung, and is labelled as
	// authored wherever it is reported. Its maximum is uniform on
	// [0.1, 0.4] of capacity, far below anything mainnet does at that
	// density, so every channel binds and splitting becomes mandatory. Its
	// minimum is the measured ladder, so the stress lives entirely in the
	// ceiling.
	HtlcLimitFamilyTight = "tight"
)

// simHtlcFloorMsat is the floor at or above which an announced minimum htlc is
// counted as a real constraint rather than a dust guard. It is the threshold
// the mainnet survey used, where 5.9% of directed policies sit at or above it.
const simHtlcFloorMsat lnwire.MilliSatoshi = 100_000

// tightMaxFracLow and tightMaxFracHigh bound the authored stress family's
// maximum htlc, as a fraction of channel capacity.
const (
	tightMaxFracLow  = 0.1
	tightMaxFracHigh = 0.4
)

// SimHtlcLimitsParams parameterizes the announced htlc limits of every
// directed policy in the network. The zero value changes nothing, so a
// scenario file that omits the section keeps the constant limits every
// synthetic tier has carried for the whole program.
type SimHtlcLimitsParams struct {
	// MaxHtlcFracFamily names the family the maximum htlc of each directed
	// policy is drawn from, as a fraction of the channel's capacity. An
	// empty string leaves every maximum exactly as the topology generator
	// or the describegraph loader left it.
	MaxHtlcFracFamily string `json:"max_htlc_frac_family"`

	// MinHtlcFamily names the family the minimum htlc of each directed
	// policy is drawn from. An empty string leaves every minimum alone.
	MinHtlcFamily string `json:"min_htlc_family"`

	// Seed seeds the draw. Zero derives one deterministically from the
	// scenario's liquidity seed, so omitting it is still reproducible.
	Seed int64 `json:"seed"`
}

// enabled reports whether this section asks for any draw at all.
func (p *SimHtlcLimitsParams) enabled() bool {
	return p != nil &&
		(p.MaxHtlcFracFamily != "" || p.MinHtlcFamily != "")
}

// SimHtlcLimitStats describes how binding the announced limits of a network
// are. It is the static half of the manipulation check: a tier whose limits
// never bind is a tier that is testing nothing, and exp-016 is the standing
// reminder that a knob without a counter cannot tell "the mechanism did not
// matter" from "the mechanism never fired".
type SimHtlcLimitStats struct {
	// Policies is how many directed policies the network carries.
	Policies int `json:"htlc_limit_policies,omitempty"`

	// Bounded is how many of them announce a maximum htlc strictly below
	// their channel's capacity, which is the only maximum that can ever
	// refuse an htlc the liquidity would have carried.
	Bounded int `json:"htlc_limit_bounded,omitempty"`

	// Floors is how many of them announce a minimum htlc at or above
	// simHtlcFloorMsat, i.e. a floor that a real shard can fall under
	// rather than a dust guard.
	Floors int `json:"htlc_limit_floors,omitempty"`
}

// htlcLimitFamily is the parsed form of a family name.
type htlcLimitFamily uint8

const (
	// htlcLimitFamilyNone leaves the limit untouched.
	htlcLimitFamilyNone htlcLimitFamily = iota

	// htlcLimitFamilyEmpirical draws from the measured mainnet marginal.
	htlcLimitFamilyEmpirical

	// htlcLimitFamilyTight draws from the authored stress shape.
	htlcLimitFamilyTight
)

// parseHtlcLimitFamily turns a family name into the family it names. The empty
// string is not an error: it is how a section asks for one limit to be drawn
// and the other left alone.
func parseHtlcLimitFamily(name string) (htlcLimitFamily, error) {
	switch name {
	case "":
		return htlcLimitFamilyNone, nil

	case HtlcLimitFamilyMainnet:
		return htlcLimitFamilyEmpirical, nil

	case HtlcLimitFamilyTight:
		return htlcLimitFamilyTight, nil
	}

	return htlcLimitFamilyNone, fmt.Errorf("unknown htlc limit family %q "+
		"(want %q or %q)", name, HtlcLimitFamilyMainnet,
		HtlcLimitFamilyTight)
}

// htlcMaxFracKnot is one point of the measured max_htlc/capacity distribution:
// the fraction of capacity at the given quantile of the directed policies that
// announce a maximum.
type htlcMaxFracKnot struct {
	// quantile is the share of policies at or below frac.
	quantile float64

	// frac is max_htlc divided by capacity in msat.
	frac float64
}

// mainnetMaxHtlcFrac is the measured quantile ladder of max_htlc/capacity. The
// knots are dense at the bottom because that is where the whole distribution
// lives: three quarters of the real policies sit on the single point 0.99, so
// everything interesting happens in the first fifth of the ladder. Sampling
// interpolates linearly between knots, which reproduces both the point mass at
// 0.99 (a flat stretch of the ladder) and the long thin tail of policies whose
// announced ceiling is a rounding error away from zero.
var mainnetMaxHtlcFrac = []htlcMaxFracKnot{
	{0.000, 0.000000000005},
	{0.002, 0.0000001},
	{0.005, 0.00005},
	{0.0075, 0.0004096},
	{0.010, 0.001024},
	{0.015, 0.00826676247},
	{0.020, 0.0198},
	{0.030, 0.0898883353},
	{0.040, 0.133333333},
	{0.050, 0.2000615},
	{0.060, 0.3},
	{0.080, 0.45},
	{0.120, 0.45},
	{0.140, 0.51221711},
	{0.160, 0.8},
	{0.180, 0.9},
	{0.200, 0.9727},
	{0.220, 0.99},
	{0.900, 0.990000301},
	{0.950, 1.0},
	{1.000, 1.0},
}

// htlcMinKnot is one value of the measured min_htlc distribution and the share
// of directed policies that announce it.
type htlcMinKnot struct {
	// msat is the announced minimum.
	msat lnwire.MilliSatoshi

	// prob is the share of policies announcing it, renormalized over the
	// twelve values in the ladder.
	prob float64
}

// mainnetMinHtlc is the measured value ladder of min_htlc. Announced minimums
// are extremely lumpy on the real graph: 104 distinct values exist, but the
// twelve below account for 98.9% of all directed policies, so the ladder is
// those twelve renormalized to one. The tail this drops is a scatter of
// one-operator values with no shape worth reproducing.
var mainnetMinHtlc = []htlcMinKnot{
	{1_000, 0.792829},
	{1, 0.108579},
	{100_000, 0.028091},
	{1_000_000, 0.023388},
	{0, 0.017090},
	{10_000, 0.006701},
	{5_000, 0.005589},
	{3_000, 0.004542},
	{100, 0.004429},
	{9_000, 0.003399},
	{300_000, 0.003157},
	{10_000_000, 0.002207},
}

// sampleMaxHtlcFrac maps a uniform draw to a max_htlc/capacity fraction under
// the given family.
func sampleMaxHtlcFrac(family htlcLimitFamily, u float64) float64 {
	if family == htlcLimitFamilyTight {
		return tightMaxFracLow + u*(tightMaxFracHigh-tightMaxFracLow)
	}

	knots := mainnetMaxHtlcFrac
	for i := 1; i < len(knots); i++ {
		if u > knots[i].quantile {
			continue
		}

		lo, hi := knots[i-1], knots[i]
		span := hi.quantile - lo.quantile
		if span <= 0 {
			return hi.frac
		}

		return lo.frac + (u-lo.quantile)/span*(hi.frac-lo.frac)
	}

	return knots[len(knots)-1].frac
}

// sampleMinHtlc maps a uniform draw to an announced minimum htlc. Both
// families share the measured ladder: the tight family's stress is in its
// ceiling, and an authored floor would only add a second moving part to a
// stress rung that already has one.
func sampleMinHtlc(u float64) lnwire.MilliSatoshi {
	var cumulative float64
	for _, knot := range mainnetMinHtlc {
		cumulative += knot.prob
		if u < cumulative {
			return knot.msat
		}
	}

	// The ladder's probabilities are rounded, so a draw can land in the
	// sliver past their sum. It belongs to the mode, which is where 79% of
	// the real policies are.
	return mainnetMinHtlc[0].msat
}

// simHtlcLimitDraws is how many uniform draws each directed policy consumes,
// whatever the section asks for. Holding it fixed makes the minimum's stream
// independent of whether a maximum was drawn, so the two families can be moved
// one at a time without either one shifting the other's values.
const simHtlcLimitDraws = 2

// simHtlcLimitsSeed derives the limit seed from the scenario's liquidity seed.
// It is a different mixer step from simAttributionSeed's so that a file which
// pins neither seed does not run its two degradation streams in lockstep.
func simHtlcLimitsSeed(liquiditySeed int64) int64 {
	return liquiditySeed*2862933555777941757 + 3037000493
}

// ApplyHtlcLimits redraws the announced min and max htlc of every directed
// policy in the graph from the named families, deterministically from the
// seed. Policies are visited in channel id order and node1 end first, so a
// given (families, seed) pair always produces the same network regardless of
// map iteration.
//
// Nothing new reaches a router: both fields are already on the gossip struct
// and already read by every arm. What changes is that they finally carry
// something other than a constant.
//
// A nil or empty section is a no-op, which is what makes a corpus generated
// without the section byte identical to every corpus generated before it
// existed. Applying the section to a graph loaded from a describegraph
// snapshot OVERWRITES the real announced limits with drawn ones, which is why
// the mainnet tier is run without it.
func (g *SimGraph) ApplyHtlcLimits(params *SimHtlcLimitsParams,
	defaultSeed int64) error {

	if !params.enabled() {
		return nil
	}

	// Parse both families before touching a single policy, so that a
	// malformed section cannot leave the network half redrawn.
	maxFamily, err := parseHtlcLimitFamily(params.MaxHtlcFracFamily)
	if err != nil {
		return err
	}
	minFamily, err := parseHtlcLimitFamily(params.MinHtlcFamily)
	if err != nil {
		return err
	}

	seed := params.Seed
	if seed == 0 {
		seed = simHtlcLimitsSeed(defaultSeed)
	}
	rng := rand.New(rand.NewSource(seed))

	ids := make([]uint64, 0, len(g.channels))
	for id := range g.channels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		channel := g.channels[id]
		capacityMsat := lnwire.NewMSatFromSatoshis(channel.Capacity)

		for i := range channel.ends {
			var draws [simHtlcLimitDraws]float64
			for j := range draws {
				draws[j] = rng.Float64()
			}

			policy := &channel.ends[i].policy

			if maxFamily != htlcLimitFamilyNone {
				frac := sampleMaxHtlcFrac(maxFamily, draws[0])

				// A zero maximum means "no maximum" everywhere
				// in the simulator, so the smallest ceiling
				// this can announce is one millisatoshi. The
				// real graph has policies that thin, and they
				// are dead directions there too.
				maxMsat := lnwire.MilliSatoshi(
					frac * float64(capacityMsat),
				)
				if maxMsat < 1 {
					maxMsat = 1
				}

				policy.MaxHTLCMsat = maxMsat
			}

			if minFamily != htlcLimitFamilyNone {
				minMsat := sampleMinHtlc(draws[1])

				// No mainnet policy announces a floor above its
				// own ceiling, so neither does a drawn one: a
				// policy that cannot forward any amount at all
				// is an artifact of drawing two marginals
				// independently, not a fact about the network.
				ceiling := policy.MaxHTLCMsat
				if ceiling != 0 && minMsat > ceiling {
					minMsat = ceiling
				}

				policy.MinHTLCMsat = minMsat
			}
		}
	}

	return nil
}

// HtlcLimitStats counts how many of the network's directed policies announce a
// limit that can actually bind. It reads the graph as it stands, so it reports
// the real mainnet limits on a loaded snapshot just as it reports drawn ones on
// a generated tier.
func (g *SimGraph) HtlcLimitStats() SimHtlcLimitStats {
	var stats SimHtlcLimitStats

	for _, channel := range g.channels {
		capacityMsat := lnwire.NewMSatFromSatoshis(channel.Capacity)

		for i := range channel.ends {
			policy := &channel.ends[i].policy
			stats.Policies++

			if policy.MaxHTLCMsat != 0 &&
				policy.MaxHTLCMsat < capacityMsat {

				stats.Bounded++
			}

			if policy.MinHTLCMsat >= simHtlcFloorMsat {
				stats.Floors++
			}
		}
	}

	return stats
}
