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

type resultTestCase struct {
	name          string
	route         *route.Route
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
			pairResults: map[DirectedNodePair]lnwire.MilliSatoshi{
				NewDirectedNodePair(hops[1], hops[2]): 99,
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
			nodeFailure: &hops[3],
		},
	},
}

// TestResultInterpretation executes a list of test cases that test the result
// interpretation logic.
func TestResultInterpretation(t *testing.T) {
	emptyResults := make(map[DirectedNodePair]lnwire.MilliSatoshi)

	for _, testCase := range resultTestCases {
		t.Run(testCase.name, func(t *testing.T) {
			i := interpretResult(
				testCase.route, &testCase.failureSrcIdx,
				testCase.failure,
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
