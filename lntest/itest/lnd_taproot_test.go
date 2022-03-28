package itest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	testTaprootKeyFamily = 77
	testAmount           = 800_000
	signMethodBip86      = signrpc.SignMethod_SIGN_METHOD_TAPROOT_KEY_SPEND_BIP0086
	signMethodRootHash   = signrpc.SignMethod_SIGN_METHOD_TAPROOT_KEY_SPEND
	signMethodTapscript  = signrpc.SignMethod_SIGN_METHOD_TAPROOT_SCRIPT_SPEND
)

var (
	dummyInternalKeyBytes, _ = hex.DecodeString(
		"03464805f5468e294d88cf15a3f06aef6c89d63ef1bd7b42db2e0c74c1ac" +
			"eb90fe",
	)
	dummyInternalKey, _ = btcec.ParsePubKey(dummyInternalKeyBytes)
)

// testTaproot ensures that the daemon can send to and spend from taproot (p2tr)
// outputs.
func testTaproot(ht *lntest.HarnessTest) {
	testTaprootComputeInputScriptKeySpendBip86(ht, ht.Alice())
	testTaprootSignOutputRawScriptSpend(ht, ht.Alice())
	testTaprootSignOutputRawKeySpendBip86(ht, ht.Alice())
	testTaprootSignOutputRawKeySpendRootHash(ht, ht.Alice())
	testTaprootMuSig2KeySpendBip86(ht, ht.Alice())
	testTaprootMuSig2KeySpendRootHash(ht, ht.Alice())
	testTaprootMuSig2ScriptSpend(ht, ht.Alice())
	testTaprootMuSig2CombinedLeafKeySpend(ht, ht.Alice())
}

// testTaprootComputeInputScriptKeySpendBip86 tests sending to and spending from
// p2tr key spend only (BIP-0086) addresses through the SendCoins RPC which
// internally uses the ComputeInputScript method for signing.
func testTaprootComputeInputScriptKeySpendBip86(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// We'll start the test by sending Alice some coins, which she'll use
	// to send to herself on a p2tr output.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, alice)

	// Let's create a p2tr address now.
	p2trResp := ht.NewAddress(alice, lnrpc.AddressType_TAPROOT_PUBKEY)

	// Assert this is a segwit v1 address that starts with bcrt1p.
	require.Contains(
		ht, p2trResp.Address, ht.Miner().ActiveNet.Bech32HRPSegwit+"1p",
	)

	// Send the coins from Alice's wallet to her own, but to the new p2tr
	// address.
	req := &lnrpc.SendCoinsRequest{
		Addr:   p2trResp.Address,
		Amount: 0.5 * btcutil.SatoshiPerBitcoin,
	}
	ht.SendCoinFromNode(alice, req)

	// Wait until bob has seen the tx and considers it as owned.
	txid := ht.AssertNumTxsInMempool(1)[0]
	p2trOutputIndex := ht.GetOutputIndex(txid, p2trResp.Address)
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	assertWalletUnspent(ht, alice, op)

	// Mine a block to clean up the mempool.
	ht.MineBlocksAndAssertTx(1, 1)

	// Let's sweep the whole wallet to a new p2tr address, making sure we
	// can sign transactions with v0 and v1 inputs.
	p2trResp = ht.NewAddress(alice, AddrTypeTaprootPubkey)
	req = &lnrpc.SendCoinsRequest{
		Addr:    p2trResp.Address,
		SendAll: true,
	}
	ht.SendCoinFromNode(alice, req)

	// Make sure the coins sent to the address are confirmed correctly,
	// including the confirmation notification.
	confirmAddress(ht, alice, p2trResp.Address)
}

// testTaprootSignOutputRawScriptSpend tests sending to and spending from p2tr
// script addresses using the script path with the SignOutputRaw RPC.
func testTaprootSignOutputRawScriptSpend(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc := ht.DeriveNextKey(alice, req)

	leafSigningKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(ht, err)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))

	// Let's add a second script output as well to test the partial reveal.
	leaf2 := testScriptSchnorrSig(ht.T, leafSigningKey)

	inclusionProof := leaf1.TapHash()
	tapscript := input.TapscriptPartialReveal(
		dummyInternalKey, leaf2, inclusionProof[:],
	)
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(ht, err)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(
		ht, alice, taprootKey, testAmount,
	)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTapscriptInput(
		input.TaprootSignatureWitnessSize, tapscript,
	)
	estimator.AddP2WKHOutput()
	estimatedWeight := int64(estimator.Weight())
	requiredFee := feeRate.FeeForWeight(estimatedWeight)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: p2trOutpoint,
	}}
	value := int64(testAmount - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	utxoInfo := []*signrpc.TxOut{{
		PkScript: p2trPkScript,
		Value:    testAmount,
	}}

	// Before we actually sign, we want to make sure that we get an error
	// when we try to sign for a Taproot output without specifying all UTXO
	// information.
	signReq := &signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs: []*signrpc.SignDescriptor{{
			Output:        utxoInfo[0],
			InputIndex:    0,
			KeyDesc:       keyDesc,
			Sighash:       uint32(txscript.SigHashDefault),
			WitnessScript: leaf2.Script,
			SignMethod:    signMethodTapscript,
		}},
	}
	err = ht.SignOutputRawErr(alice, signReq)
	require.Contains(
		ht, err.Error(), "error signing taproot output, transaction "+
			"input 0 is missing its previous outpoint information",
	)

	// We also want to make sure we get an error when we don't specify the
	// correct signing method.
	signReq = &signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs: []*signrpc.SignDescriptor{{
			Output:        utxoInfo[0],
			InputIndex:    0,
			KeyDesc:       keyDesc,
			Sighash:       uint32(txscript.SigHashDefault),
			WitnessScript: leaf2.Script,
			SignMethod:    signMethodTapscript,
		}},
		PrevOutputs: utxoInfo,
	}
	err = ht.SignOutputRawErr(alice, signReq)
	require.Contains(
		ht, err.Error(), "selected sign method witness_v0 is not "+
			"compatible with given pk script 5120",
	)

	// Do the actual signing now.
	signReq = &signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs: []*signrpc.SignDescriptor{{
			Output:        utxoInfo[0],
			InputIndex:    0,
			KeyDesc:       keyDesc,
			Sighash:       uint32(txscript.SigHashDefault),
			WitnessScript: leaf2.Script,
			SignMethod:    signMethodTapscript,
		}},
		PrevOutputs: utxoInfo,
	}
	signResp := ht.SignOutputRaw(alice, signReq)

	// We can now assemble the witness stack.
	controlBlockBytes, err := tapscript.ControlBlock.ToBytes()
	require.NoError(ht, err)

	tx.TxIn[0].Witness = wire.TxWitness{
		signResp.RawSigs[0],
		leaf2.Script,
		controlBlockBytes,
	}

	// Serialize, weigh and publish the TX now, then make sure the
	// coins are sent and confirmed to the final sweep destination address.
	publishTxAndConfirmSweep(
		ht, alice, tx, estimatedWeight,
		&chainrpc.SpendRequest{
			Outpoint: &chainrpc.Outpoint{
				Hash:  p2trOutpoint.Hash[:],
				Index: p2trOutpoint.Index,
			},
			Script: p2trPkScript,
		},
		p2wkhAddr.String(),
	)
}

// testTaprootSignOutputRawKeySpendBip86 tests that a tapscript address can
// also be spent using the key spend path through the SignOutputRaw RPC using a
// BIP0086 key spend only commitment.
func testTaprootSignOutputRawKeySpendBip86(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc := ht.DeriveNextKey(alice, req)

	internalKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(ht, err)

	// We want to make sure we can still use a tweaked key, even if it ends
	// up being essentially double tweaked because of the taproot root hash.
	dummyKeyTweak := sha256.Sum256([]byte("this is a key tweak"))
	internalKey = input.TweakPubKeyWithTweak(internalKey, dummyKeyTweak[:])

	// Our taproot key is a BIP0086 key spend only construction that just
	// commits to the internal key and no root hash.
	taprootKey := txscript.ComputeTaprootKeyNoScript(internalKey)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(
		ht, alice, taprootKey, testAmount,
	)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	estimator.AddP2WKHOutput()
	estimatedWeight := int64(estimator.Weight())
	requiredFee := feeRate.FeeForWeight(estimatedWeight)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: p2trOutpoint,
	}}
	value := int64(testAmount - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	utxoInfo := []*signrpc.TxOut{{
		PkScript: p2trPkScript,
		Value:    testAmount,
	}}
	signReq := &signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs: []*signrpc.SignDescriptor{{
			Output:      utxoInfo[0],
			InputIndex:  0,
			KeyDesc:     keyDesc,
			SingleTweak: dummyKeyTweak[:],
			Sighash:     uint32(txscript.SigHashDefault),
			SignMethod:  signMethodBip86,
		}},
		PrevOutputs: utxoInfo,
	}
	signResp := ht.SignOutputRaw(alice, signReq)

	tx.TxIn[0].Witness = wire.TxWitness{
		signResp.RawSigs[0],
	}

	// Serialize, weigh and publish the TX now, then make sure the
	// coins are sent and confirmed to the final sweep destination address.
	publishTxAndConfirmSweep(
		ht, alice, tx, estimatedWeight,
		&chainrpc.SpendRequest{
			Outpoint: &chainrpc.Outpoint{
				Hash:  p2trOutpoint.Hash[:],
				Index: p2trOutpoint.Index,
			},
			Script: p2trPkScript,
		},
		p2wkhAddr.String(),
	)
}

// testTaprootSignOutputRawKeySpendRootHash tests that a tapscript address can
// also be spent using the key spend path through the SignOutputRaw RPC using a
// tapscript root hash.
func testTaprootSignOutputRawKeySpendRootHash(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc := ht.DeriveNextKey(alice, req)

	internalKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(ht, err)

	// We want to make sure we can still use a tweaked key, even if it ends
	// up being essentially double tweaked because of the taproot root hash.
	dummyKeyTweak := sha256.Sum256([]byte("this is a key tweak"))
	internalKey = input.TweakPubKeyWithTweak(internalKey, dummyKeyTweak[:])

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))

	rootHash := leaf1.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash[:])

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(
		ht, alice, taprootKey, testAmount,
	)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	estimator.AddP2WKHOutput()
	estimatedWeight := int64(estimator.Weight())
	requiredFee := feeRate.FeeForWeight(estimatedWeight)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: p2trOutpoint,
	}}
	value := int64(testAmount - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	utxoInfo := []*signrpc.TxOut{{
		PkScript: p2trPkScript,
		Value:    testAmount,
	}}
	signReq := &signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs: []*signrpc.SignDescriptor{{
			Output:      utxoInfo[0],
			InputIndex:  0,
			KeyDesc:     keyDesc,
			SingleTweak: dummyKeyTweak[:],
			Sighash:     uint32(txscript.SigHashDefault),
			TapTweak:    rootHash[:],
			SignMethod:  signMethodRootHash,
		}},
		PrevOutputs: utxoInfo,
	}
	signResp := ht.SignOutputRaw(alice, signReq)
	tx.TxIn[0].Witness = wire.TxWitness{
		signResp.RawSigs[0],
	}

	// Serialize, weigh and publish the TX now, then make sure the
	// coins are sent and confirmed to the final sweep destination address.
	publishTxAndConfirmSweep(
		ht, alice, tx, estimatedWeight,
		&chainrpc.SpendRequest{
			Outpoint: &chainrpc.Outpoint{
				Hash:  p2trOutpoint.Hash[:],
				Index: p2trOutpoint.Index,
			},
			Script: p2trPkScript,
		},
		p2wkhAddr.String(),
	)
}

// testTaprootMuSig2KeySpendBip86 tests that a combined MuSig2 key can also be
// used as a BIP-0086 key spend only key.
func testTaprootMuSig2KeySpendBip86(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// We're not going to commit to a script. So our taproot tweak will be
	// empty and just specify the necessary flag.
	taprootTweak := &signrpc.TaprootTweakDesc{
		KeySpendOnly: true,
	}

	keyDesc1, keyDesc2, keyDesc3, allPubKeys := deriveSigningKeys(ht, alice)
	_, taprootKey, sessResp1, sessResp2, sessResp3 := createMuSigSessions(
		ht, alice, taprootTweak, keyDesc1, keyDesc2, keyDesc3,
		allPubKeys,
	)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(
		ht, alice, taprootKey, testAmount,
	)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	estimator.AddP2WKHOutput()
	estimatedWeight := int64(estimator.Weight())
	requiredFee := feeRate.FeeForWeight(estimatedWeight)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: p2trOutpoint,
	}}
	value := int64(testAmount - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	utxoInfo := []*signrpc.TxOut{{
		PkScript: p2trPkScript,
		Value:    testAmount,
	}}

	// We now need to create the raw sighash of the transaction, as that
	// will be the message we're signing collaboratively.
	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(
		utxoInfo[0].PkScript, utxoInfo[0].Value,
	)
	sighashes := txscript.NewTxSigHashes(tx, prevOutputFetcher)

	sigHash, err := txscript.CalcTaprootSignatureHash(
		sighashes, txscript.SigHashDefault, tx, 0, prevOutputFetcher,
	)
	require.NoError(ht, err)

	// Now that we have the transaction prepared, we need to start with the
	// signing. We simulate all three parties here, so we need to do
	// everything three times. But because we're going to use session 1 to
	// combine everything, we don't need its response, as it will store its
	// own signature.
	signReq := &signrpc.MuSig2SignRequest{
		SessionId:     sessResp1.SessionId,
		MessageDigest: sigHash,
	}
	ht.MuSig2Sign(alice, signReq)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp2.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp2 := ht.MuSig2Sign(alice, signReq)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp3 := ht.MuSig2Sign(alice, signReq)

	// Luckily only one of the signers needs to combine the signature, so
	// let's do that now.
	combineReq := &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp2.LocalPartialSignature,
			signResp3.LocalPartialSignature,
		},
	}
	combineResp := ht.MuSig2CombineSig(alice, combineReq)
	require.Equal(ht, true, combineResp.HaveAllSignatures)
	require.NotEmpty(ht, combineResp.FinalSignature)

	sig, err := schnorr.ParseSignature(combineResp.FinalSignature)
	require.NoError(ht, err)
	require.True(ht, sig.Verify(sigHash, taprootKey))

	tx.TxIn[0].Witness = wire.TxWitness{
		combineResp.FinalSignature,
	}

	// Serialize, weigh and publish the TX now, then make sure the
	// coins are sent and confirmed to the final sweep destination address.
	publishTxAndConfirmSweep(
		ht, alice, tx, estimatedWeight,
		&chainrpc.SpendRequest{
			Outpoint: &chainrpc.Outpoint{
				Hash:  p2trOutpoint.Hash[:],
				Index: p2trOutpoint.Index,
			},
			Script: p2trPkScript,
		},
		p2wkhAddr.String(),
	)
}

// testTaprootMuSig2KeySpendRootHash tests that a tapscript address can also be
// spent using a MuSig2 combined key.
func testTaprootMuSig2KeySpendRootHash(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// We're going to commit to a script as well. This is a hash lock with a
	// simple preimage of "foobar". We need to know this upfront so, we can
	// specify the taproot tweak with the root hash when creating the Musig2
	// signing session.
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))
	rootHash := leaf1.TapHash()
	taprootTweak := &signrpc.TaprootTweakDesc{
		ScriptRoot: rootHash[:],
	}

	keyDesc1, keyDesc2, keyDesc3, allPubKeys := deriveSigningKeys(
		ht, alice,
	)
	_, taprootKey, sessResp1, sessResp2, sessResp3 := createMuSigSessions(
		ht, alice, taprootTweak, keyDesc1, keyDesc2, keyDesc3,
		allPubKeys,
	)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(
		ht, alice, taprootKey, testAmount,
	)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	estimator.AddP2WKHOutput()
	estimatedWeight := int64(estimator.Weight())
	requiredFee := feeRate.FeeForWeight(estimatedWeight)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: p2trOutpoint,
	}}
	value := int64(testAmount - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	utxoInfo := []*signrpc.TxOut{{
		PkScript: p2trPkScript,
		Value:    testAmount,
	}}

	// We now need to create the raw sighash of the transaction, as that
	// will be the message we're signing collaboratively.
	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(
		utxoInfo[0].PkScript, utxoInfo[0].Value,
	)
	sighashes := txscript.NewTxSigHashes(tx, prevOutputFetcher)

	sigHash, err := txscript.CalcTaprootSignatureHash(
		sighashes, txscript.SigHashDefault, tx, 0, prevOutputFetcher,
	)
	require.NoError(ht, err)

	// Now that we have the transaction prepared, we need to start with the
	// signing. We simulate all three parties here, so we need to do
	// everything three times. But because we're going to use session 1 to
	// combine everything, we don't need its response, as it will store its
	// own signature.
	req := &signrpc.MuSig2SignRequest{
		SessionId:     sessResp1.SessionId,
		MessageDigest: sigHash,
	}
	ht.MuSig2Sign(alice, req)

	req = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp2.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp2 := ht.MuSig2Sign(alice, req)

	req = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp3 := ht.MuSig2Sign(alice, req)

	// Luckily only one of the signers needs to combine the signature, so
	// let's do that now.
	combineReq := &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp2.LocalPartialSignature,
			signResp3.LocalPartialSignature,
		},
	}
	combineResp := ht.MuSig2CombineSig(alice, combineReq)
	require.Equal(ht, true, combineResp.HaveAllSignatures)
	require.NotEmpty(ht, combineResp.FinalSignature)

	sig, err := schnorr.ParseSignature(combineResp.FinalSignature)
	require.NoError(ht, err)
	require.True(ht, sig.Verify(sigHash, taprootKey))

	tx.TxIn[0].Witness = wire.TxWitness{
		combineResp.FinalSignature,
	}

	// Serialize, weigh and publish the TX now, then make sure the
	// coins are sent and confirmed to the final sweep destination address.
	publishTxAndConfirmSweep(
		ht, alice, tx, estimatedWeight,
		&chainrpc.SpendRequest{
			Outpoint: &chainrpc.Outpoint{
				Hash:  p2trOutpoint.Hash[:],
				Index: p2trOutpoint.Index,
			},
			Script: p2trPkScript,
		},
		p2wkhAddr.String(),
	)
}

// testTaprootMuSig2ScriptSpend tests that a tapscript address with an internal
// key that is a MuSig2 combined key can also be spent using the script path.
func testTaprootMuSig2ScriptSpend(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// We're going to commit to a script and spend the output using the
	// script. This is a hash lock with a simple preimage of "foobar". We
	// need to know this upfront so, we can specify the taproot tweak with
	// the root hash when creating the Musig2 signing session.
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))
	rootHash := leaf1.TapHash()
	taprootTweak := &signrpc.TaprootTweakDesc{
		ScriptRoot: rootHash[:],
	}

	keyDesc1, keyDesc2, keyDesc3, allPubKeys := deriveSigningKeys(
		ht, alice,
	)
	internalKey, taprootKey, _, _, _ := createMuSigSessions(
		ht, alice, taprootTweak, keyDesc1, keyDesc2, keyDesc3,
		allPubKeys,
	)

	// Because we know the internal key and the script we want to spend, we
	// can now create the tapscript struct that's used for assembling the
	// control block and fee estimation.
	tapscript := input.TapscriptFullTree(internalKey, leaf1)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(
		ht, alice, taprootKey, testAmount,
	)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTapscriptInput(
		len([]byte("foobar"))+len(leaf1.Script)+1, tapscript,
	)
	estimator.AddP2WKHOutput()
	estimatedWeight := int64(estimator.Weight())
	requiredFee := feeRate.FeeForWeight(estimatedWeight)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: p2trOutpoint,
	}}
	value := int64(testAmount - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	// We can now assemble the witness stack.
	controlBlockBytes, err := tapscript.ControlBlock.ToBytes()
	require.NoError(ht, err)

	tx.TxIn[0].Witness = wire.TxWitness{
		[]byte("foobar"),
		leaf1.Script,
		controlBlockBytes,
	}

	// Serialize, weigh and publish the TX now, then make sure the
	// coins are sent and confirmed to the final sweep destination address.
	publishTxAndConfirmSweep(
		ht, alice, tx, estimatedWeight,
		&chainrpc.SpendRequest{
			Outpoint: &chainrpc.Outpoint{
				Hash:  p2trOutpoint.Hash[:],
				Index: p2trOutpoint.Index,
			},
			Script: p2trPkScript,
		},
		p2wkhAddr.String(),
	)
}

// testTaprootMuSig2CombinedLeafKeySpend tests that a MuSig2 combined key can be
// used for an OP_CHECKSIG inside a tap script leaf spend.
func testTaprootMuSig2CombinedLeafKeySpend(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// We're using the combined MuSig2 key in a script leaf. So we need to
	// derive the combined key first, before we can build the script.
	keyDesc1, keyDesc2, keyDesc3, allPubKeys := deriveSigningKeys(
		ht, alice,
	)
	req := &signrpc.MuSig2CombineKeysRequest{
		AllSignerPubkeys: allPubKeys,
	}
	combineResp := ht.MuSig2CombineKeys(alice, req)
	combinedPubKey, err := schnorr.ParsePubKey(combineResp.CombinedKey)
	require.NoError(ht, err)

	// We're going to commit to a script and spend the output using the
	// script. This is just an OP_CHECKSIG with the combined MuSig2 public
	// key.
	leaf := testScriptSchnorrSig(ht.T, combinedPubKey)
	tapscript := input.TapscriptPartialReveal(dummyInternalKey, leaf, nil)
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(ht, err)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(
		ht, alice, taprootKey, testAmount,
	)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTapscriptInput(
		input.TaprootSignatureWitnessSize, tapscript,
	)
	estimator.AddP2WKHOutput()
	estimatedWeight := int64(estimator.Weight())
	requiredFee := feeRate.FeeForWeight(estimatedWeight)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: p2trOutpoint,
	}}
	value := int64(testAmount - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	utxoInfo := []*signrpc.TxOut{{
		PkScript: p2trPkScript,
		Value:    testAmount,
	}}

	// Do the actual signing now.
	_, _, sessResp1, sessResp2, sessResp3 := createMuSigSessions(
		ht, alice, nil, keyDesc1, keyDesc2, keyDesc3, allPubKeys,
	)
	require.NoError(ht, err)

	// We now need to create the raw sighash of the transaction, as that
	// will be the message we're signing collaboratively.
	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(
		utxoInfo[0].PkScript, utxoInfo[0].Value,
	)
	sighashes := txscript.NewTxSigHashes(tx, prevOutputFetcher)

	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sighashes, txscript.SigHashDefault, tx, 0, prevOutputFetcher,
		leaf,
	)
	require.NoError(ht, err)

	// Now that we have the transaction prepared, we need to start with the
	// signing. We simulate all three parties here, so we need to do
	// everything three times. But because we're going to use session 1 to
	// combine everything, we don't need its response, as it will store its
	// own signature.
	signReq := &signrpc.MuSig2SignRequest{
		SessionId:     sessResp1.SessionId,
		MessageDigest: sigHash,
	}
	ht.MuSig2Sign(alice, signReq)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp2.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp2 := ht.MuSig2Sign(alice, signReq)

	// Before we have all partial signatures, we shouldn't get a final
	// signature back.
	combineReq := &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp2.LocalPartialSignature,
		},
	}
	combineSigResp := ht.MuSig2CombineSig(alice, combineReq)
	require.False(ht, combineSigResp.HaveAllSignatures)
	require.Empty(ht, combineSigResp.FinalSignature)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: sigHash,
	}
	signResp3 := ht.MuSig2Sign(alice, signReq)

	// We manually clean up session 3, just to make sure that works as well.
	cleanReq := &signrpc.MuSig2CleanupRequest{
		SessionId: sessResp3.SessionId,
	}
	ht.MuSig2Cleanup(alice, cleanReq)

	// A second call to that cleaned up session should now fail with a
	// specific error.
	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: sigHash,
	}
	err = ht.MuSig2SignErr(alice, signReq)
	require.Contains(ht, err.Error(), "not found")

	// Luckily only one of the signers needs to combine the signature, so
	// let's do that now.
	combineReq = &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp3.LocalPartialSignature,
		},
	}
	combineResp1 := ht.MuSig2CombineSig(alice, combineReq)
	require.Equal(ht, true, combineResp1.HaveAllSignatures)
	require.NotEmpty(ht, combineResp1.FinalSignature)

	sig, err := schnorr.ParseSignature(combineResp1.FinalSignature)
	require.NoError(ht, err)
	require.True(ht, sig.Verify(sigHash, combinedPubKey))

	// We can now assemble the witness stack.
	controlBlockBytes, err := tapscript.ControlBlock.ToBytes()
	require.NoError(ht, err)

	tx.TxIn[0].Witness = wire.TxWitness{
		combineResp1.FinalSignature,
		leaf.Script,
		controlBlockBytes,
	}

	// Serialize, weigh and publish the TX now, then make sure the
	// coins are sent and confirmed to the final sweep destination address.
	publishTxAndConfirmSweep(
		ht, alice, tx, estimatedWeight,
		&chainrpc.SpendRequest{
			Outpoint: &chainrpc.Outpoint{
				Hash:  p2trOutpoint.Hash[:],
				Index: p2trOutpoint.Index,
			},
			Script: p2trPkScript,
		},
		p2wkhAddr.String(),
	)
}

// testScriptHashLock returns a simple bitcoin script that locks the funds to
// a hash lock of the given preimage.
func testScriptHashLock(t *testing.T, preimage []byte) txscript.TapLeaf {
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_DUP)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(btcutil.Hash160(preimage))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	script1, err := builder.Script()
	require.NoError(t, err)
	return txscript.NewBaseTapLeaf(script1)
}

// testScriptSchnorrSig returns a simple bitcoin script that locks the funds to
// a Schnorr signature of the given public key.
func testScriptSchnorrSig(t *testing.T,
	pubKey *btcec.PublicKey) txscript.TapLeaf {

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(pubKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	script2, err := builder.Script()
	require.NoError(t, err)
	return txscript.NewBaseTapLeaf(script2)
}

// newAddrWithScript returns a new address and its pkScript.
func newAddrWithScript(ht *lntest.HarnessTest, node *lntest.HarnessNode,
	addrType lnrpc.AddressType) (btcutil.Address, []byte) {

	p2wkhResp := ht.NewAddress(node, addrType)
	p2wkhAddr, err := btcutil.DecodeAddress(
		p2wkhResp.Address, harnessNetParams,
	)
	require.NoError(ht, err)

	p2wkhPkScript, err := txscript.PayToAddrScript(p2wkhAddr)
	require.NoError(ht, err)

	return p2wkhAddr, p2wkhPkScript
}

// sendToTaprootOutput sends coins to a p2tr output of the given taproot key and
// mines a block to confirm the coins.
func sendToTaprootOutput(ht *lntest.HarnessTest, node *lntest.HarnessNode,
	taprootKey *btcec.PublicKey, amt int64) (wire.OutPoint, []byte) {

	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(ht, err)

	// Send some coins to the generated tapscript address.
	req := &lnrpc.SendCoinsRequest{
		Addr:   tapScriptAddr.String(),
		Amount: amt,
	}
	ht.SendCoinFromNode(node, req)

	// Wait until the TX is found in the mempool.
	txid := ht.AssertNumTxsInMempool(1)[0]
	p2trOutputIndex := ht.GetOutputIndex(txid, tapScriptAddr.String())
	p2trOutpoint := wire.OutPoint{
		Hash:  *txid,
		Index: uint32(p2trOutputIndex),
	}

	// Clear the mempool.
	ht.MineBlocksAndAssertTx(1, 1)

	return p2trOutpoint, p2trPkScript
}

// publishTxAndConfirmSweep is a helper function that publishes a transaction
// after checking its weight against an estimate. After asserting the given
// spend request, the given sweep address' balance is verified to be seen as
// funds belonging to the wallet.
func publishTxAndConfirmSweep(ht *lntest.HarnessTest, node *lntest.HarnessNode,
	tx *wire.MsgTx, estimatedWeight int64,
	spendRequest *chainrpc.SpendRequest, sweepAddr string) {

	// Before we publish the tx that spends the p2tr transaction, we want to
	// register a spend listener that we expect to fire after mining the
	// block.
	_, currentHeight := ht.GetBestBlock()

	// For a Taproot output we cannot leave the outpoint empty. Let's make
	// sure the API returns the correct error here.
	req := &chainrpc.SpendRequest{
		Script:     spendRequest.Script,
		HeightHint: uint32(currentHeight),
	}
	spendClient := ht.RegisterSpendNtfn(node, req)

	// The error is only thrown when trying to read a message.
	_, err := spendClient.Recv()
	require.Contains(
		ht, err.Error(),
		"cannot register witness v1 spend request without outpoint",
	)

	// Now try again, this time with the outpoint set.
	req = &chainrpc.SpendRequest{
		Outpoint:   spendRequest.Outpoint,
		Script:     spendRequest.Script,
		HeightHint: uint32(currentHeight),
	}
	spendClient = ht.RegisterSpendNtfn(node, req)

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	// Since Schnorr signatures are fixed size, we must be able to estimate
	// the size of this transaction exactly.
	txWeight := blockchain.GetTransactionWeight(btcutil.NewTx(tx))
	require.Equal(ht, estimatedWeight, txWeight)

	txReq := &walletrpc.Transaction{
		TxHex: buf.Bytes(),
	}
	ht.PublishTransaction(node, txReq)

	// Make sure the coins sent to the address are confirmed correctly,
	// including the confirmation notification.
	confirmAddress(ht, node, sweepAddr)

	// We now expect our spend event to go through.
	spendMsg, err := spendClient.Recv()
	require.NoError(ht, err)
	spend := spendMsg.GetSpend()
	require.NotNil(ht, spend)
	require.Equal(ht, spend.SpendingHeight, uint32(currentHeight+1))
}

// confirmAddress makes sure that a transaction in the mempool spends funds to
// the given address. It also checks that a confirmation notification for the
// address is triggered when the transaction is mined.
func confirmAddress(ht *lntest.HarnessTest, node *lntest.HarnessNode,
	addrString string) {

	// Wait until the tx that sends to the address is found.
	txid := ht.AssertNumTxsInMempool(1)[0]

	// Wait until bob has seen the tx and considers it as owned.
	addrOutputIndex := ht.GetOutputIndex(txid, addrString)
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(addrOutputIndex),
	}
	assertWalletUnspent(ht, node, op)

	// Before we confirm the transaction, let's register a confirmation
	// listener for it, which we expect to fire after mining a block.
	parsedAddr, err := btcutil.DecodeAddress(addrString, harnessNetParams)
	require.NoError(ht, err)
	addrPkScript, err := txscript.PayToAddrScript(parsedAddr)
	require.NoError(ht, err)

	_, currentHeight := ht.GetBestBlock()
	req := &chainrpc.ConfRequest{
		Script:     addrPkScript,
		Txid:       txid[:],
		HeightHint: uint32(currentHeight),
		NumConfs:   1,
	}
	confClient := ht.RegisterConfirmationsNtfn(node, req)

	// Mine another block to clean up the mempool.
	ht.MineBlocksAndAssertTx(1, 1)

	// We now expect our confirmation to go through.
	confMsg, err := confClient.Recv()
	require.NoError(ht, err)
	conf := confMsg.GetConf()
	require.NotNil(ht, conf)
	require.Equal(ht, conf.BlockHeight, uint32(currentHeight+1))
}

// deriveSigningKeys derives three signing keys and returns their descriptors,
// as well as the public keys in the Schnorr serialized format.
func deriveSigningKeys(ht *lntest.HarnessTest,
	node *lntest.HarnessNode) (*signrpc.KeyDescriptor,
	*signrpc.KeyDescriptor, *signrpc.KeyDescriptor, [][]byte) {

	// For muSig2 we need multiple keys. We derive three of them from the
	// same wallet, just so we know we can also sign for them again.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc1 := ht.DeriveNextKey(node, req)
	pubKey1, err := btcec.ParsePubKey(keyDesc1.RawKeyBytes)
	require.NoError(ht, err)

	keyDesc2 := ht.DeriveNextKey(node, req)
	pubKey2, err := btcec.ParsePubKey(keyDesc2.RawKeyBytes)
	require.NoError(ht, err)

	keyDesc3 := ht.DeriveNextKey(node, req)
	pubKey3, err := btcec.ParsePubKey(keyDesc3.RawKeyBytes)
	require.NoError(ht, err)

	// Now that we have all three keys we can create three sessions, one
	// for each of the signers. This would of course normally not happen on
	// the same node.
	allPubKeys := [][]byte{
		schnorr.SerializePubKey(pubKey1),
		schnorr.SerializePubKey(pubKey2),
		schnorr.SerializePubKey(pubKey3),
	}

	return keyDesc1, keyDesc2, keyDesc3, allPubKeys
}

// createMuSigSessions creates a MuSig2 session with three keys that are
// combined into a single key. The same node is used for the three signing
// participants but a separate key is generated for each session. So the result
// should be the same as if it were three different nodes.
func createMuSigSessions(ht *lntest.HarnessTest, node *lntest.HarnessNode,
	taprootTweak *signrpc.TaprootTweakDesc,
	keyDesc1, keyDesc2, keyDesc3 *signrpc.KeyDescriptor,
	allPubKeys [][]byte) (*btcec.PublicKey, *btcec.PublicKey,
	*signrpc.MuSig2SessionResponse, *signrpc.MuSig2SessionResponse,
	*signrpc.MuSig2SessionResponse) {

	req := &signrpc.MuSig2SessionRequest{
		KeyLoc:           keyDesc1.KeyLoc,
		AllSignerPubkeys: allPubKeys,
		TaprootTweak:     taprootTweak,
	}
	sessResp1 := ht.MuSig2CreateSession(node, req)

	// Now that we have the three keys in a combined form, we want to make
	// sure the tweaking for the taproot key worked correctly. We first need
	// to parse the combined key without any tweaks applied to it. That will
	// be our internal key. Once we know that, we can tweak it with the
	// tapHash of the script root hash. We should arrive at the same result
	// as the API.
	combinedKey, err := schnorr.ParsePubKey(sessResp1.CombinedKey)
	require.NoError(ht, err)

	// When combining the key without creating a session, we expect the same
	// combined key to be created.
	expectedCombinedKey := combinedKey

	// Without a tweak, the internal key is equal to the combined key.
	internalKey := combinedKey

	// If there is a tweak, then there is the internal, pre-tweaked combined
	// key and the taproot key which is fully tweaked.
	if taprootTweak != nil {
		internalKey, err = schnorr.ParsePubKey(
			sessResp1.TaprootInternalKey,
		)
		require.NoError(ht, err)

		// We now know the taproot key. The session with the tweak
		// applied should produce the same key!
		expectedCombinedKey = txscript.ComputeTaprootOutputKey(
			internalKey, taprootTweak.ScriptRoot,
		)
		require.Equal(
			ht, schnorr.SerializePubKey(expectedCombinedKey),
			schnorr.SerializePubKey(combinedKey),
		)
	}

	// We should also get the same keys when just calling the
	// MuSig2CombineKeys RPC.
	combineReq := &signrpc.MuSig2CombineKeysRequest{
		AllSignerPubkeys: allPubKeys,
		TaprootTweak:     taprootTweak,
	}
	combineResp := ht.MuSig2CombineKeys(node, combineReq)
	require.Equal(
		ht, schnorr.SerializePubKey(expectedCombinedKey),
		combineResp.CombinedKey,
	)
	require.Equal(
		ht, schnorr.SerializePubKey(internalKey),
		combineResp.TaprootInternalKey,
	)

	// Everything is good so far, let's continue with creating the signing
	// session for the other two participants.
	req = &signrpc.MuSig2SessionRequest{
		KeyLoc:           keyDesc2.KeyLoc,
		AllSignerPubkeys: allPubKeys,
		OtherSignerPublicNonces: [][]byte{
			sessResp1.LocalPublicNonces,
		},
		TaprootTweak: taprootTweak,
	}
	sessResp2 := ht.MuSig2CreateSession(node, req)
	require.Equal(ht, sessResp1.CombinedKey, sessResp2.CombinedKey)

	req = &signrpc.MuSig2SessionRequest{
		KeyLoc:           keyDesc3.KeyLoc,
		AllSignerPubkeys: allPubKeys,
		OtherSignerPublicNonces: [][]byte{
			sessResp1.LocalPublicNonces,
			sessResp2.LocalPublicNonces,
		},
		TaprootTweak: taprootTweak,
	}
	sessResp3 := ht.MuSig2CreateSession(node, req)
	require.NoError(ht, err)
	require.Equal(ht, sessResp2.CombinedKey, sessResp3.CombinedKey)
	require.Equal(ht, true, sessResp3.HaveAllNonces)

	// We need to distribute the rest of the nonces.
	nonceReq := &signrpc.MuSig2RegisterNoncesRequest{
		SessionId: sessResp1.SessionId,
		OtherSignerPublicNonces: [][]byte{
			sessResp2.LocalPublicNonces,
			sessResp3.LocalPublicNonces,
		},
	}
	nonceResp1 := ht.MuSig2RegisterNonces(node, nonceReq)
	require.True(ht, nonceResp1.HaveAllNonces)

	nonceReq = &signrpc.MuSig2RegisterNoncesRequest{
		SessionId: sessResp2.SessionId,
		OtherSignerPublicNonces: [][]byte{
			sessResp3.LocalPublicNonces,
		},
	}
	nonceResp2 := ht.MuSig2RegisterNonces(node, nonceReq)
	require.True(ht, nonceResp2.HaveAllNonces)

	return internalKey, combinedKey, sessResp1, sessResp2, sessResp3
}
