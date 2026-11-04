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

// TestMissionControlStateFeeLevel tests functionality of the fee level
// mechanism.
func TestMissionControlStateFeeLevel(t *testing.T) {
	var (
		feeLevelDecay = time.Hour
		now           = time.Now()
		state         = newMissionControlState(0)
	)

	// Query the fee level for a node that has not been set yet.
	node := route.Vertex{1}
	feeLevel := state.getFeeLevel(now, node, feeLevelDecay)
	require.Zero(t, feeLevel)

	feeLevelDelta := uint64(100)
	state.setFeeDelta(
		now, node, feeLevelDelta, feeLevelDecay, maxFeeLevelPPM,
	)

	feeLevel = state.getFeeLevel(now, node, feeLevelDecay)
	require.Equal(t, feeLevelDelta, feeLevel)

	// Set the fee level for the node again. The fee level should have
	// accumulated.
	state.setFeeDelta(
		now, node, feeLevelDelta, feeLevelDecay, maxFeeLevelPPM,
	)
	feeLevel = state.getFeeLevel(now, node, feeLevelDecay)
	require.Equal(t, feeLevelDelta*2, feeLevel)

	// Query the fee level after some time. The fee level should have
	// decayed.
	decayTime := now.Add(feeLevelDecay)
	feeLevel = state.getFeeLevel(decayTime, node, feeLevelDecay)
	require.Equal(t, feeLevelDelta, feeLevel)

	// Query the fee level after some more time. The fee level should have
	// decayed by another 50%.
	decayTime = decayTime.Add(feeLevelDecay)
	feeLevel = state.getFeeLevel(decayTime, node, feeLevelDecay)
	require.Equal(t, feeLevelDelta/2, feeLevel)
}

// TestMissionControlStateFeeLevelCap tests that repeated deltas do not raise a
// fee level above the given maximum.
func TestMissionControlStateFeeLevelCap(t *testing.T) {
	var (
		feeLevelDecay = time.Hour
		now           = time.Now()
		state         = newMissionControlState(0)
		node          = route.Vertex{1}
	)

	for range 3 {
		state.setFeeDelta(now, node, 400, feeLevelDecay, 1000)
	}
	require.EqualValues(
		t, 1000, state.getFeeLevel(now, node, feeLevelDecay),
	)
}

// TestMissionControlStateFeeLevelClockStep tests that a clock step back
// neither drops a delta nor decays a fee level.
func TestMissionControlStateFeeLevelClockStep(t *testing.T) {
	var (
		feeLevelDecay = time.Hour
		now           = time.Now()
		earlier       = now.Add(-feeLevelDecay)
		state         = newMissionControlState(0)
		node          = route.Vertex{1}
	)

	state.setFeeDelta(now, node, 100, feeLevelDecay, maxFeeLevelPPM)

	// A read before the last update gives the full fee level.
	require.EqualValues(
		t, 100, state.getFeeLevel(earlier, node, feeLevelDecay),
	)

	// A delta before the last update adds up and keeps the later time.
	state.setFeeDelta(earlier, node, 100, feeLevelDecay, maxFeeLevelPPM)
	require.EqualValues(
		t, 200, state.getFeeLevel(now, node, feeLevelDecay),
	)
	require.Equal(t, now, state.feeLevels[node].LastUpdate)
}

// TestMissionControlStateResetFeeLevels tests that a history reset clears the
// fee levels.
func TestMissionControlStateResetFeeLevels(t *testing.T) {
	var (
		now   = time.Now()
		state = newMissionControlState(0)
		node  = route.Vertex{1}
	)

	state.setFeeDelta(now, node, 100, time.Hour, maxFeeLevelPPM)
	state.resetHistory()
	require.Zero(t, state.getFeeLevel(now, node, time.Hour))
}
