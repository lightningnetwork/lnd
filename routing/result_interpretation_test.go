package routing

import (
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"

	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	hops = []route.Vertex{
		{1, 0}, {1, 1}, {1, 2}, {1, 3}, {1, 4},
	}

	routeOneHop = route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{PubKeyBytes: hops[1], AmtToForward: 99},
		},
	}

	routeTwoHop = route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{PubKeyBytes: hops[1], AmtToForward: 99},
			{PubKeyBytes: hops[2], AmtToForward: 97},
		},
	}

	routeThreeHop = route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{PubKeyBytes: hops[1], AmtToForward: 99},
			{PubKeyBytes: hops[2], AmtToForward: 97},
			{PubKeyBytes: hops[3], AmtToForward: 94},
		},
	}

	routeFourHop = route.Route{
		SourcePubKey: hops[0],
		TotalAmount:  100,
		Hops: []*route.Hop{
			{PubKeyBytes: hops[1], AmtToForward: 99},
			{PubKeyBytes: hops[2], AmtToForward: 97},
			{PubKeyBytes: hops[3], AmtToForward: 94},
			{PubKeyBytes: hops[4], AmtToForward: 90},
		},
	}
)

func getTestPair(from, to int) DirectedNodePair {
	return NewDirectedNodePair(hops[from], hops[to])
}

func getPolicyFailure(from, to int) *DirectedNodePair {
	pair := getTestPair(from, to)
	return &pair
}

type resultTestCase struct {
	name          string
	route         *route.Route
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
		route:         &routeTwoHop,
		failureSrcIdx: 1,
		failure:       lnwire.NewTemporaryChannelFailure(nil),

		expectedResult: &interpretedResult{
			pairResults: map[DirectedNodePair]pairResult{
				getTestPair(0, 1): successPairResult(100),
				getTestPair(1, 2): failPairResult(99),
			},
		},
	},

	// Tests that a expiry too soon failure result is properly interpreted.
	{
		name:          "fail expiry too soon",
		route:         &routeFourHop,
		failureSrcIdx: 3,
		failure:       lnwire.NewExpiryTooSoon(lnwire.ChannelUpdate{}),

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
		route:         &routeTwoHop,
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
		route:   &routeOneHop,
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
		route:   &routeTwoHop,
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
		route:         &routeTwoHop,
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
		route:         &routeOneHop,
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
		route:         &routeFourHop,
		failureSrcIdx: 2,
		failure:       lnwire.NewFeeInsufficient(0, lnwire.ChannelUpdate{}),

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
		route:         &routeFourHop,
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
		route:         &routeThreeHop,
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
		route:         &routeFourHop,
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
		route:         &routeOneHop,
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
		route:         &routeOneHop,
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
		route:         &routeTwoHop,
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
}

// TestResultInterpretation executes a list of test cases that test the result
// interpretation logic.
func TestResultInterpretation(t *testing.T) {
	emptyResults := make(map[DirectedNodePair]pairResult)

	for _, testCase := range resultTestCases {
		t.Run(testCase.name, func(t *testing.T) {
			i := interpretResult(
				testCase.route, testCase.success,
				&testCase.failureSrcIdx, testCase.failure,
			)

			expected := testCase.expectedResult

			// Replace nil pairResults with empty map to satisfy
			// DeepEqual.
			if expected.pairResults == nil {
				expected.pairResults = emptyResults
			}

			if !reflect.DeepEqual(i, expected) {
				t.Fatalf("unexpected result\nwant: %v\ngot: %v",
					spew.Sdump(expected), spew.Sdump(i))
			}
		})
	}
}
