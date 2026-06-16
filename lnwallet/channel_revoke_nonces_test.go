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
// properly populates the nonce fields based on the channel type.
// Staging taproot channels populate only LocalNonce (legacy behavior).
// Final taproot channels populate only LocalNonces (map-based).
// This ensures backwards compatibility while supporting production peers.
func TestRevokeAndAckTaprootLocalNonces(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SimpleTaprootFeatureBit

	t.Run("legacy nonce type only populates LocalNonce",
		func(t *testing.T) {
			t.Parallel()

			// Staging taproot channels populate only the
			// LocalNonce field (legacy behavior).
			revMsg, _, _, err := generateAndProcessRevocation(
				t, chanType, nil,
			)
			require.NoError(t, err)

			// Verify only LocalNonce is populated (legacy
			// behavior).
			require.True(
				t, revMsg.LocalNonce.IsSome(),
				"LocalNonce should be populated for legacy "+
					"nonce type",
			)
			require.True(
				t, revMsg.LocalNonces.IsNone(),
				"LocalNonces should NOT be populated for "+
					"legacy nonce type",
			)
		})

	t.Run("extracted nonce from legacy field", func(t *testing.T) {
		t.Parallel()

		revMsg, _, _, err := generateAndProcessRevocation(
			t, chanType, nil,
		)
		require.NoError(t, err)

		// Verify we can extract the nonce from the legacy field.
		legacyNonce := revMsg.LocalNonce.UnwrapOrFailV(t)
		extractedNonce := extractRevocationNonce(t, revMsg)
		require.Equal(
			t, legacyNonce, extractedNonce,
			"Extracted nonce should match legacy nonce",
		)
	})

	t.Run("receive with only LocalNonces field", func(t *testing.T) {
		t.Parallel()

		// We need to know the funding txid to use as the map key,
		// so we first create channels to get it, then use a
		// modifier that moves the nonce to the correct map key.
		aliceChannel, bobChannel, err := CreateTestChannels(
			t, chanType,
		)
		require.NoError(t, err)

		fundingTxid := aliceChannel.channelState.FundingOutpoint.Hash

		aliceNewCommit, err := aliceChannel.SignNextCommitment(ctxb)
		require.NoError(t, err)

		err = bobChannel.ReceiveNewCommitment(
			aliceNewCommit.CommitSigs,
		)
		require.NoError(t, err)

		bobRevocation, _, _, err := bobChannel.RevokeCurrentCommitment()
		require.NoError(t, err)

		// Move the nonce from LocalNonce to LocalNonces map,
		// keyed by the actual funding txid.
		legacyNonce := bobRevocation.LocalNonce.UnwrapOrFailV(t)
		noncesMap := make(
			map[chainhash.Hash]lnwire.Musig2Nonce,
		)
		noncesMap[fundingTxid] = legacyNonce
		bobRevocation.LocalNonces = lnwire.SomeLocalNonces(
			lnwire.LocalNoncesData{NoncesMap: noncesMap},
		)
		bobRevocation.LocalNonce = lnwire.OptMusig2NonceTLV{}

		_, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
		require.NoError(
			t, err,
			"should successfully process revocation "+
				"with only LocalNonces",
		)
	})

	t.Run("receive with only LocalNonce field (legacy peer)",
		func(t *testing.T) {
			t.Parallel()

			// Modify the message to clear the LocalNonces field.
			clearLocalNonces := func(rev *lnwire.RevokeAndAck) {
				rev.LocalNonces = lnwire.OptLocalNonces{}
			}

			// Processing should still succeed with only LocalNonce
			// (backwards compat).
			_, _, _, err := generateAndProcessRevocation(
				t, chanType, clearLocalNonces,
			)
			require.NoError(
				t, err,
				"successfully process "+
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

			emptyNonces := make(
				map[chainhash.Hash]lnwire.Musig2Nonce,
			)
			rev.LocalNonces = lnwire.SomeLocalNonces(
				lnwire.LocalNoncesData{
					NoncesMap: emptyNonces,
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
			t, err.Error(), "no nonce for funding txid",
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
