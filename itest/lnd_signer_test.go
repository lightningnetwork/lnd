package itest

import (
	"bytes"
	"crypto/sha256"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// testDeriveSharedKey checks the ECDH performed by the endpoint
// DeriveSharedKey. It creates an ephemeral private key, performing an ECDH with
// the node's pubkey and a customized public key to check the validity of the
// result.
func testDeriveSharedKey(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	runDeriveSharedKey(ht, alice)
}

// runDeriveSharedKey checks the ECDH performed by the endpoint
// DeriveSharedKey. It creates an ephemeral private key, performing an ECDH with
// the node's pubkey and a customized public key to check the validity of the
// result.
func runDeriveSharedKey(ht *lntest.HarnessTest, alice *node.HarnessNode) {
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

		resp := alice.RPC.DeriveSharedKey(req)

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
	resp := alice.RPC.DeriveKey(deriveReq)
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
		err := alice.RPC.DeriveSharedKeyErr(req)
		require.Contains(ht, err.Error(), match, "error not match")
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
	alice := ht.NewNodeWithCoins("Alice", nil)

	runSignOutputRaw(ht, alice)
}

// runSignOutputRaw makes sure that the SignOutputRaw RPC can be used with all
// custom ways of specifying the signing key in the key descriptor/locator.
func runSignOutputRaw(ht *lntest.HarnessTest, alice *node.HarnessNode) {
	// For the next step, we need a public key. Let's use a special family
	// for this. We want this to be an index of zero.
	const testCustomKeyFamily = 44
	req := &walletrpc.KeyReq{
		KeyFamily: testCustomKeyFamily,
	}
	keyDesc := alice.RPC.DeriveNextKey(req)
	require.Equal(ht, int32(0), keyDesc.KeyLoc.KeyIndex)

	targetPubKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(ht, err)

	// First, try with a key descriptor that only sets the public key.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			RawKeyBytes: keyDesc.RawKeyBytes,
		}, txscript.SigHashAll,
	)

	// Now try again, this time only with the (0 index!) key locator.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: keyDesc.KeyLoc.KeyFamily,
				KeyIndex:  keyDesc.KeyLoc.KeyIndex,
			},
		}, txscript.SigHashAll,
	)

	// And now test everything again with a new key where we know the index
	// is not 0.
	req = &walletrpc.KeyReq{
		KeyFamily: testCustomKeyFamily,
	}
	keyDesc = alice.RPC.DeriveNextKey(req)
	require.Equal(ht, int32(1), keyDesc.KeyLoc.KeyIndex)

	targetPubKey, err = btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(ht, err)

	// First, try with a key descriptor that only sets the public key.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			RawKeyBytes: keyDesc.RawKeyBytes,
		}, txscript.SigHashAll,
	)

	// Now try again, this time only with the key locator.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: keyDesc.KeyLoc.KeyFamily,
				KeyIndex:  keyDesc.KeyLoc.KeyIndex,
			},
		}, txscript.SigHashAll,
	)

	// Finally, we'll try again, but this time with a non-default sighash.
	assertSignOutputRaw(
		ht, alice, targetPubKey, &signrpc.KeyDescriptor{
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: keyDesc.KeyLoc.KeyFamily,
				KeyIndex:  keyDesc.KeyLoc.KeyIndex,
			},
		}, txscript.SigHashSingle,
	)
}

// assertSignOutputRaw sends coins to a p2wkh address derived from the given
// target public key and then tries to spend that output again by invoking the
// SignOutputRaw RPC with the key descriptor provided.
func assertSignOutputRaw(ht *lntest.HarnessTest,
	alice *node.HarnessNode, targetPubKey *btcec.PublicKey,
	keyDesc *signrpc.KeyDescriptor,
	sigHash txscript.SigHashType) {

	pubKeyHash := btcutil.Hash160(targetPubKey.SerializeCompressed())
	targetAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, harnessNetParams,
	)
	require.NoError(ht, err)
	targetScript, err := txscript.PayToAddrScript(targetAddr)
	require.NoError(ht, err)

	// Send some coins to the generated p2wpkh address.
	req := &lnrpc.SendCoinsRequest{
		Addr:       targetAddr.String(),
		Amount:     800_000,
		TargetConf: 6,
	}
	alice.RPC.SendCoins(req)

	// Wait until the TX is found in the mempool.
	txid := ht.AssertNumTxsInMempool(1)[0]

	targetOutputIndex := ht.GetOutputIndex(txid, targetAddr.String())

	// Clear the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Try to spend the output now to a new p2wkh address.
	addrReq := &lnrpc.NewAddressRequest{Type: AddrTypeWitnessPubkeyHash}
	p2wkhResp := alice.RPC.NewAddress(addrReq)

	p2wkhAdrr, err := btcutil.DecodeAddress(
		p2wkhResp.Address, harnessNetParams,
	)
	require.NoError(ht, err)

	p2wkhPkScript, err := txscript.PayToAddrScript(p2wkhAdrr)
	require.NoError(ht, err)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  txid,
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
			Sighash:       uint32(sigHash),
			WitnessScript: targetScript,
		}},
	}
	signResp := alice.RPC.SignOutputRaw(signReq)

	tx.TxIn[0].Witness = wire.TxWitness{
		append(signResp.RawSigs[0], byte(sigHash)),
		targetPubKey.SerializeCompressed(),
	}

	buf.Reset()
	require.NoError(ht, tx.Serialize(&buf))

	alice.RPC.PublishTransaction(&walletrpc.Transaction{
		TxHex: buf.Bytes(),
	})

	// Wait until the spending tx is found.
	txid = ht.AssertNumTxsInMempool(1)[0]
	p2wkhOutputIndex := ht.GetOutputIndex(txid, p2wkhAdrr.String())

	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2wkhOutputIndex),
	}
	ht.AssertUTXOInWallet(alice, op, "")

	// Mine another block to clean up the mempool and to make sure the
	// spend tx is actually included in a block.
	ht.MineBlocksAndAssertNumTxes(1, 1)
}

// testSignVerifyMessage makes sure that the SignMessage RPC can be used with
// all custom flags by verifying with VerifyMessage. Tests both ECDSA and
// Schnorr signatures.
func testSignVerifyMessage(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	runSignVerifyMessage(ht, alice)
}

// runSignVerifyMessage makes sure that the SignMessage RPC can be used with
// all custom flags by verifying with VerifyMessage. Tests both ECDSA and
// Schnorr signatures.
func runSignVerifyMessage(ht *lntest.HarnessTest, alice *node.HarnessNode) {
	aliceMsg := []byte("alice msg")
	keyLoc := &signrpc.KeyLocator{
		KeyFamily: int32(keychain.KeyFamilyNodeKey),
		KeyIndex:  1,
	}

	// Sign a message with the default ECDSA.
	signMsgReq := &signrpc.SignMessageReq{
		Msg:        aliceMsg,
		KeyLoc:     keyLoc,
		SchnorrSig: false,
	}

	signMsgResp := alice.RPC.SignMessageSigner(signMsgReq)

	deriveCustomizedKey := func() *btcec.PublicKey {
		resp := alice.RPC.DeriveKey(keyLoc)
		pub, err := btcec.ParsePubKey(resp.RawKeyBytes)
		require.NoError(ht, err, "failed to parse node pubkey")

		return pub
	}

	customPubKey := deriveCustomizedKey()

	verifyReq := &signrpc.VerifyMessageReq{
		Msg:          aliceMsg,
		Signature:    signMsgResp.Signature,
		Pubkey:       customPubKey.SerializeCompressed(),
		IsSchnorrSig: false,
	}
	verifyResp := alice.RPC.VerifyMessageSigner(verifyReq)
	require.True(ht, verifyResp.Valid, "failed to verify message")

	// Use a different key locator.
	keyLoc = &signrpc.KeyLocator{
		KeyFamily: int32(keychain.KeyFamilyNodeKey),
		KeyIndex:  2,
	}

	// Sign a message with Schnorr signature.
	signMsgReq = &signrpc.SignMessageReq{
		Msg:        aliceMsg,
		KeyLoc:     keyLoc,
		SchnorrSig: true,
	}
	signMsgResp = alice.RPC.SignMessageSigner(signMsgReq)
	customPubKey = deriveCustomizedKey()

	// Verify the Schnorr signature.
	verifyReq = &signrpc.VerifyMessageReq{
		Msg:          aliceMsg,
		Signature:    signMsgResp.Signature,
		Pubkey:       schnorr.SerializePubKey(customPubKey),
		IsSchnorrSig: true,
	}
	verifyResp = alice.RPC.VerifyMessageSigner(verifyReq)
	require.True(ht, verifyResp.Valid, "failed to verify message")

	// Also test that we can tweak a private key and verify the message
	// against the tweaked public key.
	tweakBytes := sha256.Sum256([]byte("some text"))
	tweakedPubKey := txscript.ComputeTaprootOutputKey(
		customPubKey, tweakBytes[:],
	)

	signMsgReq.SchnorrSigTapTweak = tweakBytes[:]
	signMsgResp = alice.RPC.SignMessageSigner(signMsgReq)

	verifyReq = &signrpc.VerifyMessageReq{
		Msg:          aliceMsg,
		Signature:    signMsgResp.Signature,
		Pubkey:       schnorr.SerializePubKey(tweakedPubKey),
		IsSchnorrSig: true,
	}
	verifyResp = alice.RPC.VerifyMessageSigner(verifyReq)
	require.True(ht, verifyResp.Valid, "failed to verify message")

	// Now let's try signing and verifying a tagged hash.
	tag := []byte("lightninginvoice_requestsignature")

	signMsgReq = &signrpc.SignMessageReq{
		Msg:        aliceMsg,
		KeyLoc:     keyLoc,
		SchnorrSig: true,
		Tag:        tag,
	}
	signMsgResp = alice.RPC.SignMessageSigner(signMsgReq)
	customPubKey = deriveCustomizedKey()

	verifyReq = &signrpc.VerifyMessageReq{
		Msg:          aliceMsg,
		Signature:    signMsgResp.Signature,
		Pubkey:       schnorr.SerializePubKey(customPubKey),
		IsSchnorrSig: true,
		Tag:          tag,
	}
	verifyResp = alice.RPC.VerifyMessageSigner(verifyReq)
	require.True(ht, verifyResp.Valid, "failed to verify message")

	// Verify that both SignMessage and VerifyMessage error if a tag is
	// provided but the Schnorr option is not set.
	signMsgReq = &signrpc.SignMessageReq{
		Msg:    aliceMsg,
		KeyLoc: keyLoc,
		Tag:    tag,
	}

	expectedErr := "tag can only be used when the Schnorr signature " +
		"option is set"
	ctxt := ht.Context()
	_, err := alice.RPC.Signer.SignMessage(ctxt, signMsgReq)
	require.ErrorContains(ht, err, expectedErr)

	verifyReq = &signrpc.VerifyMessageReq{
		Msg:       aliceMsg,
		Signature: signMsgResp.Signature,
		Pubkey:    schnorr.SerializePubKey(customPubKey),
		Tag:       tag,
	}

	_, err = alice.RPC.Signer.VerifyMessage(ctxt, verifyReq)
	require.ErrorContains(ht, err, expectedErr)

	// Make sure that SignMessage throws an error if a BIP0340 or
	// TapSighash tag is provided.
	signMsgReq = &signrpc.SignMessageReq{
		Msg:        aliceMsg,
		KeyLoc:     keyLoc,
		SchnorrSig: true,
		Tag:        []byte("BIP0340/challenge"),
	}

	_, err = alice.RPC.Signer.SignMessage(ctxt, signMsgReq)
	require.ErrorContains(ht, err, "tag cannot have BIP0340 prefix")

	signMsgReq = &signrpc.SignMessageReq{
		Msg:        aliceMsg,
		KeyLoc:     keyLoc,
		SchnorrSig: true,
		Tag:        chainhash.TagTapSighash,
	}

	_, err = alice.RPC.Signer.SignMessage(ctxt, signMsgReq)
	require.ErrorContains(ht, err, "tag cannot be TapSighash")
}
