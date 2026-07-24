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
	// Type selects the generator: "line", "grid", "hubspoke" or
	// "smallworld".
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

	default:
		return nil, fmt.Errorf("unknown topology type %q", spec.Type)
	}

	return g, nil
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
