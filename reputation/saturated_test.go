package reputation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSatFromUint checks conversion from unsigned values, which must clamp
// rather than wrap when the value does not fit in an int64.
func TestSatFromUint(t *testing.T) {
	t.Parallel()

	require.Equal(t, satFromInt(0), satFromUint(0))
	require.Equal(t, satFromInt(1000), satFromUint(1000))
	require.Equal(
		t, satFromInt(math.MaxInt64), satFromUint(math.MaxInt64),
	)

	// Anything above MaxInt64 saturates instead of wrapping negative.
	require.Equal(
		t, satFromInt(math.MaxInt64), satFromUint(math.MaxInt64+1),
	)
	require.Equal(
		t, satFromInt(math.MaxInt64), satFromUint(math.MaxUint64),
	)
}

// TestSatFromFloat checks conversion from floats. An out-of-range float-to-int
// conversion is undefined in Go, so the bounds must be clamped explicitly.
func TestSatFromFloat(t *testing.T) {
	t.Parallel()

	require.Equal(t, satFromInt(0), satFromFloat(0))
	require.Equal(t, satFromInt(1000), satFromFloat(1000))
	require.Equal(t, satFromInt(-1000), satFromFloat(-1000))

	// float64(MaxInt64) rounds up to 2^63, so it is out of range and must
	// saturate rather than yield MinInt64.
	require.Equal(
		t, satFromInt(math.MaxInt64),
		satFromFloat(float64(math.MaxInt64)),
	)
	require.Equal(
		t, satFromInt(math.MinInt64),
		satFromFloat(float64(math.MinInt64)),
	)
	require.Equal(t, satFromInt(math.MaxInt64), satFromFloat(1e30))
	require.Equal(t, satFromInt(math.MinInt64), satFromFloat(-1e30))
}

// TestSaturatedAdd checks that addition clamps at both bounds instead of
// wrapping.
func TestSaturatedAdd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		a, b     int64
		expected int64
	}{{
		name:     "simple",
		a:        5,
		b:        3,
		expected: 8,
	}, {
		name:     "add negative",
		a:        5,
		b:        -3,
		expected: 2,
	}, {
		name:     "overflow saturates",
		a:        math.MaxInt64,
		b:        1,
		expected: math.MaxInt64,
	}, {
		name:     "underflow saturates",
		a:        math.MinInt64,
		b:        -1,
		expected: math.MinInt64,
	}, {
		name:     "opposite extremes cancel",
		a:        math.MaxInt64,
		b:        math.MinInt64,
		expected: -1,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := satFromInt(test.a).Add(satFromInt(test.b))
			require.Equal(t, satFromInt(test.expected), got)
		})
	}
}

// TestSaturatedSub checks that subtraction clamps at both bounds instead of
// wrapping, including the extremes where negating the operand would itself
// overflow.
func TestSaturatedSub(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		a, b     int64
		expected int64
	}{{
		name:     "simple",
		a:        5,
		b:        3,
		expected: 2,
	}, {
		name:     "subtract negative",
		a:        5,
		b:        -3,
		expected: 8,
	}, {
		name:     "result goes negative",
		a:        3,
		b:        5,
		expected: -2,
	}, {
		// Both operands are negative, so the result is exactly
		// representable and must not be clamped.
		name:     "min plus one minus min",
		a:        math.MinInt64 + 1,
		b:        math.MinInt64,
		expected: 1,
	}, {
		// 0 - (-2^63) = 2^63, which exceeds MaxInt64.
		name:     "overflow saturates",
		a:        0,
		b:        math.MinInt64,
		expected: math.MaxInt64,
	}, {
		name:     "underflow saturates",
		a:        math.MinInt64,
		b:        1,
		expected: math.MinInt64,
	}, {
		name:     "max minus negative saturates",
		a:        math.MaxInt64,
		b:        -1,
		expected: math.MaxInt64,
	}, {
		name:     "min minus max saturates",
		a:        math.MinInt64,
		b:        math.MaxInt64,
		expected: math.MinInt64,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := satFromInt(test.a).Sub(satFromInt(test.b))
			require.Equal(t, satFromInt(test.expected), got)
		})
	}
}

// TestSaturatedInt64 checks the conversion back to a plain int64.
func TestSaturatedInt64(t *testing.T) {
	t.Parallel()

	require.Equal(t, int64(1000), satFromInt(1000).Int64())
	require.Equal(
		t, int64(math.MinInt64), satFromInt(math.MinInt64).Int64(),
	)
}
