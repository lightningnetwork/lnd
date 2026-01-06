package input

import (
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestZeroFeeAnchorSpendWitness tests that the ZeroFeeAnchorSpend witness type
// generates an empty witness as required by BIP-431/TRUC for P2A outputs.
func TestZeroFeeAnchorSpendWitness(t *testing.T) {
	t.Parallel()

	// Create a dummy transaction for the witness generator.
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    330,
		PkScript: []byte{},
	})

	// Create an empty descriptor - ZeroFeeAnchorSpend doesn't use it,
	// but the WitnessGenerator modifies the descriptor before the switch.
	desc := &SignDescriptor{}

	// Get the witness generator for ZeroFeeAnchorSpend.
	// The signer can be nil since P2A doesn't require signing.
	genFunc := ZeroFeeAnchorSpend.WitnessGenerator(nil, desc)
	require.NotNil(t, genFunc, "WitnessGenerator should return non-nil")

	// Generate the witness script.
	script, err := genFunc(tx, nil, 0)
	require.NoError(t, err, "WitnessGenerator should succeed")
	require.NotNil(t, script, "script should not be nil")

	// Verify the witness is empty (no elements).
	require.Empty(t, script.Witness, "P2A witness must be empty")
}

// TestZeroFeeAnchorSpendSize tests that the ZeroFeeAnchorSpend witness type
// returns the correct size upper bound (1 byte for empty witness).
func TestZeroFeeAnchorSpendSize(t *testing.T) {
	t.Parallel()

	size, isNestedP2SH, err := ZeroFeeAnchorSpend.SizeUpperBound()
	require.NoError(t, err, "SizeUpperBound should succeed")
	require.False(t, isNestedP2SH, "P2A is not nested P2SH")

	// Empty witness = 1 byte (just the count of 0 elements).
	require.EqualValues(t, 1, size, "P2A witness size should be 1")
}

// TestZeroFeeAnchorSpendString tests that the ZeroFeeAnchorSpend witness type
// has the correct string representation.
func TestZeroFeeAnchorSpendString(t *testing.T) {
	t.Parallel()

	require.Equal(t, "ZeroFeeAnchorSpend", ZeroFeeAnchorSpend.String())
}

// TestZeroFeeAnchorSpendValue tests that the ZeroFeeAnchorSpend witness type
// has the expected constant value.
func TestZeroFeeAnchorSpendValue(t *testing.T) {
	t.Parallel()

	// ZeroFeeAnchorSpend should be witness type 35.
	require.EqualValues(t, 35, ZeroFeeAnchorSpend)
}

// TestPayToAnchorScript tests that btcd's PayToAnchorScript creates the
// correct P2A script (OP_1 <0x4e73>).
func TestPayToAnchorScript(t *testing.T) {
	t.Parallel()

	// The P2A script should be: OP_1 (0x51) + PUSH2 (0x02) + 0x4e73
	// But btcd uses witness v1 format: OP_1 (0x51) + data_len (0x02) + data
	expectedScript := txscript.PayToAnchorScript

	// Verify the script is the expected length (4 bytes).
	require.Len(t, expectedScript, 4)

	// Verify it starts with OP_1 (witness version 1).
	require.Equal(t, byte(txscript.OP_1), expectedScript[0])

	// Verify the push data length is 2.
	require.Equal(t, byte(2), expectedScript[1])

	// Verify the anchor data is 0x4e73.
	require.Equal(t, []byte{0x4e, 0x73}, expectedScript[2:])
}

// TestP2AScriptIsPayToAnchor tests that btcd correctly identifies P2A scripts.
func TestP2AScriptIsPayToAnchor(t *testing.T) {
	t.Parallel()

	// PayToAnchorScript should be recognized as P2A.
	require.True(
		t, txscript.IsPayToAnchorScript(txscript.PayToAnchorScript),
		"PayToAnchorScript should be recognized as P2A",
	)

	// Regular P2WPKH should not be recognized as P2A.
	p2wpkh := make([]byte, 22)
	p2wpkh[0] = txscript.OP_0
	p2wpkh[1] = 0x14 // 20 byte push
	require.False(
		t, txscript.IsPayToAnchorScript(p2wpkh),
		"P2WPKH should not be recognized as P2A",
	)

	// Regular P2WSH should not be recognized as P2A.
	p2wsh := make([]byte, 34)
	p2wsh[0] = txscript.OP_0
	p2wsh[1] = 0x20 // 32 byte push
	require.False(
		t, txscript.IsPayToAnchorScript(p2wsh),
		"P2WSH should not be recognized as P2A",
	)
}
