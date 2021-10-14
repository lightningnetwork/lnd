package itest

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testDeriveSharedKey checks the ECDH performed by the endpoint
// DeriveSharedKey. It creates an ephemeral private key, performing an ECDH with
// the node's pubkey and a customized public key to check the validity of the
// result.
func testDeriveSharedKey(net *lntest.NetworkHarness, t *harnessTest) {
	runDeriveSharedKey(t, net.Alice)
}

// runDeriveSharedKey checks the ECDH performed by the endpoint
// DeriveSharedKey. It creates an ephemeral private key, performing an ECDH with
// the node's pubkey and a customized public key to check the validity of the
// result.
func runDeriveSharedKey(t *harnessTest, alice *lntest.HarnessNode) {
	ctxb := context.Background()

	// Create an ephemeral key, extracts its public key, and make a
	// PrivKeyECDH using the ephemeral key.
	ephemeralPriv, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t.t, err, "failed to create ephemeral key")

	ephemeralPubBytes := ephemeralPriv.PubKey().SerializeCompressed()
	privKeyECDH := &keychain.PrivKeyECDH{PrivKey: ephemeralPriv}

	// assertECDHMatch checks the correctness of the ECDH between the
	// ephemeral key and the given public key.
	assertECDHMatch := func(pub *btcec.PublicKey,
		req *signrpc.SharedKeyRequest) {

		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		resp, err := alice.SignerClient.DeriveSharedKey(ctxt, req)
		require.NoError(t.t, err, "calling DeriveSharedKey failed")

		sharedKey, _ := privKeyECDH.ECDH(pub)
		require.Equal(
			t.t, sharedKey[:], resp.SharedKey,
			"failed to derive the expected key",
		)
	}

	nodePub, err := btcec.ParsePubKey(alice.PubKey[:], btcec.S256())
	require.NoError(t.t, err, "failed to parse node pubkey")

	customizedKeyFamily := int32(keychain.KeyFamilyMultiSig)
	customizedIndex := int32(1)
	customizedPub, err := deriveCustomizedKey(
		ctxb, alice, customizedKeyFamily, customizedIndex,
	)
	require.NoError(t.t, err, "failed to create customized pubkey")

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
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		_, err := alice.SignerClient.DeriveSharedKey(ctxt, req)
		require.Error(t.t, err, "expected to have an error")
		require.Contains(
			t.t, err.Error(), match, "error failed to match",
		)
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

// deriveCustomizedKey uses the family and index to derive a public key from
// the node's walletkit client.
func deriveCustomizedKey(ctx context.Context, node *lntest.HarnessNode,
	family, index int32) (*btcec.PublicKey, error) {

	ctxt, _ := context.WithTimeout(ctx, defaultTimeout)
	req := &signrpc.KeyLocator{
		KeyFamily: family,
		KeyIndex:  index,
	}
	resp, err := node.WalletKitClient.DeriveKey(ctxt, req)
	if err != nil {
		return nil, fmt.Errorf("failed to derive key: %v", err)
	}
	pub, err := btcec.ParsePubKey(resp.RawKeyBytes, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("failed to parse node pubkey: %v", err)
	}
	return pub, nil
}
