package fn

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func even(a int) bool { return a%2 == 0 }
func odd(a int) bool  { return a%2 != 0 }

func TestAll(t *testing.T) {
	x := []int{0, 2, 4, 6, 8}
	require.True(t, All(even, x))
	require.False(t, All(odd, x))

	y := []int{1, 3, 5, 7, 9}
	require.False(t, All(even, y))
	require.True(t, All(odd, y))

	z := []int{0, 2, 4, 6, 9}
	require.False(t, All(even, z))
	require.False(t, All(odd, z))
}

func TestAny(t *testing.T) {
	x := []int{1, 3, 5, 7, 9}
	require.False(t, Any(even, x))
	require.True(t, Any(odd, x))

	y := []int{0, 3, 5, 7, 9}
	require.True(t, Any(even, y))
	require.True(t, Any(odd, y))

	z := []int{0, 2, 4, 6, 8}
	require.True(t, Any(even, z))
	require.False(t, Any(odd, z))
}

func TestMap(t *testing.T) {
	inc := func(i int) int { return i + 1 }

	x := []int{0, 2, 4, 6, 8}

	y := Map(inc, x)

	z := []int{1, 3, 5, 7, 9}

	require.True(t, slices.Equal(y, z))
}

func TestFilter(t *testing.T) {
	x := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}

	y := Filter(even, x)

	require.True(t, All(even, y))

	z := Filter(odd, y)

	require.Zero(t, len(z))
}

func TestFoldl(t *testing.T) {
	seed := []int{}
	stupid := func(s []int, a int) []int { return append(s, a) }

	x := []int{0, 1, 2, 3, 4}

	r := Foldl(stupid, seed, x)

	require.True(t, slices.Equal(x, r))
}

func TestFoldr(t *testing.T) {
	seed := []int{}
	stupid := func(a int, s []int) []int { return append(s, a) }

	x := []int{0, 1, 2, 3, 4}

	z := Foldr(stupid, seed, x)

	slices.Reverse[[]int](x)

	require.True(t, slices.Equal(x, z))
}

func TestFind(t *testing.T) {
	x := []int{10, 11, 12, 13, 14, 15}

	div3 := func(a int) bool { return a%3 == 0 }
	div8 := func(a int) bool { return a%8 == 0 }

	require.Equal(t, Find(div3, x), Some(12))

	require.Equal(t, Find(div8, x), None[int]())
}

func TestFlatten(t *testing.T) {
	x := [][]int{{0}, {1}, {2}}

	y := Flatten(x)

	require.True(t, slices.Equal(y, []int{0, 1, 2}))
}

func TestReplicate(t *testing.T) {
	require.True(t, slices.Equal([]int{1, 1, 1, 1, 1}, Replicate(5, 1)))
}

func TestSpan(t *testing.T) {
	x := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	lt5 := func(a int) bool { return a < 5 }

	low, high := Span(lt5, x)

	require.True(t, slices.Equal(low, []int{0, 1, 2, 3, 4}))
	require.True(t, slices.Equal(high, []int{5, 6, 7, 8, 9}))
}

func TestSplitAt(t *testing.T) {
	x := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	fst, snd := SplitAt(5, x)

	require.True(t, slices.Equal(fst, []int{0, 1, 2, 3, 4}))
	require.True(t, slices.Equal(snd, []int{5, 6, 7, 8, 9}))
}

func TestZipWith(t *testing.T) {
	eq := func(a, b int) bool { return a == b }
	x := []int{0, 1, 2, 3, 4}
	y := Replicate(5, 1)
	z := ZipWith(eq, x, y)
	require.True(t, slices.Equal(
		z, []bool{false, true, false, false, false},
	))
}

// TestSum checks if the Sum function correctly calculates the sum of the
// numbers in the slice.
func TestSum(t *testing.T) {
	tests := []struct {
		name   string
		items  interface{}
		result interface{}
	}{
		{
			name:   "Sum of positive integers",
			items:  []int{1, 2, 3},
			result: 6,
		},
		{
			name:   "Sum of negative integers",
			items:  []int{-1, -2, -3},
			result: -6,
		},
		{
			name:   "Sum of float numbers",
			items:  []float64{1.1, 2.2, 3.3},
			result: 6.6,
		},
		{
			name: "Sum of complex numbers",
			items: []complex128{
				complex(1, 1),
				complex(2, 2),
				complex(3, 3),
			},
			result: complex(6, 6),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch v := tt.items.(type) {
			case []int:
				require.Equal(t, tt.result, Sum(v))
			case []float64:
				require.Equal(t, tt.result, Sum(v))
			case []complex128:
				require.Equal(t, tt.result, Sum(v))
			}
		})
	}
}

// TestSliceToMap tests the SliceToMap function.
func TestSliceToMap(t *testing.T) {
	tests := []struct {
		name      string
		slice     []int
		keyFunc   func(int) int
		valueFunc func(int) string
		expected  map[int]string
	}{
		{
			name:    "Integers to string map",
			slice:   []int{1, 2, 3},
			keyFunc: func(a int) int { return a },
			valueFunc: func(a int) string {
				return fmt.Sprintf("Value%d", a)
			},
			expected: map[int]string{
				1: "Value1",
				2: "Value2",
				3: "Value3",
			},
		},
		{
			name:    "Duplicates in slice",
			slice:   []int{1, 2, 2, 3},
			keyFunc: func(a int) int { return a },
			valueFunc: func(a int) string {
				return fmt.Sprintf("Value%d", a)
			},
			expected: map[int]string{
				1: "Value1",
				2: "Value2",
				3: "Value3",
			},
		},
		{
			name:    "Empty slice",
			slice:   []int{},
			keyFunc: func(a int) int { return a },
			valueFunc: func(a int) string {
				return fmt.Sprintf("Value%d", a)
			},
			expected: map[int]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(
				t, tt.expected,
				SliceToMap(tt.slice, tt.keyFunc, tt.valueFunc),
			)
		})
	}
}
