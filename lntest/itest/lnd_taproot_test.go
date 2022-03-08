package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func setup(
	net *lntest.NetworkHarness, t *harnessTest,
	ctxt context.Context) {

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
	p2trOutputIndex := getOutputIndex(t, net, txid, p2trResp.Address)
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
	p2trOutputIndex = getOutputIndex(t, net, txid, p2trResp.Address)
	op = &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	assertWalletUnspent(t, net.Alice, op)

	// Mine another block to clean up the mempool.
	mineBlocks(t, net, 1, 1)

}

/*
	CLAIM PATH

	<reciever_key> OP_CHECKSIGVERIFY OP_SIZE 20 OP_EQUALVERIFY OP_RIPEMD160 <hash> OP_EQUALVERIFY 1 OP_CHECKSEQUENCEVERIFY
*/
func createClaimPathLeaf(t *harnessTest, recieverHtlcKey [32]byte, swapHash lntypes.Hash) (txscript.TapLeaf, []byte) {
	builder := txscript.NewScriptBuilder()

	builder.AddData(recieverHtlcKey[:])
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(20)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_RIPEMD160)
	builder.AddData(input.Ripemd160H(swapHash[:]))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddInt64(1)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	script, err := builder.Script()
	require.NoError(t.t, err)

	return txscript.NewBaseTapLeaf(script), script
}

/*
	TIMEOUT PATH

	<timeout_key> OP_CHECKSIGVERIFY <timeout height> OP_CHECKLOCKTIMEVERIFY
*/
func createTimeoutPathLeaf(
	t *harnessTest, senderHtlcKey [32]byte, timeoutHeight int64) (txscript.TapLeaf, []byte) {

	// Let's add a second script output as well to test the partial reveal.
	builder := txscript.NewScriptBuilder()
	builder.AddData(senderHtlcKey[:])
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(timeoutHeight)
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)

	script, err := builder.Script()
	require.NoError(t.t, err)
	return txscript.NewBaseTapLeaf(script), script
}

// testTaproot ensures that the daemon can send to and spend from taproot (p2tr)
// outputs.
func testTaproot(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

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
	p2trOutputIndex := getOutputIndex(t, net, txid, p2trResp.Address)
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
	p2trOutputIndex = getOutputIndex(t, net, txid, p2trResp.Address)
	op = &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	assertWalletUnspent(t, net.Alice, op)

	// Mine another block to clean up the mempool.
	mineBlocks(t, net, 1, 1)

	// For the next step, we need a public key. Let's use a special family
	// for this.
	const taprootKeyFamily = 77
	keyDesc, err := net.Alice.WalletKitClient.DeriveNextKey(
		ctxt, &walletrpc.KeyReq{
			KeyFamily: taprootKeyFamily,
		},
	)
	require.NoError(t.t, err)

	internalPubKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(t.t, err)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_DUP)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(btcutil.Hash160([]byte("foobar")))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	script1, err := builder.Script()
	require.NoError(t.t, err)
	leaf1 := txscript.NewBaseTapLeaf(script1)

	// Let's add a second script output as well to test the partial reveal.
	builder = txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(internalPubKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	script2, err := builder.Script()
	require.NoError(t.t, err)
	leaf2 := txscript.NewBaseTapLeaf(script2)

	tree := txscript.AssembleTaprootScriptTree(leaf1, leaf2)
	rootHash := tree.RootNode.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(
		internalPubKey, rootHash[:],
	)

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
	txid, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	p2trOutputIndex = getOutputIndex(t, net, txid, tapScriptAddr.String())

	// Spend the output again, this time back to a p2wkh address.
	p2wkhResp, err := net.Alice.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	require.NoError(t.t, err)

	p2wkhAdrr, err := btcutil.DecodeAddress(
		p2wkhResp.Address, harnessNetParams,
	)
	require.NoError(t.t, err)

	p2wkhPkScript, err := txscript.PayToAddrScript(p2wkhAdrr)
	require.NoError(t.t, err)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *txid,
			Index: uint32(p2trOutputIndex),
		},
	}}
	value := int64(800_000 - 500) // TODO(guggero): Calculate actual fee.
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(t.t, tx.Serialize(&buf))

	signResp, err := net.Alice.SignerClient.SignOutputRaw(
		ctxt, &signrpc.SignReq{
			RawTxBytes: buf.Bytes(),
			SignDescs: []*signrpc.SignDescriptor{{
				Output: &signrpc.TxOut{
					PkScript: p2trPkScript,
					Value:    800_000,
				},
				InputIndex:    0,
				KeyDesc:       keyDesc,
				Sighash:       0,
				WitnessScript: script2,
			}},
		},
	)
	require.NoError(t.t, err)

	// With the commitment computed we can obtain the bit that denotes if
	// the resulting key has an odd y coordinate or not.
	var outputKeyYIsOdd bool
	if taprootKey.SerializeCompressed()[0] == secp.PubKeyFormatCompressedOdd {
		outputKeyYIsOdd = true
	}

	script1Proof := leaf1.TapHash()
	controlBlock := txscript.ControlBlock{
		InternalKey:     internalPubKey,
		OutputKeyYIsOdd: outputKeyYIsOdd,
		LeafVersion:     txscript.BaseLeafVersion,
		InclusionProof:  script1Proof[:],
	}
	controlBlockBytes, err := controlBlock.ToBytes()
	require.NoError(t.t, err)

	tx.TxIn[0].Witness = wire.TxWitness{
		signResp.RawSigs[0],
		script2,
		controlBlockBytes,
	}

	buf.Reset()
	require.NoError(t.t, tx.Serialize(&buf))

	_, err = net.Alice.WalletKitClient.PublishTransaction(
		ctxt, &walletrpc.Transaction{
			TxHex: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)
}

func CreateKey(index int32) (*btcec.PrivateKey, *btcec.PublicKey) {
	// Avoid all zeros, because it results in an invalid key.
	privKey, pubKey := btcec.PrivKeyFromBytes(
		[]byte{0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, byte(index + 1)})
	return privKey, pubKey
}

func testTaprootHtlcTimeoutPath(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	setup(net, t, ctxt)

	// For the next step, we need a public key. Let's use a special family
	// for this.
	randomPub, _ := hex.DecodeString(
		"03fcb7d1b502bd59f4dbc6cf503e5c280189e0e6dd2d10c4c14d97ed8611" +
			"a99178",
	)

	internalPubKey, err := btcec.ParsePubKey(randomPub)
	require.NoError(t.t, err)

	preimage := lntypes.Preimage([32]byte{1, 2, 3})
	hashedPreimage := sha256.Sum256(preimage[:])

	senderPrivKey, senderPubKey := CreateKey(1)
	_, receiverPubKey := CreateKey(2)

	locktime := 10

	var (
		senderKey   [32]byte
		receiverKey [32]byte
	)
	copy(senderKey[:], schnorr.SerializePubKey(senderPubKey))
	copy(receiverKey[:], schnorr.SerializePubKey(receiverPubKey))

	claimPathLeaf, _ := createClaimPathLeaf(
		t, receiverKey, hashedPreimage,
	)
	timeoutPathLeaf, timeoutPathScript := createTimeoutPathLeaf(
		t, senderKey, int64(locktime),
	)

	tree := txscript.AssembleTaprootScriptTree(
		claimPathLeaf, timeoutPathLeaf,
	)

	rootHash := tree.RootNode.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(
		internalPubKey, rootHash[:],
	)

	// Generate a tapscript address from our tree
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

	mineBlocks(t, net, 11, 1)

	p2trOutputIndex := getOutputIndex(t, net, txid, tapScriptAddr.String())

	// Spend the output again, this time back to a p2wkh address.
	p2wkhResp, err := net.Alice.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	require.NoError(t.t, err)

	p2wkhAdrr, err := btcutil.DecodeAddress(
		p2wkhResp.Address, harnessNetParams,
	)
	require.NoError(t.t, err)

	p2wkhPkScript, err := txscript.PayToAddrScript(p2wkhAdrr)
	require.NoError(t.t, err)

	tx := wire.NewMsgTx(2)
	tx.LockTime = uint32(locktime)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *txid,
			Index: uint32(p2trOutputIndex),
		},
		Sequence: 10, // Is this necessary?
	}}
	value := int64(800_000 - 500) // TODO(guggero): Calculate actual fee.
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(t.t, tx.Serialize(&buf))

	require.NoError(t.t, err)

	// With the commitment computed we can obtain the bit that denotes if
	// the resulting key has an odd y coordinate or not.
	var outputKeyYIsOdd bool
	if taprootKey.SerializeCompressed()[0] == secp.PubKeyFormatCompressedOdd {
		outputKeyYIsOdd = true
	}

	proof := claimPathLeaf.TapHash()
	controlBlock := txscript.ControlBlock{
		InternalKey:     internalPubKey,
		OutputKeyYIsOdd: outputKeyYIsOdd,
		LeafVersion:     txscript.BaseLeafVersion,
		InclusionProof:  proof[:],
	}
	controlBlockBytes, err := controlBlock.ToBytes()
	require.NoError(t.t, err)

	senderSig, err := txscript.RawTxInTapscriptSignature(
		tx, txscript.NewTxSigHashes(
			tx, txscript.NewCannedPrevOutputFetcher(
				p2trPkScript, 800_000,
			),
		), 0, 800_000, p2trPkScript, timeoutPathLeaf,
		txscript.SigHashDefault, senderPrivKey,
	)

	require.NoError(t.t, err)

	tx.TxIn[0].Witness = wire.TxWitness{
		senderSig,
		timeoutPathScript,
		controlBlockBytes,
	}

	buf.Reset()
	require.NoError(t.t, tx.Serialize(&buf))

	_, err = net.Alice.WalletKitClient.PublishTransaction(
		ctxt, &walletrpc.Transaction{
			TxHex: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)
}

func testTaprootHtlcClaimPath(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	setup(net, t, ctxt)

	// For the next step, we need a public key. Let's use a special family
	// for this.
	randomPub, _ := hex.DecodeString(
		"03fcb7d1b502bd59f4dbc6cf503e5c280189e0e6dd2d10c4c14d97ed8611" +
			"a99178",
	)

	internalPubKey, err := btcec.ParsePubKey(randomPub)
	require.NoError(t.t, err)

	preimage := lntypes.Preimage([32]byte{1, 2, 3})
	hashedPreimage := sha256.Sum256(preimage[:])

	senderPrivKey, senderPubKey := CreateKey(1)
	receiverPrivKey, receiverPubKey := CreateKey(2)

	locktime := 10

	var (
		senderKey   [32]byte
		receiverKey [32]byte
	)
	copy(senderKey[:], schnorr.SerializePubKey(senderPubKey))
	copy(receiverKey[:], schnorr.SerializePubKey(receiverPubKey))

	claimPathLeaf, claimPathScript := createClaimPathLeaf(
		t, senderKey, hashedPreimage,
	)
	timeoutPathLeaf, _ := createTimeoutPathLeaf(
		t, senderKey, int64(locktime),
	)

	tree := txscript.AssembleTaprootScriptTree(
		claimPathLeaf, timeoutPathLeaf,
	)

	rootHash := tree.RootNode.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(
		internalPubKey, rootHash[:],
	)

	// Generate a tapscript address from our tree
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

	mineBlocks(t, net, 11, 1)

	p2trOutputIndex := getOutputIndex(t, net, txid, tapScriptAddr.String())

	// Spend the output again, this time back to a p2wkh address.
	p2wkhResp, err := net.Alice.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	require.NoError(t.t, err)

	p2wkhAdrr, err := btcutil.DecodeAddress(
		p2wkhResp.Address, harnessNetParams,
	)
	require.NoError(t.t, err)

	p2wkhPkScript, err := txscript.PayToAddrScript(p2wkhAdrr)
	require.NoError(t.t, err)

	tx := wire.NewMsgTx(2)
	tx.LockTime = uint32(locktime)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *txid,
			Index: uint32(p2trOutputIndex),
		},
		Sequence: 10,
	}}
	value := int64(800_000 - 500) // TODO(guggero): Calculate actual fee.
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(t.t, tx.Serialize(&buf))

	require.NoError(t.t, err)

	// With the commitment computed we can obtain the bit that denotes if
	// the resulting key has an odd y coordinate or not.
	var outputKeyYIsOdd bool
	if taprootKey.SerializeCompressed()[0] == secp.PubKeyFormatCompressedOdd {
		outputKeyYIsOdd = true
	}

	proof := timeoutPathLeaf.TapHash()
	controlBlock := txscript.ControlBlock{
		InternalKey:     internalPubKey,
		OutputKeyYIsOdd: outputKeyYIsOdd,
		LeafVersion:     txscript.BaseLeafVersion,
		InclusionProof:  proof[:],
	}
	controlBlockBytes, err := controlBlock.ToBytes()
	require.NoError(t.t, err)

	_, err = txscript.RawTxInTapscriptSignature(
		tx, txscript.NewTxSigHashes(
			tx, txscript.NewCannedPrevOutputFetcher(
				p2trPkScript, 800_000,
			),
		), 0, 800_000, p2trPkScript, claimPathLeaf,
		txscript.SigHashDefault, senderPrivKey,
	)

	fmt.Println(len(receiverPrivKey.Serialize()))
	require.NoError(t.t, err)

	tx.TxIn[0].Witness = wire.TxWitness{
		// receiverSig,
		hashedPreimage[:],
		claimPathScript,
		controlBlockBytes,
	}

	buf.Reset()
	require.NoError(t.t, tx.Serialize(&buf))

	_, err = net.Alice.WalletKitClient.PublishTransaction(
		ctxt, &walletrpc.Transaction{
			TxHex: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)
}
