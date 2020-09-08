package chanfitness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGetRateLimit tests getting of our rate limit using the current constants.
// It creates test cases that are relative to our constants so that they
// can be adjusted without breaking the unit test.
func TestGetRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		flapCount int
		rateLimit time.Duration
	}{
		{
			name:      "zero flaps",
			flapCount: 0,
			rateLimit: rateLimits[0],
		},
		{
			name:      "middle tier",
			flapCount: rateLimitScale * (len(rateLimits) / 2),
			rateLimit: rateLimits[len(rateLimits)/2],
		},
		{
			name:      "last tier",
			flapCount: rateLimitScale * (len(rateLimits) - 1),
			rateLimit: rateLimits[len(rateLimits)-1],
		},
		{
			name:      "beyond last tier",
			flapCount: rateLimitScale * (len(rateLimits) * 2),
			rateLimit: rateLimits[len(rateLimits)-1],
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			limit := getRateLimit(test.flapCount)
			require.Equal(t, test.rateLimit, limit)
		})
	}
}

// TestCooldownFlapCount tests cooldown of all time flap counts.
func TestCooldownFlapCount(t *testing.T) {
	tests := []struct {
		name      string
		flapCount int
		lastFlap  time.Time
		expected  int
	}{
		{
			name:      "just flapped, do not cooldown",
			flapCount: 1,
			lastFlap:  testNow,
			expected:  1,
		},
		{
			name:      "period not elapsed, do not cooldown",
			flapCount: 1,
			lastFlap:  testNow.Add(flapCountCooldownPeriod / 2 * -1),
			expected:  1,
		},
		{
			name:      "rounded to 0",
			flapCount: 1,
			lastFlap:  testNow.Add(flapCountCooldownPeriod * -1),
			expected:  0,
		},
		{
			name:      "decreased to integer value",
			flapCount: 10,
			lastFlap:  testNow.Add(flapCountCooldownPeriod * -1),
			expected:  9,
		},
		{
			name:      "multiple cooldown periods",
			flapCount: 10,
			lastFlap:  testNow.Add(flapCountCooldownPeriod * -3),
			expected:  8,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			flapCount := cooldownFlapCount(
				testNow, test.flapCount, test.lastFlap,
			)
			require.Equal(t, test.expected, flapCount)
		})
	}
}
