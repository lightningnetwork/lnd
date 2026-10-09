package bolt12handler

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/stretchr/testify/require"
)

// TestKeyRingSignerSignInvoiceRoot verifies that KeyRingSigner signs the
// tagged hash of the root under the BOLT 12 invoice tag.
func TestKeyRingSignerSignInvoiceRoot(t *testing.T) {
	t.Parallel()

	var seed [32]byte
	seed[0] = 0x01
	nodeKey, _ := btcec.PrivKeyFromBytes(seed[:])

	signer := NewKeyRingSigner(
		&mock.SecretKeyRing{RootKey: nodeKey},
		keychain.KeyLocator{Family: keychain.KeyFamilyNodeKey},
		nodeKey.PubKey(),
	)

	var root [32]byte
	root[0] = 0xAB

	sigBytes, err := signer.SignInvoiceRoot(root)
	require.NoError(t, err)

	sig, err := schnorr.ParseSignature(sigBytes[:])
	require.NoError(t, err)

	digest := chainhash.TaggedHash(
		[]byte(bolt12.InvoiceSignatureTag), root[:],
	)
	require.True(t, sig.Verify(digest[:], nodeKey.PubKey()))
}
