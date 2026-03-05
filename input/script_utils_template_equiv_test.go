package input

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// testKeyBytes returns deterministic key bytes for testing. The index parameter
// produces different keys for different roles by deriving private keys from a
// hash and computing the corresponding public key on secp256k1.
func testKeyBytes(t *testing.T, index byte) *btcec.PublicKey {
	t.Helper()

	hash := sha256.Sum256([]byte{index})
	privKey, _ := btcec.PrivKeyFromBytes(hash[:])

	return privKey.PubKey()
}

// testPaymentHash returns a deterministic 32-byte payment hash.
func testPaymentHash() []byte {
	h := sha256.Sum256([]byte("test-payment-preimage"))
	return h[:]
}

// TestTemplateVsBuilderEquivalence verifies that the new ScriptTemplate-based
// functions produce byte-for-byte identical output to the old ScriptBuilder
// versions for all script types.
func TestTemplateVsBuilderEquivalence(t *testing.T) {
	t.Parallel()

	// Set up test keys for various roles.
	senderKey := testKeyBytes(t, 1)
	receiverKey := testKeyBytes(t, 2)
	revokeKey := testKeyBytes(t, 3)
	selfKey := testKeyBytes(t, 4)
	delayKey := testKeyBytes(t, 5)
	remoteKey := testKeyBytes(t, 6)

	payHash := testPaymentHash()

	const (
		csvDelay    uint32 = 144
		cltvExpiry  uint32 = 800000
		leaseExpiry uint32 = 900000
	)

	t.Run("WitnessScriptHash", func(t *testing.T) {
		t.Parallel()
		witnessScript := []byte("test-witness-script")

		got, err := WitnessScriptHash(witnessScript)
		require.NoError(t, err)

		want, err := legacyWitnessScriptHash(witnessScript)
		require.NoError(t, err)

		require.Equal(t, want, got,
			"WitnessScriptHash mismatch:\n"+
				"  legacy:   %x\n  template: %x",
			want, got,
		)
	})

	t.Run("WitnessPubKeyHash", func(t *testing.T) {
		t.Parallel()
		pubkey := senderKey.SerializeCompressed()

		got, err := WitnessPubKeyHash(pubkey)
		require.NoError(t, err)

		want, err := legacyWitnessPubKeyHash(pubkey)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("GenerateP2SH", func(t *testing.T) {
		t.Parallel()
		script := []byte("test-redeem-script")

		got, err := GenerateP2SH(script)
		require.NoError(t, err)

		want, err := legacyGenerateP2SH(script)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("GenerateP2PKH", func(t *testing.T) {
		t.Parallel()
		pubkey := senderKey.SerializeCompressed()

		got, err := GenerateP2PKH(pubkey)
		require.NoError(t, err)

		want, err := legacyGenerateP2PKH(pubkey)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("GenMultiSigScript", func(t *testing.T) {
		t.Parallel()
		aPub := senderKey.SerializeCompressed()
		bPub := receiverKey.SerializeCompressed()

		got, err := GenMultiSigScript(aPub, bPub)
		require.NoError(t, err)

		want, err := legacyGenMultiSigScript(aPub, bPub)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("SenderHTLCScript/confirmed", func(t *testing.T) {
		t.Parallel()

		got, err := SenderHTLCScript(
			senderKey, receiverKey, revokeKey, payHash, true,
		)
		require.NoError(t, err)

		want, err := legacySenderHTLCScript(
			senderKey, receiverKey, revokeKey, payHash, true,
		)
		require.NoError(t, err)

		require.Equal(t, want, got,
			"SenderHTLCScript(confirmed) mismatch:\n"+
				"  legacy:   %x\n  template: %x",
			want, got,
		)
	})

	t.Run("SenderHTLCScript/unconfirmed", func(t *testing.T) {
		t.Parallel()

		got, err := SenderHTLCScript(
			senderKey, receiverKey, revokeKey, payHash, false,
		)
		require.NoError(t, err)

		want, err := legacySenderHTLCScript(
			senderKey, receiverKey, revokeKey, payHash, false,
		)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("ReceiverHTLCScript/confirmed", func(t *testing.T) {
		t.Parallel()

		got, err := ReceiverHTLCScript(
			cltvExpiry, senderKey, receiverKey, revokeKey,
			payHash, true,
		)
		require.NoError(t, err)

		want, err := legacyReceiverHTLCScript(
			cltvExpiry, senderKey, receiverKey, revokeKey,
			payHash, true,
		)
		require.NoError(t, err)

		require.Equal(t, want, got,
			"ReceiverHTLCScript(confirmed) mismatch:\n"+
				"  legacy:   %x\n  template: %x",
			want, got,
		)
	})

	t.Run("ReceiverHTLCScript/unconfirmed", func(t *testing.T) {
		t.Parallel()

		got, err := ReceiverHTLCScript(
			cltvExpiry, senderKey, receiverKey, revokeKey,
			payHash, false,
		)
		require.NoError(t, err)

		want, err := legacyReceiverHTLCScript(
			cltvExpiry, senderKey, receiverKey, revokeKey,
			payHash, false,
		)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("SecondLevelHtlcScript", func(t *testing.T) {
		t.Parallel()

		got, err := SecondLevelHtlcScript(
			revokeKey, delayKey, csvDelay,
		)
		require.NoError(t, err)

		want, err := legacySecondLevelHtlcScript(
			revokeKey, delayKey, csvDelay,
		)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("CommitScriptToSelf", func(t *testing.T) {
		t.Parallel()

		got, err := CommitScriptToSelf(csvDelay, selfKey, revokeKey)
		require.NoError(t, err)

		want, err := legacyCommitScriptToSelf(
			csvDelay, selfKey, revokeKey,
		)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("LeaseCommitScriptToSelf", func(t *testing.T) {
		t.Parallel()

		got, err := LeaseCommitScriptToSelf(
			selfKey, revokeKey, csvDelay, leaseExpiry,
		)
		require.NoError(t, err)

		want, err := legacyLeaseCommitScriptToSelf(
			selfKey, revokeKey, csvDelay, leaseExpiry,
		)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("CommitScriptUnencumbered", func(t *testing.T) {
		t.Parallel()

		got, err := CommitScriptUnencumbered(remoteKey)
		require.NoError(t, err)

		want, err := legacyCommitScriptUnencumbered(remoteKey)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("CommitScriptToRemoteConfirmed", func(t *testing.T) {
		t.Parallel()

		got, err := CommitScriptToRemoteConfirmed(remoteKey)
		require.NoError(t, err)

		want, err := legacyCommitScriptToRemoteConfirmed(remoteKey)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("LeaseCommitScriptToRemoteConfirmed", func(t *testing.T) {
		t.Parallel()

		got, err := LeaseCommitScriptToRemoteConfirmed(
			remoteKey, leaseExpiry,
		)
		require.NoError(t, err)

		want, err := legacyLeaseCommitScriptToRemoteConfirmed(
			remoteKey, leaseExpiry,
		)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("CommitScriptAnchor", func(t *testing.T) {
		t.Parallel()

		got, err := CommitScriptAnchor(senderKey)
		require.NoError(t, err)

		want, err := legacyCommitScriptAnchor(senderKey)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	t.Run("LeaseSecondLevelHtlcScript", func(t *testing.T) {
		t.Parallel()

		got, err := LeaseSecondLevelHtlcScript(
			revokeKey, delayKey, csvDelay, cltvExpiry,
		)
		require.NoError(t, err)

		want, err := legacyLeaseSecondLevelHtlcScript(
			revokeKey, delayKey, csvDelay, cltvExpiry,
		)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	// Taproot script equivalence tests. These compare the non-prod
	// (default) variant of the template functions against the old builder
	// code which also produced the non-prod scripts.
	t.Run("SenderHTLCTapLeafTimeout", func(t *testing.T) {
		t.Parallel()

		got, err := SenderHTLCTapLeafTimeout(senderKey, receiverKey)
		require.NoError(t, err)

		want, err := legacySenderHTLCTapLeafTimeout(
			senderKey, receiverKey,
		)
		require.NoError(t, err)

		require.Equal(t, want.Script, got.Script)
	})

	t.Run("SenderHTLCTapLeafSuccess", func(t *testing.T) {
		t.Parallel()

		got, err := SenderHTLCTapLeafSuccess(receiverKey, payHash)
		require.NoError(t, err)

		want, err := legacySenderHTLCTapLeafSuccess(
			receiverKey, payHash,
		)
		require.NoError(t, err)

		require.Equal(t, want.Script, got.Script)
	})

	t.Run("ReceiverHtlcTapLeafTimeout", func(t *testing.T) {
		t.Parallel()

		got, err := ReceiverHtlcTapLeafTimeout(
			senderKey, cltvExpiry,
		)
		require.NoError(t, err)

		want, err := legacyReceiverHtlcTapLeafTimeout(
			senderKey, cltvExpiry,
		)
		require.NoError(t, err)

		require.Equal(t, want.Script, got.Script)
	})

	t.Run("ReceiverHtlcTapLeafSuccess", func(t *testing.T) {
		t.Parallel()

		got, err := ReceiverHtlcTapLeafSuccess(
			receiverKey, senderKey, payHash,
		)
		require.NoError(t, err)

		want, err := legacyReceiverHtlcTapLeafSuccess(
			receiverKey, senderKey, payHash,
		)
		require.NoError(t, err)

		require.Equal(t, want.Script, got.Script)
	})

	t.Run("TaprootSecondLevelTapLeaf", func(t *testing.T) {
		t.Parallel()

		got, err := TaprootSecondLevelTapLeaf(delayKey, csvDelay)
		require.NoError(t, err)

		want, err := legacyTaprootSecondLevelTapLeaf(
			delayKey, csvDelay,
		)
		require.NoError(t, err)

		require.Equal(t, want.Script, got.Script)
	})

	t.Run("TaprootLocalCommitDelayScript", func(t *testing.T) {
		t.Parallel()

		got, err := TaprootLocalCommitDelayScript(
			csvDelay, selfKey,
		)
		require.NoError(t, err)

		want, err := legacyTaprootLocalCommitDelayScript(
			csvDelay, selfKey,
		)
		require.NoError(t, err)

		require.Equal(t, want, got,
			"TaprootLocalCommitDelayScript mismatch:\n"+
				"  legacy:   %x\n  template: %x",
			want, got,
		)
	})

	t.Run("TaprootLocalCommitRevokeScript", func(t *testing.T) {
		t.Parallel()

		got, err := TaprootLocalCommitRevokeScript(
			selfKey, revokeKey,
		)
		require.NoError(t, err)

		want, err := legacyTaprootLocalCommitRevokeScript(
			selfKey, revokeKey,
		)
		require.NoError(t, err)

		require.Equal(t, want, got)
	})

	// Log a summary of all scripts tested for visual inspection.
	t.Log("All 22 template vs builder script equivalence checks passed")
}

// TestTemplateScriptDisassembly provides human-readable output of a few key
// scripts to make it easy to verify correctness visually.
func TestTemplateScriptDisassembly(t *testing.T) {
	t.Parallel()

	senderKey := testKeyBytes(t, 1)
	receiverKey := testKeyBytes(t, 2)
	revokeKey := testKeyBytes(t, 3)
	payHash := testPaymentHash()

	// SenderHTLCScript with confirmed spend.
	script, err := SenderHTLCScript(
		senderKey, receiverKey, revokeKey, payHash, true,
	)
	require.NoError(t, err)
	t.Logf("SenderHTLCScript (confirmed):\n  %s",
		hex.EncodeToString(script))

	// ReceiverHTLCScript with confirmed spend.
	script, err = ReceiverHTLCScript(
		800000, senderKey, receiverKey, revokeKey, payHash, true,
	)
	require.NoError(t, err)
	t.Logf("ReceiverHTLCScript (confirmed):\n  %s",
		hex.EncodeToString(script))
}
