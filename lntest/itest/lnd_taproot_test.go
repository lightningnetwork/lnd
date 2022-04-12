package itest

import (
	"bytes"
	"context"
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

// testTaproot ensures that the daemon can send to and spend from taproot (p2tr)
// outputs.
func testTaproot(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	testTaprootKeySpend(ctxt, t, net)
	testTaprootScriptSpend(ctxt, t, net)
	testTaprootKeySpendRPC(ctxt, t, net)
}

// testTaprootKeySpend tests sending to and spending from p2tr key spend only
// (BIP-0086) addresses.
func testTaprootKeySpend(ctxt context.Context, t *harnessTest,
	net *lntest.NetworkHarness) {

	// We'll start the test by sending Alice some coins, which she'll use to
	// send to herself on a p2tr output.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, net.Alice)

	// Let's create a p2tr address now.
	p2trResp, err := net.Alice.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	require.NoError(t.t, err)

	// Assert this is a segwit v1 address that starts with bcrt1p.
	require.Contains(
		t.t, p2trResp.Address, net.Miner.ActiveNet.Bech32HRPSegwit+"1p",
	)

	// Send the coins from Alice's wallet to her own, but to the new p2tr
	// address.
	_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:   p2trResp.Address,
		Amount: 0.5 * btcutil.SatoshiPerBitcoin,
	})
	require.NoError(t.t, err)

	txid, err := waitForTxInMempool(net.Miner.Client, defaultTimeout)
	require.NoError(t.t, err)

	// Wait until bob has seen the tx and considers it as owned.
	p2trOutputIndex := getOutputIndex(t, net.Miner, txid, p2trResp.Address)
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	assertWalletUnspent(t, net.Alice, op)

	// Mine a block to clean up the mempool.
	mineBlocks(t, net, 1, 1)

	// Let's sweep the whole wallet to a new p2tr address, making sure we
	// can sign transactions with v0 and v1 inputs.
	p2trResp, err = net.Alice.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	require.NoError(t.t, err)

	_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:    p2trResp.Address,
		SendAll: true,
	})
	require.NoError(t.t, err)

	// Wait until the wallet cleaning sweep tx is found.
	txid, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	// Wait until bob has seen the tx and considers it as owned.
	p2trOutputIndex = getOutputIndex(t, net.Miner, txid, p2trResp.Address)
	op = &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	assertWalletUnspent(t, net.Alice, op)

	// Before we confirm the transaction, let's register a confirmation
	// listener for it, which we expect to fire after mining a block.
	p2trAddr, err := btcutil.DecodeAddress(
		p2trResp.Address, harnessNetParams,
	)
	require.NoError(t.t, err)
	p2trPkScript, err := txscript.PayToAddrScript(p2trAddr)
	require.NoError(t.t, err)

	_, currentHeight, err := net.Miner.Client.GetBestBlock()
	require.NoError(t.t, err)
	confClient, err := net.Alice.ChainClient.RegisterConfirmationsNtfn(
		ctxt, &chainrpc.ConfRequest{
			Script:     p2trPkScript,
			Txid:       txid[:],
			HeightHint: uint32(currentHeight),
			NumConfs:   1,
		},
	)
	require.NoError(t.t, err)

	// Mine another block to clean up the mempool.
	mineBlocks(t, net, 1, 1)

	// We now expect our confirmation to go through.
	confMsg, err := confClient.Recv()
	require.NoError(t.t, err)
	conf := confMsg.GetConf()
	require.NotNil(t.t, conf)
	require.Equal(t.t, conf.BlockHeight, uint32(currentHeight+1))
}

// testTaprootScriptSpend tests sending to and spending from p2tr script
// addresses.
func testTaprootScriptSpend(ctxt context.Context, t *harnessTest,
	net *lntest.NetworkHarness) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	const taprootKeyFamily = 77
	keyDesc, err := net.Alice.WalletKitClient.DeriveNextKey(
		ctxt, &walletrpc.KeyReq{
			KeyFamily: taprootKeyFamily,
		},
	)
	require.NoError(t.t, err)

	leafSigningKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(t.t, err)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(t.t, []byte("foobar"))

	// Let's add a second script output as well to test the partial reveal.
	leaf2 := testScriptSchnorrSig(t.t, leafSigningKey)

	dummyInternalKeyBytes, _ := hex.DecodeString(
		"03464805f5468e294d88cf15a3f06aef6c89d63ef1bd7b42db2e0c74c1ac" +
			"eb90fe",
	)
	dummyInternalKey, _ := btcec.ParsePubKey(dummyInternalKeyBytes)

	tapscript := input.TapscriptPartialReveal(
		dummyInternalKey, leaf2, leaf1.TapHash(),
	)
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(t.t, err)

	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(t.t, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(t.t, err)

	// Send some coins to the generated tapscript address.
	_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:   tapScriptAddr.String(),
		Amount: 800_000,
	})
	require.NoError(t.t, err)

	// Wait until the TX is found in the mempool.
	txid, err := waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	p2trOutputIndex := getOutputIndex(
		t, net.Miner, txid, tapScriptAddr.String(),
	)
	p2trOutpoint := wire.OutPoint{
		Hash:  *txid,
		Index: uint32(p2trOutputIndex),
	}

	// Clear the mempool.
	mineBlocks(t, net, 1, 1)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ctxt, t.t, net.Alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
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
	value := int64(800_000 - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(t.t, tx.Serialize(&buf))

	utxoInfo := []*signrpc.TxOut{{
		PkScript: p2trPkScript,
		Value:    800_000,
	}}

	// Before we actually sign, we want to make sure that we get an error
	// when we try to sign for a Taproot output without specifying all UTXO
	// information.
	_, err = net.Alice.SignerClient.SignOutputRaw(
		ctxt, &signrpc.SignReq{
			RawTxBytes: buf.Bytes(),
			SignDescs: []*signrpc.SignDescriptor{{
				Output:        utxoInfo[0],
				InputIndex:    0,
				KeyDesc:       keyDesc,
				Sighash:       uint32(txscript.SigHashDefault),
				WitnessScript: leaf2.Script,
			}},
		},
	)
	require.Error(t.t, err)
	require.Contains(
		t.t, err.Error(), "error signing taproot output, transaction "+
			"input 0 is missing its previous outpoint information",
	)

	// Do the actual signing now.
	signResp, err := net.Alice.SignerClient.SignOutputRaw(
		ctxt, &signrpc.SignReq{
			RawTxBytes: buf.Bytes(),
			SignDescs: []*signrpc.SignDescriptor{{
				Output:        utxoInfo[0],
				InputIndex:    0,
				KeyDesc:       keyDesc,
				Sighash:       uint32(txscript.SigHashDefault),
				WitnessScript: leaf2.Script,
			}},
			PrevOutputs: utxoInfo,
		},
	)
	require.NoError(t.t, err)

	// We can now assemble the witness stack.
	controlBlockBytes, err := tapscript.ControlBlock.ToBytes()
	require.NoError(t.t, err)

	tx.TxIn[0].Witness = wire.TxWitness{
		signResp.RawSigs[0],
		leaf2.Script,
		controlBlockBytes,
	}

	buf.Reset()
	require.NoError(t.t, tx.Serialize(&buf))

	// Since Schnorr signatures are fixed size, we must be able to estimate
	// the size of this transaction exactly.
	txWeight := blockchain.GetTransactionWeight(btcutil.NewTx(tx))
	require.Equal(t.t, txWeight, estimatedWeight)

	// Before we publish the tx that spends the p2tr transaction, we want to
	// register a spend listener that we expect to fire after mining the
	// block.
	_, currentHeight, err := net.Miner.Client.GetBestBlock()
	require.NoError(t.t, err)

	// For a Taproot output we cannot leave the outpoint empty. Let's make
	// sure the API returns the correct error here.
	spendClient, err := net.Alice.ChainClient.RegisterSpendNtfn(
		ctxt, &chainrpc.SpendRequest{
			Script:     p2trPkScript,
			HeightHint: uint32(currentHeight),
		},
	)
	require.NoError(t.t, err)

	// The error is only thrown when trying to read a message.
	_, err = spendClient.Recv()
	require.Contains(
		t.t, err.Error(),
		"cannot register witness v1 spend request without outpoint",
	)

	// Now try again, this time with the outpoint set.
	spendClient, err = net.Alice.ChainClient.RegisterSpendNtfn(
		ctxt, &chainrpc.SpendRequest{
			Outpoint: &chainrpc.Outpoint{
				Hash:  p2trOutpoint.Hash[:],
				Index: p2trOutpoint.Index,
			},
			Script:     p2trPkScript,
			HeightHint: uint32(currentHeight),
		},
	)
	require.NoError(t.t, err)

	_, err = net.Alice.WalletKitClient.PublishTransaction(
		ctxt, &walletrpc.Transaction{
			TxHex: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)

	// Wait until the spending tx is found.
	txid, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)
	p2wpkhOutputIndex := getOutputIndex(
		t, net.Miner, txid, p2wkhAddr.String(),
	)
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2wpkhOutputIndex),
	}
	assertWalletUnspent(t, net.Alice, op)

	// Mine another block to clean up the mempool and to make sure the spend
	// tx is actually included in a block.
	mineBlocks(t, net, 1, 1)

	// We now expect our spend event to go through.
	spendMsg, err := spendClient.Recv()
	require.NoError(t.t, err)
	spend := spendMsg.GetSpend()
	require.NotNil(t.t, spend)
	require.Equal(t.t, spend.SpendingHeight, uint32(currentHeight+1))
}

// testTaprootKeySpendRPC tests that a tapscript address can also be spent using
// the key spend path through the RPC.
func testTaprootKeySpendRPC(ctxt context.Context, t *harnessTest,
	net *lntest.NetworkHarness) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	const taprootKeyFamily = 77
	keyDesc, err := net.Alice.WalletKitClient.DeriveNextKey(
		ctxt, &walletrpc.KeyReq{
			KeyFamily: taprootKeyFamily,
		},
	)
	require.NoError(t.t, err)

	internalKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(t.t, err)

	// We want to make sure we can still use a tweaked key, even if it ends
	// up being essentially double tweaked because of the taproot root hash.
	dummyKeyTweak := sha256.Sum256([]byte("this is a key tweak"))
	internalKey = input.TweakPubKeyWithTweak(internalKey, dummyKeyTweak[:])

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(t.t, []byte("foobar"))

	rootHash := leaf1.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash[:])

	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(t.t, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(t.t, err)

	// Send some coins to the generated tapscript address.
	_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:   tapScriptAddr.String(),
		Amount: 800_000,
	})
	require.NoError(t.t, err)

	// Wait until the TX is found in the mempool.
	txid, err := waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	p2trOutputIndex := getOutputIndex(
		t, net.Miner, txid, tapScriptAddr.String(),
	)

	// Clear the mempool.
	mineBlocks(t, net, 1, 1)

	// Spend the output again, this time back to a p2wkh address.
	p2wkhAddr, p2wkhPkScript := newAddrWithScript(
		ctxt, t.t, net.Alice, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
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
		PreviousOutPoint: wire.OutPoint{
			Hash:  *txid,
			Index: uint32(p2trOutputIndex),
		},
	}}
	value := int64(800_000 - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(t.t, tx.Serialize(&buf))

	utxoInfo := []*signrpc.TxOut{{
		PkScript: p2trPkScript,
		Value:    800_000,
	}}
	signResp, err := net.Alice.SignerClient.SignOutputRaw(
		ctxt, &signrpc.SignReq{
			RawTxBytes: buf.Bytes(),
			SignDescs: []*signrpc.SignDescriptor{{
				Output:          utxoInfo[0],
				InputIndex:      0,
				KeyDesc:         keyDesc,
				SingleTweak:     dummyKeyTweak[:],
				Sighash:         uint32(txscript.SigHashDefault),
				WitnessScript:   rootHash[:],
				TaprootKeySpend: true,
			}},
			PrevOutputs: utxoInfo,
		},
	)
	require.NoError(t.t, err)

	tx.TxIn[0].Witness = wire.TxWitness{
		signResp.RawSigs[0],
	}

	buf.Reset()
	require.NoError(t.t, tx.Serialize(&buf))

	// Since Schnorr signatures are fixed size, we must be able to estimate
	// the size of this transaction exactly.
	txWeight := blockchain.GetTransactionWeight(btcutil.NewTx(tx))
	require.Equal(t.t, txWeight, estimatedWeight)

	_, err = net.Alice.WalletKitClient.PublishTransaction(
		ctxt, &walletrpc.Transaction{
			TxHex: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)

	// Wait until the spending tx is found.
	txid, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)
	p2wpkhOutputIndex := getOutputIndex(
		t, net.Miner, txid, p2wkhAddr.String(),
	)
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2wpkhOutputIndex),
	}
	assertWalletUnspent(t, net.Alice, op)

	// Mine another block to clean up the mempool and to make sure the spend
	// tx is actually included in a block.
	mineBlocks(t, net, 1, 1)
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
func newAddrWithScript(ctx context.Context, t *testing.T,
	node *lntest.HarnessNode, addrType lnrpc.AddressType) (btcutil.Address,
	[]byte) {

	p2wkhResp, err := node.NewAddress(ctx, &lnrpc.NewAddressRequest{
		Type: addrType,
	})
	require.NoError(t, err)

	p2wkhAddr, err := btcutil.DecodeAddress(
		p2wkhResp.Address, harnessNetParams,
	)
	require.NoError(t, err)

	p2wkhPkScript, err := txscript.PayToAddrScript(p2wkhAddr)
	require.NoError(t, err)

	return p2wkhAddr, p2wkhPkScript
}
