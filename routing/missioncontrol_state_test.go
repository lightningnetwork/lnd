package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/routing/route"
)

// TestMissionControlStateFailureResult tests setting failure results on the
// mission control state.
func TestMissionControlStateFailureResult(t *testing.T) {
	const minFailureRelaxInterval = time.Minute
	state := newMissionControlState(minFailureRelaxInterval)

	var (
		from      = route.Vertex{1}
		to        = route.Vertex{2}
		timestamp = testTime
	)

	// Report a 1000 sat failure.
	state.setLastPairResult(from, to, timestamp, &pairResult{amt: 1000})
	result, _ := state.getLastPairResult(from)
	if result[to].FailAmt != 1000 {
		t.Fatalf("unexpected fail amount %v", result[to].FailAmt)
	}

	// Report an 1100 sat failure one hour later. It is expected to
	// overwrite the previous failure.
	timestamp = timestamp.Add(time.Hour)
	state.setLastPairResult(from, to, timestamp, &pairResult{amt: 1100})
	result, _ = state.getLastPairResult(from)
	if result[to].FailAmt != 1100 {
		t.Fatalf("unexpected fail amount %v", result[to].FailAmt)
	}

	// Report a 1200 sat failure one second later. Because this increase of
	// the failure amount is too soon after the previous failure, the result
	// is not applied.
	timestamp = timestamp.Add(time.Second)
	state.setLastPairResult(from, to, timestamp, &pairResult{amt: 1200})
	result, _ = state.getLastPairResult(from)
	if result[to].FailAmt != 1100 {
		t.Fatalf("unexpected fail amount %v", result[to].FailAmt)
	}
}
