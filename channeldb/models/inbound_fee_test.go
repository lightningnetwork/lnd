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

	require.Equal(t, int64(6), i.CalcFee(2))

	// Expect fee to be rounded down.
	require.Equal(t, int64(6), i.CalcFee(3))

	// Test negative fee.
	i = InboundFee{
		Base: -5,
		Rate: -500000,
	}

	require.Equal(t, int64(-6), i.CalcFee(2))

	// Expect fee to be rounded up.
	require.Equal(t, int64(-6), i.CalcFee(3))
}
