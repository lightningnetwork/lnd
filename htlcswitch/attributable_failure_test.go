package htlcswitch

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// deriveSharedSecrets derives the shared secrets for each hop along the payment
// path using the session key, mirroring the logic of sphinx's internal
// generateSharedSecrets.
func deriveSharedSecrets(t *testing.T, paymentPath []*btcec.PublicKey,
	sessionKey *btcec.PrivateKey) []sphinx.Hash256 {

	t.Helper()

	numHops := len(paymentPath)
	secrets := make([]sphinx.Hash256, numHops)

	ephemECDH := &sphinx.PrivKeyECDH{PrivKey: sessionKey}

	// First hop.
	ss, err := ephemECDH.ECDH(paymentPath[0])
	require.NoError(t, err)
	secrets[0] = ss

	// Subsequent hops: derive the next ephemeral private key using the
	// blinding factor.
	for i := 1; i < numHops; i++ {
		nextPriv, err := sphinx.NextEphemeralPriv(
			ephemECDH, paymentPath[i-1],
		)
		require.NoError(t, err)

		ephemECDH = &sphinx.PrivKeyECDH{PrivKey: nextPriv}

		ss, err = ephemECDH.ECDH(paymentPath[i])
		require.NoError(t, err)
		secrets[i] = ss
	}

	return secrets
}

// TestAttributableFailureEndToEnd exercises the full encrypt → intermediate
// encrypt → decrypt flow with attribution data and validates that HoldTimes
// are correctly populated.
func TestAttributableFailureEndToEnd(t *testing.T) {
	t.Parallel()

	const numHops = 4

	// Generate random node keys for the payment path.
	paymentPath := make([]*btcec.PublicKey, numHops)
	for i := 0; i < numHops; i++ {
		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		paymentPath[i] = privKey.PubKey()
	}

	// Use a deterministic session key.
	sessionKey, _ := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{0x42}, 32),
	)

	// Derive per-hop shared secrets.
	sharedSecrets := deriveSharedSecrets(t, paymentPath, sessionKey)

	// The failing node is hop index 2 (third node, 0-indexed).
	failingHopIdx := 2

	// Create a failure message at the failing hop.
	failureMsg := lnwire.NewFailIncorrectDetails(1000, 100)

	// Create the error encrypter at the failing hop, with a creation time
	// slightly in the past to get a non-zero hold time.
	failEncrypter := hop.NewSphinxErrorEncrypter(
		paymentPath[failingHopIdx],
		sharedSecrets[failingHopIdx],
	)
	failEncrypter.CreatedAt = time.Now().Add(-200 * time.Millisecond)

	// Encrypt at the origin of the failure.
	reason, attrData, err := failEncrypter.EncryptFirstHop(failureMsg)
	require.NoError(t, err)
	require.NotEmpty(t, reason)
	require.NotEmpty(t, attrData, "attribution data should be populated")

	// Wrap the attribution data in ExtraOpaqueData for transmission.
	extraData, err := lnwire.AttrDataToExtraData(attrData)
	require.NoError(t, err)

	// Intermediate encrypt at each hop back to the sender.
	for i := failingHopIdx - 1; i >= 0; i-- {
		intermediateEnc := hop.NewSphinxErrorEncrypter(
			paymentPath[i],
			sharedSecrets[i],
		)
		// Set a slightly older creation time to simulate hold time.
		intermediateEnc.CreatedAt = time.Now().Add(
			-100 * time.Millisecond,
		)

		// Extract attr data from the extra data (as it would come from
		// the wire message).
		attrData, err = lnwire.ExtraDataToAttrData(extraData)
		require.NoError(t, err)

		reason, attrData, err = intermediateEnc.IntermediateEncrypt(
			reason, attrData,
		)
		require.NoError(t, err)

		extraData, err = lnwire.AttrDataToExtraData(attrData)
		require.NoError(t, err)
	}

	// Now decrypt at the sender using the SphinxErrorDecrypter.
	circuit := &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: paymentPath,
	}
	decrypter := NewSphinxErrorDecrypter(circuit)

	attrData, err = lnwire.ExtraDataToAttrData(extraData)
	require.NoError(t, err)

	fwdErr, err := decrypter.DecryptError(reason, attrData)
	require.NoError(t, err)

	// Verify the failure source is identified correctly.
	// SenderIdx is 1-indexed (0 = self), so failing hop index 2 means
	// SenderIdx = 3.
	require.Equal(t, failingHopIdx+1, fwdErr.FailureSourceIdx,
		"failure source index mismatch")

	// Verify we got the right failure message back.
	wireMsg := fwdErr.WireMessage()
	incorrectDetails, ok := wireMsg.(*lnwire.FailIncorrectDetails)
	require.True(t, ok, "expected FailIncorrectDetails, got %T", wireMsg)
	require.EqualValues(t, 1000, incorrectDetails.Amount())
	require.EqualValues(t, 100, incorrectDetails.Height())

	// Verify that HoldTimes are populated. We should have hold times for
	// hops 1 through failingHopIdx (the failing node plus intermediates).
	require.NotEmpty(t, fwdErr.HoldTimes,
		"expected non-empty hold times")
}

// TestAttributableFailureWithoutAttrData tests that decryption works without
// attribution data (backward compatibility with non-attributable errors).
func TestAttributableFailureWithoutAttrData(t *testing.T) {
	t.Parallel()

	const numHops = 3

	paymentPath := make([]*btcec.PublicKey, numHops)
	for i := 0; i < numHops; i++ {
		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		paymentPath[i] = privKey.PubKey()
	}

	sessionKey, _ := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{0x33}, 32),
	)

	sharedSecrets := deriveSharedSecrets(t, paymentPath, sessionKey)

	// Failing hop is the last node.
	failingHopIdx := numHops - 1

	failureMsg := lnwire.NewFailIncorrectDetails(500, 50)

	failEncrypter := hop.NewSphinxErrorEncrypter(
		paymentPath[failingHopIdx],
		sharedSecrets[failingHopIdx],
	)

	reason, _, err := failEncrypter.EncryptFirstHop(failureMsg)
	require.NoError(t, err)

	// Intermediate hops encrypt WITHOUT using attribution data (passing
	// nil), simulating nodes that don't support attributable failures.
	for i := failingHopIdx - 1; i >= 0; i-- {
		intermediateEnc := hop.NewSphinxErrorEncrypter(
			paymentPath[i],
			sharedSecrets[i],
		)

		reason, _, err = intermediateEnc.IntermediateEncrypt(
			reason, nil,
		)
		require.NoError(t, err)
	}

	// Decrypt at the sender without attribution data.
	circuit := &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: paymentPath,
	}
	decrypter := NewSphinxErrorDecrypter(circuit)

	fwdErr, err := decrypter.DecryptError(reason, nil)
	require.NoError(t, err)

	// The failure source should still be correctly identified via the
	// legacy HMAC-based mechanism.
	require.Equal(t, failingHopIdx+1, fwdErr.FailureSourceIdx)

	wireMsg := fwdErr.WireMessage()
	incorrectDetails, ok := wireMsg.(*lnwire.FailIncorrectDetails)
	require.True(t, ok)
	require.EqualValues(t, 500, incorrectDetails.Amount())
}

// TestNewForwardingErrorHoldTimes verifies that NewForwardingError correctly
// stores and exposes HoldTimes.
func TestNewForwardingErrorHoldTimes(t *testing.T) {
	t.Parallel()

	holdTimes := []uint32{10, 20, 30, 40}
	failure := lnwire.NewFailIncorrectDetails(100, 10)

	fwdErr := NewForwardingError(failure, 3, holdTimes)

	require.Equal(t, 3, fwdErr.FailureSourceIdx)
	require.Equal(t, holdTimes, fwdErr.HoldTimes)
	require.NotNil(t, fwdErr.WireMessage())

	// With nil hold times.
	fwdErr2 := NewForwardingError(failure, 1, nil)
	require.Nil(t, fwdErr2.HoldTimes)
}
