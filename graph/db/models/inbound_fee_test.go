package models

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInboundFee(t *testing.T) {
	t.Parallel()

	// Test positive fee.
	i := InboundFee{
		Base: 5,
		Rate: 500000,
	}

	fee, err := i.CalcFee(2)
	require.NoError(t, err)
	require.Equal(t, int64(6), fee)

	// Expect fee to be rounded down.
	fee, err = i.CalcFee(3)
	require.NoError(t, err)
	require.Equal(t, int64(6), fee)

	// Test negative fee.
	i = InboundFee{
		Base: -5,
		Rate: -500000,
	}

	fee, err = i.CalcFee(2)
	require.NoError(t, err)
	require.Equal(t, int64(-6), fee)

	// Expect fee to be rounded up.
	fee, err = i.CalcFee(3)
	require.NoError(t, err)
	require.Equal(t, int64(-6), fee)
}
