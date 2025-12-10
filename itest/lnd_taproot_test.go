package itest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntypes"
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
	hexDecode = func(keyStr string) []byte {
		keyBytes, _ := hex.DecodeString(keyStr)
		return keyBytes
	}
	dummyInternalKey, _ = btcec.ParsePubKey(hexDecode(
		"03464805f5468e294d88cf15a3f06aef6c89d63ef1bd7b42db2e0c74c1ac" +
			"eb90fe",
	))
)

// testTaprootSpend ensures that the daemon can send to and spend from taproot
// (p2tr) outputs.
func testTaprootSpend(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	testTaprootSendCoinsKeySpendBip86(ht, alice)
	testTaprootComputeInputScriptKeySpendBip86(ht, alice)
	testTaprootSignOutputRawScriptSpend(ht, alice)
	testTaprootSignOutputRawScriptSpend(
		ht, alice, txscript.SigHashSingle,
	)
	testTaprootSignOutputRawKeySpendBip86(ht, alice)
	testTaprootSignOutputRawKeySpendBip86(
		ht, alice, txscript.SigHashSingle,
	)
	testTaprootSignOutputRawKeySpendRootHash(ht, alice)
}

// testTaprootMuSig2 ensures that the daemon can send to and spend from taproot
// (p2tr) outputs using musig2.
func testTaprootMuSig2(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)

	muSig2Versions := []signrpc.MuSig2Version{
		signrpc.MuSig2Version_MUSIG2_VERSION_V040,
		signrpc.MuSig2Version_MUSIG2_VERSION_V100RC2,
	}
	for _, version := range muSig2Versions {
		testTaprootMuSig2KeySpendBip86(ht, alice, version)
		testTaprootMuSig2KeySpendRootHash(ht, alice, version)
		testTaprootMuSig2ScriptSpend(ht, alice, version)
		testTaprootMuSig2CombinedLeafKeySpend(ht, alice, version)
		testMuSig2CombineKey(ht, alice, version)
		testTaprootMuSig2CombinedNonceCoordinator(ht, alice, version)
	}
}

// testTaprootImportScripts ensures that the daemon can import taproot scripts.
func testTaprootImportScripts(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)

	testTaprootImportTapscriptFullTree(ht, alice)
	testTaprootImportTapscriptPartialReveal(ht, alice)
	testTaprootImportTapscriptRootHashOnly(ht, alice)
	testTaprootImportTapscriptFullKey(ht, alice)

	testTaprootImportTapscriptFullKeyFundPsbt(ht, alice)
}

// testTaprootSendCoinsKeySpendBip86 tests sending to and spending from
// p2tr key spend only (BIP-0086) addresses through the SendCoins RPC which
// internally uses the ComputeInputScript method for signing.
func testTaprootSendCoinsKeySpendBip86(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// We'll start the test by sending Alice some coins, which she'll use to
	// send to herself on a p2tr output.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Let's create a p2tr address now.
	p2trResp := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: AddrTypeTaprootPubkey,
	})

	// Assert this is a segwit v1 address that starts with bcrt1p.
	require.Contains(
		ht, p2trResp.Address, ht.Miner().ActiveNet.Bech32HRPSegwit+"1p",
	)

	// Send the coins from Alice's wallet to her own, but to the new p2tr
	// address.
	alice.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:       p2trResp.Address,
		Amount:     0.5 * btcutil.SatoshiPerBitcoin,
		TargetConf: 6,
	})

	txid := ht.AssertNumTxsInMempool(1)[0]

	// Wait until bob has seen the tx and considers it as owned.
	p2trOutputIndex := ht.GetOutputIndex(txid, p2trResp.Address)
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	ht.AssertUTXOInWallet(alice, op, "")

	// Mine a block to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Let's sweep the whole wallet to a new p2tr address, making sure we
	// can sign transactions with v0 and v1 inputs.
	p2trResp = alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})

	alice.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:       p2trResp.Address,
		SendAll:    true,
		TargetConf: 6,
	})

	// Make sure the coins sent to the address are confirmed correctly,
	// including the confirmation notification.
	confirmAddress(ht, alice, p2trResp.Address)
}

// testTaprootComputeInputScriptKeySpendBip86 tests sending to and spending from
// p2tr key spend only (BIP-0086) addresses through the SendCoins RPC which
// internally uses the ComputeInputScript method for signing.
func testTaprootComputeInputScriptKeySpendBip86(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// We'll start the test by sending Alice some coins, which she'll use
	// to send to herself on a p2tr output.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Let's create a p2tr address now.
	p2trAddr, p2trPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_TAPROOT_PUBKEY,
	)

	// Send the coins from Alice's wallet to her own, but to the new p2tr
	// address.
	req := &lnrpc.SendCoinsRequest{
		Addr:       p2trAddr.String(),
		Amount:     testAmount,
		TargetConf: 6,
	}
	alice.RPC.SendCoins(req)

	// Wait until bob has seen the tx and considers it as owned.
	txid := ht.AssertNumTxsInMempool(1)[0]
	p2trOutputIndex := ht.GetOutputIndex(txid, p2trAddr.String())
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	ht.AssertUTXOInWallet(alice, op, "")

	p2trOutpoint := wire.OutPoint{
		Hash:  txid,
		Index: uint32(p2trOutputIndex),
	}

	// Mine a block to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// We'll send the coins back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	estimator.AddP2WKHOutput()
	estimatedWeight := estimator.Weight()
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
			Output:     utxoInfo[0],
			InputIndex: 0,
			Sighash:    uint32(txscript.SigHashDefault),
		}},
		PrevOutputs: utxoInfo,
	}
	signResp := alice.RPC.ComputeInputScript(signReq)

	tx.TxIn[0].Witness = signResp.InputScripts[0].Witness

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

// testTaprootSignOutputRawScriptSpend tests sending to and spending from p2tr
// script addresses using the script path with the SignOutputRaw RPC.
func testTaprootSignOutputRawScriptSpend(ht *lntest.HarnessTest,
	alice *node.HarnessNode, sigHashType ...txscript.SigHashType) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc := alice.RPC.DeriveNextKey(req)

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
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

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

	estimatedWeight := estimator.Weight()
	sigHash := txscript.SigHashDefault
	if len(sigHashType) != 0 {
		sigHash = sigHashType[0]

		// If a non-default sighash is used, then we'll need to add an
		// extra byte to account for the sighash that doesn't exist in
		// the default case.
		estimatedWeight++
	}

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
			Sighash:       uint32(sigHash),
			WitnessScript: leaf2.Script,
			SignMethod:    signMethodTapscript,
		}},
	}
	err = alice.RPC.SignOutputRawErr(signReq)
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
			Sighash:       uint32(sigHash),
			WitnessScript: leaf2.Script,
		}},
		PrevOutputs: utxoInfo,
	}
	err = alice.RPC.SignOutputRawErr(signReq)
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
			Sighash:       uint32(sigHash),
			WitnessScript: leaf2.Script,
			SignMethod:    signMethodTapscript,
		}},
		PrevOutputs: utxoInfo,
	}
	signResp := alice.RPC.SignOutputRaw(signReq)

	// We can now assemble the witness stack.
	controlBlockBytes, err := tapscript.ControlBlock.ToBytes()
	require.NoError(ht, err)

	sig := signResp.RawSigs[0]
	if len(sigHashType) != 0 {
		sig = append(sig, byte(sigHashType[0]))
	}
	tx.TxIn[0].Witness = wire.TxWitness{
		sig, leaf2.Script, controlBlockBytes,
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
	alice *node.HarnessNode, sigHashType ...txscript.SigHashType) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc := alice.RPC.DeriveNextKey(req)

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
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	sigHash := txscript.SigHashDefault
	if len(sigHashType) != 0 {
		sigHash = sigHashType[0]
	}

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(sigHash)
	estimator.AddP2WKHOutput()
	estimatedWeight := estimator.Weight()
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
			Sighash:     uint32(sigHash),
			SignMethod:  signMethodBip86,
		}},
		PrevOutputs: utxoInfo,
	}
	signResp := alice.RPC.SignOutputRaw(signReq)

	sig := signResp.RawSigs[0]
	if len(sigHashType) != 0 {
		sig = append(sig, byte(sigHash))
	}
	tx.TxIn[0].Witness = wire.TxWitness{sig}

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
	alice *node.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc := alice.RPC.DeriveNextKey(req)

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
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	estimator.AddP2WKHOutput()
	estimatedWeight := estimator.Weight()
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
	signResp := alice.RPC.SignOutputRaw(signReq)
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
	alice *node.HarnessNode, version signrpc.MuSig2Version) {

	// We're not going to commit to a script. So our taproot tweak will be
	// empty and just specify the necessary flag.
	taprootTweak := &signrpc.TaprootTweakDesc{
		KeySpendOnly: true,
	}

	keyDesc1, keyDesc2, keyDesc3, allPubKeys := deriveSigningKeys(
		ht, alice, version,
	)
	_, taprootKey, sessResp1, sessResp2, sessResp3 := createMuSigSessions(
		ht, alice, taprootTweak, keyDesc1, keyDesc2, keyDesc3,
		allPubKeys, version,
	)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	estimator.AddP2WKHOutput()
	estimatedWeight := estimator.Weight()
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
	alice.RPC.MuSig2Sign(signReq)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp2.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp2 := alice.RPC.MuSig2Sign(signReq)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp3 := alice.RPC.MuSig2Sign(signReq)

	// Luckily only one of the signers needs to combine the signature, so
	// let's do that now.
	combineReq := &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp2.LocalPartialSignature,
			signResp3.LocalPartialSignature,
		},
	}
	combineResp := alice.RPC.MuSig2CombineSig(combineReq)
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
	alice *node.HarnessNode, version signrpc.MuSig2Version) {

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
		ht, alice, version,
	)
	_, taprootKey, sessResp1, sessResp2, sessResp3 := createMuSigSessions(
		ht, alice, taprootTweak, keyDesc1, keyDesc2, keyDesc3,
		allPubKeys, version,
	)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	estimator.AddP2WKHOutput()
	estimatedWeight := estimator.Weight()
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
	alice.RPC.MuSig2Sign(req)

	req = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp2.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp2 := alice.RPC.MuSig2Sign(req)

	req = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp3 := alice.RPC.MuSig2Sign(req)

	// Luckily only one of the signers needs to combine the signature, so
	// let's do that now.
	combineReq := &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp2.LocalPartialSignature,
			signResp3.LocalPartialSignature,
		},
	}
	combineResp := alice.RPC.MuSig2CombineSig(combineReq)
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
	alice *node.HarnessNode, version signrpc.MuSig2Version) {

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
		ht, alice, version,
	)
	internalKey, taprootKey, _, _, _ := createMuSigSessions(
		ht, alice, taprootTweak, keyDesc1, keyDesc2, keyDesc3,
		allPubKeys, version,
	)

	// Because we know the internal key and the script we want to spend, we
	// can now create the tapscript struct that's used for assembling the
	// control block and fee estimation.
	tapscript := input.TapscriptFullTree(internalKey, leaf1)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	// Create fee estimation for a p2tr input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddTapscriptInput(
		lntypes.WeightUnit(len([]byte("foobar"))+len(leaf1.Script)+1),
		tapscript,
	)
	estimator.AddP2WKHOutput()
	estimatedWeight := estimator.Weight()
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
	alice *node.HarnessNode, version signrpc.MuSig2Version) {

	// We're using the combined MuSig2 key in a script leaf. So we need to
	// derive the combined key first, before we can build the script.
	keyDesc1, keyDesc2, keyDesc3, allPubKeys := deriveSigningKeys(
		ht, alice, version,
	)
	req := &signrpc.MuSig2CombineKeysRequest{
		AllSignerPubkeys: allPubKeys,
		Version:          version,
	}
	combineResp := alice.RPC.MuSig2CombineKeys(req)
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
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

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
	estimatedWeight := estimator.Weight()
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
		version,
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
	alice.RPC.MuSig2Sign(signReq)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp2.SessionId,
		MessageDigest: sigHash,
		Cleanup:       true,
	}
	signResp2 := alice.RPC.MuSig2Sign(signReq)

	// Before we have all partial signatures, we shouldn't get a final
	// signature back.
	combineReq := &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp2.LocalPartialSignature,
		},
	}
	combineSigResp := alice.RPC.MuSig2CombineSig(combineReq)
	require.False(ht, combineSigResp.HaveAllSignatures)
	require.Empty(ht, combineSigResp.FinalSignature)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: sigHash,
	}
	signResp3 := alice.RPC.MuSig2Sign(signReq)

	// We manually clean up session 3, just to make sure that works as well.
	cleanReq := &signrpc.MuSig2CleanupRequest{
		SessionId: sessResp3.SessionId,
	}
	alice.RPC.MuSig2Cleanup(cleanReq)

	// A second call to that cleaned up session should now fail with a
	// specific error.
	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: sigHash,
	}
	err = alice.RPC.MuSig2SignErr(signReq)
	require.Contains(ht, err.Error(), "not found")

	// Luckily only one of the signers needs to combine the signature, so
	// let's do that now.
	combineReq = &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp3.LocalPartialSignature,
		},
	}
	combineResp1 := alice.RPC.MuSig2CombineSig(combineReq)
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

// testTaprootImportTapscriptScriptSpend tests importing p2tr script addresses
// using the script path with the full tree known.
func testTaprootImportTapscriptFullTree(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	_, internalKey, derivationPath := deriveInternalKey(ht, alice)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))

	// Let's add a second script output as well to test the partial reveal.
	leaf2 := testScriptSchnorrSig(ht.T, internalKey)

	tapscript := input.TapscriptFullTree(internalKey, leaf1, leaf2)
	tree := txscript.AssembleTaprootScriptTree(leaf1, leaf2)
	rootHash := tree.RootNode.TapHash()
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(ht, err)

	// Import the scripts and make sure we get the same address back as we
	// calculated ourselves.
	req := &walletrpc.ImportTapscriptRequest{
		InternalPublicKey: schnorr.SerializePubKey(internalKey),
		Script: &walletrpc.ImportTapscriptRequest_FullTree{
			FullTree: &walletrpc.TapscriptFullTree{
				AllLeaves: []*walletrpc.TapLeaf{{
					LeafVersion: uint32(
						leaf1.LeafVersion,
					),
					Script: leaf1.Script,
				}, {
					LeafVersion: uint32(
						leaf2.LeafVersion,
					),
					Script: leaf2.Script,
				}},
			},
		},
	}
	importResp := alice.RPC.ImportTapscript(req)

	calculatedAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	require.Equal(ht, calculatedAddr.String(), importResp.P2TrAddress)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)
	p2trOutputRPC := &lnrpc.OutPoint{
		TxidBytes:   p2trOutpoint.Hash[:],
		OutputIndex: p2trOutpoint.Index,
	}
	ht.AssertUTXOInWallet(alice, p2trOutputRPC, "imported")
	ht.AssertWalletAccountBalance(alice, "imported", testAmount, 0)

	// Funding a PSBT from an imported script is not yet possible. So we
	// basically need to add all information manually for the wallet to be
	// able to sign for it.
	utxo := &wire.TxOut{
		Value:    testAmount,
		PkScript: p2trPkScript,
	}
	clearWalletImportedTapscriptBalance(
		ht, alice, utxo, p2trOutpoint, internalKey, derivationPath,
		rootHash[:],
	)
}

// testTaprootImportTapscriptPartialReveal tests importing p2tr script addresses
// for which we only know part of the tree.
func testTaprootImportTapscriptPartialReveal(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	_, internalKey, derivationPath := deriveInternalKey(ht, alice)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))

	// Let's add a second script output as well to test the partial reveal.
	leaf2 := testScriptSchnorrSig(ht.T, internalKey)
	leaf2Hash := leaf2.TapHash()

	tapscript := input.TapscriptPartialReveal(
		internalKey, leaf1, leaf2Hash[:],
	)
	rootHash := tapscript.ControlBlock.RootHash(leaf1.Script)
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(ht, err)

	// Import the scripts and make sure we get the same address back as we
	// calculated ourselves.
	req := &walletrpc.ImportTapscriptRequest{
		InternalPublicKey: schnorr.SerializePubKey(internalKey),
		Script: &walletrpc.ImportTapscriptRequest_PartialReveal{
			PartialReveal: &walletrpc.TapscriptPartialReveal{
				RevealedLeaf: &walletrpc.TapLeaf{
					LeafVersion: uint32(leaf1.LeafVersion),
					Script:      leaf1.Script,
				},
				FullInclusionProof: leaf2Hash[:],
			},
		},
	}
	importResp := alice.RPC.ImportTapscript(req)

	calculatedAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	require.Equal(ht, calculatedAddr.String(), importResp.P2TrAddress)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	p2trOutputRPC := &lnrpc.OutPoint{
		TxidBytes:   p2trOutpoint.Hash[:],
		OutputIndex: p2trOutpoint.Index,
	}
	ht.AssertUTXOInWallet(alice, p2trOutputRPC, "imported")
	ht.AssertWalletAccountBalance(alice, "imported", testAmount, 0)

	// Funding a PSBT from an imported script is not yet possible. So we
	// basically need to add all information manually for the wallet to be
	// able to sign for it.
	utxo := &wire.TxOut{
		Value:    testAmount,
		PkScript: p2trPkScript,
	}
	clearWalletImportedTapscriptBalance(
		ht, alice, utxo, p2trOutpoint, internalKey, derivationPath,
		rootHash,
	)
}

// testTaprootImportTapscriptRootHashOnly tests importing p2tr script addresses
// for which we only know the root hash.
func testTaprootImportTapscriptRootHashOnly(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	_, internalKey, derivationPath := deriveInternalKey(ht, alice)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))
	rootHash := leaf1.TapHash()

	tapscript := input.TapscriptRootHashOnly(internalKey, rootHash[:])
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(ht, err)

	// Import the scripts and make sure we get the same address back as we
	// calculated ourselves.
	req := &walletrpc.ImportTapscriptRequest{
		InternalPublicKey: schnorr.SerializePubKey(internalKey),
		Script: &walletrpc.ImportTapscriptRequest_RootHashOnly{
			RootHashOnly: rootHash[:],
		},
	}
	importResp := alice.RPC.ImportTapscript(req)

	calculatedAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	require.Equal(ht, calculatedAddr.String(), importResp.P2TrAddress)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	p2trOutputRPC := &lnrpc.OutPoint{
		TxidBytes:   p2trOutpoint.Hash[:],
		OutputIndex: p2trOutpoint.Index,
	}
	ht.AssertUTXOInWallet(alice, p2trOutputRPC, "imported")
	ht.AssertWalletAccountBalance(alice, "imported", testAmount, 0)

	// Funding a PSBT from an imported script is not yet possible. So we
	// basically need to add all information manually for the wallet to be
	// able to sign for it.
	utxo := &wire.TxOut{
		Value:    testAmount,
		PkScript: p2trPkScript,
	}
	clearWalletImportedTapscriptBalance(
		ht, alice, utxo, p2trOutpoint, internalKey, derivationPath,
		rootHash[:],
	)
}

// testTaprootImportTapscriptFullKey tests importing p2tr script addresses for
// which we only know the full Taproot key.
func testTaprootImportTapscriptFullKey(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	_, internalKey, derivationPath := deriveInternalKey(ht, alice)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))

	tapscript := input.TapscriptFullTree(internalKey, leaf1)
	rootHash := leaf1.TapHash()
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(ht, err)

	// Import the scripts and make sure we get the same address back as we
	// calculated ourselves.
	req := &walletrpc.ImportTapscriptRequest{
		InternalPublicKey: schnorr.SerializePubKey(taprootKey),
		Script: &walletrpc.ImportTapscriptRequest_FullKeyOnly{
			FullKeyOnly: true,
		},
	}
	importResp := alice.RPC.ImportTapscript(req)

	calculatedAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	require.Equal(ht, calculatedAddr.String(), importResp.P2TrAddress)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	p2trOutputRPC := &lnrpc.OutPoint{
		TxidBytes:   p2trOutpoint.Hash[:],
		OutputIndex: p2trOutpoint.Index,
	}
	ht.AssertUTXOInWallet(alice, p2trOutputRPC, "imported")
	ht.AssertWalletAccountBalance(alice, "imported", testAmount, 0)

	// Funding a PSBT from an imported script is not yet possible. So we
	// basically need to add all information manually for the wallet to be
	// able to sign for it.
	utxo := &wire.TxOut{
		Value:    testAmount,
		PkScript: p2trPkScript,
	}
	clearWalletImportedTapscriptBalance(
		ht, alice, utxo, p2trOutpoint, internalKey, derivationPath,
		rootHash[:],
	)
}

// testTaprootImportTapscriptFullKeyFundPsbt tests importing p2tr script
// addresses for which we only know the full Taproot key. We also test that we
// can use such an imported script to fund a PSBT.
func testTaprootImportTapscriptFullKeyFundPsbt(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	_, internalKey, derivationPath := deriveInternalKey(ht, alice)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))

	tapscript := input.TapscriptFullTree(internalKey, leaf1)
	rootHash := leaf1.TapHash()
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(ht, err)

	// Import the scripts and make sure we get the same address back as we
	// calculated ourselves.
	req := &walletrpc.ImportTapscriptRequest{
		InternalPublicKey: schnorr.SerializePubKey(taprootKey),
		Script: &walletrpc.ImportTapscriptRequest_FullKeyOnly{
			FullKeyOnly: true,
		},
	}
	importResp := alice.RPC.ImportTapscript(req)

	calculatedAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	require.Equal(ht, calculatedAddr.String(), importResp.P2TrAddress)

	// Send some coins to the generated tapscript address.
	p2trOutpoint, p2trPkScript := sendToTaprootOutput(ht, alice, taprootKey)

	p2trOutputRPC := &lnrpc.OutPoint{
		TxidBytes:   p2trOutpoint.Hash[:],
		OutputIndex: p2trOutpoint.Index,
	}
	ht.AssertUTXOInWallet(alice, p2trOutputRPC, "imported")
	ht.AssertWalletAccountBalance(alice, "imported", testAmount, 0)

	// We now fund a PSBT that spends the imported tapscript address.
	utxo := &wire.TxOut{
		Value:    testAmount,
		PkScript: p2trPkScript,
	}
	_, sweepPkScript := newAddrWithScript(
		ht, alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	output := &wire.TxOut{
		PkScript: sweepPkScript,
		Value:    1,
	}
	packet, err := psbt.New(
		[]*wire.OutPoint{&p2trOutpoint}, []*wire.TxOut{output}, 2, 0,
		[]uint32{0},
	)
	require.NoError(ht, err)

	// We have everything we need to know to sign the PSBT.
	in := &packet.Inputs[0]
	in.Bip32Derivation = []*psbt.Bip32Derivation{{
		PubKey:    internalKey.SerializeCompressed(),
		Bip32Path: derivationPath,
	}}
	in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
		XOnlyPubKey: schnorr.SerializePubKey(internalKey),
		Bip32Path:   derivationPath,
	}}
	in.SighashType = txscript.SigHashDefault
	in.TaprootMerkleRoot = rootHash[:]
	in.WitnessUtxo = utxo

	var buf bytes.Buffer
	require.NoError(ht, packet.Serialize(&buf))

	change := &walletrpc.PsbtCoinSelect_ExistingOutputIndex{
		ExistingOutputIndex: 0,
	}
	fundResp := alice.RPC.FundPsbt(&walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_CoinSelect{
			CoinSelect: &walletrpc.PsbtCoinSelect{
				Psbt:         buf.Bytes(),
				ChangeOutput: change,
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 1,
		},
	})

	// Sign the manually funded PSBT now.
	signResp := alice.RPC.SignPsbt(&walletrpc.SignPsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	})

	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(signResp.SignedPsbt), false,
	)
	require.NoError(ht, err)

	// We should be able to finalize the PSBT and extract the sweep TX now.
	err = psbt.MaybeFinalizeAll(signedPacket)
	require.NoError(ht, err)

	sweepTx, err := psbt.Extract(signedPacket)
	require.NoError(ht, err)

	buf.Reset()
	err = sweepTx.Serialize(&buf)
	require.NoError(ht, err)

	// Publish the sweep transaction and then mine it as well.
	alice.RPC.PublishTransaction(&walletrpc.Transaction{
		TxHex: buf.Bytes(),
	})

	// Mine one block which should contain the sweep transaction.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	sweepTxHash := sweepTx.TxHash()
	ht.AssertTxInBlock(block, sweepTxHash)
}

// clearWalletImportedTapscriptBalance manually assembles and then attempts to
// sign a TX to sweep funds from an imported tapscript address.
func clearWalletImportedTapscriptBalance(ht *lntest.HarnessTest,
	hn *node.HarnessNode, utxo *wire.TxOut, outPoint wire.OutPoint,
	internalKey *btcec.PublicKey, derivationPath []uint32,
	rootHash []byte) {

	_, sweepPkScript := newAddrWithScript(
		ht, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	)

	output := &wire.TxOut{
		PkScript: sweepPkScript,
		Value:    utxo.Value - 1000,
	}
	packet, err := psbt.New(
		[]*wire.OutPoint{&outPoint}, []*wire.TxOut{output}, 2, 0,
		[]uint32{0},
	)
	require.NoError(ht, err)

	// We have everything we need to know to sign the PSBT.
	in := &packet.Inputs[0]
	in.Bip32Derivation = []*psbt.Bip32Derivation{{
		PubKey:    internalKey.SerializeCompressed(),
		Bip32Path: derivationPath,
	}}
	in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
		XOnlyPubKey: schnorr.SerializePubKey(internalKey),
		Bip32Path:   derivationPath,
	}}
	in.SighashType = txscript.SigHashDefault
	in.TaprootMerkleRoot = rootHash
	in.WitnessUtxo = utxo

	var buf bytes.Buffer
	require.NoError(ht, packet.Serialize(&buf))

	// Sign the manually funded PSBT now.
	signResp := hn.RPC.SignPsbt(&walletrpc.SignPsbtRequest{
		FundedPsbt: buf.Bytes(),
	})

	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(signResp.SignedPsbt), false,
	)
	require.NoError(ht, err)

	// We should be able to finalize the PSBT and extract the sweep TX now.
	err = psbt.MaybeFinalizeAll(signedPacket)
	require.NoError(ht, err)

	sweepTx, err := psbt.Extract(signedPacket)
	require.NoError(ht, err)

	buf.Reset()
	err = sweepTx.Serialize(&buf)
	require.NoError(ht, err)

	// Publish the sweep transaction and then mine it as well.
	hn.RPC.PublishTransaction(&walletrpc.Transaction{
		TxHex: buf.Bytes(),
	})

	// Mine one block which should contain the sweep transaction.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	sweepTxHash := sweepTx.TxHash()
	ht.AssertTxInBlock(block, sweepTxHash)
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
func newAddrWithScript(ht *lntest.HarnessTest, node *node.HarnessNode,
	addrType lnrpc.AddressType) (btcutil.Address, []byte) {

	p2wkhResp := node.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: addrType,
	})
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
func sendToTaprootOutput(ht *lntest.HarnessTest, hn *node.HarnessNode,
	taprootKey *btcec.PublicKey) (wire.OutPoint, []byte) {

	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(ht, err)

	// Send some coins to the generated tapscript address.
	req := &lnrpc.SendCoinsRequest{
		Addr:       tapScriptAddr.String(),
		Amount:     testAmount,
		TargetConf: 6,
	}
	hn.RPC.SendCoins(req)

	// Wait until the TX is found in the mempool.
	txid := ht.AssertNumTxsInMempool(1)[0]
	p2trOutputIndex := ht.GetOutputIndex(txid, tapScriptAddr.String())
	p2trOutpoint := wire.OutPoint{
		Hash:  txid,
		Index: uint32(p2trOutputIndex),
	}

	// Make sure the transaction is recognized by our wallet and has the
	// correct output type.
	var outputDetail *lnrpc.OutputDetail
	walletTxns := hn.RPC.GetTransactions(&lnrpc.GetTransactionsRequest{
		StartHeight: 0,
		EndHeight:   -1,
	})
	require.NotEmpty(ht, walletTxns.Transactions)
	for _, tx := range walletTxns.Transactions {
		if tx.TxHash != txid.String() {
			continue
		}

		for outputIdx, out := range tx.OutputDetails {
			if out.Address != tapScriptAddr.String() {
				continue
			}

			outputDetail = tx.OutputDetails[outputIdx]
			break
		}
	}
	require.NotNil(ht, outputDetail, "transaction not found in wallet")
	require.Equal(
		ht, lnrpc.OutputScriptType_SCRIPT_TYPE_WITNESS_V1_TAPROOT,
		outputDetail.OutputType,
	)

	// Clear the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	return p2trOutpoint, p2trPkScript
}

// publishTxAndConfirmSweep is a helper function that publishes a transaction
// after checking its weight against an estimate. After asserting the given
// spend request, the given sweep address' balance is verified to be seen as
// funds belonging to the wallet.
func publishTxAndConfirmSweep(ht *lntest.HarnessTest, node *node.HarnessNode,
	tx *wire.MsgTx, estimatedWeight lntypes.WeightUnit,
	spendRequest *chainrpc.SpendRequest, sweepAddr string) {

	ht.Helper()

	// Before we publish the tx that spends the p2tr transaction, we want to
	// register a spend listener that we expect to fire after mining the
	// block.
	currentHeight := ht.CurrentHeight()

	// For a Taproot output we cannot leave the outpoint empty. Let's make
	// sure the API returns the correct error here.
	req := &chainrpc.SpendRequest{
		Script:     spendRequest.Script,
		HeightHint: currentHeight,
	}
	spendClient := node.RPC.RegisterSpendNtfn(req)

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
		HeightHint: currentHeight,
	}
	spendClient = node.RPC.RegisterSpendNtfn(req)

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	// Since Schnorr signatures are fixed size, we must be able to estimate
	// the size of this transaction exactly.
	txWeight := blockchain.GetTransactionWeight(btcutil.NewTx(tx))
	require.EqualValues(ht, estimatedWeight, txWeight)

	txReq := &walletrpc.Transaction{
		TxHex: buf.Bytes(),
	}
	node.RPC.PublishTransaction(txReq)

	// Make sure the coins sent to the address are confirmed correctly,
	// including the confirmation notification.
	confirmAddress(ht, node, sweepAddr)

	// We now expect our spend event to go through.
	spendMsg, err := spendClient.Recv()
	require.NoError(ht, err)
	spend := spendMsg.GetSpend()
	require.NotNil(ht, spend)
	require.Equal(ht, spend.SpendingHeight, currentHeight+1)
}

// confirmAddress makes sure that a transaction in the mempool spends funds to
// the given address. It also checks that a confirmation notification for the
// address is triggered when the transaction is mined.
func confirmAddress(ht *lntest.HarnessTest, hn *node.HarnessNode,
	addrString string) {

	// Wait until the tx that sends to the address is found.
	txid := ht.AssertNumTxsInMempool(1)[0]

	// Wait until bob has seen the tx and considers it as owned.
	addrOutputIndex := ht.GetOutputIndex(txid, addrString)
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(addrOutputIndex),
	}
	ht.AssertUTXOInWallet(hn, op, "")

	// Before we confirm the transaction, let's register a confirmation
	// listener for it, which we expect to fire after mining a block.
	parsedAddr, err := btcutil.DecodeAddress(addrString, harnessNetParams)
	require.NoError(ht, err)
	addrPkScript, err := txscript.PayToAddrScript(parsedAddr)
	require.NoError(ht, err)

	currentHeight := ht.CurrentHeight()
	req := &chainrpc.ConfRequest{
		Script:       addrPkScript,
		Txid:         txid[:],
		HeightHint:   currentHeight,
		NumConfs:     1,
		IncludeBlock: true,
	}
	confClient := hn.RPC.RegisterConfirmationsNtfn(req)

	// Mine another block to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// We now expect our confirmation to go through, and also that the
	// block was specified.
	confMsg, err := confClient.Recv()
	require.NoError(ht, err)
	conf := confMsg.GetConf()
	require.NotNil(ht, conf)
	require.Equal(ht, conf.BlockHeight, currentHeight+1)
	require.NotNil(ht, conf.RawBlock)

	// We should also be able to decode the raw block.
	var blk wire.MsgBlock
	require.NoError(ht, blk.Deserialize(bytes.NewReader(conf.RawBlock)))
}

// deriveSigningKeys derives three signing keys and returns their descriptors,
// as well as the public keys in the Schnorr serialized format.
func deriveSigningKeys(ht *lntest.HarnessTest, node *node.HarnessNode,
	version signrpc.MuSig2Version) (*signrpc.KeyDescriptor,
	*signrpc.KeyDescriptor, *signrpc.KeyDescriptor, [][]byte) {

	// For muSig2 we need multiple keys. We derive three of them from the
	// same wallet, just so we know we can also sign for them again.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc1 := node.RPC.DeriveNextKey(req)
	pubKey1, err := btcec.ParsePubKey(keyDesc1.RawKeyBytes)
	require.NoError(ht, err)

	keyDesc2 := node.RPC.DeriveNextKey(req)
	pubKey2, err := btcec.ParsePubKey(keyDesc2.RawKeyBytes)
	require.NoError(ht, err)

	keyDesc3 := node.RPC.DeriveNextKey(req)
	pubKey3, err := btcec.ParsePubKey(keyDesc3.RawKeyBytes)
	require.NoError(ht, err)

	// Now that we have all three keys we can create three sessions, one
	// for each of the signers. This would of course normally not happen on
	// the same node.
	var allPubKeys [][]byte
	switch version {
	case signrpc.MuSig2Version_MUSIG2_VERSION_V040:
		allPubKeys = [][]byte{
			schnorr.SerializePubKey(pubKey1),
			schnorr.SerializePubKey(pubKey2),
			schnorr.SerializePubKey(pubKey3),
		}

	case signrpc.MuSig2Version_MUSIG2_VERSION_V100RC2:
		allPubKeys = [][]byte{
			pubKey1.SerializeCompressed(),
			pubKey2.SerializeCompressed(),
			pubKey3.SerializeCompressed(),
		}
	}

	return keyDesc1, keyDesc2, keyDesc3, allPubKeys
}

// createMuSigSessions creates a MuSig2 session with three keys that are
// combined into a single key. The same node is used for the three signing
// participants but a separate key is generated for each session. So the result
// should be the same as if it were three different nodes.
func createMuSigSessions(ht *lntest.HarnessTest, node *node.HarnessNode,
	taprootTweak *signrpc.TaprootTweakDesc,
	keyDesc1, keyDesc2, keyDesc3 *signrpc.KeyDescriptor,
	allPubKeys [][]byte, version signrpc.MuSig2Version) (*btcec.PublicKey,
	*btcec.PublicKey, *signrpc.MuSig2SessionResponse,
	*signrpc.MuSig2SessionResponse, *signrpc.MuSig2SessionResponse) {

	// Make sure that when not specifying a version we get an error, since
	// it is mandatory.
	err := node.RPC.MuSig2CreateSessionErr(&signrpc.MuSig2SessionRequest{})
	require.ErrorContains(ht, err, "unknown MuSig2 version")

	// Create the actual session with the version specified.
	sessResp1 := node.RPC.MuSig2CreateSession(&signrpc.MuSig2SessionRequest{
		KeyLoc:           keyDesc1.KeyLoc,
		AllSignerPubkeys: allPubKeys,
		TaprootTweak:     taprootTweak,
		Version:          version,
	})
	require.Equal(ht, version, sessResp1.Version)

	// Make sure the version is returned correctly.
	require.Equal(ht, version, sessResp1.Version)

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

	// Same with the combine keys RPC, no version specified should give us
	// an error.
	err = node.RPC.MuSig2CombineKeysErr(&signrpc.MuSig2CombineKeysRequest{})
	require.ErrorContains(ht, err, "unknown MuSig2 version")

	// We should also get the same keys when just calling the
	// MuSig2CombineKeys RPC.
	combineReq := &signrpc.MuSig2CombineKeysRequest{
		AllSignerPubkeys: allPubKeys,
		TaprootTweak:     taprootTweak,
		Version:          version,
	}
	combineResp := node.RPC.MuSig2CombineKeys(combineReq)
	require.Equal(
		ht, schnorr.SerializePubKey(expectedCombinedKey),
		combineResp.CombinedKey,
	)
	require.Equal(
		ht, schnorr.SerializePubKey(internalKey),
		combineResp.TaprootInternalKey,
	)
	require.Equal(ht, version, combineResp.Version)

	// Everything is good so far, let's continue with creating the signing
	// session for the other two participants.
	req := &signrpc.MuSig2SessionRequest{
		KeyLoc:           keyDesc2.KeyLoc,
		AllSignerPubkeys: allPubKeys,
		OtherSignerPublicNonces: [][]byte{
			sessResp1.LocalPublicNonces,
		},
		TaprootTweak: taprootTweak,
		Version:      version,
	}
	sessResp2 := node.RPC.MuSig2CreateSession(req)
	require.Equal(ht, sessResp1.CombinedKey, sessResp2.CombinedKey)
	require.Equal(ht, version, sessResp2.Version)

	req = &signrpc.MuSig2SessionRequest{
		KeyLoc:           keyDesc3.KeyLoc,
		AllSignerPubkeys: allPubKeys,
		OtherSignerPublicNonces: [][]byte{
			sessResp1.LocalPublicNonces,
			sessResp2.LocalPublicNonces,
		},
		TaprootTweak: taprootTweak,
		Version:      version,
	}
	sessResp3 := node.RPC.MuSig2CreateSession(req)
	require.Equal(ht, sessResp2.CombinedKey, sessResp3.CombinedKey)
	require.Equal(ht, version, sessResp3.Version)
	require.Equal(ht, true, sessResp3.HaveAllNonces)

	// We need to distribute the rest of the nonces.
	nonceReq := &signrpc.MuSig2RegisterNoncesRequest{
		SessionId: sessResp1.SessionId,
		OtherSignerPublicNonces: [][]byte{
			sessResp2.LocalPublicNonces,
			sessResp3.LocalPublicNonces,
		},
	}
	nonceResp1 := node.RPC.MuSig2RegisterNonces(nonceReq)
	require.True(ht, nonceResp1.HaveAllNonces)

	nonceReq = &signrpc.MuSig2RegisterNoncesRequest{
		SessionId: sessResp2.SessionId,
		OtherSignerPublicNonces: [][]byte{
			sessResp3.LocalPublicNonces,
		},
	}
	nonceResp2 := node.RPC.MuSig2RegisterNonces(nonceReq)
	require.True(ht, nonceResp2.HaveAllNonces)

	return internalKey, combinedKey, sessResp1, sessResp2, sessResp3
}

// testTaprootCoopClose asserts that if both peers signal ShutdownAnySegwit,
// then a taproot closing addr is used. Otherwise, we shouldn't expect one to
// be used.
func testTaprootCoopClose(ht *lntest.HarnessTest) {
	// We'll start by making two new nodes, and funding a channel between
	// them.
	carol := ht.NewNode("Carol", nil)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	dave := ht.NewNode("Dave", nil)
	ht.EnsureConnected(carol, dave)

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)
	satPerVbyte := btcutil.Amount(1)

	// We'll now open a channel between Carol and Dave.
	chanPoint := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{
			Amt:         chanAmt,
			PushAmt:     pushAmt,
			SatPerVByte: satPerVbyte,
		},
	)

	// We'll now close out the channel and obtain the closing TXID.
	closingTxid := ht.CloseChannel(carol, chanPoint)

	// assertTaprootDeliveryUsed returns true if a Taproot addr was used in
	// the co-op close transaction.
	assertTaprootDeliveryUsed := func(closingTxid chainhash.Hash) bool {
		tx := ht.GetRawTransaction(closingTxid)
		for _, txOut := range tx.MsgTx().TxOut {
			if !txscript.IsPayToTaproot(txOut.PkScript) {
				return false
			}
		}

		return true
	}

	// We expect that the closing transaction only has P2TR addresses.
	require.True(ht, assertTaprootDeliveryUsed(closingTxid),
		"taproot addr not used!")

	// Now we'll bring Eve into the mix, Eve is running older software that
	// doesn't understand Taproot.
	eveArgs := []string{"--protocol.no-any-segwit"}
	eve := ht.NewNode("Eve", eveArgs)
	ht.EnsureConnected(carol, eve)

	// We'll now open up a chanel again between Carol and Eve.
	chanPoint = ht.OpenChannel(
		carol, eve, lntest.OpenChannelParams{
			Amt:         chanAmt,
			PushAmt:     pushAmt,
			SatPerVByte: satPerVbyte,
		},
	)

	// We'll now close out this channel and expect that no Taproot
	// addresses are used in the co-op close transaction.
	closingTxid = ht.CloseChannel(carol, chanPoint)
	require.False(ht, assertTaprootDeliveryUsed(closingTxid),
		"taproot addr shouldn't be used!")
}

// testMuSig2CombineKey makes sure that combining a key with MuSig2 returns the
// correct result according to the MuSig2 version specified.
func testMuSig2CombineKey(ht *lntest.HarnessTest, alice *node.HarnessNode,
	version signrpc.MuSig2Version) {

	testVector040Key1 := hexDecode(
		"F9308A019258C31049344F85F89D5229B531C845836F99B08601F113BCE0" +
			"36F9",
	)
	testVector040Key2 := hexDecode(
		"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502B" +
			"A659",
	)
	testVector040Key3 := hexDecode(
		"3590A94E768F8E1815C2F24B4D80A8E3149316C3518CE7B7AD338368D038" +
			"CA66",
	)

	testVector100Key1 := hexDecode(
		"02F9308A019258C31049344F85F89D5229B531C845836F99B08601F113BC" +
			"E036F9",
	)
	testVector100Key2 := hexDecode(
		"03DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B50" +
			"2BA659",
	)
	testVector100Key3 := hexDecode(
		"023590A94E768F8E1815C2F24B4D80A8E3149316C3518CE7B7AD338368D0" +
			"38CA66",
	)

	var allPubKeys [][]byte
	switch version {
	case signrpc.MuSig2Version_MUSIG2_VERSION_V040:
		allPubKeys = [][]byte{
			testVector040Key1, testVector040Key2, testVector040Key3,
		}

	case signrpc.MuSig2Version_MUSIG2_VERSION_V100RC2:
		allPubKeys = [][]byte{
			testVector100Key1, testVector100Key2, testVector100Key3,
		}
	}

	resp := alice.RPC.MuSig2CombineKeys(&signrpc.MuSig2CombineKeysRequest{
		AllSignerPubkeys: allPubKeys,
		TaprootTweak: &signrpc.TaprootTweakDesc{
			KeySpendOnly: true,
		},
		Version: version,
	})

	expectedFinalKey040 := hexDecode(
		"5b257b4e785d61157ef5303051f45184bd5cb47bc4b4069ed4dd453645" +
			"9cb83b",
	)
	expectedPreTweakKey040 := hexDecode(
		"d70cd69a2647f7390973df48cbfa2ccc407b8b2d60b08c5f1641185c79" +
			"98a290",
	)

	expectedFinalKey100 := hexDecode(
		"79e6c3e628c9bfbce91de6b7fb28e2aec7713d377cf260ab599dcbc40e54" +
			"2312",
	)
	expectedPreTweakKey100 := hexDecode(
		"789d937bade6673538f3e28d8368dda4d0512f94da44cf477a505716d26a" +
			"1575",
	)

	switch version {
	case signrpc.MuSig2Version_MUSIG2_VERSION_V040:
		require.Equal(ht, expectedFinalKey040, resp.CombinedKey)
		require.Equal(
			ht, expectedPreTweakKey040, resp.TaprootInternalKey,
		)

	case signrpc.MuSig2Version_MUSIG2_VERSION_V100RC2:
		require.Equal(ht, expectedFinalKey100, resp.CombinedKey)
		require.Equal(
			ht, expectedPreTweakKey100, resp.TaprootInternalKey,
		)
	}
}

// testTaprootMuSig2CombinedNonceCoordinator tests the coordinator pattern where
// a single party aggregates all nonces and distributes the combined nonce to
// participants using MuSig2RegisterCombinedNonce.
func testTaprootMuSig2CombinedNonceCoordinator(ht *lntest.HarnessTest,
	alice *node.HarnessNode, version signrpc.MuSig2Version) {

	// We're using a simple BIP-86 key spend only setup.
	taprootTweak := &signrpc.TaprootTweakDesc{
		KeySpendOnly: true,
	}

	// Derive signing keys for our three participants.
	keyDesc1, keyDesc2, keyDesc3, allPubKeys := deriveSigningKeys(
		ht, alice, version,
	)

	// Create three sessions WITHOUT exchanging nonces initially. This
	// simulates the coordinator pattern where the coordinator collects
	// nonces first, then aggregates them externally.
	sessResp1 := alice.RPC.MuSig2CreateSession(
		&signrpc.MuSig2SessionRequest{
			KeyLoc:           keyDesc1.KeyLoc,
			AllSignerPubkeys: allPubKeys,
			TaprootTweak:     taprootTweak,
			Version:          version,
		},
	)
	require.Equal(ht, version, sessResp1.Version)
	require.False(ht, sessResp1.HaveAllNonces)

	sessResp2 := alice.RPC.MuSig2CreateSession(
		&signrpc.MuSig2SessionRequest{
			KeyLoc:           keyDesc2.KeyLoc,
			AllSignerPubkeys: allPubKeys,
			TaprootTweak:     taprootTweak,
			Version:          version,
		},
	)
	require.False(ht, sessResp2.HaveAllNonces)

	sessResp3 := alice.RPC.MuSig2CreateSession(
		&signrpc.MuSig2SessionRequest{
			KeyLoc:           keyDesc3.KeyLoc,
			AllSignerPubkeys: allPubKeys,
			TaprootTweak:     taprootTweak,
			Version:          version,
		},
	)
	require.False(ht, sessResp3.HaveAllNonces)

	// The coordinator collects all individual nonces.
	allNonces := [][]byte{
		sessResp1.LocalPublicNonces,
		sessResp2.LocalPublicNonces,
		sessResp3.LocalPublicNonces,
	}

	// For v0.4.0, both RegisterCombinedNonce and GetCombinedNonce should
	// return unsupported errors.
	if version == signrpc.MuSig2Version_MUSIG2_VERSION_V040 {
		// Try to register a combined nonce - should fail with
		// unsupported error.
		var dummyNonce [66]byte
		err := alice.RPC.MuSig2RegisterCombinedNonceErr(
			&signrpc.MuSig2RegisterCombinedNonceRequest{
				SessionId:           sessResp1.SessionId,
				CombinedPublicNonce: dummyNonce[:],
			},
		)
		require.ErrorContains(ht, err, "not supported")

		// Try to get combined nonce - should also fail.
		err = alice.RPC.MuSig2GetCombinedNonceErr(
			&signrpc.MuSig2GetCombinedNonceRequest{
				SessionId: sessResp1.SessionId,
			},
		)
		require.ErrorContains(ht, err, "not supported")

		// For v0.4.0, we can't proceed with the coordinator pattern,
		// so we're done with this version.
		return
	}

	// Copy the nonces over to slice of fixed byte arrays and then use the
	// musig2 library to aggregate them.
	var nonces [][musig2.PubNonceSize]byte
	for _, nonce := range allNonces {
		var n [musig2.PubNonceSize]byte
		copy(n[:], nonce)
		nonces = append(nonces, n)
	}

	combinedNonce, err := musig2.AggregateNonces(nonces)
	require.NoError(ht, err)

	// The coordinator now distributes the combined nonce to all
	// participants.
	alice.RPC.MuSig2RegisterCombinedNonce(
		&signrpc.MuSig2RegisterCombinedNonceRequest{
			SessionId:           sessResp1.SessionId,
			CombinedPublicNonce: combinedNonce[:],
		},
	)

	alice.RPC.MuSig2RegisterCombinedNonce(
		&signrpc.MuSig2RegisterCombinedNonceRequest{
			SessionId:           sessResp2.SessionId,
			CombinedPublicNonce: combinedNonce[:],
		},
	)

	alice.RPC.MuSig2RegisterCombinedNonce(
		&signrpc.MuSig2RegisterCombinedNonceRequest{
			SessionId:           sessResp3.SessionId,
			CombinedPublicNonce: combinedNonce[:],
		},
	)

	// Verify we can retrieve the combined nonce.
	getNonceResp := alice.RPC.MuSig2GetCombinedNonce(
		&signrpc.MuSig2GetCombinedNonceRequest{
			SessionId: sessResp1.SessionId,
		},
	)
	require.Equal(ht, combinedNonce[:], getNonceResp.CombinedPublicNonce)

	// Test mutual exclusivity: trying to register individual nonces after
	// combined nonce should fail.
	err = alice.RPC.MuSig2RegisterNoncesErr(
		&signrpc.MuSig2RegisterNoncesRequest{
			SessionId: sessResp1.SessionId,
			OtherSignerPublicNonces: [][]byte{
				sessResp2.LocalPublicNonces,
			},
		},
	)
	require.ErrorContains(ht, err, "already have all nonces")

	// Now complete a full signing flow to verify everything works.
	combinedKey, err := schnorr.ParsePubKey(sessResp1.CombinedKey)
	require.NoError(ht, err)

	// Create a simple message to sign.
	var msg [32]byte
	copy(msg[:], []byte("test message for combined nonce"))

	// All three participants sign the message.
	signReq := &signrpc.MuSig2SignRequest{
		SessionId:     sessResp1.SessionId,
		MessageDigest: msg[:],
	}
	alice.RPC.MuSig2Sign(signReq)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp2.SessionId,
		MessageDigest: msg[:],
		Cleanup:       true,
	}
	signResp2 := alice.RPC.MuSig2Sign(signReq)

	signReq = &signrpc.MuSig2SignRequest{
		SessionId:     sessResp3.SessionId,
		MessageDigest: msg[:],
		Cleanup:       true,
	}
	signResp3 := alice.RPC.MuSig2Sign(signReq)

	// Combine the signatures.
	combineReq := &signrpc.MuSig2CombineSigRequest{
		SessionId: sessResp1.SessionId,
		OtherPartialSignatures: [][]byte{
			signResp2.LocalPartialSignature,
			signResp3.LocalPartialSignature,
		},
	}
	combineResp := alice.RPC.MuSig2CombineSig(combineReq)
	require.True(ht, combineResp.HaveAllSignatures)
	require.NotEmpty(ht, combineResp.FinalSignature)

	// Verify the final signature is valid.
	sig, err := schnorr.ParseSignature(combineResp.FinalSignature)
	require.NoError(ht, err)
	require.True(ht, sig.Verify(msg[:], combinedKey))
}
