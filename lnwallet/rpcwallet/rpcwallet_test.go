package rpcwallet

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// errNotMine mirrors lnwallet.ErrNotMine for the parts of these tests that
// just need *some* "wallet doesn't know this outpoint" sentinel.
var errNotMine = errors.New("not mine")

// makeOutPoint returns a wire.OutPoint with a unique, deterministic hash so
// each test case can build inputs without colliding.
func makeOutPoint(t *testing.T, idx uint32) wire.OutPoint {
	t.Helper()
	var h chainhash.Hash
	h[0] = byte(idx + 1)

	return wire.OutPoint{Hash: h, Index: idx}
}

// makeTxAndPacket builds a wire tx with the given outpoints and the matching
// empty PSBT skeleton ready for WitnessUtxo to be filled in per input.
func makeTxAndPacket(t *testing.T,
	outpoints []wire.OutPoint) (*wire.MsgTx, *psbt.Packet) {

	t.Helper()
	tx := wire.NewMsgTx(2)
	for _, op := range outpoints {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: op})
	}
	// At least one output is required by psbt.NewFromUnsignedTx.
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: []byte{0x51}})

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return tx, packet
}

// TestPopulateNonSignedInputWitnessUtxosFromWallet verifies that an input
// whose outpoint is known to the wallet is annotated with the wallet's
// view of the prev output.
func TestPopulateNonSignedInputWitnessUtxosFromWallet(t *testing.T) {
	t.Parallel()

	walletOp := makeOutPoint(t, 0)
	signingOp := makeOutPoint(t, 1)

	tx, packet := makeTxAndPacket(
		t, []wire.OutPoint{walletOp, signingOp},
	)

	walletPkScript := []byte{0x51, 0x20, 0xaa, 0xbb}
	fetchInfo := func(op *wire.OutPoint) (*lnwallet.Utxo, error) {
		if *op == walletOp {
			return &lnwallet.Utxo{
				Value:    12345,
				PkScript: walletPkScript,
			}, nil
		}

		return nil, errNotMine
	}

	signDesc := &input.SignDescriptor{InputIndex: 1}
	populateNonSignedInputWitnessUtxos(packet, tx, signDesc, fetchInfo)

	require.NotNil(t, packet.Inputs[0].WitnessUtxo)
	require.Equal(t, int64(12345), packet.Inputs[0].WitnessUtxo.Value)
	require.Equal(t, walletPkScript, packet.Inputs[0].WitnessUtxo.PkScript)

	// The signed input must be untouched.
	require.Nil(t, packet.Inputs[1].WitnessUtxo)
}

// TestPopulateNonSignedInputWitnessUtxosFromFetcher verifies that when the
// wallet does not know about an outpoint, the helper falls back to the
// sign descriptor's PrevOutputFetcher.
func TestPopulateNonSignedInputWitnessUtxosFromFetcher(t *testing.T) {
	t.Parallel()

	externalOp := makeOutPoint(t, 0)
	signingOp := makeOutPoint(t, 1)

	tx, packet := makeTxAndPacket(
		t, []wire.OutPoint{externalOp, signingOp},
	)

	externalUtxo := &wire.TxOut{
		Value:    50000,
		PkScript: []byte{0x51, 0x20, 0xcc, 0xdd},
	}
	prevFetcher := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{externalOp: externalUtxo},
	)

	fetchInfo := func(*wire.OutPoint) (*lnwallet.Utxo, error) {
		return nil, errNotMine
	}

	signDesc := &input.SignDescriptor{
		InputIndex:        1,
		PrevOutputFetcher: prevFetcher,
	}
	populateNonSignedInputWitnessUtxos(packet, tx, signDesc, fetchInfo)

	require.NotNil(t, packet.Inputs[0].WitnessUtxo)
	require.Equal(
		t, externalUtxo.Value, packet.Inputs[0].WitnessUtxo.Value,
	)
	require.True(t, bytes.Equal(
		externalUtxo.PkScript, packet.Inputs[0].WitnessUtxo.PkScript,
	))
}

// TestPopulateNonSignedInputWitnessUtxosZeroValueFromFetcher is the
// regression test for the BIP-322 case. The to_spend output is mandated
// by BIP-322 to have value=0 with the message commitment as its pk_script,
// and that output is referenced as input 0 of every BIP-322 to_sign tx.
// The helper must accept that zero-value entry — otherwise the resulting
// PSBT is rejected by walletkit.SignPsbt with "input (index=N) doesn't
// specify any UTXO info".
func TestPopulateNonSignedInputWitnessUtxosZeroValueFromFetcher(t *testing.T) {
	t.Parallel()

	bip322ToSpendOp := makeOutPoint(t, 0)
	signingOp := makeOutPoint(t, 1)

	tx, packet := makeTxAndPacket(
		t, []wire.OutPoint{bip322ToSpendOp, signingOp},
	)

	// The BIP-322 to_spend output: value=0, pk_script is the message
	// commitment script (P2WSH of OP_0 <msg_hash>). For the purposes of
	// this test the exact script bytes don't matter; only that we have
	// a non-empty pk_script with a zero Value.
	msgCommitment := []byte{0x00, 0x20, 0xde, 0xad, 0xbe, 0xef}
	zeroValueUtxo := &wire.TxOut{Value: 0, PkScript: msgCommitment}

	prevFetcher := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{bip322ToSpendOp: zeroValueUtxo},
	)

	fetchInfo := func(*wire.OutPoint) (*lnwallet.Utxo, error) {
		return nil, errNotMine
	}

	signDesc := &input.SignDescriptor{
		InputIndex:        1,
		PrevOutputFetcher: prevFetcher,
	}
	populateNonSignedInputWitnessUtxos(packet, tx, signDesc, fetchInfo)

	require.NotNil(t, packet.Inputs[0].WitnessUtxo,
		"BIP-322 to_spend output must populate WitnessUtxo "+
			"despite its zero Value")
	require.Equal(t, int64(0), packet.Inputs[0].WitnessUtxo.Value)
	require.Equal(
		t, msgCommitment, packet.Inputs[0].WitnessUtxo.PkScript,
	)
}

// TestPopulateNonSignedInputWitnessUtxosNoFallback verifies the helper
// leaves an input bare when neither the wallet nor a fetcher can resolve
// the outpoint. The caller logs a warning; the unsigned PSBT will still
// fail downstream validation, but that failure should be exposed to the
// caller rather than silently masked.
func TestPopulateNonSignedInputWitnessUtxosNoFallback(t *testing.T) {
	t.Parallel()

	unknownOp := makeOutPoint(t, 0)
	signingOp := makeOutPoint(t, 1)

	tx, packet := makeTxAndPacket(
		t, []wire.OutPoint{unknownOp, signingOp},
	)

	fetchInfo := func(*wire.OutPoint) (*lnwallet.Utxo, error) {
		return nil, errNotMine
	}
	// No PrevOutputFetcher on the sign descriptor.
	signDesc := &input.SignDescriptor{InputIndex: 1}

	populateNonSignedInputWitnessUtxos(packet, tx, signDesc, fetchInfo)

	require.Nil(t, packet.Inputs[0].WitnessUtxo)
}

// TestPopulateNonSignedInputWitnessUtxosEmptyPkScript guards the helper
// against fetchers that return a non-nil TxOut with an empty PkScript.
// Such an entry is not a usable WitnessUtxo (a PSBT WitnessUtxo with an
// empty PkScript is malformed at serialization), so the helper should
// treat it as unknown and skip the input.
func TestPopulateNonSignedInputWitnessUtxosEmptyPkScript(t *testing.T) {
	t.Parallel()

	bogusOp := makeOutPoint(t, 0)
	signingOp := makeOutPoint(t, 1)

	tx, packet := makeTxAndPacket(
		t, []wire.OutPoint{bogusOp, signingOp},
	)

	prevFetcher := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			bogusOp: {Value: 100, PkScript: nil},
		},
	)

	fetchInfo := func(*wire.OutPoint) (*lnwallet.Utxo, error) {
		return nil, errNotMine
	}
	signDesc := &input.SignDescriptor{
		InputIndex:        1,
		PrevOutputFetcher: prevFetcher,
	}

	populateNonSignedInputWitnessUtxos(packet, tx, signDesc, fetchInfo)

	require.Nil(t, packet.Inputs[0].WitnessUtxo)
}
