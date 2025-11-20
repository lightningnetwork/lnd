package routerrpc

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestParseDuration tests the hybrid duration parsing with explicit examples.
func TestParseDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected time.Duration
		wantErr  bool
	}{
		// Standard Go durations.
		{
			name:     "standard go hours",
			input:    "-24h",
			expected: -24 * time.Hour,
		},
		{
			name:     "standard go fractional hours",
			input:    "-1.5h",
			expected: time.Duration(-1.5 * float64(time.Hour)),
		},
		{
			name:     "standard go minutes",
			input:    "-30m",
			expected: -30 * time.Minute,
		},
		{
			name:     "standard go seconds",
			input:    "-60s",
			expected: -60 * time.Second,
		},
		{
			name:     "standard go milliseconds",
			input:    "-500ms",
			expected: -500 * time.Millisecond,
		},
		{
			name:     "standard go microseconds",
			input:    "-1000us",
			expected: -1000 * time.Microsecond,
		},
		{
			name:     "standard go complex",
			input:    "-2h30m45s",
			expected: -(2*time.Hour + 30*time.Minute + 45*time.Second),
		},

		// Custom units.
		{
			name:     "custom days",
			input:    "-1d",
			expected: -24 * time.Hour,
		},
		{
			name:     "custom multiple days",
			input:    "-7d",
			expected: -7 * 24 * time.Hour,
		},
		{
			name:     "custom weeks",
			input:    "-1w",
			expected: -7 * 24 * time.Hour,
		},
		{
			name:     "custom multiple weeks",
			input:    "-4w",
			expected: -4 * 7 * 24 * time.Hour,
		},
		{
			name:     "custom months",
			input:    "-1M",
			expected: time.Duration(-30.44 * 24 * float64(time.Hour)),
		},
		{
			name:     "custom years",
			input:    "-1y",
			expected: time.Duration(-365.25 * 24 * float64(time.Hour)),
		},

		// Error cases.
		{
			name:    "positive duration",
			input:   "1h",
			wantErr: true,
		},
		{
			name:    "no minus sign custom",
			input:   "1d",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "just minus",
			input:   "-",
			wantErr: true,
		},
		{
			name:    "no number",
			input:   "-d",
			wantErr: true,
		},
		{
			name:    "no unit",
			input:   "-5",
			wantErr: true,
		},
		{
			name:    "invalid unit",
			input:   "-1x",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseDuration(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expected, got)
		})
	}
}

// TestParseDurationProperties uses property-based testing to verify invariants.
func TestParseDurationProperties(t *testing.T) {
	t.Parallel()

	// Test that all valid standard Go durations work.
	t.Run("standard go durations", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			// Generate a random duration string using
			// time.Duration's String() method.
			hours := rapid.IntRange(-8760, -1).Draw(rt, "hours")
			minutes := rapid.IntRange(0, 59).Draw(rt, "minutes")
			seconds := rapid.IntRange(0, 59).Draw(rt, "seconds")

			d := time.Duration(hours)*time.Hour +
				time.Duration(minutes)*time.Minute +
				time.Duration(seconds)*time.Second

			// Parse it back.
			parsed, err := parseDuration(d.String())
			require.NoError(rt, err)

			// Should match original (within a small margin for
			// float precision).
			diff := parsed - d
			if diff < 0 {
				diff = -diff
			}
			require.Less(
				rt, diff, time.Microsecond,
				"parsed duration differs: got %v, want %v",
				parsed, d,
			)
		})
	})

	// Test that custom units produce negative durations.
	t.Run("custom units negative", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			value := rapid.IntRange(1, 1000).Draw(rt, "value")
			unit := rapid.SampledFrom([]string{"d", "w", "M", "y"}).
				Draw(rt, "unit")

			input := fmt.Sprintf("-%d%s", value, unit)

			parsed, err := parseDuration(input)
			require.NoError(rt, err)
			require.Less(
				rt, parsed, time.Duration(0),
				"custom unit should produce negative duration",
			)
		})
	})

	// Test that days are always 24 hours.
	t.Run("days invariant", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			days := rapid.IntRange(1, 365).Draw(rt, "days")
			input := fmt.Sprintf("-%dd", days)

			parsed, err := parseDuration(input)
			require.NoError(rt, err)

			expected := time.Duration(-days) * 24 * time.Hour
			require.Equal(rt, expected, parsed)
		})
	})

	// Test that weeks are always 7 days.
	t.Run("weeks invariant", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			weeks := rapid.IntRange(1, 52).Draw(rt, "weeks")
			input := fmt.Sprintf("-%dw", weeks)

			parsed, err := parseDuration(input)
			require.NoError(rt, err)

			expected := time.Duration(-weeks) * 7 * 24 * time.Hour
			require.Equal(rt, expected, parsed)
		})
	})

	// Test that positive durations always error.
	t.Run("positive durations error", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			value := rapid.IntRange(1, 1000).Draw(rt, "value")
			unit := rapid.SampledFrom([]string{
				"s", "m", "h", "d", "w", "M", "y",
			}).Draw(rt, "unit")

			// Positive duration (no minus sign).
			input := fmt.Sprintf("%d%s", value, unit)

			_, err := parseDuration(input)
			require.Error(rt, err,
				"positive duration should error: %s", input)
		})
	})
}
