package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// extractRevocationNonce is a helper function to extract the nonce from a
// RevokeAndAck message, preferring LocalNonces over LocalNonce.
func extractRevocationNonce(t *testing.T,
	msg *lnwire.RevokeAndAck) lnwire.Musig2Nonce {

	if msg.LocalNonces.IsSome() {
		noncesData := msg.LocalNonces.UnwrapOrFail(t)

		for _, nonce := range noncesData.NoncesMap {
			return nonce
		}

		// If map is empty, fall back to LocalNonce.
	}

	return msg.LocalNonce.UnwrapOrFailV(t)
}

// revokeModifier is a functional option to modify a RevokeAndAck message.
type revokeModifier func(*lnwire.RevokeAndAck)

// generateAndProcessRevocation creates fresh channels, performs a state
// transition to generate a RevokeAndAck message, optionally modifies it, and
// processes it. Returns the revocation message and channels for further
// testing.
func generateAndProcessRevocation(t *testing.T, chanType channeldb.ChannelType,
	modifier revokeModifier) (
	*lnwire.RevokeAndAck, *LightningChannel, *LightningChannel, error) {

	aliceChannel, bobChannel, err := CreateTestChannels(t, chanType)
	require.NoError(t, err)

	aliceNewCommit, err := aliceChannel.SignNextCommitment(ctxb)
	if err != nil {
		return nil, nil, nil, err
	}
	err = bobChannel.ReceiveNewCommitment(aliceNewCommit.CommitSigs)
	if err != nil {
		return nil, nil, nil, err
	}

	bobRevocation, _, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		return nil, nil, nil, err
	}

	// Apply the modifier if provided, we'll use this to mutate things to
	// test our logic.
	if modifier != nil {
		modifier(bobRevocation)
	}

	_, _, err = aliceChannel.ReceiveRevocation(bobRevocation)

	return bobRevocation, aliceChannel, bobChannel, err
}

// TestRevokeAndAckTaprootLocalNonces tests that the RevokeAndAck message
// properly populates and parses both the legacy LocalNonce field and the new
// LocalNonces field for taproot channels. This ensures backwards compatibility
// while supporting future splice operations that may require multiple nonces.
func TestRevokeAndAckTaprootLocalNonces(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SimpleTaprootFeatureBit

	t.Run("both fields populated", func(t *testing.T) {
		t.Parallel()

		revMsg, _, _, err := generateAndProcessRevocation(
			t, chanType, nil,
		)
		require.NoError(t, err)

		// Verify both fields are populated.
		require.True(
			t, revMsg.LocalNonce.IsSome(),
			"LocalNonce should be populated",
		)
		require.True(
			t, revMsg.LocalNonces.IsSome(),
			"LocalNonces should be populated",
		)
	})

	t.Run("nonces match between fields", func(t *testing.T) {
		t.Parallel()

		revMsg, _, bobChannel, err := generateAndProcessRevocation(
			t, chanType, nil,
		)
		require.NoError(t, err)

		// Verify that the noncee map field is populated and is keyed
		// properly.
		noncesData := revMsg.LocalNonces.UnwrapOrFail(t)
		require.Len(
			t, noncesData.NoncesMap, 1,
			"LocalNonces map should contain exactly one entry",
		)
		var mapNonce lnwire.Musig2Nonce
		for txid, nonce := range noncesData.NoncesMap {
			mapNonce = nonce

			// Verify it's keyed by funding txid.
			//
			//nolint:ll
			fundingTxid := bobChannel.channelState.FundingOutpoint.Hash
			require.Equal(
				t, fundingTxid, txid,
				"Nonce should be keyed by funding txid",
			)
			break
		}

		legacyNonce := revMsg.LocalNonce.UnwrapOrFailV(t)

		// Both nonces should match.
		require.Equal(
			t, legacyNonce, mapNonce,
			"Nonces in LocalNonce and LocalNonces should match",
		)
		extractedNonce := extractRevocationNonce(t, revMsg)
		require.Equal(
			t, legacyNonce, extractedNonce,
			"Extracted nonce should match legacy nonce",
		)
	})

	t.Run("receive with only LocalNonces field", func(t *testing.T) {
		t.Parallel()

		// Modify the message to clear the LocalNonce field.
		clearLocalNonce := func(rev *lnwire.RevokeAndAck) {
			rev.LocalNonce = lnwire.OptMusig2NonceTLV{}
		}

		// They should still work properly as the other nonce is
		// available.
		_, _, _, err := generateAndProcessRevocation(
			t, chanType, clearLocalNonce,
		)
		require.NoError(
			t, err,
			"should successfully process revocation "+
				"with only LocalNonces",
		)
	})

	t.Run("receive with only LocalNonce field (legacy peer)", func(t *testing.T) {
		t.Parallel()

		// Modify the message to clear the LocalNonces field.
		clearLocalNonces := func(rev *lnwire.RevokeAndAck) {
			rev.LocalNonces = lnwire.OptLocalNonces{}
		}

		// This should should successfully process with only LocalNonce
		// (backwards compat).
		_, _, _, err := generateAndProcessRevocation(
			t, chanType, clearLocalNonces,
		)
		require.NoError(
			t, err,
			"should successfully process "+
				"revocation with only LocalNonce for "+
				"backwards compatibility",
		)

	})

	t.Run("error when LocalNonces map is empty", func(t *testing.T) {
		t.Parallel()

		// Modify the message to have empty LocalNonces map and no
		// LocalNonce.
		emptyMap := func(rev *lnwire.RevokeAndAck) {
			rev.LocalNonce = lnwire.OptMusig2NonceTLV{}
			rev.LocalNonces = lnwire.SomeLocalNonces(
				lnwire.LocalNoncesData{
					NoncesMap: make(
						map[chainhash.Hash]lnwire.Musig2Nonce,
					),
				},
			)
		}

		// We should get an error when the LocalNonces map is empty.
		_, _, _, err := generateAndProcessRevocation(
			t, chanType, emptyMap,
		)
		require.Error(
			t, err, "Should error when LocalNonces map is empty",
		)
		require.Contains(
			t, err.Error(), "remote verification nonce not sent",
		)
	})

	t.Run("error when both fields missing", func(t *testing.T) {
		t.Parallel()

		clearBoth := func(rev *lnwire.RevokeAndAck) {
			rev.LocalNonce = lnwire.OptMusig2NonceTLV{}
			rev.LocalNonces = lnwire.OptLocalNonces{}
		}

		// If both fields are missing, we should get an error.
		_, _, _, err := generateAndProcessRevocation(
			t, chanType, clearBoth,
		)
		require.Error(
			t, err, "Should error when both fields are missing",
		)
		require.Contains(
			t, err.Error(), "remote verification nonce not sent",
		)
	})
}
