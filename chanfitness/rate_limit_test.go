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
