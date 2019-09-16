package routing

import (
	"reflect"
	"testing"

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
				getTestPair(0, 1): {
					success: true,
				},
				getTestPair(1, 2): {
					minPenalizeAmt: 99,
				},
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
				getTestPair(0, 1): {},
				getTestPair(1, 0): {},
				getTestPair(1, 2): {},
				getTestPair(2, 1): {},
				getTestPair(2, 3): {},
				getTestPair(3, 2): {},
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
				getTestPair(0, 1): {
					success: true,
				},
				getTestPair(1, 2): {
					success: true,
				},
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
				getTestPair(0, 1): {
					success: true,
				},
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
				getTestPair(0, 1): {
					success: true,
				},
				getTestPair(1, 2): {
					success: true,
				},
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
				t.Fatal("unexpected result")
			}
		})
	}
}
