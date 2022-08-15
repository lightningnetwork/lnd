package btcwallet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

var (
	netParams             = &chaincfg.RegressionNetParams
	testValue      int64  = 345678
	testCSVTimeout uint32 = 2016

	testCommitSecretBytes, _ = hex.DecodeString(
		"9f1f0db609718cf70c580aec6a0e570c3f086ec85a2a6119295b1d64240d" +
			"aca5",
	)
	testCommitSecret, testCommitPoint = btcec.PrivKeyFromBytes(
		testCommitSecretBytes,
	)

	remoteRevocationBasePubKeyBytes, _ = hex.DecodeString(
		"02baf067bfd1a6cf7229c7c459b106d384ad33e948ea1d561f2034475ff1" +
			"7359fb",
	)
	remoteRevocationBasePubKey, _ = btcec.ParsePubKey(
		remoteRevocationBasePubKeyBytes,
	)

	testTweakSingle, _ = hex.DecodeString(
		"020143a30cf6b71ca2af01efbd1758a04b4c7f5c2299f2ea63a8a6b58107" +
			"63b1ed",
	)
)

// testInputType is a type that represents different types of inputs that are
// signed within a PSBT.
type testInputType uint8

const (
	plainP2WKH                  testInputType = 0
	tweakedP2WKH                testInputType = 1
	nestedP2WKH                 testInputType = 2
	singleKeyP2WSH              testInputType = 3
	singleKeyDoubleTweakedP2WSH testInputType = 4
)

func (i testInputType) keyPath() []uint32 {
	switch i {
	case nestedP2WKH:
		return []uint32{
			hardenedKey(waddrmgr.KeyScopeBIP0049Plus.Purpose),
			hardenedKey(0),
			hardenedKey(0),
			0, 0,
		}

	case singleKeyP2WSH:
		return []uint32{
			hardenedKey(keychain.BIP0043Purpose),
			hardenedKey(netParams.HDCoinType),
			hardenedKey(uint32(keychain.KeyFamilyPaymentBase)),
			0, 7,
		}

	case singleKeyDoubleTweakedP2WSH:
		return []uint32{
			hardenedKey(keychain.BIP0043Purpose),
			hardenedKey(netParams.HDCoinType),
			hardenedKey(uint32(keychain.KeyFamilyDelayBase)),
			0, 9,
		}

	default:
		return []uint32{
			hardenedKey(waddrmgr.KeyScopeBIP0084.Purpose),
			hardenedKey(0),
			hardenedKey(0),
			0, 0,
		}
	}
}

func (i testInputType) output(t *testing.T,
	privKey *btcec.PrivateKey) (*wire.TxOut, []byte) {

	var (
		addr          btcutil.Address
		witnessScript []byte
		err           error
	)
	switch i {
	case plainP2WKH:
		h := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
		addr, err = btcutil.NewAddressWitnessPubKeyHash(h, netParams)
		require.NoError(t, err)

	case tweakedP2WKH:
		privKey = input.TweakPrivKey(privKey, testTweakSingle)

		h := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
		addr, err = btcutil.NewAddressWitnessPubKeyHash(h, netParams)
		require.NoError(t, err)

	case nestedP2WKH:
		h := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
		witnessAddr, err := btcutil.NewAddressWitnessPubKeyHash(
			h, netParams,
		)
		require.NoError(t, err)

		witnessProgram, err := txscript.PayToAddrScript(witnessAddr)
		require.NoError(t, err)

		addr, err = btcutil.NewAddressScriptHash(
			witnessProgram, netParams,
		)
		require.NoError(t, err)

	case singleKeyP2WSH:
		// We're simulating a delay-to-self script which we're going to
		// spend through the time lock path. We don't actually need to
		// know the private key of the remote revocation base key.
		revokeKey := input.DeriveRevocationPubkey(
			remoteRevocationBasePubKey, testCommitPoint,
		)
		witnessScript, err = input.CommitScriptToSelf(
			testCSVTimeout, privKey.PubKey(), revokeKey,
		)
		require.NoError(t, err)

		h := sha256.Sum256(witnessScript)
		addr, err = btcutil.NewAddressWitnessScriptHash(h[:], netParams)
		require.NoError(t, err)

	case singleKeyDoubleTweakedP2WSH:
		// We're simulating breaching a remote party's delay-to-self
		// output which we're going to spend through the revocation
		// path. In that case the self key is the other party's self key
		// and, we only know the revocation base private key and commit
		// secret.
		revokeKey := input.DeriveRevocationPubkey(
			privKey.PubKey(), testCommitPoint,
		)
		witnessScript, err = input.CommitScriptToSelf(
			testCSVTimeout, remoteRevocationBasePubKey, revokeKey,
		)
		require.NoError(t, err)

		h := sha256.Sum256(witnessScript)
		addr, err = btcutil.NewAddressWitnessScriptHash(h[:], netParams)
		require.NoError(t, err)

	default:
		t.Fatalf("invalid input type")
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	return &wire.TxOut{
		Value:    testValue,
		PkScript: pkScript,
	}, witnessScript
}

func (i testInputType) decorateInput(t *testing.T, privKey *btcec.PrivateKey,
	in *psbt.PInput) {

	switch i {
	case tweakedP2WKH:
		in.Unknowns = []*psbt.Unknown{{
			Key:   PsbtKeyTypeInputSignatureTweakSingle,
			Value: testTweakSingle,
		}}

	case nestedP2WKH:
		h := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
		witnessAddr, err := btcutil.NewAddressWitnessPubKeyHash(
			h, netParams,
		)
		require.NoError(t, err)

		witnessProgram, err := txscript.PayToAddrScript(witnessAddr)
		require.NoError(t, err)
		in.RedeemScript = witnessProgram

	case singleKeyDoubleTweakedP2WSH:
		in.Unknowns = []*psbt.Unknown{{
			Key:   PsbtKeyTypeInputSignatureTweakDouble,
			Value: testCommitSecret.Serialize(),
		}}
	}
}

func (i testInputType) finalize(t *testing.T, packet *psbt.Packet) {
	in := &packet.Inputs[0]
	sigBytes := in.PartialSigs[0].Signature

	var witnessStack wire.TxWitness
	switch i {
	case singleKeyP2WSH:
		witnessStack = make([][]byte, 3)
		witnessStack[0] = sigBytes
		witnessStack[1] = nil
		witnessStack[2] = in.WitnessScript

	case singleKeyDoubleTweakedP2WSH:
		// Place a 1 as the first item in the evaluated witness stack to
		// force script execution to the revocation clause.
		witnessStack = make([][]byte, 3)
		witnessStack[0] = sigBytes
		witnessStack[1] = []byte{1}
		witnessStack[2] = in.WitnessScript

	default:
		// The PSBT finalizer knows what to do if we're not using a
		// custom script.
		err := psbt.MaybeFinalizeAll(packet)
		require.NoError(t, err)

		return
	}

	var err error
	in.FinalScriptWitness, err = serializeTxWitness(witnessStack)
	require.NoError(t, err)
}

// serializeTxWitness return the wire witness stack into raw bytes.
func serializeTxWitness(txWitness wire.TxWitness) ([]byte, error) {
	var witnessBytes bytes.Buffer
	err := psbt.WriteTxWitness(&witnessBytes, txWitness)
	if err != nil {
		return nil, fmt.Errorf("error serializing witness: %v", err)
	}

	return witnessBytes.Bytes(), nil
}

// TestSignPsbt tests the PSBT signing functionality.
func TestSignPsbt(t *testing.T) {
	w, _ := newTestWallet(t, netParams, seedBytes)

	testCases := []struct {
		name      string
		inputType testInputType
	}{{
		name:      "plain P2WKH",
		inputType: plainP2WKH,
	}, {
		name:      "tweaked P2WKH",
		inputType: tweakedP2WKH,
	}, {
		name:      "nested P2WKH",
		inputType: nestedP2WKH,
	}, {
		name:      "single key P2WSH",
		inputType: singleKeyP2WSH,
	}, {
		name:      "single key double tweaked P2WSH",
		inputType: singleKeyDoubleTweakedP2WSH,
	}}

	for _, tc := range testCases {
		tc := tc

		// This is the private key we're going to sign with.
		privKey, err := w.deriveKeyByBIP32Path(tc.inputType.keyPath())
		require.NoError(t, err)

		txOut, witnessScript := tc.inputType.output(t, privKey)

		// Create the reference transaction that has the input that is
		// going to be spent by our PSBT.
		refTx := wire.NewMsgTx(2)
		refTx.AddTxIn(&wire.TxIn{})
		refTx.AddTxOut(txOut)

		// Create the unsigned spend transaction that is going to be the
		// main content of our PSBT.
		spendTx := wire.NewMsgTx(2)
		spendTx.LockTime = testCSVTimeout
		spendTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  refTx.TxHash(),
				Index: 0,
			},
			Sequence: testCSVTimeout,
		})
		spendTx.AddTxOut(txOut)

		// Convert it to a PSBT now and add all required signing
		// metadata to it.
		packet, err := psbt.NewFromUnsignedTx(spendTx)
		require.NoError(t, err)
		packet.Inputs[0].WitnessScript = witnessScript
		packet.Inputs[0].SighashType = txscript.SigHashAll
		packet.Inputs[0].WitnessUtxo = refTx.TxOut[0]
		packet.Inputs[0].Bip32Derivation = []*psbt.Bip32Derivation{{
			PubKey:    privKey.PubKey().SerializeCompressed(),
			Bip32Path: tc.inputType.keyPath(),
		}}
		tc.inputType.decorateInput(t, privKey, &packet.Inputs[0])

		// Let the wallet do its job. We expect to be the only signer
		// for this PSBT, so we'll be able to finalize it later.
		signedInputs, err := w.SignPsbt(packet)
		require.NoError(t, err)

		// We expect one signed input at index 0.
		require.Len(t, signedInputs, 1)
		require.EqualValues(t, 0, signedInputs[0])

		// If the witness stack needs to be assembled, give the caller
		// the option to do that now.
		tc.inputType.finalize(t, packet)

		finalTx, err := psbt.Extract(packet)
		require.NoError(t, err)

		vm, err := txscript.NewEngine(
			refTx.TxOut[0].PkScript, finalTx, 0,
			txscript.StandardVerifyFlags, nil, nil,
			refTx.TxOut[0].Value,
			txscript.NewCannedPrevOutputFetcher(
				refTx.TxOut[0].PkScript, refTx.TxOut[0].Value,
			),
		)
		require.NoError(t, err)
		require.NoError(t, vm.Execute())
	}
}
