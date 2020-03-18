package routing

import (
	"testing"
)

// TestProbabilityExtrapolation tests that probabilities for tried channels are
// extrapolated to untried channels. This is a way to improve pathfinding
// success by steering away from bad nodes.
func TestProbabilityExtrapolation(t *testing.T) {
	ctx := newIntegratedRoutingContext(t)

	// Create the following network of nodes:
	// source -> expensiveNode (charges routing fee) -> target
	// source -> intermediate1 (free routing) -> intermediate(1-10) (free routing) -> target
	g := ctx.graph

	const expensiveNodeID = 3
	expensiveNode := newMockNode(expensiveNodeID)
	expensiveNode.baseFee = 10000
	g.addNode(expensiveNode)

	g.addChannel(100, sourceNodeID, expensiveNodeID, 100000)
	g.addChannel(101, targetNodeID, expensiveNodeID, 100000)

	const intermediate1NodeID = 4
	intermediate1 := newMockNode(intermediate1NodeID)
	g.addNode(intermediate1)
	g.addChannel(102, sourceNodeID, intermediate1NodeID, 100000)

	for i := 0; i < 10; i++ {
		imNodeID := byte(10 + i)
		imNode := newMockNode(imNodeID)
		g.addNode(imNode)
		g.addChannel(uint64(200+i), imNodeID, targetNodeID, 100000)
		g.addChannel(uint64(300+i), imNodeID, intermediate1NodeID, 100000)

		// The channels from intermediate1 all have insufficient balance.
		g.nodes[intermediate1.pubkey].channels[imNode.pubkey].balance = 0
	}

	// It is expected that pathfinding will try to explore the routes via
	// intermediate1 first, because those are free. But as failures happen,
	// the node probability of intermediate1 will go down in favor of the
	// paid route via expensiveNode.
	//
	// The exact number of attempts required is dependent on mission control
	// config. For this test, it would have been enough to only assert that
	// we are not trying all routes via intermediate1. However, we do assert
	// a specific number of attempts to safe-guard against accidental
	// modifications anywhere in the chain of components that is involved in
	// this test.
	attempts, err := ctx.testPayment()
	if err != nil {
		t.Fatalf("payment failed: %v", err)
	}
	if len(attempts) != 5 {
		t.Fatalf("expected 5 attempts, but needed %v", len(attempts))
	}

	// If we use a static value for the node probability (no extrapolation
	// of data from other channels), all ten bad channels will be tried
	// first before switching to the paid channel.
	ctx.mcCfg.AprioriWeight = 1
	attempts, err = ctx.testPayment()
	if err != nil {
		t.Fatalf("payment failed: %v", err)
	}
	if len(attempts) != 11 {
		t.Fatalf("expected 11 attempts, but needed %v", len(attempts))
	}
}
