package routing

import (
	"fmt"
	"math/rand"
	"sort"

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

// AssignLiquidity redistributes the hidden balances of all channels in the
// graph according to the given model, deterministically derived from the
// seed. Existing balances are overwritten.
func (g *SimGraph) AssignLiquidity(model LiquidityModel, seed int64) error {
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
		switch model {
		case LiquidityHalf:
			node1Balance = capacityMsat / 2

		case LiquidityUniform:
			node1Balance = lnwire.MilliSatoshi(
				rng.Int63n(int64(capacityMsat) + 1),
			)

		case LiquidityBimodal:
			// Draw from an exponential-like distribution hugging
			// one of the two ends, mirroring the bimodal
			// liquidity hypothesis the bimodal estimator is built
			// on.
			frac := rng.ExpFloat64() * 0.05
			if frac > 1 {
				frac = 1
			}
			node1Balance = lnwire.MilliSatoshi(
				frac * float64(capacityMsat),
			)
			if rng.Intn(2) == 0 {
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
