package sqlc

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkMakeQueryParams benchmarks the makeQueryParams function for
// various argument sizes. This helps to ensure the function performs
// efficiently when generating SQL query parameter strings for different
// input sizes.
func BenchmarkMakeQueryParams(b *testing.B) {
	cases := []struct {
		totalArgs int
		listArgs  int
	}{
		{totalArgs: 5, listArgs: 2},
		{totalArgs: 10, listArgs: 3},
		{totalArgs: 50, listArgs: 10},
		{totalArgs: 100, listArgs: 20},
	}

	for _, c := range cases {
		name := fmt.Sprintf(
			"totalArgs=%d/listArgs=%d", c.totalArgs,
			c.listArgs,
		)
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = makeQueryParams(
					c.totalArgs, c.listArgs,
				)
			}
		})
	}
}

// TestMakeQueryParams tests the makeQueryParams function for various
// argument sizes and verifies the output matches the expected SQL
// parameter string. The function is assumed to generate a comma-separated
// list of parameters in the form $N for use in SQL queries.
func TestMakeQueryParams(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		totalArgs int
		listArgs  int
		expected  string
	}{
		{totalArgs: 5, listArgs: 2, expected: "$4,$5"},
		{totalArgs: 10, listArgs: 3, expected: "$8,$9,$10"},
		{totalArgs: 1, listArgs: 1, expected: "$1"},
		{totalArgs: 3, listArgs: 0, expected: ""},
		{totalArgs: 4, listArgs: 4, expected: "$1,$2,$3,$4"},
	}

	for _, tc := range testCases {
		result := makeQueryParams(tc.totalArgs, tc.listArgs)
		require.Equal(
			t, tc.expected, result,
			"unexpected result for totalArgs=%d, "+
				"listArgs=%d", tc.totalArgs, tc.listArgs,
		)
	}
}
