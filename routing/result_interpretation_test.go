package routing

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	hops = []route.Vertex{
		{1, 0}, {1, 1}, {1, 2}, {1, 3}, {1, 4},
	}

	routeOneHop = extractMCRoute(&route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{
				PubKeyBytes:  hops[1],
				AmtToForward: 99,
			},
		},
	})

	routeTwoHop = extractMCRoute(&route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{
				PubKeyBytes:  hops[1],
				AmtToForward: 99,
			},
			{
				PubKeyBytes:  hops[2],
				AmtToForward: 97,
			},
		},
	})

	routeThreeHop = extractMCRoute(&route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{
				PubKeyBytes:  hops[1],
				AmtToForward: 99,
			},
			{
				PubKeyBytes:  hops[2],
				AmtToForward: 97,
			},
			{
				PubKeyBytes:  hops[3],
				AmtToForward: 94,
			},
		},
	})

	routeFourHop = extractMCRoute(&route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{
				PubKeyBytes:  hops[1],
				AmtToForward: 99,
			},
			{
				PubKeyBytes:  hops[2],
				AmtToForward: 97,
			},
			{
				PubKeyBytes:  hops[3],
				AmtToForward: 94,
			},
			{
				PubKeyBytes:  hops[4],
				AmtToForward: 90,
			},
		},
	})

	// blindedMultiHop is a blinded path where there are cleartext hops
	// before the introduction node, and an intermediate blinded hop before
	// the recipient after it.
	blindedMultiHop = extractMCRoute(&route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{
				PubKeyBytes:  hops[1],
				AmtToForward: 99,
			},
			{
				PubKeyBytes: hops[2],
				// Intermediate blinded hops don't have an
				// amount set.
				AmtToForward:  0,
				BlindingPoint: genTestPubKey(),
			},
			{
				PubKeyBytes: hops[3],
				// Intermediate blinded hops don't have an
				// amount set.
				AmtToForward: 0,
			},
			{
				PubKeyBytes:  hops[4],
				AmtToForward: 77,
			},
		},
	})

	// blindedSingleHop is a blinded path with a single blinded hop after
	// the introduction node.
	blindedSingleHop = extractMCRoute(&route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{
				PubKeyBytes:  hops[1],
				AmtToForward: 99,
			},
			{
				PubKeyBytes: hops[2],
				// Intermediate blinded hops don't have an
				// amount set.
				AmtToForward:  0,
				BlindingPoint: genTestPubKey(),
			},
			{
				PubKeyBytes:  hops[3],
				AmtToForward: 88,
			},
		},
	})

	// blindedMultiToIntroduction is a blinded path which goes directly
	// to the introduction node, with multiple blinded hops after it.
	blindedMultiToIntroduction = extractMCRoute(&route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{
				PubKeyBytes: hops[1],
				// Intermediate blinded hops don't have an
				// amount set.
				AmtToForward:  0,
				BlindingPoint: genTestPubKey(),
			},
			{
				PubKeyBytes: hops[2],
				// Intermediate blinded hops don't have an
				// amount set.
				AmtToForward: 0,
			},
			{
				PubKeyBytes:  hops[3],
				AmtToForward: 58,
			},
		},
	})

	// blindedIntroReceiver is a blinded path where the introduction node
	// is the recipient.
	blindedIntroReceiver = extractMCRoute(&route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{
				PubKeyBytes:  hops[1],
				AmtToForward: 95,
			},
			{
				PubKeyBytes:   hops[2],
				AmtToForward:  90,
				BlindingPoint: genTestPubKey(),
			},
		},
	})
)

func genTestPubKey() *btcec.PublicKey {
	key, _ := btcec.NewPrivateKey()

	return key.PubKey()
}

func getTestPair(from, to int) DirectedNodePair {
	return NewDirectedNodePair(hops[from], hops[to])
}

func getPolicyFailure(from, to int) *DirectedNodePair {
	pair := getTestPair(from, to)
	return &pair
}

type resultTestCase struct {
	name          string
	route         *mcRoute
	success       bool
	failureSrcIdx int
	failure       lnwire.FailureMessage

	expectedResult *interpretedResult
}

var resultTestCases = []resultTestCase{
	// Tests that a temporary channel failure result is properly
	// interpreted.
	{
		name:          "fail",
		route:         routeTwoHop,
		failureSrcIdx: 1,
		failure:       lnwire.NewTemporaryChannelFailure(nil),

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): failPairResult(99),
			},
		},
	},

	// Tests that an expiry too soon failure result is properly interpreted.
	{
		name:          "fail expiry too soon",
		route:         routeFourHop,
		failureSrcIdx: 3,
		failure:       lnwire.NewExpiryTooSoon(lnwire.ChannelUpdate1{}),

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): failPairResult(0),
				getTestPair(1, 0): failPairResult(0),
				getTestPair(1, 2): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
				getTestPair(2, 3): failPairResult(0),
				getTestPair(3, 2): failPairResult(0),
			},
		},
	},

	// Tests an incorrect payment details result. This should be a final
	// failure, but mark all pairs along the route as successful.
	{
		name:          "fail incorrect details",
		route:         routeTwoHop,
		failureSrcIdx: 2,
		failure:       lnwire.NewFailIncorrectDetails(97, 0),

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): successPairResult(99),
			},
			finalFailureReason: &reasonIncorrectDetails,
		},
	},

	// Tests a successful direct payment.
	{
		name:    "success direct",
		route:   routeOneHop,
		success: true,

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
			},
		},
	},

	// Tests a successful two hop payment.
	{
		name:    "success",
		route:   routeTwoHop,
		success: true,

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): successPairResult(99),
			},
		},
	},

	// Tests a malformed htlc from a direct peer.
	{
		name:          "fail malformed htlc from direct peer",
		route:         routeTwoHop,
		failureSrcIdx: 0,
		failure:       lnwire.NewInvalidOnionKey(nil),

		expectedResult: &interpretedResult{
			nodeFailure: &hops[1],
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(1, 0): failPairResult(0),
				getTestPair(1, 2): failPairResult(0),
				getTestPair(0, 1): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
			},
		},
	},

	// Tests a malformed htlc from a direct peer that is also the final
	// destination.
	{
		name:          "fail malformed htlc from direct final peer",
		route:         routeOneHop,
		failureSrcIdx: 0,
		failure:       lnwire.NewInvalidOnionKey(nil),

		expectedResult: &interpretedResult{
			finalFailureReason: &reasonError,
			nodeFailure:        &hops[1],
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(1, 0): failPairResult(0),
				getTestPair(0, 1): failPairResult(0),
			},
		},
	},

	// Tests that a fee insufficient failure to an intermediate hop with
	// index 2 results in the first hop marked as success, and then a
	// bidirectional failure for the incoming channel. It should also result
	// in a policy failure for the outgoing hop.
	{
		name:          "fail fee insufficient intermediate",
		route:         routeFourHop,
		failureSrcIdx: 2,
		failure: lnwire.NewFeeInsufficient(
			0, lnwire.ChannelUpdate1{},
		),
		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): {
					success: true,
					amt:     100,
				},
				getTestPair(1, 2): {},
				getTestPair(2, 1): {},
			},
			policyFailure: getPolicyFailure(2, 3),
		},
	},

	// Tests an invalid onion payload from a final hop. The final hop should
	// be failed while the proceeding hops are reproed as successes. The
	// failure is terminal since the receiver can't process our onion.
	{
		name:          "fail invalid onion payload final hop four",
		route:         routeFourHop,
		failureSrcIdx: 4,
		failure:       lnwire.NewInvalidOnionPayload(0, 0),

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): {
					success: true,
					amt:     100,
				},
				getTestPair(1, 2): {
					success: true,
					amt:     99,
				},
				getTestPair(2, 3): {
					success: true,
					amt:     97,
				},
				getTestPair(4, 3): {},
				getTestPair(3, 4): {},
			},
			finalFailureReason: &reasonError,
			nodeFailure:        &hops[4],
		},
	},

	// Tests an invalid onion payload from a final hop on a three hop route.
	{
		name:          "fail invalid onion payload final hop three",
		route:         routeThreeHop,
		failureSrcIdx: 3,
		failure:       lnwire.NewInvalidOnionPayload(0, 0),

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): {
					success: true,
					amt:     100,
				},
				getTestPair(1, 2): {
					success: true,
					amt:     99,
				},
				getTestPair(3, 2): {},
				getTestPair(2, 3): {},
			},
			finalFailureReason: &reasonError,
			nodeFailure:        &hops[3],
		},
	},

	// Tests an invalid onion payload from an intermediate hop. Only the
	// reporting node should be failed. The failure is non-terminal since we
	// can still try other paths.
	{
		name:          "fail invalid onion payload intermediate",
		route:         routeFourHop,
		failureSrcIdx: 3,
		failure:       lnwire.NewInvalidOnionPayload(0, 0),

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): {
					success: true,
					amt:     100,
				},
				getTestPair(1, 2): {
					success: true,
					amt:     99,
				},
				getTestPair(3, 2): {},
				getTestPair(3, 4): {},
				getTestPair(2, 3): {},
				getTestPair(4, 3): {},
			},
			nodeFailure: &hops[3],
		},
	},

	// Tests an invalid onion payload in a direct peer that is also the
	// final hop. The final node should be failed and the error is terminal
	// since the remote node can't process our onion.
	{
		name:          "fail invalid onion payload direct",
		route:         routeOneHop,
		failureSrcIdx: 1,
		failure:       lnwire.NewInvalidOnionPayload(0, 0),

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(1, 0): {},
				getTestPair(0, 1): {},
			},
			finalFailureReason: &reasonError,
			nodeFailure:        &hops[1],
		},
	},

	// Tests a single hop mpp timeout. Test that final node is not
	// penalized. This is a temporary measure while we decide how to
	// penalize mpp timeouts.
	{
		name:          "one hop mpp timeout",
		route:         routeOneHop,
		failureSrcIdx: 1,
		failure:       &lnwire.FailMPPTimeout{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
			},
			nodeFailure: nil,
		},
	},

	// Tests a two hop mpp timeout. Test that final node is not penalized
	// and the intermediate hop is attributed the success. This is a
	// temporary measure while we decide how to penalize mpp timeouts.
	{
		name:          "two hop mpp timeout",
		route:         routeTwoHop,
		failureSrcIdx: 2,
		failure:       &lnwire.FailMPPTimeout{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): successPairResult(99),
			},
			nodeFailure: nil,
		},
	},

	// Test a channel disabled failure from the final hop in two hops. Only the
	// disabled channel should be penalized for any amount.
	{
		name:          "two hop channel disabled",
		route:         routeTwoHop,
		failureSrcIdx: 1,
		failure:       &lnwire.FailChannelDisabled{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(1, 2): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
				getTestPair(0, 1): successPairResult(100),
			},
			policyFailure: getPolicyFailure(1, 2),
		},
	},
	// Test the case where a node after the introduction node returns a
	// error. In this case the introduction node is penalized because it
	// has not followed the specification properly.
	{
		name:          "error after introduction",
		route:         blindedMultiToIntroduction,
		failureSrcIdx: 2,
		// Note that the failure code doesn't matter in this case -
		// all we're worried about is errors originating after the
		// introduction node.
		failure: &lnwire.FailExpiryTooSoon{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): failPairResult(0),
				getTestPair(1, 0): failPairResult(0),
				getTestPair(1, 2): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
			},
			// Note: introduction node is failed even though the
			// error source is after it.
			nodeFailure: &hops[1],
		},
	},
	// Test the case where we get a blinding failure from a blinded final
	// hop when we expected the introduction node to convert.
	{
		name:          "final failure expected intro",
		route:         blindedMultiHop,
		failureSrcIdx: 4,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
				getTestPair(2, 3): failPairResult(0),
				getTestPair(3, 2): failPairResult(0),
			},
			// Note that the introduction node is penalized, not
			// the final hop.
			nodeFailure:        &hops[2],
			finalFailureReason: &reasonError,
		},
	},
	// Test a multi-hop blinded route where the failure occurs at the
	// introduction point.
	{
		name:          "blinded multi-hop introduction",
		route:         blindedMultiHop,
		failureSrcIdx: 2,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): successPairResult(99),

				// The amount for the last hop is always the
				// receiver amount because the amount to forward
				// is always set to 0 for intermediate blinded
				// hops.
				getTestPair(3, 4): failPairResult(77),
			},
		},
	},
	// Test a multi-hop blinded route where the failure occurs at the
	// introduction point, which is a direct peer.
	{
		name:          "blinded multi-hop introduction peer",
		route:         blindedMultiToIntroduction,
		failureSrcIdx: 1,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),

				// The amount for the last hop is always the
				// receiver amount because the amount to forward
				// is always set to 0 for intermediate blinded
				// hops.
				getTestPair(2, 3): failPairResult(58),
			},
		},
	},
	// Test a single-hop blinded route where the recipient is directly
	// connected to the introduction node.
	{
		name:          "blinded single hop introduction failure",
		route:         blindedSingleHop,
		failureSrcIdx: 2,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): successPairResult(99),
			},
			finalFailureReason: &reasonError,
		},
	},
	// Test the case where a node before the introduction node returns a
	// blinding error and is penalized for returning the wrong error.
	{
		name:          "error before introduction",
		route:         blindedMultiHop,
		failureSrcIdx: 1,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				// Failures from failing hops[1].
				getTestPair(0, 1): failPairResult(0),
				getTestPair(1, 0): failPairResult(0),
				getTestPair(1, 2): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
			},
			nodeFailure: &hops[1],
		},
	},
	// Test the case where an intermediate node that is not in a blinded
	// route returns an invalid blinding error and there was one
	// successful hop before the incorrect error.
	{
		name:          "intermediate unexpected blinding",
		route:         routeThreeHop,
		failureSrcIdx: 2,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				// Failures from failing hops[2].
				getTestPair(1, 2): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
				getTestPair(2, 3): failPairResult(0),
				getTestPair(3, 2): failPairResult(0),
			},
			nodeFailure: &hops[2],
		},
	},
	// Test the case where an intermediate node that is not in a blinded
	// route returns an invalid blinding error and there were no successful
	// hops before the erring incoming link (the erring node if our peer).
	{
		name:          "peer unexpected blinding",
		route:         routeThreeHop,
		failureSrcIdx: 1,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				// Failures from failing hops[1].
				getTestPair(0, 1): failPairResult(0),
				getTestPair(1, 0): failPairResult(0),
				getTestPair(1, 2): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
			},
			nodeFailure: &hops[1],
		},
	},
	// A node in a non-blinded route returns a blinding related error.
	{
		name:          "final node unexpected blinding",
		route:         routeThreeHop,
		failureSrcIdx: 3,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): successPairResult(99),
				getTestPair(2, 3): failPairResult(0),
				getTestPair(3, 2): failPairResult(0),
			},
			nodeFailure:        &hops[3],
			finalFailureReason: &reasonError,
		},
	},
	// Introduction node returns invalid blinding erroneously.
	{
		name:          "final node intro blinding",
		route:         blindedIntroReceiver,
		failureSrcIdx: 2,
		failure:       &lnwire.FailInvalidBlinding{},

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): failPairResult(0),
				getTestPair(2, 1): failPairResult(0),
			},
			nodeFailure:        &hops[2],
			finalFailureReason: &reasonError,
		},
	},
	// Test a multi-hop blinded route and that in a success case the amounts
	// for the blinded route part are correctly set to the receiver amount.
	{
		name:    "blinded multi-hop success",
		route:   blindedMultiToIntroduction,
		success: true,
		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),

				// For the route blinded part of the route the
				// success amount is determined by the receiver
				// amount because the intermediate blinded hops
				// set the forwarded amount to 0.
				getTestPair(1, 2): successPairResult(58),
				getTestPair(2, 3): successPairResult(58),
			},
		},
	},
}

// TestResultInterpretation executes a list of test cases that test the result
// interpretation logic.
func TestResultInterpretation(t *testing.T) {
	emptyResults := make(map[DirectedNodePair]pairResult)

	for _, testCase := range resultTestCases {
		t.Run(testCase.name, func(t *testing.T) {
			var failure fn.Option[paymentFailure]
			if !testCase.success {
				failure = fn.Some(newPaymentFailure(
					&testCase.failureSrcIdx,
					testCase.failure,
				))
			}

			i := interpretResult(
				InterpretCfg{}, testCase.route, failure,
			)

			expected := testCase.expectedResult

			// Replace nil pairResults with empty map to satisfy
			// DeepEqual.
			if expected.pairResults == nil {
				expected.pairResults = emptyResults
			}

			// We don't want to test the node fee deltas here, so we
			// set them to nil.
			i.nodeFeeDeltas = nil

			if !reflect.DeepEqual(i, expected) {
				t.Fatalf("unexpected result\nwant: %v\ngot: %v",
					spew.Sdump(expected), spew.Sdump(i))
			}
		})
	}
}

// TestFeeDeltas tests the fee level deltas of a temporary channel failure.
// The delta peaks at the node behind the failing channel and halves with each
// reach in hops.
func TestFeeDeltas(t *testing.T) {
	t.Parallel()

	// The test reads the nodes from the route, because other tests change
	// the hops variable.
	node := func(idx int) route.Vertex {
		return routeFourHop.hops.Val[idx].pubKeyBytes.Val
	}

	tests := []struct {
		name      string
		reach     float64
		symmetric bool
		expected  map[route.Vertex]uint64
	}{
		{
			name:  "asymmetric",
			reach: 1,
			expected: map[route.Vertex]uint64{
				node(1): 100, node(2): 50, node(3): 25,
			},
		},
		{
			name:      "symmetric",
			reach:     2,
			symmetric: true,
			expected: map[route.Vertex]uint64{
				node(0): 70, node(1): 100, node(2): 70,
				node(3): 50,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			failureSrcIdx := 1
			failure := fn.Some(newPaymentFailure(
				&failureSrcIdx,
				lnwire.NewTemporaryChannelFailure(nil),
			))
			i := interpretResult(
				InterpretCfg{
					FeeLevelPPM:       100,
					FeeLevelReach:     test.reach,
					FeeLevelSymmetric: test.symmetric,
					FeeLevelDecay:     time.Hour,
				},
				routeFourHop, failure,
			)

			require.Equal(t, test.expected, i.nodeFeeDeltas)
		})
	}
}

// TestInterpretCfgValidate tests the fee level config rules. A fee level PPM
// of zero turns fee levels off, so the other rules do not apply.
func TestInterpretCfgValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ppm   uint64
		max   uint64
		reach float64
		decay time.Duration
		valid bool
	}{
		{"valid", 100, 1000, 1, time.Hour, true},
		{"max equals PPM", 100, 100, 1, time.Hour, true},
		{"off", 0, 0, 0, 0, true},
		{"zero reach", 100, 1000, 0, time.Hour, false},
		{"negative reach", 100, 1000, -1, time.Hour, false},
		{"NaN reach", 100, 1000, math.NaN(), time.Hour, false},
		{"infinite reach", 100, 1000, math.Inf(1), time.Hour, false},
		{"max below PPM", 100, 99, 1, time.Hour, false},
		{"max above cap", 100, maxFeeLevelPPM + 1, 1, time.Hour, false},
		{"zero decay", 100, 1000, 1, 0, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := InterpretCfg{
				FeeLevelPPM:   test.ppm,
				FeeLevelMax:   test.max,
				FeeLevelReach: test.reach,
				FeeLevelDecay: test.decay,
			}.Validate()

			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// TestClampFeeLevel tests the conversion of float fee levels to PPM.
func TestClampFeeLevel(t *testing.T) {
	t.Parallel()

	require.EqualValues(t, 0, clampFeeLevel(math.NaN()))
	require.EqualValues(t, 0, clampFeeLevel(math.Inf(-1)))
	require.EqualValues(t, 0, clampFeeLevel(-5))
	require.EqualValues(t, 42, clampFeeLevel(42.9))
	require.EqualValues(t, maxFeeLevelPPM, clampFeeLevel(math.Inf(1)))
	require.EqualValues(t, maxFeeLevelPPM, clampFeeLevel(1e19))
}
