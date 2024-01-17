package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestBumpResultValidate tests the validate method of the BumpResult struct.
func TestBumpResultValidate(t *testing.T) {
	t.Parallel()

	// An empty result will give an error.
	b := BumpResult{}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// Unknown event type will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: sentinalEvent,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A replacing event without a new tx will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxReplaced,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A failed event without a failure reason will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxFailed,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A confirmed event without fee info will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxConfirmed,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// Test a valid result.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxPublished,
	}
	require.NoError(t, b.Validate())
}
