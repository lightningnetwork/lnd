package routing

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// LiquidityModel names a distribution used to assign the hidden balances of
// simulated channels. The distribution is what path finding is implicitly
// trying to predict, so scenario corpora should mix several models to avoid
// optimizing for a single liquidity regime.
type LiquidityModel string

const (
	// LiquidityHalf splits every channel 50/50. The easiest regime: any
	// payment below half the capacity succeeds.
	LiquidityHalf LiquidityModel = "half"

	// LiquidityUniform draws the node1 balance uniformly from the channel
	// capacity.
	LiquidityUniform LiquidityModel = "uniform"

	// LiquidityBimodal places nearly all of the capacity on one side of
	// the channel, chosen at random. This mirrors the empirical
	// observation that depleted channels dominate on the real network and
	// is the hardest regime.
	LiquidityBimodal LiquidityModel = "bimodal"
)

// Besides the three names above, AssignLiquidity accepts parameterized model
// strings so that a scenario file can request a distribution family that the
// evolved routers were never trained against:
//
//	bimodal:<scale>     the bimodal shape with the exponential scale given
//	                    explicitly; "bimodal" is exactly "bimodal:0.05".
//	beta:<a>:<b>        the node1 fraction drawn from Beta(a, b); a and b
//	                    must both be positive. Beta(0.3, 0.3) is U shaped,
//	                    Beta(2, 2) is centered on an even split.
//	hubdrain:<scale>    the bimodal shape again, but the depleted end is
//	                    picked from the topology instead of a fair coin.
//
// The parameterized families exist for the robustness sweep: an evolved
// router whose constants merely fit the generator should lose ground as the
// generator moves underneath it.
const (
	// defaultBimodalScale is the exponential scale the legacy "bimodal"
	// model has always used. Nothing but the plain "bimodal" string may
	// depend on it, since every corpus regenerates from a fixed seed and
	// must keep producing the balances it produced before.
	defaultBimodalScale = 0.05

	// hubDrainProb is the probability that the depleted end of a channel
	// is the one facing the higher-degree of its two nodes.
	hubDrainProb = 0.85

	// betaMaxRejections caps the rejection loop of Jöhnk's algorithm. The
	// loop terminates with probability one, but its acceptance rate falls
	// off quickly as a and b grow, so a cap keeps a pathological
	// parameter choice from hanging the simulator.
	betaMaxRejections = 10_000
)

// liquidityKind enumerates the distribution families AssignLiquidity knows
// how to draw from. It is the parsed form of a LiquidityModel string.
type liquidityKind uint8

const (
	// liquidityKindHalf is the legacy "half" model.
	liquidityKindHalf liquidityKind = iota

	// liquidityKindUniform is the legacy "uniform" model.
	liquidityKindUniform

	// liquidityKindBimodal is the legacy "bimodal" model and its
	// parameterized "bimodal:<scale>" form.
	liquidityKindBimodal

	// liquidityKindBeta is the "beta:<a>:<b>" model.
	liquidityKindBeta

	// liquidityKindHubDrain is the "hubdrain:<scale>" model.
	liquidityKindHubDrain
)

// liquiditySpec is a liquidity model string after parsing: the family to draw
// from plus whatever parameters that family takes.
type liquiditySpec struct {
	// kind is the distribution family.
	kind liquidityKind

	// scale is the exponential scale of the bimodal and hubdrain
	// families, ignored by the others.
	scale float64

	// alphaParam and betaParam are the two shape parameters of the beta
	// family, ignored by the others.
	alphaParam, betaParam float64
}

// parseLiquidityModel turns a model string into the family and parameters it
// names. A malformed string is an error rather than a panic, and the whole
// string is parsed before any balance is touched so that a bad scenario file
// cannot leave the graph half assigned.
func parseLiquidityModel(model LiquidityModel) (liquiditySpec, error) {
	// The three legacy names take no parameters and must keep drawing
	// exactly what they always drew.
	switch model {
	case LiquidityHalf:
		return liquiditySpec{kind: liquidityKindHalf}, nil

	case LiquidityUniform:
		return liquiditySpec{kind: liquidityKindUniform}, nil

	case LiquidityBimodal:
		return liquiditySpec{
			kind:  liquidityKindBimodal,
			scale: defaultBimodalScale,
		}, nil
	}

	fields := strings.Split(string(model), ":")

	// parsePositive parses one colon separated field as a float that must
	// be strictly positive and finite.
	parsePositive := func(name, field string) (float64, error) {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return 0, fmt.Errorf("liquidity model %v: invalid %v "+
				"%q: %w", model, name, field, err)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
			return 0, fmt.Errorf("liquidity model %v: %v must be "+
				"a positive finite number, got %q", model,
				name, field)
		}

		return value, nil
	}

	switch fields[0] {
	case string(LiquidityBimodal), "hubdrain":
		if len(fields) != 2 {
			return liquiditySpec{}, fmt.Errorf("liquidity model "+
				"%v: expected %v:<scale>", model, fields[0])
		}

		scale, err := parsePositive("scale", fields[1])
		if err != nil {
			return liquiditySpec{}, err
		}

		kind := liquidityKindBimodal
		if fields[0] == "hubdrain" {
			kind = liquidityKindHubDrain
		}

		return liquiditySpec{kind: kind, scale: scale}, nil

	case "beta":
		if len(fields) != 3 {
			return liquiditySpec{}, fmt.Errorf("liquidity model "+
				"%v: expected beta:<a>:<b>", model)
		}

		alphaParam, err := parsePositive("a", fields[1])
		if err != nil {
			return liquiditySpec{}, err
		}
		betaParam, err := parsePositive("b", fields[2])
		if err != nil {
			return liquiditySpec{}, err
		}

		return liquiditySpec{
			kind:       liquidityKindBeta,
			alphaParam: alphaParam,
			betaParam:  betaParam,
		}, nil
	}

	return liquiditySpec{}, fmt.Errorf("unknown liquidity model %v", model)
}

// betaSample draws from a Beta(a, b) distribution using Jöhnk's algorithm: it
// draws U and V uniformly, raises them to 1/a and 1/b, and accepts the pair
// when X+Y <= 1, at which point X/(X+Y) is Beta(a, b) distributed. The number
// of iterations varies, but every draw comes from the passed rng, so a given
// (model, seed) pair still produces one fixed sequence of balances.
//
// Acceptance becomes rare for large a and b, so after betaMaxRejections
// rejections the function gives up and normalizes the last pair it drew.
// That fallback is NOT Beta distributed: it is a same-support stand-in that
// keeps the simulator moving, and the families the sweep actually uses
// (a, b <= 2) reach it with probability far below any rate that could shift a
// corpus.
func betaSample(rng *rand.Rand, a, b float64) float64 {
	var x, y float64
	for i := 0; i < betaMaxRejections; i++ {
		x = math.Pow(rng.Float64(), 1/a)
		y = math.Pow(rng.Float64(), 1/b)

		if sum := x + y; sum <= 1 && sum > 0 {
			return x / sum
		}
	}

	// The rejection loop gave up. Normalize the last pair rather than
	// returning a degenerate value.
	if sum := x + y; sum > 0 {
		return x / sum
	}

	return 0.5
}

// degree returns the number of channels the given node is a party to, the
// topological signal the hubdrain model correlates its depleted ends with.
func (g *SimGraph) degree(v route.Vertex) int {
	node, ok := g.nodes[v]
	if !ok {
		return 0
	}

	return len(node.channels)
}

// AssignLiquidity redistributes the hidden balances of all channels in the
// graph according to the given model, deterministically derived from the
// seed. Existing balances are overwritten. The model may be one of the three
// legacy names or a parameterized family; see the LiquidityModel constants
// for the full grammar.
func (g *SimGraph) AssignLiquidity(model LiquidityModel, seed int64) error {
	spec, err := parseLiquidityModel(model)
	if err != nil {
		return err
	}

	rng := rand.New(rand.NewSource(seed))

	// Iterate channels in a deterministic order so that a given seed
	// always produces the same assignment regardless of map iteration.
	ids := make([]uint64, 0, len(g.channels))
	for id := range g.channels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		channel := g.channels[id]
		capacityMsat := lnwire.NewMSatFromSatoshis(channel.Capacity)

		var node1Balance lnwire.MilliSatoshi
		switch spec.kind {
		case liquidityKindHalf:
			node1Balance = capacityMsat / 2

		case liquidityKindUniform:
			node1Balance = lnwire.MilliSatoshi(
				rng.Int63n(int64(capacityMsat) + 1),
			)

		case liquidityKindBimodal:
			// Draw from an exponential-like distribution hugging
			// one of the two ends, mirroring the bimodal
			// liquidity hypothesis the bimodal estimator is built
			// on. The plain "bimodal" model passes the historical
			// 0.05 scale here, so its draws are unchanged.
			frac := rng.ExpFloat64() * spec.scale
			if frac > 1 {
				frac = 1
			}
			node1Balance = lnwire.MilliSatoshi(
				frac * float64(capacityMsat),
			)
			if rng.Intn(2) == 0 {
				node1Balance = capacityMsat - node1Balance
			}

		case liquidityKindBeta:
			frac := betaSample(
				rng, spec.alphaParam, spec.betaParam,
			)
			node1Balance = lnwire.MilliSatoshi(
				frac * float64(capacityMsat),
			)

		case liquidityKindHubDrain:
			// The magnitude is the bimodal draw, but the side is
			// not a fair coin: the depleted end is the one the
			// higher-degree node owns, with probability
			// hubDrainProb. That models liquidity draining away
			// from a hub on the hub's own side of the channel,
			// which is what a hub that sources more traffic than
			// it sinks ends up looking like.
			frac := rng.ExpFloat64() * spec.scale
			if frac > 1 {
				frac = 1
			}
			node1Balance = lnwire.MilliSatoshi(
				frac * float64(capacityMsat),
			)

			deg1 := g.degree(channel.ends[0].owner)
			deg2 := g.degree(channel.ends[1].owner)

			// depleteNode1 says whether the small side computed
			// above stays with node1. Ties fall back to the fair
			// coin, since neither end is the hub.
			var depleteNode1 bool
			switch {
			case deg1 == deg2:
				depleteNode1 = rng.Intn(2) == 0

			case deg1 > deg2:
				depleteNode1 = rng.Float64() < hubDrainProb

			default:
				depleteNode1 = rng.Float64() >= hubDrainProb
			}

			if !depleteNode1 {
				node1Balance = capacityMsat - node1Balance
			}

		default:
			return fmt.Errorf("unknown liquidity model %v", model)
		}

		channel.ends[0].balance = node1Balance
		channel.ends[1].balance = capacityMsat - node1Balance
	}

	return nil
}

// BalanceNodeChannels resets all channels of the given node to a 50/50
// split. Applied to the payment source after AssignLiquidity, it models a
// sender that manages its own outbound liquidity, so that scenario failures
// reflect routing difficulty rather than an underfunded sender.
func (g *SimGraph) BalanceNodeChannels(v route.Vertex) error {
	node, ok := g.nodes[v]
	if !ok {
		return fmt.Errorf("unknown node %v", v)
	}

	for _, channel := range node.channels {
		capacityMsat := lnwire.NewMSatFromSatoshis(channel.Capacity)
		channel.ends[0].balance = capacityMsat / 2
		channel.ends[1].balance = capacityMsat / 2
	}

	return nil
}

// LiquiditySnapshot records the hidden balance of every channel end so
// that a later call to RestoreLiquidity can put the network back exactly
// as it was.
type LiquiditySnapshot struct {
	balances map[uint64][2]lnwire.MilliSatoshi
}

// SnapshotLiquidity captures the current hidden balances.
//
// The warmup phase of a scenario runs real payments, so it teaches a
// router about the network and drains that network at the same time.
// Those two effects answer different questions: a served weight cache
// hands a fresh node knowledge without also having spent the liquidity
// that knowledge describes. Snapshotting before the warmup and
// restoring after it isolates the value of the knowledge alone.
func (g *SimGraph) SnapshotLiquidity() *LiquiditySnapshot {
	snap := &LiquiditySnapshot{
		balances: make(map[uint64][2]lnwire.MilliSatoshi, len(g.channels)),
	}
	for id, channel := range g.channels {
		snap.balances[id] = [2]lnwire.MilliSatoshi{
			channel.ends[0].balance, channel.ends[1].balance,
		}
	}

	return snap
}

// RestoreLiquidity puts the hidden balances back to a snapshot. Any
// outstanding holds are cleared as well, since a restored network has no
// payment in flight over it.
func (g *SimGraph) RestoreLiquidity(snap *LiquiditySnapshot) {
	if snap == nil {
		return
	}

	for id, balances := range snap.balances {
		channel, ok := g.channels[id]
		if !ok {
			continue
		}
		channel.ends[0].balance = balances[0]
		channel.ends[1].balance = balances[1]
		channel.ends[0].held = 0
		channel.ends[1].held = 0
	}

	g.holds = make(map[uint64][]balanceMove)
}
