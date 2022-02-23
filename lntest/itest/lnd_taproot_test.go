package itest

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

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
