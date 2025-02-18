package routing

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
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
	attempts, err := ctx.testPayment(1)
	require.NoError(t, err, "payment failed")
	if len(attempts) != 5 {
		t.Fatalf("expected 5 attempts, but needed %v", len(attempts))
	}

	// If we use a static value for the node probability (no extrapolation
	// of data from other channels), all ten bad channels will be tried
	// first before switching to the paid channel.
	estimator, ok := ctx.mcCfg.Estimator.(*AprioriEstimator)
	if ok {
		estimator.AprioriWeight = 1
	}
	attempts, err = ctx.testPayment(1)
	require.NoError(t, err, "payment failed")
	if len(attempts) != 11 {
		t.Fatalf("expected 11 attempts, but needed %v", len(attempts))
	}
}

type mppSendTestCase struct {
	name             string
	amt              btcutil.Amount
	expectedAttempts int

	// expectedSuccesses is a list of htlcs that made it to the receiver,
	// regardless of whether the final set became complete or not.
	expectedSuccesses []expectedHtlcSuccess

	graph           func(g *mockGraph)
	expectedFailure bool
	maxParts        uint32
	maxShardSize    btcutil.Amount
}

const (
	chanSourceIm1 = 13
	chanIm1Target = 32
	chanSourceIm2 = 14
	chanIm2Target = 42
)

func onePathGraph(g *mockGraph) {
	// Create the following network of nodes:
	// source -> intermediate1 -> target

	const im1NodeID = 3
	intermediate1 := newMockNode(im1NodeID)
	g.addNode(intermediate1)

	g.addChannel(chanSourceIm1, sourceNodeID, im1NodeID, 200000)
	g.addChannel(chanIm1Target, targetNodeID, im1NodeID, 100000)
}

func twoPathGraph(g *mockGraph, capacityOut, capacityIn btcutil.Amount) {
	// Create the following network of nodes:
	// source -> intermediate1 -> target
	// source -> intermediate2 -> target

	const im1NodeID = 3
	intermediate1 := newMockNode(im1NodeID)
	g.addNode(intermediate1)

	const im2NodeID = 4
	intermediate2 := newMockNode(im2NodeID)
	g.addNode(intermediate2)

	g.addChannel(chanSourceIm1, sourceNodeID, im1NodeID, capacityOut)
	g.addChannel(chanSourceIm2, sourceNodeID, im2NodeID, capacityOut)
	g.addChannel(chanIm1Target, targetNodeID, im1NodeID, capacityIn)
	g.addChannel(chanIm2Target, targetNodeID, im2NodeID, capacityIn)
}

var mppTestCases = []mppSendTestCase{
	// Test a two-path graph with sufficient liquidity. It is expected that
	// pathfinding will try first try to send the full amount via the two
	// available routes. When that fails, it will half the amount to 35k sat
	// and retry. That attempt reaches the target successfully. Then the
	// same route is tried again. Because the channel only had 50k sat, it
	// will fail. Finally the second route is tried for 35k and it succeeds
	// too. Mpp payment complete.
	{

		name: "sufficient inbound",
		graph: func(g *mockGraph) {
			twoPathGraph(g, 200000, 100000)
		},
		amt:              70000,
		expectedAttempts: 5,
		expectedSuccesses: []expectedHtlcSuccess{
			{
				amt:   35000,
				chans: []uint64{chanSourceIm1, chanIm1Target},
			},
			{
				amt:   35000,
				chans: []uint64{chanSourceIm2, chanIm2Target},
			},
		},
		maxParts: 1000,
	},

	// Test that a cap on the max htlcs makes it impossible to pay.
	{
		name: "no splitting",
		graph: func(g *mockGraph) {
			twoPathGraph(g, 200000, 100000)
		},
		amt:               70000,
		expectedAttempts:  2,
		expectedSuccesses: []expectedHtlcSuccess{},
		expectedFailure:   true,
		maxParts:          1,
	},

	// Test that an attempt is made to split the payment in multiple parts
	// that all use the same route if the full amount cannot be sent in a
	// single htlc. The sender is effectively probing the receiver's
	// incoming channel to see if it has sufficient balance. In this test
	// case, the endeavour fails.
	{

		name:             "one path split",
		graph:            onePathGraph,
		amt:              70000,
		expectedAttempts: 7,
		expectedSuccesses: []expectedHtlcSuccess{
			{
				amt:   35000,
				chans: []uint64{chanSourceIm1, chanIm1Target},
			},
			{
				amt:   8750,
				chans: []uint64{chanSourceIm1, chanIm1Target},
			},
		},
		expectedFailure: true,
		maxParts:        1000,
	},

	// Test that no attempts are made if the total local balance is
	// insufficient.
	{
		name: "insufficient total balance",
		graph: func(g *mockGraph) {
			twoPathGraph(g, 100000, 500000)
		},
		amt:              300000,
		expectedAttempts: 0,
		expectedFailure:  true,
		maxParts:         10,
	},

	// Test that if maxShardSize is set, then all attempts are below the
	// max shard size, yet still sum up to the total payment amount. A
	// payment of 30k satoshis with a max shard size of 10k satoshis should
	// produce 3 payments of 10k sats each.
	{
		name:             "max shard size clamping",
		graph:            onePathGraph,
		amt:              30_000,
		expectedAttempts: 3,
		expectedSuccesses: []expectedHtlcSuccess{
			{
				amt:   10_000,
				chans: []uint64{chanSourceIm1, chanIm1Target},
			},
			{
				amt:   10_000,
				chans: []uint64{chanSourceIm1, chanIm1Target},
			},
			{
				amt:   10_000,
				chans: []uint64{chanSourceIm1, chanIm1Target},
			},
		},
		maxParts:     1000,
		maxShardSize: 10_000,
	},
}

// TestBadFirstHopHint tests that a payment with a first hop hint with an
// invalid channel id still works since the node already knows its channel and
// doesn't need hints.
func TestBadFirstHopHint(t *testing.T) {
	t.Parallel()

	ctx := newIntegratedRoutingContext(t)

	onePathGraph(ctx.graph)

	sourcePubKey, _ := btcec.ParsePubKey(ctx.source.pubkey[:])

	hopHint := zpay32.HopHint{
		NodeID:                    sourcePubKey,
		ChannelID:                 66,
		FeeBaseMSat:               0,
		FeeProportionalMillionths: 0,
		CLTVExpiryDelta:           100,
	}

	ctx.routeHints = [][]zpay32.HopHint{{hopHint}}

	ctx.amt = lnwire.NewMSatFromSatoshis(100)
	_, err := ctx.testPayment(1)

	require.NoError(t, err, "payment failed")
}

// TestMppSend tests that a payment can be completed using multiple shards.
func TestMppSend(t *testing.T) {
	for _, testCase := range mppTestCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			testMppSend(t, &testCase)
		})
	}
}

func testMppSend(t *testing.T, testCase *mppSendTestCase) {
	ctx := newIntegratedRoutingContext(t)

	g := ctx.graph
	testCase.graph(g)

	ctx.amt = lnwire.NewMSatFromSatoshis(testCase.amt)

	if testCase.maxShardSize != 0 {
		shardAmt := lnwire.NewMSatFromSatoshis(testCase.maxShardSize)
		ctx.maxShardAmt = &shardAmt
	}

	attempts, err := ctx.testPayment(testCase.maxParts)
	switch {
	case err == nil && testCase.expectedFailure:
		t.Fatal("expected payment to fail")
	case err != nil && !testCase.expectedFailure:
		t.Fatalf("expected payment to succeed, got %v", err)
	}

	if len(attempts) != testCase.expectedAttempts {
		t.Fatalf("expected %v attempts, but needed %v",
			testCase.expectedAttempts, len(attempts),
		)
	}

	assertSuccessAttempts(t, attempts, testCase.expectedSuccesses)
}

// expectedHtlcSuccess describes an expected successful htlc attempt.
type expectedHtlcSuccess struct {
	amt   btcutil.Amount
	chans []uint64
}

// equals matches the expectation with an actual attempt.
func (e *expectedHtlcSuccess) equals(a htlcAttempt) bool {
	if a.route.TotalAmount !=
		lnwire.NewMSatFromSatoshis(e.amt) {

		return false
	}

	if len(a.route.Hops) != len(e.chans) {
		return false
	}

	for i, h := range a.route.Hops {
		if h.ChannelID != e.chans[i] {
			return false
		}
	}

	return true
}

// assertSuccessAttempts asserts that the set of successful htlc attempts
// matches the given expectation.
func assertSuccessAttempts(t *testing.T, attempts []htlcAttempt,
	expected []expectedHtlcSuccess) {

	successCount := 0
loop:
	for _, a := range attempts {
		if !a.success {
			continue
		}

		successCount++

		for _, exp := range expected {
			if exp.equals(a) {
				continue loop
			}
		}

		t.Fatalf("htlc success %v not found", a)
	}

	if successCount != len(expected) {
		t.Fatalf("expected %v successful htlcs, but got %v",
			expected, successCount)
	}
}

// TestPaymentAddrOnlyNoSplit tests that if the dest of a payment only has the
// payment addr feature bit set, then we won't attempt to split payments.
func TestPaymentAddrOnlyNoSplit(t *testing.T) {
	t.Parallel()

	// First, we'll create the routing context, then create a simple two
	// path graph where the sender has two paths to the destination.
	ctx := newIntegratedRoutingContext(t)

	// We'll have a basic graph with 2 mil sats of capacity, with 1 mil
	// sats available on either end.
	const chanSize = 2_000_000
	twoPathGraph(ctx.graph, chanSize, chanSize)

	payAddrOnlyFeatures := []lnwire.FeatureBit{
		lnwire.TLVOnionPayloadRequired,
		lnwire.PaymentAddrOptional,
	}

	// We'll make a payment of 1.5 mil satoshis our single chan sizes,
	// which should cause a split attempt _if_ we had MPP bits activated.
	// However, we only have the payment addr on, so we shouldn't split at
	// all.
	//
	// We'll set a non-zero value for max parts as well, which should be
	// ignored.
	const maxParts = 5
	ctx.amt = lnwire.NewMSatFromSatoshis(1_500_000)

	attempts, err := ctx.testPayment(maxParts, payAddrOnlyFeatures...)
	require.NotNil(
		t,
		err,
		fmt.Sprintf("expected path finding to fail instead made "+
			"attempts: %v", spew.Sdump(attempts)),
	)

	// The payment should have failed since we need to split in order to
	// route a payment to the destination, but they don't actually support
	// MPP.
	require.ErrorIs(t, err, errNoPathFound)
}
