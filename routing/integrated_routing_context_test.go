package routing

import (
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// integratedRoutingContext defines the context in which integrated routing
// tests run.
type integratedRoutingContext struct {
	graph *mockGraph
	t     *testing.T

	source *mockNode
	target *mockNode

	amt         lnwire.MilliSatoshi
	finalExpiry int32

	mcCfg          MissionControlConfig
	pathFindingCfg PathFindingConfig
}

// newIntegratedRoutingContext instantiates a new integrated routing test
// context with a source and a target node.
func newIntegratedRoutingContext(t *testing.T) *integratedRoutingContext {
	// Instantiate a mock graph.
	source := newMockNode()
	target := newMockNode()

	graph := newMockGraph(t)
	graph.addNode(source)
	graph.addNode(target)
	graph.source = source

	// Initiate the test context with a set of default configuration values.
	// We don't use the lnd defaults here, because otherwise changing the
	// defaults would break the unit tests. The actual values picked aren't
	// critical to excite certain behavior, but do need to be aligned with
	// the test case assertions.
	ctx := integratedRoutingContext{
		t:           t,
		graph:       graph,
		amt:         100000,
		finalExpiry: 40,

		mcCfg: MissionControlConfig{
			PenaltyHalfLife:       30 * time.Minute,
			AprioriHopProbability: 0.6,
			AprioriWeight:         0.5,
			SelfNode:              source.pubkey,
		},

		pathFindingCfg: PathFindingConfig{
			PaymentAttemptPenalty: 1000,
		},

		source: source,
		target: target,
	}

	return &ctx
}

// testPayment launches a test payment and asserts that it is completed after
// the expected number of attempts.
func (c *integratedRoutingContext) testPayment(expectedNofAttempts int) {
	var nextPid uint64

	// Create temporary database for mission control.
	file, err := ioutil.TempFile("", "*.db")
	if err != nil {
		c.t.Fatal(err)
	}

	dbPath := file.Name()
	defer os.Remove(dbPath)

	db, err := bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	defer db.Close()

	// Instantiate a new mission control with the current configuration
	// values.
	mc, err := NewMissionControl(db, &c.mcCfg)
	if err != nil {
		c.t.Fatal(err)
	}

	// Instruct pathfinding to use mission control as a probabiltiy source.
	restrictParams := RestrictParams{
		ProbabilitySource: mc.GetProbability,
		FeeLimit:          lnwire.MaxMilliSatoshi,
	}

	// Now the payment control loop starts. It will keep trying routes until
	// the payment succeeds.
	for {
		// Create bandwidth hints based on local channel balances.
		bandwidthHints := map[uint64]lnwire.MilliSatoshi{}
		for _, ch := range c.graph.nodes[c.source.pubkey].channels {
			bandwidthHints[ch.id] = ch.balance
		}

		// Find a route.
		path, err := findPathInternal(
			nil, bandwidthHints, c.graph,
			&restrictParams,
			&c.pathFindingCfg,
			c.source.pubkey, c.target.pubkey,
			c.amt, c.finalExpiry,
		)
		if err != nil {
			c.t.Fatal(err)
		}

		finalHop := finalHopParams{
			amt:       c.amt,
			cltvDelta: uint16(c.finalExpiry),
		}

		route, err := newRoute(c.source.pubkey, path, 0, finalHop)
		if err != nil {
			c.t.Fatal(err)
		}

		// Send out the htlc on the mock graph.
		pid := nextPid
		nextPid++
		htlcResult, err := c.graph.sendHtlc(route)
		if err != nil {
			c.t.Fatal(err)
		}

		// Process the result.
		if htlcResult.failure == nil {
			err := mc.ReportPaymentSuccess(pid, route)
			if err != nil {
				c.t.Fatal(err)
			}

			// If the payment is successful, the control loop can be
			// broken out of.
			break
		}

		// Failure, update mission control and retry.
		c.t.Logf("fail: %v @ %v\n", htlcResult.failure, htlcResult.failureSource)

		finalResult, err := mc.ReportPaymentFail(
			pid, route,
			getNodeIndex(route, htlcResult.failureSource),
			htlcResult.failure,
		)
		if err != nil {
			c.t.Fatal(err)
		}

		if finalResult != nil {
			c.t.Logf("final result: %v\n", finalResult)
			break
		}
	}

	c.t.Logf("Payment attempts: %v\n", nextPid)
	if expectedNofAttempts != int(nextPid) {
		c.t.Fatalf("expected %v attempts, but needed %v",
			expectedNofAttempts, nextPid)
	}
}

// getNodeIndex returns the zero-based index of the given node in the route.
func getNodeIndex(route *route.Route, failureSource route.Vertex) *int {
	if failureSource == route.SourcePubKey {
		idx := 0
		return &idx
	}

	for i, h := range route.Hops {
		if h.PubKeyBytes == failureSource {
			idx := i + 1
			return &idx
		}
	}
	return nil
}
