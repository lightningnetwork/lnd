package lntypes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTxSizeUnit tests the conversion of tx size to different units.
func TestTxSizeUnit(t *testing.T) {
	t.Parallel()

	// Test normal conversion from 100wu to 25vb.
	wu := WeightUnit(100)
	vb := VByte(25)
	require.Equal(t, vb, wu.ToVB(), "wu -> vb conversion "+
		"failed: want %v, got %v", vb, wu.ToVB())
	require.Equal(t, wu, vb.ToWU(), "vb -> wu conversion "+
		"failed: want %v, got %v", wu, vb.ToWU())

	// Test rounding up conversion from 99wu to 25vb.
	wu = WeightUnit(99)
	vb = VByte(25)
	require.Equal(t, vb, wu.ToVB(), "wu -> vb conversion "+
		"failed: want %v, got %v", vb, wu.ToVB())
	require.Equal(t, WeightUnit(100), vb.ToWU(), "vb -> wu conversion "+
		"failed: want %v, got %v", 100, vb.ToWU())
}
