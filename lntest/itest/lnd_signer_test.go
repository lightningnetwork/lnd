package itest

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testDeriveSharedKey checks the ECDH performed by the endpoint
// DeriveSharedKey. It creates an ephemeral private key, performing an ECDH with
// the node's pubkey and a customized public key to check the validity of the
// result.
func testDeriveSharedKey(ht *lntest.HarnessTest) {
	runDeriveSharedKey(ht, ht.Alice)
}

// runDeriveSharedKey checks the ECDH performed by the endpoint
// DeriveSharedKey. It creates an ephemeral private key, performing an ECDH with
// the node's pubkey and a customized public key to check the validity of the
// result.
func runDeriveSharedKey(ht *lntest.HarnessTest, alice *lntest.HarnessNode) {
	// Create an ephemeral key, extracts its public key, and make a
	// PrivKeyECDH using the ephemeral key.
	ephemeralPriv, err := btcec.NewPrivateKey()
	require.NoError(ht, err, "failed to create ephemeral key")

	ephemeralPubBytes := ephemeralPriv.PubKey().SerializeCompressed()
	privKeyECDH := &keychain.PrivKeyECDH{PrivKey: ephemeralPriv}

	// assertECDHMatch checks the correctness of the ECDH between the
	// ephemeral key and the given public key.
	assertECDHMatch := func(pub *btcec.PublicKey,
		req *signrpc.SharedKeyRequest) {

		resp := ht.DeriveSharedKey(alice, req)

		sharedKey, _ := privKeyECDH.ECDH(pub)
		require.Equal(ht, sharedKey[:], resp.SharedKey,
			"failed to derive the expected key")
	}

	nodePub, err := btcec.ParsePubKey(alice.PubKey[:])
	require.NoError(ht, err, "failed to parse node pubkey")

	customizedKeyFamily := int32(keychain.KeyFamilyMultiSig)
	customizedIndex := int32(1)

	// Derive a customized key.
	deriveReq := &signrpc.KeyLocator{
		KeyFamily: customizedKeyFamily,
		KeyIndex:  customizedIndex,
	}
	resp := ht.DeriveKey(alice, deriveReq)
	customizedPub, err := btcec.ParsePubKey(resp.RawKeyBytes)
	require.NoError(ht, err, "failed to parse node pubkey")

	// Test DeriveSharedKey with no optional arguments. It will result in
	// performing an ECDH between the ephemeral key and the node's pubkey.
	req := &signrpc.SharedKeyRequest{EphemeralPubkey: ephemeralPubBytes}
	assertECDHMatch(nodePub, req)

	// Test DeriveSharedKey with a KeyLoc which points to the node's pubkey.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(keychain.KeyFamilyNodeKey),
			KeyIndex:  0,
		},
	}
	assertECDHMatch(nodePub, req)

	// Test DeriveSharedKey with a KeyLoc being set in KeyDesc. The KeyLoc
	// points to the node's pubkey.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyDesc: &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: int32(keychain.KeyFamilyNodeKey),
				KeyIndex:  0,
			},
		},
	}
	assertECDHMatch(nodePub, req)

	// Test DeriveSharedKey with RawKeyBytes set in KeyDesc. The RawKeyBytes
	// is the node's pubkey bytes, and the KeyFamily is KeyFamilyNodeKey.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyDesc: &signrpc.KeyDescriptor{
			RawKeyBytes: alice.PubKey[:],
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: int32(keychain.KeyFamilyNodeKey),
			},
		},
	}
	assertECDHMatch(nodePub, req)

	// Test DeriveSharedKey with a KeyLoc which points to the customized
	// public key.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: customizedKeyFamily,
			KeyIndex:  customizedIndex,
		},
	}
	assertECDHMatch(customizedPub, req)

	// Test DeriveSharedKey with a KeyLoc being set in KeyDesc. The KeyLoc
	// points to the customized public key.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyDesc: &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: customizedKeyFamily,
				KeyIndex:  customizedIndex,
			},
		},
	}
	assertECDHMatch(customizedPub, req)

	// Test DeriveSharedKey with RawKeyBytes set in KeyDesc. The RawKeyBytes
	// is the customized public key. The KeyLoc is also set with the family
	// being the customizedKeyFamily.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyDesc: &signrpc.KeyDescriptor{
			RawKeyBytes: customizedPub.SerializeCompressed(),
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: customizedKeyFamily,
			},
		},
	}
	assertECDHMatch(customizedPub, req)

	// assertErrorMatch checks when calling DeriveSharedKey with invalid
	// params, the expected error is returned.
	assertErrorMatch := func(match string, req *signrpc.SharedKeyRequest) {
		err := ht.DeriveSharedKeyErr(alice, req)
		require.Contains(ht, err.Error(), match,
			"error failed to match")
	}

	// Test that EphemeralPubkey must be supplied.
	req = &signrpc.SharedKeyRequest{}
	assertErrorMatch("must provide ephemeral pubkey", req)

	// Test that cannot use both KeyDesc and KeyLoc.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyDesc: &signrpc.KeyDescriptor{
			RawKeyBytes: customizedPub.SerializeCompressed(),
		},
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: customizedKeyFamily,
			KeyIndex:  0,
		},
	}
	assertErrorMatch("use either key_desc or key_loc", req)

	// Test when KeyDesc is used, KeyLoc must be set.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyDesc: &signrpc.KeyDescriptor{
			RawKeyBytes: alice.PubKey[:],
		},
	}
	assertErrorMatch("key_desc.key_loc must also be set", req)

	// Test that cannot use both RawKeyBytes and KeyIndex.
	req = &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubBytes,
		KeyDesc: &signrpc.KeyDescriptor{
			RawKeyBytes: customizedPub.SerializeCompressed(),
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: customizedKeyFamily,
				KeyIndex:  1,
			},
		},
	}
	assertErrorMatch("use either raw_key_bytes or key_index", req)
}

// testSignOutputRaw makes sure that the SignOutputRaw RPC can be used with all
// custom ways of specifying the signing key in the key descriptor/locator.
func testSignOutputRaw(ht *lntest.HarnessTest) {
	runSignOutputRaw(ht, ht.Alice)
}

// runSignOutputRaw makes sure that the SignOutputRaw RPC can be used with all
// custom ways of specifying the signing key in the key descriptor/locator.
func runSignOutputRaw(ht *lntest.HarnessTest, alice *lntest.HarnessNode) {
	// For the next step, we need a public key. Let's use a special family
	// for this. We want this to be an index of zero.
	const testCustomKeyFamily = 44
	req := &walletrpc.KeyReq{
		KeyFamily: testCustomKeyFamily,
	}
	keyDesc := ht.DeriveNextKey(alice, req)
	require.Equal(ht, int32(0), keyDesc.KeyLoc.KeyIndex)

	targetPubKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(ht, err)

	// First, try with a key descriptor that only sets the public key.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			RawKeyBytes: keyDesc.RawKeyBytes,
		},
	)

	// Now try again, this time only with the (0 index!) key locator.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: keyDesc.KeyLoc.KeyFamily,
				KeyIndex:  keyDesc.KeyLoc.KeyIndex,
			},
		},
	)

	// And now test everything again with a new key where we know the index
	// is not 0.
	req = &walletrpc.KeyReq{
		KeyFamily: testCustomKeyFamily,
	}
	keyDesc = ht.DeriveNextKey(alice, req)
	require.Equal(ht, int32(1), keyDesc.KeyLoc.KeyIndex)

	targetPubKey, err = btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(ht, err)

	// First, try with a key descriptor that only sets the public key.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			RawKeyBytes: keyDesc.RawKeyBytes,
		},
	)

	// Now try again, this time only with the key locator.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: keyDesc.KeyLoc.KeyFamily,
				KeyIndex:  keyDesc.KeyLoc.KeyIndex,
			},
		},
	)
}

// assertSignOutputRaw sends coins to a p2wkh address derived from the given
// target public key and then tries to spend that output again by invoking the
// SignOutputRaw RPC with the key descriptor provided.
func assertSignOutputRaw(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode, targetPubKey *btcec.PublicKey,
	keyDesc *signrpc.KeyDescriptor) {

	pubKeyHash := btcutil.Hash160(targetPubKey.SerializeCompressed())
	targetAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, harnessNetParams,
	)
	require.NoError(ht, err)
	targetScript, err := txscript.PayToAddrScript(targetAddr)
	require.NoError(ht, err)

	// Send some coins to the generated p2wpkh address.
	req := &lnrpc.SendCoinsRequest{
		Addr:   targetAddr.String(),
		Amount: 800_000,
	}
	ht.SendCoinFromNode(alice, req)

	// Wait until the TX is found in the mempool.
	txid := ht.AssertNumTxsInMempool(1)[0]

	targetOutputIndex := ht.GetOutputIndex(txid, targetAddr.String())

	// Clear the mempool.
	ht.MineBlocksAndAssertTx(1, 1)

	// Try to spend the output now to a new p2wkh address.
	p2wkhResp := ht.NewAddress(alice, AddrTypeWitnessPubkeyHash)

	p2wkhAdrr, err := btcutil.DecodeAddress(
		p2wkhResp.Address, harnessNetParams,
	)
	require.NoError(ht, err)

	p2wkhPkScript, err := txscript.PayToAddrScript(p2wkhAdrr)
	require.NoError(ht, err)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *txid,
			Index: uint32(targetOutputIndex),
		},
	}}
	value := int64(800_000 - 200)
	tx.TxOut = []*wire.TxOut{{
		PkScript: p2wkhPkScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	signReq := &signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs: []*signrpc.SignDescriptor{{
			Output: &signrpc.TxOut{
				PkScript: targetScript,
				Value:    800_000,
			},
			InputIndex:    0,
			KeyDesc:       keyDesc,
			Sighash:       uint32(txscript.SigHashAll),
			WitnessScript: targetScript,
		}},
	}
	signResp := ht.SignOutputRaw(alice, signReq)

	tx.TxIn[0].Witness = wire.TxWitness{
		append(signResp.RawSigs[0], byte(txscript.SigHashAll)),
		targetPubKey.SerializeCompressed(),
	}

	buf.Reset()
	require.NoError(ht, tx.Serialize(&buf))

	ht.PublishTransaction(alice, &walletrpc.Transaction{
		TxHex: buf.Bytes(),
	})

	// Wait until the spending tx is found.
	txid = ht.AssertNumTxsInMempool(1)[0]
	p2wkhOutputIndex := ht.GetOutputIndex(txid, p2wkhAdrr.String())

	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2wkhOutputIndex),
	}
	assertWalletUnspent(ht, alice, op)

	// Mine another block to clean up the mempool and to make sure the spend
	// tx is actually included in a block.
	ht.MineBlocksAndAssertTx(1, 1)
}
