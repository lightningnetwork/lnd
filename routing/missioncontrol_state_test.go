package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestMissionControlStateSetLastPairResult tests setting mission control state
// pair results.
func TestMissionControlStateSetLastPairResult(t *testing.T) {
	const minFailureRelaxInterval = time.Minute
	state := newMissionControlState(minFailureRelaxInterval)

	var (
		from      = route.Vertex{1}
		to        = route.Vertex{2}
		timestamp = testTime
	)

	// Report a 1000 sat failure.
	state.setLastPairResult(
		from, to, timestamp, &pairResult{amt: 1000}, false,
	)
	result, _ := state.getLastPairResult(from)
	if result[to].FailAmt != 1000 {
		t.Fatalf("unexpected fail amount %v", result[to].FailAmt)
	}

	// Report an 1100 sat failure one hour later. It is expected to
	// overwrite the previous failure.
	timestamp = timestamp.Add(time.Hour)
	state.setLastPairResult(
		from, to, timestamp, &pairResult{amt: 1100}, false,
	)
	result, _ = state.getLastPairResult(from)
	if result[to].FailAmt != 1100 {
		t.Fatalf("unexpected fail amount %v", result[to].FailAmt)
	}

	// Report a 1200 sat failure one second later. Because this increase of
	// the failure amount is too soon after the previous failure, the result
	// is not applied.
	timestamp = timestamp.Add(time.Second)
	state.setLastPairResult(
		from, to, timestamp, &pairResult{amt: 1200}, false,
	)
	result, _ = state.getLastPairResult(from)
	if result[to].FailAmt != 1100 {
		t.Fatalf("unexpected fail amount %v", result[to].FailAmt)
	}

	// Roll back time 1 second to test forced import.
	timestamp = testTime
	state.setLastPairResult(
		from, to, timestamp, &pairResult{amt: 999}, true,
	)
	result, _ = state.getLastPairResult(from)
	require.Equal(t, 999, int(result[to].FailAmt))

	// Report an 1500 sat success.
	timestamp = timestamp.Add(time.Second)
	state.setLastPairResult(
		from, to, timestamp, &pairResult{amt: 1500, success: true}, false,
	)
	result, _ = state.getLastPairResult(from)
	// We don't expect the failtime to change, only the fail amount, we
	// expect however change of both the success time and the success amount.
	expected := TimedPairResult{
		FailTime:    timestamp.Add(-time.Second),
		FailAmt:     1501,
		SuccessTime: timestamp,
		SuccessAmt:  1500,
	}
	require.Equal(t, expected, result[to])

	// Again roll back time to test forced import.
	state.setLastPairResult(
		from, to, testTime, &pairResult{amt: 50, success: true}, true,
	)
	result, _ = state.getLastPairResult(from)
	// We don't expect the failtime to change, only the fail amount, we
	// expect however change of both the success time and the success amount.
	expected = TimedPairResult{
		FailTime:    timestamp.Add(-time.Second),
		FailAmt:     51,
		SuccessTime: testTime,
		SuccessAmt:  50,
	}
	require.Equal(t, expected, result[to])
}
