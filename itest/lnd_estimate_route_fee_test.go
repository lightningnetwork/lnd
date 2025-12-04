package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

var (
	probeInitiator *node.HarnessNode
	probeAmount    = btcutil.Amount(100_000)
	probeAmt       = int64(probeAmount) * 1_000

	failureReasonNone    = lnrpc.PaymentFailureReason_FAILURE_REASON_NONE
	failureReasonNoRoute = lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE //nolint:ll
)

const (
	ErrNoRouteInGraph = "unable to find a path to destination"
)

type estimateRouteFeeTestCase struct {
	// name is the name of the target test case.
	name string

	// probing is a flag that indicates whether the test case estimates fees
	// using the graph or by probing.
	probing bool

	// destination is the node that will receive the probe.
	destination *node.HarnessNode

	// routeHints are the route hints that will be used for the probe.
	routeHints []*lnrpc.RouteHint

	// expectedRoutingFeesMsat are the expected routing fees that will be
	// returned by the probe.
	expectedRoutingFeesMsat int64

	// expectedCltvDelta is the expected cltv delta that will be returned by
	// the fee estimation.
	expectedCltvDelta int64

	// expectedFailureReason is the expected payment failure reason that
	// will be returned by the probe.
	expectedFailureReason lnrpc.PaymentFailureReason

	// expectedError are the expected error that will be returned if the
	// probing fails.
	expectedError string
}

// testEstimateRouteFee tests the estimation of routing fees using either graph
// data or sending out a probe payment. This test validates graph-based fee
// estimation, probe-based fee estimation with single LSP, probe-based fee
// estimation with multiple route hints to same LSP (worst-case fee selection),
// probe-based fee estimation with multiple different public LSPs (worst-case
// fee selection across LSPs, up to MaxLspsToProbe), and non-LSP probing (all
// private destination hops).
//
// Note: We test with exactly MaxLspsToProbe (3) LSPs. Testing with more LSPs
// is not feasible because the LSP selection uses map iteration, which has
// non-deterministic order in Go, making it impossible to predict which LSPs
// will be probed.
func testEstimateRouteFee(ht *lntest.HarnessTest) {
	// Ensure MaxLspsToProbe is set to 3 as expected by this test. The test
	// uses exactly 3 LSPs in the multi-LSP test case. If MaxLspsToProbe
	// changes, this assertion will fail as a reminder to update the test.
	require.Equal(ht, 3, routerrpc.MaxLspsToProbe,
		"MaxLspsToProbe should be 3")

	mts := newMppTestScenario(ht)

	// We extend the regular mpp test scenario with two new nodes:
	// - Paula: connected to Bob and Eve through private channels
	// - Frank: connected to Dave through a private channel
	//
	//                  /-------------\
	//              _ Eve _  (private) \
	//             /       \            \
	// Alice -- Carol ---- Bob --------- Paula
	//      \              /   (private)
	//       \__ Dave ____/
	//             \
	//              \__ Frank (private)
	//
	req := &mppOpenChannelRequest{
		amtAliceCarol: 200_000,
		amtAliceDave:  200_000,
		amtCarolBob:   200_000,
		amtCarolEve:   200_000,
		amtDaveBob:    200_000,
		amtEveBob:     200_000,
	}
	mts.openChannels(req)
	chanPointDaveBob := mts.channelPoints[4]
	chanPointEveBob := mts.channelPoints[5]

	// Alice will initiate all probe payments.
	probeInitiator = mts.alice

	paula := ht.NewNode("Paula", nil)
	frank := ht.NewNode("Frank", nil)

	// The channel from Bob to Paula actually doesn't have enough liquidity
	// to carry out the probe. We assume in normal operation that hop hints
	// added to the invoice always have enough liquidity, but here we check
	// that the prober uses the more expensive route.
	ht.EnsureConnected(mts.bob, paula)
	ht.OpenChannel(mts.bob, paula, lntest.OpenChannelParams{
		Private: true,
		Amt:     90_000,
		PushAmt: 69_000,
	})

	ht.EnsureConnected(mts.eve, paula)
	ht.OpenChannel(mts.eve, paula, lntest.OpenChannelParams{
		Private: true,
		Amt:     1_000_000,
	})

	// Frank is a private node connected to Dave (public LSP).
	ht.EnsureConnected(mts.dave, frank)
	ht.OpenChannel(mts.dave, frank, lntest.OpenChannelParams{
		Private: true,
		Amt:     1_000_000,
	})

	bobsPrivChannels := mts.bob.RPC.ListChannels(&lnrpc.ListChannelsRequest{
		PrivateOnly: true,
	})
	require.Len(ht, bobsPrivChannels.Channels, 1)
	bobPaulaChanID := bobsPrivChannels.Channels[0].ChanId

	evesPrivChannels := mts.eve.RPC.ListChannels(&lnrpc.ListChannelsRequest{
		PrivateOnly: true,
	})
	require.Len(ht, evesPrivChannels.Channels, 1)
	evePaulaChanID := evesPrivChannels.Channels[0].ChanId

	davesPrivChannels := mts.dave.RPC.ListChannels(
		&lnrpc.ListChannelsRequest{
			PrivateOnly: true,
		},
	)
	require.Len(ht, davesPrivChannels.Channels, 1)
	daveFrankChanID := davesPrivChannels.Channels[0].ChanId

	// Let's disable the paths from Alice to Bob through Dave and Eve with
	// high fees. This ensures that the path estimates are based on Carol's
	// channel to Bob for the first set of tests.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      200_000,
		FeeRateMilliMsat: int64(0.001 * 1_000_000),
		TimeLockDelta:    40,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      133_650_000,
	}
	mts.dave.UpdateGlobalPolicy(expectedPolicy)
	ht.AssertChannelPolicyUpdate(
		mts.alice, mts.dave, expectedPolicy, chanPointDaveBob, false,
	)
	expectedPolicy.FeeBaseMsat = 500_000
	mts.eve.UpdateGlobalPolicy(expectedPolicy)
	ht.AssertChannelPolicyUpdate(
		mts.alice, mts.eve, expectedPolicy, chanPointEveBob, false,
	)

	var (
		bobHopHint = &lnrpc.HopHint{
			NodeId:                    mts.bob.PubKeyStr,
			FeeBaseMsat:               1_000,
			FeeProportionalMillionths: 1,
			CltvExpiryDelta:           100,
			ChanId:                    bobPaulaChanID,
		}

		bobExpHint = &lnrpc.HopHint{
			NodeId:                    mts.bob.PubKeyStr,
			FeeBaseMsat:               2000,
			FeeProportionalMillionths: 2000,
			CltvExpiryDelta:           80,
			ChanId:                    bobPaulaChanID,
		}

		eveHopHint = &lnrpc.HopHint{
			NodeId:                    mts.eve.PubKeyStr,
			FeeBaseMsat:               1_000_000,
			FeeProportionalMillionths: 1,
			CltvExpiryDelta:           200,
			ChanId:                    evePaulaChanID,
		}

		singleRouteHint = []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{
					bobHopHint,
				},
			},
		}

		lspDifferentFeesHints = []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{
					bobHopHint,
				},
			},
			{
				HopHints: []*lnrpc.HopHint{
					bobExpHint,
				},
			},
		}

		nonLspProbingRouteHints = []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{
					eveHopHint,
				},
			},
			{
				HopHints: []*lnrpc.HopHint{
					bobHopHint,
				},
			},
		}

		daveHopHint = &lnrpc.HopHint{
			NodeId:                    mts.dave.PubKeyStr,
			FeeBaseMsat:               3_000,
			FeeProportionalMillionths: 3_000,
			CltvExpiryDelta:           120,
			ChanId:                    daveFrankChanID,
		}

		// Multiple different public LSPs (Bob, Eve, Dave).
		multipleLspsRouteHints = []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{
					bobHopHint,
				},
			},
			{
				HopHints: []*lnrpc.HopHint{
					eveHopHint,
				},
			},
			{
				HopHints: []*lnrpc.HopHint{
					daveHopHint,
				},
			},
		}
	)

	defaultTimelock := int64(chainreg.DefaultBitcoinTimeLockDelta)

	// Going A -> Carol -> Bob
	feeStandardSingleHop := int64(1_000) + probeAmt/1_000_000

	// Going A -> Carol -> Bob -> Paula/no node
	feeBP := int64(bobHopHint.FeeBaseMsat) +
		int64(bobHopHint.FeeProportionalMillionths)*(probeAmt)/1_000_000
	deltaBP := int64(bobHopHint.CltvExpiryDelta)

	feeCB := int64(1_000) + (probeAmt+feeBP)/1_000_000
	deltaCB := defaultTimelock

	deltaACBP := deltaCB + deltaBP
	feeACBP := feeCB + feeBP

	// The expensive alternative to Bob.
	expFeeBP := int64(bobExpHint.FeeBaseMsat) +
		int64(bobExpHint.FeeProportionalMillionths)*(probeAmt)/1_000_000
	expFeeCB := int64(1_000) + (probeAmt+expFeeBP)/1_000_000
	expensiveFeeACBP := expFeeCB + expFeeBP

	// Going A -> Carol -> Eve -> Paula
	feeEP := int64(eveHopHint.FeeBaseMsat) +
		int64(eveHopHint.FeeProportionalMillionths)*(probeAmt)/1_000_000
	deltaEP := int64(eveHopHint.CltvExpiryDelta)

	feeCE := int64(1_000) + (probeAmt+feeEP)/1_000_000
	deltaCE := defaultTimelock

	feeACEP := feeEP + feeCE
	deltaACEP := deltaCE + deltaEP

	// For multiple LSPs test, the route with the highest fee should be
	// selected (Eve). Note that we return both fee and CLTV delta from
	// the same route (the highest-fee route), not the max fee and max
	// delta independently. This ensures the returned values represent an
	// actual viable route.
	highestFeeRouteFee := feeACEP
	highestFeeRouteDelta := deltaACEP

	initialBlockHeight := int64(mts.alice.RPC.GetInfo().BlockHeight)

	// Locktime is always composed of the initial block height and the
	// time lock delta for the first hop. Additionally, there's a block
	// accounting for block height variance.
	locktime := initialBlockHeight + defaultTimelock +
		int64(routing.BlockPadding)

	noChanNode := ht.NewNode("ImWithoutChannels", nil)

	var testCases = []*estimateRouteFeeTestCase{
		// Single hop payment is free.
		{
			name:                    "graph based estimate, 0 fee",
			probing:                 false,
			destination:             mts.dave,
			expectedRoutingFeesMsat: 0,
			expectedCltvDelta:       locktime,
			expectedFailureReason:   failureReasonNone,
		},
		// 1000 msat base fee + 100 msat(1ppm*100_000sat)
		{
			name:                    "graph based estimate",
			probing:                 false,
			destination:             mts.bob,
			expectedRoutingFeesMsat: feeStandardSingleHop,
			expectedCltvDelta:       locktime + deltaCB,
			expectedFailureReason:   failureReasonNone,
		},
		// We expect the same result as the graph based estimate to Bob.
		{
			name: "probe based estimate, empty " +
				"route hint",
			probing:                 true,
			destination:             mts.bob,
			routeHints:              []*lnrpc.RouteHint{},
			expectedRoutingFeesMsat: feeStandardSingleHop,
			expectedCltvDelta:       locktime + deltaCB,
			expectedFailureReason:   failureReasonNone,
		},
		// Rule 1: Invoice target is public (Bob), even with public
		// destination hop hints. Should route directly to Bob, NOT
		// treat as LSP.
		{
			name: "probe based estimate, public " +
				"target with public hop hints",
			probing:                 true,
			destination:             mts.bob,
			routeHints:              singleRouteHint,
			expectedRoutingFeesMsat: feeStandardSingleHop,
			expectedCltvDelta:       locktime + deltaCB,
			expectedFailureReason:   failureReasonNone,
		},
		// We expect the previous probing results adjusted by Paula's
		// hop data.
		{
			name: "probe based estimate, single" +
				" route hint",
			probing:                 true,
			destination:             paula,
			routeHints:              singleRouteHint,
			expectedRoutingFeesMsat: feeACBP,
			expectedCltvDelta:       locktime + deltaACBP,
			expectedFailureReason:   failureReasonNone,
		},
		// With multiple route hints and lsp detected, we expect the
		// highest of fee settings to be used for estimation.
		{
			name: "probe based estimate, " +
				"multiple route hints, diff lsp fees",
			probing:                 true,
			destination:             paula,
			routeHints:              lspDifferentFeesHints,
			expectedRoutingFeesMsat: expensiveFeeACBP,
			expectedCltvDelta:       locktime + deltaACBP,
			expectedFailureReason:   failureReasonNone,
		},
		// A destination without channels and an existing hop hint hop
		// should result in the same estimate as if the hidden node was
		// connected through a channel. This ensures that the probe is
		// actually just send to the LSP and not the destination.
		{
			name: "single hop hint, destination " +
				"without channels",
			probing:                 true,
			destination:             noChanNode,
			routeHints:              singleRouteHint,
			expectedRoutingFeesMsat: feeACBP,
			expectedCltvDelta:       locktime + deltaACBP,
			expectedFailureReason:   failureReasonNone,
		},
		// Test lnd native hop processing with non lsp probing. Paula is
		// lacking liqudity in the channel to Bob, so Eve is used, but
		// it has higher fees.
		{
			name: "probe based estimate, non " +
				"lsp",
			probing:                 true,
			destination:             paula,
			routeHints:              nonLspProbingRouteHints,
			expectedRoutingFeesMsat: feeACEP,
			expectedCltvDelta:       locktime + deltaACEP,
			expectedFailureReason:   failureReasonNone,
		},
		// We expect a NO_ROUTE error if route hints to paula aren't
		// provided while probing the graph.
		{
			name:          "no route via graph",
			probing:       false,
			destination:   paula,
			expectedError: ErrNoRouteInGraph,
		},
		// We expect a NO_ROUTE error if route hints to paula aren't
		// provided while sending a probe payment.
		{
			name:                    "no route via probe",
			probing:                 true,
			destination:             paula,
			expectedRoutingFeesMsat: 0,
			expectedCltvDelta:       0,
			expectedFailureReason:   failureReasonNoRoute,
		},
		// Test multiple different public LSPs. The worst-case (most
		// expensive) route should be returned. Eve has the highest
		// fees among the 3 LSPs tested. Note: We don't test with more
		// than MaxLspsToProbe LSPs because map iteration order in Go
		// is non-deterministic, making it impossible to predict which
		// LSPs will be selected for probing.
		{
			name: "probe based estimate, " +
				"multiple different public LSPs",
			probing:                 true,
			destination:             frank,
			routeHints:              multipleLspsRouteHints,
			expectedRoutingFeesMsat: highestFeeRouteFee,
			expectedCltvDelta: locktime +
				highestFeeRouteDelta,
			expectedFailureReason: failureReasonNone,
		},
	}

	for _, testCase := range testCases {
		success := ht.Run(
			testCase.name, func(tt *testing.T) {
				runFeeEstimationTestCase(ht, testCase)
			},
		)

		if !success {
			break
		}
	}
}

// runTestCase runs a single test case asserting that test conditions are met.
func runFeeEstimationTestCase(ht *lntest.HarnessTest,
	tc *estimateRouteFeeTestCase) {

	// Legacy graph based fee estimation.
	var feeReq *routerrpc.RouteFeeRequest
	if tc.probing {
		payReqs, _, _ := ht.CreatePayReqs(
			tc.destination, probeAmount, 1, tc.routeHints...,
		)
		feeReq = &routerrpc.RouteFeeRequest{
			PaymentRequest: payReqs[0],
			Timeout:        uint32(wait.PaymentTimeout.Seconds()),
		}
	} else {
		feeReq = &routerrpc.RouteFeeRequest{
			Dest:   tc.destination.PubKey[:],
			AmtSat: int64(probeAmount),
		}
	}

	ctx := ht.Context()

	// Kick off the parametrized fee estimation.
	resp, err := probeInitiator.RPC.Router.EstimateRouteFee(ctx, feeReq)
	if err != nil {
		require.ErrorContains(ht, err, tc.expectedError)

		return
	}

	require.Equal(ht, tc.expectedFailureReason, resp.FailureReason)
	require.Equal(
		ht, tc.expectedRoutingFeesMsat, resp.RoutingFeeMsat,
		"routing fees",
	)
	require.Equal(
		ht, tc.expectedCltvDelta, resp.TimeLockDelay,
		"cltv delta",
	)
}
