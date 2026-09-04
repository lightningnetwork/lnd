package itest

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// testDeriveAndStoreKey checks that the DeriveAndStoreKey endpoint records the
// keys it derives in the wallet, which advances the key family's derivation
// index past them, and that it refuses to derive an unbounded number of keys.
func testDeriveAndStoreKey(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	runDeriveAndStoreKey(ht, alice)
}

// runDeriveAndStoreKey checks that the DeriveAndStoreKey endpoint records the
// keys it derives in the wallet of the given node, which advances the key
// family's derivation index past them, and that it refuses to derive an
// unbounded number of keys.
func runDeriveAndStoreKey(ht *lntest.HarnessTest, hn *node.HarnessNode) {
	// We use a key family of our own so that no other subsystem interferes
	// with the derivation indexes we assert on below. The indexes are
	// asserted relative to where the family starts out, since a watch-only
	// wallet may already have addresses for it.
	const testCustomKeyFamily = 55

	startIndex := externalKeyCount(ht, hn, testCustomKeyFamily)
	targetIndex := startIndex + 5

	// Deriving a key without storing it must neither touch the family's
	// derivation index nor make the key known to the wallet.
	keyLoc := &signrpc.KeyLocator{
		KeyFamily: testCustomKeyFamily,
		KeyIndex:  int32(targetIndex),
	}
	unstoredDesc := hn.RPC.DeriveKey(keyLoc)
	require.Equal(
		ht, startIndex, externalKeyCount(ht, hn, testCustomKeyFamily),
	)
	assertKeyKnown(ht, hn, unstoredDesc, false)

	// Storing the very same key returns the very same key, but now fills in
	// every index up to it, so the family's next index moves past it.
	storedDesc := hn.RPC.DeriveAndStoreKey(keyLoc)
	require.Equal(ht, unstoredDesc.RawKeyBytes, storedDesc.RawKeyBytes)
	require.Equal(
		ht, targetIndex+1,
		externalKeyCount(ht, hn, testCustomKeyFamily),
	)

	// And the wallet now knows the key, which is what allows it to sign
	// with it later on.
	assertKeyKnown(ht, hn, storedDesc, true)

	// Moving the index also means the indexes we just consumed are not
	// handed out again.
	nextDesc := hn.RPC.DeriveNextKey(&walletrpc.KeyReq{
		KeyFamily: testCustomKeyFamily,
	})
	require.EqualValues(ht, targetIndex+1, nextDesc.KeyLoc.KeyIndex)

	// Storing a key at an index the family already advanced past leaves the
	// index alone, it is never rewound.
	hn.RPC.DeriveAndStoreKey(&signrpc.KeyLocator{
		KeyFamily: testCustomKeyFamily,
		KeyIndex:  int32(startIndex),
	})
	require.Equal(
		ht, targetIndex+2,
		externalKeyCount(ht, hn, testCustomKeyFamily),
	)

	// An index that is too far ahead is refused outright instead of
	// deriving an unbounded number of keys, and leaves the family alone.
	err := hn.RPC.DeriveAndStoreKeyErr(&signrpc.KeyLocator{
		KeyFamily: testCustomKeyFamily,
		KeyIndex: int32(targetIndex) + 2 +
			keychain.MaxKeyIndexExtension,
	})
	require.ErrorContains(ht, err, "key index extension too large")
	require.Equal(
		ht, targetIndex+2,
		externalKeyCount(ht, hn, testCustomKeyFamily),
	)

	// Neither a negative family nor a negative index can be mapped to the
	// unsigned values the wallet uses, so both are rejected.
	err = hn.RPC.DeriveAndStoreKeyErr(&signrpc.KeyLocator{
		KeyFamily: -1,
		KeyIndex:  0,
	})
	require.ErrorContains(ht, err, "key family must not be negative")

	// The families lnd itself derives from cannot be advanced externally.
	err = hn.RPC.DeriveAndStoreKeyErr(&signrpc.KeyLocator{
		KeyFamily: int32(keychain.KeyFamilyNodeKey),
		KeyIndex:  1,
	})
	require.ErrorContains(ht, err, "reserved by lnd")

	err = hn.RPC.DeriveAndStoreKeyErr(&signrpc.KeyLocator{
		KeyFamily: testCustomKeyFamily,
		KeyIndex:  -1,
	})
	require.ErrorContains(ht, err, "key index must not be negative")
}

// runDeriveAndStoreKeySigning checks that a key derived at an arbitrary index
// can only be signed with if it was stored in the wallet. Resolving a key
// locator from a public key alone requires the wallet to know the key's
// address, which is only the case for a stored key.
//
// NOTE: This is only meaningful against a watch-only node driving a remote
// signer, since a node with a local wallet falls back to scanning a range of
// key indexes when it is given a public key without a key locator.
func runDeriveAndStoreKeySigning(ht *lntest.HarnessTest,
	hn *node.HarnessNode) {

	const (
		testCustomKeyFamily = 56
		targetIndex         = 7
	)

	keyLoc := &signrpc.KeyLocator{
		KeyFamily: testCustomKeyFamily,
		KeyIndex:  targetIndex,
	}

	// A key that is only derived, but not stored, cannot be signed with
	// when it is referenced by its public key, since the wallet has no
	// address to map it back to a key locator with.
	unstoredDesc := hn.RPC.DeriveKey(keyLoc)
	unstoredPubKey, err := btcec.ParsePubKey(unstoredDesc.RawKeyBytes)
	require.NoError(ht, err)

	targetAddr, err := address.NewAddressWitnessPubKeyHash(
		address.Hash160(unstoredPubKey.SerializeCompressed()),
		harnessNetParams,
	)
	require.NoError(ht, err)

	targetScript, err := txscript.PayToAddrScript(targetAddr)
	require.NoError(ht, err)

	// The transaction we ask to sign never makes it to the chain, the key
	// cannot even be resolved.
	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
	}}
	tx.TxOut = []*wire.TxOut{{
		PkScript: targetScript,
		Value:    799_800,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	err = hn.RPC.SignOutputRawErr(&signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs: []*signrpc.SignDescriptor{{
			Output: &signrpc.TxOut{
				PkScript: targetScript,
				Value:    800_000,
			},
			InputIndex: 0,
			KeyDesc: &signrpc.KeyDescriptor{
				RawKeyBytes: unstoredDesc.RawKeyBytes,
			},
			Sighash:       uint32(txscript.SigHashAll),
			WitnessScript: targetScript,
		}},
	})
	require.ErrorContains(ht, err, "error fetching address info")

	// Storing the key makes it known to the wallet, so the very same
	// reference by public key now resolves and produces a valid signature.
	storedDesc := hn.RPC.DeriveAndStoreKey(keyLoc)
	require.Equal(ht, unstoredDesc.RawKeyBytes, storedDesc.RawKeyBytes)

	assertSignOutputRaw(
		ht, hn, unstoredPubKey, &signrpc.KeyDescriptor{
			RawKeyBytes: storedDesc.RawKeyBytes,
		}, txscript.SigHashAll,
	)
}

// externalKeyCount returns the number of external keys the given node's wallet
// has derived for the given key family, which is also the index the next key
// derived for that family will have.
func externalKeyCount(ht *lntest.HarnessTest, hn *node.HarnessNode,
	keyFam keychain.KeyFamily) uint32 {

	accounts := hn.RPC.ListAccounts(&walletrpc.ListAccountsRequest{})

	// The accounts of our key families are the ones under the BIP-0043
	// purpose lnd uses, with the family as the last path element.
	purpose := fmt.Sprintf("/%d'/", keychain.BIP0043Purpose)
	suffix := fmt.Sprintf("/%d'", keyFam)
	for _, account := range accounts.Accounts {
		path := account.DerivationPath
		if !strings.Contains(path, purpose) {
			continue
		}

		if strings.HasSuffix(path, suffix) {
			return account.ExternalKeyCount
		}
	}

	return 0
}

// assertKeyKnown asserts whether the given node's wallet has a record of the
// given key, which it needs in order to map the key back to its key locator.
//
// NOTE: For keys in lnd's own key scope, ListAddresses reports the public key
// itself instead of an address, since those keys are not used as addresses.
func assertKeyKnown(ht *lntest.HarnessTest, hn *node.HarnessNode,
	desc *signrpc.KeyDescriptor, known bool) {

	resp := hn.RPC.ListAddresses(&walletrpc.ListAddressesRequest{
		ShowCustomAccounts: true,
	})

	var found bool
	for _, account := range resp.AccountWithAddresses {
		for _, walletAddr := range account.Addresses {
			if bytes.Equal(walletAddr.PublicKey, desc.RawKeyBytes) {
				found = true
			}
		}
	}

	require.Equalf(
		ht, known, found, "key %x known to wallet", desc.RawKeyBytes,
	)
}

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

	pubKeyHash := address.Hash160(targetPubKey.SerializeCompressed())
	targetAddr, err := address.NewAddressWitnessPubKeyHash(
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

	p2wkhAdrr, err := address.DecodeAddress(
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
