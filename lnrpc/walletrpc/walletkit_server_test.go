//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWitnessTypeMapping tests that the two witness type enums in the `input`
// package and the `walletrpc` package remain equal.
func TestWitnessTypeMapping(t *testing.T) {
	t.Parallel()

	// Tests that both enum types have the same length except the
	// UNKNOWN_WITNESS type which is only present in the walletrpc
	// witness type enum.
	require.Equal(
		t, len(allWitnessTypes), len(WitnessType_name)-1,
		"number of witness types should match proto definition",
	)

	// Tests that the string representations of both enum types are
	// equivalent.
	for witnessType, witnessTypeProto := range allWitnessTypes {
		// Redeclare to avoid loop variables being captured
		// by func literal.
		witnessType := witnessType
		witnessTypeProto := witnessTypeProto

		t.Run(witnessType.String(), func(tt *testing.T) {
			tt.Parallel()

			witnessTypeName := witnessType.String()
			witnessTypeName = strings.ToUpper(witnessTypeName)
			witnessTypeProtoName := witnessTypeProto.String()
			witnessTypeProtoName = strings.ReplaceAll(
				witnessTypeProtoName, "_", "",
			)

			require.Equal(
				t, witnessTypeName, witnessTypeProtoName,
				"mapped witness types should be named the same",
			)
		})
	}
}
