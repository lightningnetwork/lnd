package routing

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// SimTopologySpec describes a synthetic network topology to generate. These
// are used alongside real graph snapshots to diversify the scenario corpus.
type SimTopologySpec struct {
	// Type selects the generator: "line", "grid", "hubspoke",
	// "smallworld", "scalefree" or "corridors".
	Type string `json:"type"`

	// NumNodes is the number of nodes to generate.
	NumNodes int `json:"num_nodes"`

	// ChannelSizeSat is the capacity of every generated channel.
	ChannelSizeSat int64 `json:"channel_size_sat"`

	// Seed drives the randomized generators ("smallworld") and fee
	// assignment.
	Seed int64 `json:"seed"`

	// AvgDegree is the average node degree for the "smallworld"
	// generator.
	AvgDegree int `json:"avg_degree"`

	// Corridors is the number of parallel source to target corridors the
	// "corridors" generator builds. It is ignored by every other type.
	Corridors int `json:"corridors"`
}

// defaultSimPolicy returns a randomized but realistic routing policy drawn
// from the given rng: base fees around 0-1 sat and fee rates from 0 to 1000
// ppm.
func defaultSimPolicy(rng *rand.Rand) SimPolicy {
	return SimPolicy{
		BaseFeeMsat:   lnwire.MilliSatoshi(rng.Int63n(1001)),
		FeeRatePPM:    lnwire.MilliSatoshi(rng.Int63n(1001)),
		TimeLockDelta: 40,
		MinHTLCMsat:   1000,
	}
}

// GenerateSimGraph builds a synthetic graph from the given spec. Node ids
// start at 1; the caller addresses nodes via SimNodePubKey.
func GenerateSimGraph(spec *SimTopologySpec) (*SimGraph, error) {
	if spec.NumNodes < 2 {
		return nil, fmt.Errorf("need at least 2 nodes, got %v",
			spec.NumNodes)
	}

	g := NewSimGraph()
	rng := rand.New(rand.NewSource(spec.Seed))
	capacity := btcutil.Amount(spec.ChannelSizeSat)

	// Create all nodes up front.
	for i := 1; i <= spec.NumNodes; i++ {
		_, err := g.AddNode(
			SimNodePubKey(uint32(i)), fmt.Sprintf("node-%d", i),
		)
		if err != nil {
			return nil, err
		}
	}

	addChan := func(id uint64, a, b uint32) error {
		return g.AddChannel(
			id, SimNodePubKey(a), SimNodePubKey(b), capacity,
			defaultSimPolicy(rng), defaultSimPolicy(rng),
		)
	}

	var nextChanID uint64 = 1

	switch spec.Type {
	case "line":
		for i := 1; i < spec.NumNodes; i++ {
			err := addChan(
				nextChanID, uint32(i), uint32(i+1),
			)
			if err != nil {
				return nil, err
			}
			nextChanID++
		}

	case "grid":
		// Arrange nodes in a near-square grid with channels between
		// horizontal and vertical neighbors.
		side := 1
		for side*side < spec.NumNodes {
			side++
		}
		idx := func(row, col int) uint32 {
			return uint32(row*side + col + 1)
		}
		for row := 0; row < side; row++ {
			for col := 0; col < side; col++ {
				if int(idx(row, col)) > spec.NumNodes {
					continue
				}
				right := idx(row, col+1)
				if col+1 < side &&
					int(right) <= spec.NumNodes {

					err := addChan(
						nextChanID, idx(row, col),
						right,
					)
					if err != nil {
						return nil, err
					}
					nextChanID++
				}
				down := idx(row+1, col)
				if row+1 < side &&
					int(down) <= spec.NumNodes {

					err := addChan(
						nextChanID, idx(row, col),
						down,
					)
					if err != nil {
						return nil, err
					}
					nextChanID++
				}
			}
		}

	case "hubspoke":
		// A small set of densely connected hubs, each spoke connects
		// to one hub. Roughly 1 hub per 10 nodes.
		numHubs := spec.NumNodes / 10
		if numHubs < 2 {
			numHubs = 2
		}

		// Interconnect the hubs fully.
		for i := 1; i <= numHubs; i++ {
			for j := i + 1; j <= numHubs; j++ {
				err := addChan(
					nextChanID, uint32(i), uint32(j),
				)
				if err != nil {
					return nil, err
				}
				nextChanID++
			}
		}

		// Attach every spoke to a random hub.
		for i := numHubs + 1; i <= spec.NumNodes; i++ {
			hub := uint32(rng.Intn(numHubs) + 1)
			err := addChan(nextChanID, uint32(i), hub)
			if err != nil {
				return nil, err
			}
			nextChanID++
		}

	case "smallworld":
		// A ring lattice with random long-range shortcuts, the
		// classic Watts-Strogatz-like construction.
		degree := spec.AvgDegree
		if degree < 2 {
			degree = 4
		}

		// Ring lattice: each node connects to degree/2 clockwise
		// neighbors.
		for i := 1; i <= spec.NumNodes; i++ {
			for k := 1; k <= degree/2; k++ {
				j := i + k
				if j > spec.NumNodes {
					j -= spec.NumNodes
				}
				if i == j {
					continue
				}
				err := addChan(
					nextChanID, uint32(i), uint32(j),
				)
				if err != nil {
					// Duplicate channels can occur for
					// tiny rings; skip them.
					continue
				}
				nextChanID++
			}
		}

		// Shortcuts: one long-range link per ~5 nodes.
		for i := 0; i < spec.NumNodes/5; i++ {
			a := uint32(rng.Intn(spec.NumNodes) + 1)
			b := uint32(rng.Intn(spec.NumNodes) + 1)
			if a == b {
				continue
			}
			if err := addChan(nextChanID, a, b); err != nil {
				continue
			}
			nextChanID++
		}

	case "scalefree":
		// Barabási-Albert preferential attachment: each new node
		// attaches to m existing nodes with probability proportional
		// to their degree, yielding the hub-dominated, heavy-tailed
		// degree distribution of the real Lightning graph. Channel
		// capacities are log-normal around ChannelSizeSat, so hubs
		// accumulate both many and large channels, like mainnet.
		m := spec.AvgDegree / 2
		if m < 2 {
			m = 2
		}

		// Repeated-nodes list: each channel endpoint appears once
		// per attached channel, so sampling uniformly from it is
		// degree-proportional sampling.
		var endpoints []uint32

		// Seed clique among the first m+1 nodes.
		for i := 1; i <= m && i+1 <= spec.NumNodes; i++ {
			for j := i + 1; j <= m+1; j++ {
				capacity := lognormalCapacity(
					rng, spec.ChannelSizeSat,
				)
				err := g.AddChannel(
					nextChanID, SimNodePubKey(uint32(i)),
					SimNodePubKey(uint32(j)), capacity,
					defaultSimPolicy(rng),
					defaultSimPolicy(rng),
				)
				if err != nil {
					return nil, err
				}
				nextChanID++
				endpoints = append(
					endpoints, uint32(i), uint32(j),
				)
			}
		}

		for i := m + 2; i <= spec.NumNodes; i++ {
			attached := make(map[uint32]bool)
			for len(attached) < m {
				var peer uint32
				if len(endpoints) == 0 {
					peer = uint32(rng.Intn(i-1) + 1)
				} else {
					peer = endpoints[rng.Intn(
						len(endpoints),
					)]
				}
				if peer == uint32(i) || attached[peer] {
					continue
				}
				attached[peer] = true

				capacity := lognormalCapacity(
					rng, spec.ChannelSizeSat,
				)
				err := addChanCap(
					g, nextChanID, uint32(i), peer,
					capacity, rng,
				)
				if err != nil {
					return nil, err
				}
				nextChanID++
				endpoints = append(
					endpoints, uint32(i), peer,
				)
			}
		}

	case "corridors":
		// The splitting-pressure topology (exp-010): parallel corridors
		// of deliberately unequal capacity, where a large payment can
		// only complete as an unequal split across several of them.
		if err := addCorridors(g, spec, rng, nextChanID); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown topology type %q", spec.Type)
	}

	return g, nil
}

// corridorBottleneckRatio is how much fatter a corridor's interior channels
// are than the tier channel that terminates the corridor at the target. The
// interior is fat enough that even a badly depleted interior channel usually
// still carries a full tier shard, so the tier channel is what actually limits
// a corridor: its capacity is a hard ceiling on everything the corridor can
// ever deliver, since nothing refills it while a payment runs.
//
// NOTE: simulation/gen_scenarios.py mirrors this constant to size splitting
// pressure payments; the two must move together.
const corridorBottleneckRatio = 128

// corridorTierJitterFrac is the largest downward jitter applied to a
// corridor's nominal tier capacity. Jitter keeps a router from reading a clean
// geometric ladder off the graph, and jittering only downward keeps the
// nominal tier an upper bound, which is what scenario generators rely on when
// they size a payment above every single corridor.
const corridorTierJitterFrac = 0.15

// corridorMinTierSat is the smallest tier capacity a corridors spec may
// generate. It sits above lnd's default minimum shard amount so that even the
// thinnest corridor can carry a shard the production splitter is willing to
// send.
const corridorMinTierSat = 20_000

// corridorFattestWeight is the capacity weight of the first corridor, the one
// fat corridor every corridors network has. No other corridor ever reaches it,
// which is what makes divide and conquer splitting expensive here: halving a
// payment that exceeds the fattest tier yields shards only the fattest
// corridor can take, so a halving splitter has to keep halving.
const corridorFattestWeight = 12

// corridorTierLadder is the capacity weight of the corridors after the fat
// one, repeating for as many corridors as the spec asks for. The rungs are
// deliberately uneven, and every one of them is at most half the fat corridor,
// so an amount no single corridor can carry divides unequally across several
// of them: 70/20/10-like, never as a clean halving. The rungs also span a
// factor of three among themselves, which keeps the ladder asymmetric even at
// three corridors.
//
// NOTE: simulation/gen_scenarios.py mirrors this ladder; the two must move
// together.
var corridorTierLadder = []int{6, 3, 5, 2}

// corridorTierWeights returns the capacity weight of each of the given number
// of corridors, in corridor order. The fat corridor comes first, the ladder
// rungs after it.
func corridorTierWeights(numCorridors int) []int {
	weights := make([]int, numCorridors)
	for i := range weights {
		if i == 0 {
			weights[i] = corridorFattestWeight

			continue
		}

		weights[i] = corridorTierLadder[(i-1)%len(corridorTierLadder)]
	}

	return weights
}

// CorridorTierCapacities returns the nominal tier capacity of every corridor
// of a "corridors" spec, in corridor order. A tier is the hard ceiling on what
// its corridor can deliver, so scenario generators size payments above the
// largest tier (no single path can carry them) but below the sum of all tiers
// (a split across corridors can). The generated tiers are jittered down by up
// to corridorTierJitterFrac, so these values are upper bounds.
func CorridorTierCapacities(spec *SimTopologySpec) []btcutil.Amount {
	weights := corridorTierWeights(spec.Corridors)
	if len(weights) == 0 {
		return nil
	}

	tiers := make([]btcutil.Amount, len(weights))
	for i, weight := range weights {
		tiers[i] = btcutil.Amount(
			spec.ChannelSizeSat * int64(weight) /
				(int64(weights[0]) * corridorBottleneckRatio),
		)
	}

	return tiers
}

// addCorridors builds the splitting-pressure topology: node 1 is the payment
// source, node NumNodes is the target, and the two are joined by Corridors
// parallel corridors of two to four hops each. Every corridor terminates in a
// single tier channel into the target and the target has no other channels, so
// the largest tier is a hard ceiling on any single shard and the sum of the
// tiers is a hard ceiling on the whole payment. The source funds one fat
// channel into the head of every corridor, so a failure always reflects
// downstream corridor capacity rather than an underfunded sender.
//
// The remaining node budget becomes a filler cloud hanging off the corridor
// interiors: tiny, nearly free channels that make path finding non-trivial and
// tempt a fee-greedy router, but that no meaningful shard fits through. The
// filler never touches the target, which is what keeps the inbound ceiling
// exact.
func addCorridors(g *SimGraph, spec *SimTopologySpec, rng *rand.Rand,
	nextChanID uint64) error {

	numCorridors := spec.Corridors
	if numCorridors < 2 {
		return fmt.Errorf("corridors topology needs at least 2 "+
			"corridors, got %v", numCorridors)
	}

	weights := corridorTierWeights(numCorridors)
	tiers := CorridorTierCapacities(spec)

	minTier := tiers[0]
	for _, tier := range tiers {
		if tier < minTier {
			minTier = tier
		}
	}

	// The thinnest corridor still has to carry a shard that path finding
	// will consider, so reject specs whose channels are too small for the
	// bottleneck ratio to leave a usable tier.
	if minTier < corridorMinTierSat {
		return fmt.Errorf("channel_size_sat %v is too small for %v "+
			"corridors: thinnest tier is %v sat",
			spec.ChannelSizeSat, numCorridors, minTier)
	}

	source := uint32(1)
	target := uint32(spec.NumNodes)

	// Draw the corridor lengths up front so that the node budget can be
	// checked before any channel is added. A corridor of h hops needs h-1
	// interior nodes, on top of the source and the target.
	hops := make([]int, numCorridors)
	needed := 2
	for i := range hops {
		hops[i] = 2 + rng.Intn(3)
		needed += hops[i] - 1
	}
	if spec.NumNodes < needed {
		return fmt.Errorf("corridors topology needs at least %v "+
			"nodes for %v corridors, got %v", needed, numCorridors,
			spec.NumNodes)
	}

	// interior collects the interior nodes of every corridor so that the
	// filler cloud below can hang off them.
	interior := make([][]uint32, numCorridors)

	// nextNode hands out interior node ids, leaving node 1 as the source
	// and node NumNodes as the target.
	nextNode := uint32(2)

	for k := 0; k < numCorridors; k++ {
		// The corridor interior is a fixed multiple of the tier, so the
		// tier stays the only real constraint on the corridor.
		fatCap := tiers[k] * corridorBottleneckRatio

		// Jitter the tier down a little so the ladder is not exactly
		// geometric on the wire.
		tierCap := btcutil.Amount(float64(tiers[k]) * (1 -
			corridorTierJitterFrac*rng.Float64()))

		policy := func() SimPolicy {
			return corridorPolicy(rng, weights[k], weights[0])
		}

		prev := source
		for h := 0; h < hops[k]-1; h++ {
			node := nextNode
			nextNode++
			interior[k] = append(interior[k], node)

			err := g.AddChannel(
				nextChanID, SimNodePubKey(prev),
				SimNodePubKey(node), fatCap, policy(),
				policy(),
			)
			if err != nil {
				return err
			}
			nextChanID++

			prev = node
		}

		// The tier channel into the target: the bottleneck every shard
		// down this corridor has to cross.
		err := g.AddChannel(
			nextChanID, SimNodePubKey(prev), SimNodePubKey(target),
			tierCap, policy(), policy(),
		)
		if err != nil {
			return err
		}
		nextChanID++
	}

	// Filler channels are a small fraction of the thinnest tier, so a
	// shard that matters never fits through the cloud.
	fillerCap := minTier / 4

	// attachable holds every node the filler cloud may connect to: the
	// corridor interiors plus the filler nodes already placed. The source
	// and the target are deliberately absent, the source because it only
	// funds corridor heads and the target because its inbound capacity
	// must stay exactly the sum of the tiers.
	var attachable []uint32
	for _, nodes := range interior {
		attachable = append(attachable, nodes...)
	}

	// Cross-links between the interiors of neighboring corridors: they
	// multiply the number of routes path finding sees without adding any
	// capacity that could carry a shard.
	for k := 0; k+1 < numCorridors; k++ {
		a := interior[k][rng.Intn(len(interior[k]))]
		b := interior[k+1][rng.Intn(len(interior[k+1]))]

		err := g.AddChannel(
			nextChanID, SimNodePubKey(a), SimNodePubKey(b),
			fillerCap, corridorFillerPolicy(rng),
			corridorFillerPolicy(rng),
		)
		if err != nil {
			return err
		}
		nextChanID++
	}

	// Every leftover node joins the filler cloud, attaching to one or two
	// nodes already in it.
	for node := nextNode; node < target; node++ {
		links := 1 + rng.Intn(2)
		for l := 0; l < links; l++ {
			peer := attachable[rng.Intn(len(attachable))]
			if peer == node {
				continue
			}

			err := g.AddChannel(
				nextChanID, SimNodePubKey(node),
				SimNodePubKey(peer), fillerCap,
				corridorFillerPolicy(rng),
				corridorFillerPolicy(rng),
			)
			if err != nil {
				return err
			}
			nextChanID++
		}

		attachable = append(attachable, node)
	}

	// Finally give the source one cheap way into the cloud, so that the
	// noise paths are reachable without first traversing a corridor.
	if nextNode < target {
		err := g.AddChannel(
			nextChanID, SimNodePubKey(source),
			SimNodePubKey(nextNode), fillerCap,
			corridorFillerPolicy(rng), corridorFillerPolicy(rng),
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// corridorPolicy returns the routing policy of a corridor channel. Fee rates
// run with the corridor's tier, which makes the thin corridors the cheap ones:
// a router that simply minimizes fees is drawn to exactly the corridors that
// cannot carry a large shard.
func corridorPolicy(rng *rand.Rand, weight, maxWeight int) SimPolicy {
	feeRate := 50 + int64(weight)*450/int64(maxWeight) + rng.Int63n(50)

	return SimPolicy{
		BaseFeeMsat:   lnwire.MilliSatoshi(rng.Int63n(200)),
		FeeRatePPM:    lnwire.MilliSatoshi(feeRate),
		TimeLockDelta: 40,
		MinHTLCMsat:   1000,
	}
}

// corridorFillerPolicy returns the policy of a filler channel: nearly free, so
// that a fee-minimizing router prefers the noise cloud over the corridors it
// actually needs.
func corridorFillerPolicy(rng *rand.Rand) SimPolicy {
	return SimPolicy{
		BaseFeeMsat:   lnwire.MilliSatoshi(rng.Int63n(10)),
		FeeRatePPM:    lnwire.MilliSatoshi(rng.Int63n(10)),
		TimeLockDelta: 40,
		MinHTLCMsat:   1000,
	}
}

// lognormalCapacity draws a heavy-tailed channel capacity with the given
// median, clamped to [median/20, median*50], approximating mainnet's
// capacity distribution.
func lognormalCapacity(rng *rand.Rand, medianSat int64) btcutil.Amount {
	capacity := float64(medianSat) * math.Exp(rng.NormFloat64()*1.1)

	minCap := float64(medianSat) / 20
	maxCap := float64(medianSat) * 50
	if capacity < minCap {
		capacity = minCap
	}
	if capacity > maxCap {
		capacity = maxCap
	}

	return btcutil.Amount(capacity)
}

// addChanCap adds a channel with an explicit capacity and randomized
// policies.
func addChanCap(g *SimGraph, id uint64, a, b uint32,
	capacity btcutil.Amount, rng *rand.Rand) error {

	return g.AddChannel(
		id, SimNodePubKey(a), SimNodePubKey(b), capacity,
		defaultSimPolicy(rng), defaultSimPolicy(rng),
	)
}

// RandomNodePair picks a random distinct (source, target) pair from the
// graph, deterministically from the rng.
func (g *SimGraph) RandomNodePair(rng *rand.Rand) (route.Vertex,
	route.Vertex) {

	nodes := make([]route.Vertex, 0, len(g.nodes))
	for pubKey := range g.nodes {
		nodes = append(nodes, pubKey)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return bytes.Compare(nodes[i][:], nodes[j][:]) < 0
	})

	source := nodes[rng.Intn(len(nodes))]
	target := nodes[rng.Intn(len(nodes))]
	for target == source {
		target = nodes[rng.Intn(len(nodes))]
	}

	return source, target
}
