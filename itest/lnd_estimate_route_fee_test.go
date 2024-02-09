package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

type estimateRouteFeeTestCase struct {
	// name is the name of the target test case.
	name string

	// probeInitiator is the node that will initiate the probe.
	probeInitiator *node.HarnessNode

	// probing is a flag that indicates whether the test case estimates fees
	// using the graph or by probing.
	probing bool

	// destination is the node that will receive the probe.
	destination *node.HarnessNode

	// amount is the amount that will be probed for.
	amount btcutil.Amount

	// routeHints are the route hints that will be used for the probe.
	routeHints []*lnrpc.RouteHint

	// expectedRoutingFeesMsat are the expected routing fees that will be
	// returned by the probe.
	expectedRoutingFeesMsat int64

	// expectedCltvDelta is the expected cltv delta that will be returned by
	// the fee estimation.
	expectedCltvDelta int64

	// expectedFailureReasons are the expected failure reasons that will be
	// returned by the probe.
	expectedFailureReasons lnrpc.PaymentFailureReason
}

// testEstimateRouteFee tests that we are able to successfully route a
// payment using multiple shards across different paths.
func testEstimateRouteFee(ht *lntest.HarnessTest) {
	mts := newMppTestScenario(ht)

	// Set up a network with three different paths Alice <-> Bob. Channel
	// capacities are set such that the payment can only succeed if (at
	// least) three paths are used.
	//                  /-------------\
	//              _ Eve _  (private) \
	//             /       \            \
	// Alice -- Carol ---- Bob --------- Paula
	//      \              /   (private)
	//       \__ Dave ____/
	//
	req := &mppOpenChannelRequest{
		amtAliceCarol: 235_000,
		amtAliceDave:  135_000,
		amtCarolBob:   135_000,
		amtCarolEve:   135_000,
		amtDaveBob:    135_000,
		amtEveBob:     135_000,
		amtBobPaula:   135_000,
	}
	mts.openChannels(req)
	chanPointDaveBob := mts.channelPoints[4]
	chanPointEveBob := mts.channelPoints[5]

	bobsPrivChannels := ht.Bob.RPC.ListChannels(&lnrpc.ListChannelsRequest{
		PrivateOnly: true,
	})
	require.Len(ht, bobsPrivChannels.Channels, 1)
	bobPaulaChanID := bobsPrivChannels.Channels[0].ChanId

	// Let's disable the paths from Alice to Bob through Dave and Eve with
	// high fees. This ensures that the path estimates are based on Carol's
	// channel to Bob for the first set of tests.
	// In a second test we will dry up liquidity on Carol's channel to Bob
	// and then expect the fee estimation to use Dave's path to Bob since
	// Dave is 300 sats cheaper than Eve.
	expectedPolicy := mts.dave.UpdateGlobalPolicy(
		200_000, 0.001, 133_650_000,
	)
	ht.AssertChannelPolicyUpdate(
		mts.alice, mts.dave, expectedPolicy, chanPointDaveBob, false,
	)
	expectedPolicy = mts.eve.UpdateGlobalPolicy(500_000, 0.001, 133_650_000)
	ht.AssertChannelPolicyUpdate(
		mts.alice, mts.eve, expectedPolicy, chanPointEveBob, false,
	)

	ht.EnsureConnected(mts.eve, mts.paula)
	ht.OpenChannel(mts.eve, mts.paula, lntest.OpenChannelParams{
		Private: true,
		Amt:     1_000_000,
	})

	//nolint:lll
	var (
		probeAmount     = btcutil.Amount(100_000)
		probeAmountMsat = int64(probeAmount) * 1_000

		bobHopHint = &lnrpc.HopHint{
			NodeId:                    mts.bob.PubKeyStr,
			FeeBaseMsat:               1_000,
			FeeProportionalMillionths: 1,
			CltvExpiryDelta:           144,
			ChanId:                    bobPaulaChanID,
		}

		bobHopHintLowCltv = &lnrpc.HopHint{
			NodeId:                    mts.bob.PubKeyStr,
			FeeBaseMsat:               1_000,
			FeeProportionalMillionths: 1,
			CltvExpiryDelta:           9,
			ChanId:                    bobPaulaChanID,
		}

		bobExpensiveBaseFeeMsat = int64(2_000)
		bobExpensiveFeeRatePpm  = int64(2_000)
		bobExpensiveHopHint     = &lnrpc.HopHint{
			NodeId:                    mts.bob.PubKeyStr,
			FeeBaseMsat:               uint32(bobExpensiveBaseFeeMsat),
			FeeProportionalMillionths: uint32(bobExpensiveFeeRatePpm),
			CltvExpiryDelta:           144,
			ChanId:                    bobPaulaChanID,
		}

		singleRouteHint = []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{
					bobHopHint,
				},
			},
		}
		singleRouteHintLowCltv = []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{
					bobHopHintLowCltv,
				},
			},
		}
		singleLastHopFeeMsat      = probeAmountMsat*1/1_000_000 + 1_000
		singleFeeToBobMsat        = (singleLastHopFeeMsat+probeAmountMsat)/1_000_000 + 1_000
		expectedSingleHintFeeMsat = singleFeeToBobMsat + singleLastHopFeeMsat

		eveHopHint = &lnrpc.HopHint{
			NodeId:                    mts.eve.PubKeyStr,
			FeeBaseMsat:               1_000_000,
			FeeProportionalMillionths: 1,
			CltvExpiryDelta:           288,
		}
		nonLspLastHopFeeMsat      = probeAmountMsat*1/1_000_000 + 1_000_000
		singleFeeToEveMsat        = (nonLspLastHopFeeMsat+probeAmountMsat)/1_000_000 + 1_000
		expectedNonLspHintFeeMsat = singleFeeToEveMsat + nonLspLastHopFeeMsat

		nonLspProbingRouteHints = []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{
					bobHopHint, eveHopHint,
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
					bobExpensiveHopHint,
				},
			},
		}
		lspDiffLastHopFeeMsat      = probeAmountMsat*bobExpensiveFeeRatePpm/1_000_000 + bobExpensiveBaseFeeMsat
		lspDiffFeeToBobMsat        = (lspDiffLastHopFeeMsat+probeAmountMsat)/1_000_000 + 1000
		diffHopFeeMultiHintFeeMsat = lspDiffFeeToBobMsat + lspDiffLastHopFeeMsat
	)

	initialBlockHeight := int64(mts.alice.RPC.GetInfo().BlockHeight)

	failureReasonNone := lnrpc.PaymentFailureReason_FAILURE_REASON_NONE
	defaultTimelock := int64(chainreg.DefaultBitcoinTimeLockDelta)
	var testCases = []*estimateRouteFeeTestCase{
		// 1000 msat base fee + 100 msat(1ppm*100_000sat)
		{
			name:                    "graph based estimate",
			probing:                 false,
			probeInitiator:          mts.alice,
			destination:             mts.bob,
			amount:                  probeAmount,
			expectedRoutingFeesMsat: int64(1100),
			expectedCltvDelta: initialBlockHeight +
				2*defaultTimelock +
				int64(routing.BlockPadding),
			expectedFailureReasons: failureReasonNone,
		},
		// single hop payment is free.
		{
			name:                    "graph based estimate, 0 fee",
			probing:                 false,
			probeInitiator:          mts.alice,
			destination:             mts.dave,
			amount:                  probeAmount,
			expectedRoutingFeesMsat: int64(0),
			expectedCltvDelta: initialBlockHeight +
				defaultTimelock +
				int64(routing.BlockPadding),
			expectedFailureReasons: failureReasonNone,
		},
		// we expect the same result as the graph based estimate to Bob.
		{
			name: "probe based estimate, empty " +
				"route hint",
			probing:                 true,
			probeInitiator:          mts.alice,
			destination:             mts.bob,
			amount:                  probeAmount,
			routeHints:              []*lnrpc.RouteHint{},
			expectedRoutingFeesMsat: int64(1100),
			expectedCltvDelta: initialBlockHeight +
				2*defaultTimelock +
				int64(routing.BlockPadding),
			expectedFailureReasons: failureReasonNone,
		},
		// we expect the previous probing results adjusted by bob's hop
		// data.
		{
			name: "probe based estimate, single" +
				" route hint",
			probing:                 true,
			probeInitiator:          mts.alice,
			destination:             mts.paula,
			amount:                  probeAmount,
			routeHints:              singleRouteHint,
			expectedRoutingFeesMsat: expectedSingleHintFeeMsat,
			expectedCltvDelta: initialBlockHeight +
				2*defaultTimelock +
				144 + int64(routing.BlockPadding),
			expectedFailureReasons: failureReasonNone,
		},
		// we expect the previous probing results adjusted by bob's hop
		// data. However since the hop hint cltv is below the min cltv
		// delta, we expect the total delta to be adjusted by the set
		// minimum, 80 in this case.
		{
			name: "probe based estimate, single" +
				" route hint, low cltv",
			probing:                 true,
			probeInitiator:          mts.alice,
			destination:             mts.paula,
			amount:                  probeAmount,
			routeHints:              singleRouteHintLowCltv,
			expectedRoutingFeesMsat: expectedSingleHintFeeMsat,
			expectedCltvDelta: initialBlockHeight +
				3*defaultTimelock +
				int64(routing.BlockPadding),
			expectedFailureReasons: failureReasonNone,
		},
		// with multiple route hints and lsp detected, we expect the
		// highest of fee settings to be used for estimation.
		{
			name: "probe based estimate, " +
				"multiple route hints, diff lsp fees",
			probing:                 true,
			probeInitiator:          mts.alice,
			destination:             mts.paula,
			amount:                  probeAmount,
			routeHints:              lspDifferentFeesHints,
			expectedRoutingFeesMsat: diffHopFeeMultiHintFeeMsat,
			expectedCltvDelta: initialBlockHeight +
				2*defaultTimelock + 144 +
				int64(routing.BlockPadding),
			expectedFailureReasons: failureReasonNone,
		},
		// with multiple route hints and lsp detected, we expect the
		// highest of fee settings to be used for estimation.
		{
			name: "probe based estimate, " +
				"multiple route hints, diff lsp fees",
			probing:        true,
			probeInitiator: mts.alice,
			destination: ht.NewNode(
				"ImWithoutChannels", nil,
			),
			amount:                  probeAmount,
			routeHints:              singleRouteHint,
			expectedRoutingFeesMsat: expectedSingleHintFeeMsat,
			expectedCltvDelta: initialBlockHeight +
				2*defaultTimelock + 144 +
				int64(routing.BlockPadding),
			expectedFailureReasons: failureReasonNone,
		},
		// test lnd native hop processing with non lsp probing. Eve is
		// the more expensive hop to Paula, so we expect Eve's hop hint
		// fee.
		{
			name: "probe based estimate, non " +
				"lsp",
			probing:                 true,
			probeInitiator:          mts.alice,
			destination:             mts.paula,
			amount:                  probeAmount,
			routeHints:              nonLspProbingRouteHints,
			expectedRoutingFeesMsat: expectedNonLspHintFeeMsat,
			expectedCltvDelta: initialBlockHeight +
				+2*defaultTimelock + 288 +
				int64(routing.BlockPadding),
			expectedFailureReasons: failureReasonNone,
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

	mts.closeChannels()
}

// runTestCase runs a single test case asserting that test conditions are met.
func runFeeEstimationTestCase(ht *lntest.HarnessTest,
	tc *estimateRouteFeeTestCase) {

	// Legacy graph based fee estimation.
	var feeReq *routerrpc.RouteFeeRequest
	if tc.probing {
		payReqs, _, _ := ht.CreatePayReqs(
			tc.destination, tc.amount, 1, tc.routeHints...,
		)
		feeReq = &routerrpc.RouteFeeRequest{
			PaymentRequest: payReqs[0],
			Timeout:        10,
		}
	} else {
		feeReq = &routerrpc.RouteFeeRequest{
			Dest:   tc.destination.PubKey[:],
			AmtSat: int64(tc.amount),
		}
	}

	// Probe based fee estimation starts here.
	resp := tc.probeInitiator.RPC.EstimateRouteFee(feeReq)

	require.Equal(ht, tc.expectedRoutingFeesMsat, resp.RoutingFeeMsat)
	require.Equal(ht, tc.expectedCltvDelta, resp.TimeLockDelay)
	require.Equal(ht, tc.expectedFailureReasons, resp.FailureReason)
}
