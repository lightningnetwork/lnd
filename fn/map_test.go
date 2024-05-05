package fn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeySet(t *testing.T) {
	testMap := map[string]int{"a": 1, "b": 2, "c": 3}
	expected := NewSet([]string{"a", "b", "c"}...)

	require.Equal(t, expected, KeySet(testMap))
}

// TestNewSubMap tests the NewSubMap function with various input cases.
func TestNewSubMap(t *testing.T) {
	tests := []struct {
		name     string
		original map[int]string
		keys     []int
		expected map[int]string
		wantErr  bool
	}{
		{
			name: "Successful submap creation",
			original: map[int]string{
				1: "apple",
				2: "banana",
				3: "cherry",
			},
			keys: []int{1, 3},
			expected: map[int]string{
				1: "apple",
				3: "cherry",
			},
			wantErr: false,
		},
		{
			name: "Key not found",
			original: map[int]string{
				1: "apple",
				2: "banana",
			},
			keys:     []int{1, 4},
			expected: nil,
			wantErr:  true,
		},
		{
			name: "Empty keys list",
			original: map[int]string{
				1: "apple",
				2: "banana",
			},
			keys:     []int{},
			expected: map[int]string{},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NewSubMap(tt.original, tt.keys)
			if tt.wantErr {
				require.ErrorContains(
					t, err, "NewSubMap: missing key",
				)

				require.Nil(t, result)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expected, result)
		})
	}
}

// TestNewSubMapIntersect tests the NewSubMapIntersect function for correctness.
func TestNewSubMapIntersect(t *testing.T) {
	tests := []struct {
		name     string
		original map[int]string
		keys     []int
		expected map[int]string
	}{
		{
			name: "Successful intersection",
			original: map[int]string{
				1: "apple",
				2: "banana",
				3: "cherry",
				4: "date",
			},
			keys: []int{2, 3, 5},
			expected: map[int]string{
				2: "banana",
				3: "cherry",
			},
		},
		{
			name: "No intersection",
			original: map[int]string{
				1: "apple",
				2: "banana",
			},
			keys:     []int{3, 4},
			expected: map[int]string{},
		},
		{
			name:     "Empty original map",
			original: map[int]string{},
			keys:     []int{1, 2},
			expected: map[int]string{},
		},
		{
			name: "Empty keys list",
			original: map[int]string{
				1: "apple",
				2: "banana",
			},
			keys:     []int{},
			expected: map[int]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(
				t, tt.expected,
				NewSubMapIntersect(tt.original, tt.keys))
		})
	}
}
