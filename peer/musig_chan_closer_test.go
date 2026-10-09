package peer

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestMusigChanCloserSessionCleanup asserts that the MuSig2 signer sessions
// created for cooperative close rounds are released once the round is
// finished, whether it completed or was aborted.
//
// Each RBF round calls ProposalClosingOpts() to create a fresh backing signer
// session, then hands the partial signature to the remote party. If the round
// never reaches CombineSigs (the peer disconnects, the fee doesn't match,
// etc.), InvalidateNonce() is the only place left that can release the
// session. Before the fix it only dropped the pointer, leaving one live
// session per aborted round in the signer's session manager.
func TestMusigChanCloserSessionCleanup(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.SimpleTaprootFeatureBit

	aliceChan, _, err := lnwallet.CreateTestChannels(t, chanType)
	require.NoError(t, err, "unable to create test channels")

	// The test channels are backed by a mock signer that tracks its live
	// MuSig2 sessions, so we can assert they're released.
	signer, ok := aliceChan.Signer.(*input.MockSigner)
	require.True(t, ok)

	closer := NewMusigChanCloser(aliceChan)

	// Drive five aborted co-op close rounds: each generates a local nonce,
	// binds a remote nonce, and creates the backing signer session via
	// ProposalClosingOpts(), but never combines signatures.
	const numRounds = 5
	for i := 0; i < numRounds; i++ {
		_, err := closer.ClosingNonce()
		require.NoError(t, err, "unable to generate closer nonce")

		remoteNonce, err := musig2.GenNonces(
			musig2.WithPublicKey(
				aliceChan.State().RemoteChanCfg.MultiSigKey.PubKey,
			),
		)
		require.NoError(t, err, "unable to generate remote nonce")
		closer.InitRemoteNonce(remoteNonce)

		_, err = closer.ProposalClosingOpts()
		require.NoError(t, err, "unable to get closing opts")

		// The session for this round is live in the signer's session
		// manager, waiting for the remote party's partial signature.
		require.Equal(t, 1, signer.NumLiveSessions(),
			"round %d should hold one live session", i)

		// The round is aborted, which is signalled by invalidating the
		// nonce. The backing session must be released as well.
		require.NoError(t, closer.InvalidateNonce())
		require.Equal(t, 0, signer.NumLiveSessions(),
			"aborted round %d should release its session", i)
	}

	// Invalidating again with no session or nonce held is a no-op.
	require.NoError(t, closer.InvalidateNonce())
	require.Equal(t, 0, signer.NumLiveSessions())

	// A fresh round after the aborts still works and starts from a clean
	// slate.
	_, err = closer.ClosingNonce()
	require.NoError(t, err)

	remoteNonce, err := musig2.GenNonces(
		musig2.WithPublicKey(
			aliceChan.State().RemoteChanCfg.MultiSigKey.PubKey,
		),
	)
	require.NoError(t, err)
	closer.InitRemoteNonce(remoteNonce)

	_, err = closer.ProposalClosingOpts()
	require.NoError(t, err)
	require.Equal(t, 1, signer.NumLiveSessions())
}
