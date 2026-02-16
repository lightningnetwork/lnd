package lnd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNodeAnnouncementTimestampComparison tests the timestamp comparison
// logic used in setSelfNode to ensure node announcements have strictly
// increasing timestamps at second precision (as required by BOLT-07 and
// enforced by the database storage).
func TestNodeAnnouncementTimestampComparison(t *testing.T) {
	t.Parallel()

	// Use a simple base time for the tests.
	baseTime := int64(1000)

	tests := []struct {
		name              string
		srcNodeLastUpdate time.Time
		nodeLastUpdate    time.Time
		expectedResult    time.Time
		description       string
	}{
		{
			name:              "same second different nanoseconds",
			srcNodeLastUpdate: time.Unix(baseTime, 0),
			nodeLastUpdate:    time.Unix(baseTime, 500_000_000),
			expectedResult:    time.Unix(baseTime+1, 0),
			description: "Edge case: timestamps in same second " +
				"but different nanoseconds. Must increment " +
				"to avoid persisting same second-level " +
				"timestamp.",
		},
		{
			name:              "different seconds",
			srcNodeLastUpdate: time.Unix(baseTime, 0),
			nodeLastUpdate:    time.Unix(baseTime+2, 0),
			expectedResult:    time.Unix(baseTime+2, 0),
			description: "Normal case: current time is already " +
				"in a different (later) second. No increment " +
				"needed.",
		},
		{
			name:              "exactly equal",
			srcNodeLastUpdate: time.Unix(baseTime, 123456789),
			nodeLastUpdate:    time.Unix(baseTime, 123456789),
			expectedResult:    time.Unix(baseTime+1, 123456789),
			description: "Timestamps are identical. Must " +
				"increment to ensure strictly greater " +
				"timestamp.",
		},
		{
			name:              "exactly equal - zero nanoseconds",
			srcNodeLastUpdate: time.Unix(baseTime, 0),
			nodeLastUpdate:    time.Unix(baseTime, 0),
			expectedResult:    time.Unix(baseTime+1, 0),
			description: "Timestamps are identical at second " +
				"precision (0 nanoseconds), as would be read " +
				"from DB. Must increment.",
		},
		{
			name:              "clock skew - persisted is newer",
			srcNodeLastUpdate: time.Unix(baseTime+5, 0),
			nodeLastUpdate:    time.Unix(baseTime+3, 0),
			expectedResult:    time.Unix(baseTime+6, 0),
			description: "Clock went backwards: persisted " +
				"timestamp is newer than current time. Must " +
				"increment from persisted timestamp.",
		},
		{
			name:              "clock skew - same second",
			srcNodeLastUpdate: time.Unix(baseTime+5, 100_000_000),
			nodeLastUpdate:    time.Unix(baseTime+5, 900_000_000),
			expectedResult:    time.Unix(baseTime+6, 100_000_000),
			description: "Clock skew within same second. Must " +
				"increment to ensure strictly greater " +
				"second-level timestamp.",
		},
		{
			name: "same second component different " +
				"minute",
			srcNodeLastUpdate: time.Unix(baseTime, 0),
			nodeLastUpdate:    time.Unix(baseTime+60, 0),
			expectedResult:    time.Unix(baseTime+60, 0),
			description: "Same seconds component (:00) but " +
				"different minutes. Current time is later. " +
				"Verifies we use .Unix() not .Second().",
		},
		{
			name: "lower second component but " +
				"later time",
			srcNodeLastUpdate: time.Unix(baseTime+58, 0),
			nodeLastUpdate:    time.Unix(baseTime+63, 0),
			expectedResult:    time.Unix(baseTime+63, 0),
			description: "Persisted has second=58, current has " +
				"second=3 (next minute). Current is later " +
				"overall. Verifies .Unix() not .Second().",
		},
		{
			name: "higher second component but " +
				"earlier time",
			srcNodeLastUpdate: time.Unix(baseTime+63, 0),
			nodeLastUpdate:    time.Unix(baseTime+58, 0),
			expectedResult:    time.Unix(baseTime+64, 0),
			description: "Persisted has second=3 (next minute), " +
				"current has second=58. Persisted is later " +
				"overall. Verifies .Unix() not .Second().",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := calculateNodeAnnouncementTimestamp(
				tc.srcNodeLastUpdate,
				tc.nodeLastUpdate,
			)

			// Verify we got the expected result.
			require.Equal(
				t, tc.expectedResult, result,
				"Unexpected result: %s", tc.description,
			)

			// Verify result is strictly greater than persisted
			// timestamp. This is an additional check to ensure
			// the result is strictly greater than the persisted
			// timestamp.
			require.Greater(
				t, result.Unix(), tc.srcNodeLastUpdate.Unix(),
				"Result must be strictly greater than "+
					"persisted timestamp: %s",
				tc.description,
			)
		})
	}
}

// TestPeerBackoff tests the pure peerBackoff function with table-driven cases
// covering zero backoff, short-lived connections (doubles), stable connections
// (reduces), and capping at max.
func TestPeerBackoff(t *testing.T) {
	t.Parallel()

	const (
		minBackoff         = 1 * time.Second
		maxBackoff         = 1 * time.Hour
		stableConnDuration = 10 * time.Minute
	)

	tests := []struct {
		name           string
		currentBackoff time.Duration
		startTime      time.Time
		assertBackoff  func(t *testing.T, result time.Duration)
	}{
		{
			// Peer never started: backoff should roughly double
			// (with randomization).
			name:           "zero start time doubles backoff",
			currentBackoff: 10 * time.Second,
			startTime:      time.Time{},
			assertBackoff: func(t *testing.T, result time.Duration) {
				// computeNextBackoff doubles with ±5%
				// wiggle, so result should be roughly 20s.
				require.Greater(t, result,
					15*time.Second,
					"backoff too low for failed start")
				require.Less(t, result,
					25*time.Second,
					"backoff too high for failed start")
			},
		},
		{
			// Short-lived connection (< stableConnDuration):
			// backoff should roughly double.
			name:           "short lived connection doubles backoff",
			currentBackoff: 10 * time.Second,
			startTime:      time.Now().Add(-5 * time.Minute),
			assertBackoff: func(t *testing.T, result time.Duration) {
				require.Greater(t, result,
					15*time.Second,
					"backoff too low for short conn")
				require.Less(t, result,
					25*time.Second,
					"backoff too high for short conn")
			},
		},
		{
			// Stable connection (> stableConnDuration) with
			// large backoff: should reduce. reb(30m) ≈ 60m,
			// minus 20m conn = ~40m, which is > minBackoff.
			name:           "stable connection reduces backoff",
			currentBackoff: 30 * time.Minute,
			startTime:      time.Now().Add(-20 * time.Minute),
			assertBackoff: func(t *testing.T, result time.Duration) {
				require.Greater(t, result, minBackoff,
					"should be above min")
				require.Less(t, result, 50*time.Minute,
					"should be reduced from doubled")
			},
		},
		{
			// Stable connection that lasted much longer than
			// backoff: should return minBackoff.
			// reb(1s) ≈ 2s, minus 1h conn → negative → min.
			name:           "long stable connection resets to min",
			currentBackoff: minBackoff,
			startTime:      time.Now().Add(-1 * time.Hour),
			assertBackoff: func(t *testing.T, result time.Duration) {
				require.Equal(t, minBackoff, result)
			},
		},
		{
			// Backoff at max: doubling should cap at max.
			name:           "backoff caps at max",
			currentBackoff: maxBackoff,
			startTime:      time.Time{},
			assertBackoff: func(t *testing.T, result time.Duration) {
				// After capping + wiggle.
				require.LessOrEqual(t, result,
					maxBackoff+maxBackoff/10)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := peerBackoff(
				tc.currentBackoff, tc.startTime, minBackoff,
				maxBackoff, stableConnDuration,
			)

			tc.assertBackoff(t, result)
		})
	}
}
