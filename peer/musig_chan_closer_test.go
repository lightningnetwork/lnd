package peer

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// newDeliveryScript returns a dummy P2TR-compatible delivery script for the
// co-op close tests.
func newDeliveryScript(prefix byte) []byte {
	script := bytes.Repeat([]byte{prefix}, 34)
	script[0] = txscript.OP_1
	script[1] = txscript.OP_DATA_32

	return script
}

// TestMusigChanCloserCompletedRoundCleanup drives a full cooperative close
// round against the real MusigSessionManager: both parties create their
// partial signatures over the same close transaction via
// CreateCloseProposal, the closer combines them via
// CompleteCooperativeClose, and both then invalidate their nonce as the RBF
// transitions do once a round completes.
//
// This is a regression test for two bugs: the round used to leave the
// backing signer session in the manager when it completed, and once that was
// fixed via Cleanup-on-InvalidateNonce, the post-round cleanup errored with
// "session not found" because the signer had already dropped the session
// during CombineSigs while MusigSession still held a reference to it.
func TestMusigChanCloserCompletedRoundCleanup(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.SimpleTaprootFeatureBit

	aliceChan, bobChan, err := lnwallet.CreateTestChannels(t, chanType)
	require.NoError(t, err, "unable to create test channels")

	// Both channels are backed by mock signers that hold real
	// MusigSessionManagers, so we can watch their live session sets.
	aliceSigner, ok := aliceChan.Signer.(*input.MockSigner)
	require.True(t, ok, "expected mock signer for alice")
	bobSigner, ok := bobChan.Signer.(*input.MockSigner)
	require.True(t, ok, "expected mock signer for bob")

	aliceCloser := NewMusigChanCloser(aliceChan)
	bobCloser := NewMusigChanCloser(bobChan)

	aliceScript := newDeliveryScript(0x01)
	bobScript := newDeliveryScript(0x02)

	// Exchange nonces: each party generates its own closing nonce and
	// hands the public part to the other party, mirroring the shutdown /
	// closing_complete nonce exchange.
	aliceNonce, err := aliceCloser.ClosingNonce()
	require.NoError(t, err, "unable to generate alice nonce")

	bobNonce, err := bobCloser.ClosingNonce()
	require.NoError(t, err, "unable to generate bob nonce")

	aliceCloser.InitRemoteNonce(bobNonce)
	bobCloser.InitRemoteNonce(aliceNonce)

	// Both parties create their closing options, which creates the backing
	// signer session on each side.
	aliceOpts, err := aliceCloser.ProposalClosingOpts()
	require.NoError(t, err, "unable to get alice closing opts")
	require.Equal(t, 1, aliceSigner.NumLiveSessions())

	bobOpts, err := bobCloser.ProposalClosingOpts()
	require.NoError(t, err, "unable to get bob closing opts")
	require.Equal(t, 1, bobSigner.NumLiveSessions())

	// Both parties now sign the same close transaction.
	feeRate := chainfee.SatPerKWeight(10000)
	fee := aliceChan.CalcFee(feeRate)

	aliceSig, aliceCloseTx, _, err := aliceChan.CreateCloseProposal(
		fee, aliceScript, bobScript, aliceOpts...,
	)
	require.NoError(t, err, "unable to create alice close proposal")

	bobSig, bobCloseTx, _, err := bobChan.CreateCloseProposal(
		fee, bobScript, aliceScript, bobOpts...,
	)
	require.NoError(t, err, "unable to create bob close proposal")

	// Both proposals must be over the same transaction for the partial
	// signatures to combine.
	require.Equal(t, aliceCloseTx.TxHash(), bobCloseTx.TxHash(),
		"close proposals differ")

	// The round completes: the closer (Alice) combines both partial
	// signatures into the final schnorr signature. This drives the real
	// MusigSessionManager, which drops the session from its set once all
	// partial signatures are combined.
	alicePartialSig, ok := aliceSig.(*lnwallet.MusigPartialSig)
	require.True(t, ok, "expected musig partial sig for alice")
	bobPartialSig, ok := bobSig.(*lnwallet.MusigPartialSig)
	require.True(t, ok, "expected musig partial sig for bob")

	localSig, remoteSig, combineOpts, err := aliceCloser.CombineClosingOpts(
		alicePartialSig.ToWireSig().PartialSig,
		bobPartialSig.ToWireSig().PartialSig,
	)
	require.NoError(t, err, "unable to combine closing opts")

	_, _, err = aliceChan.CompleteCooperativeClose(
		localSig, remoteSig, aliceScript, bobScript, fee,
		combineOpts...,
	)
	require.NoError(t, err, "unable to complete cooperative close")

	// The signer dropped the session during the combine, so nothing is
	// live at this point.
	require.Equal(t, 0, aliceSigner.NumLiveSessions(),
		"combined session should be gone from the manager")

	// The RBF transitions call InvalidateNonce once the round completes.
	// This must not error even though the signer already removed the
	// session during CombineSigs.
	require.NoError(t, aliceCloser.InvalidateNonce(),
		"post-round InvalidateNonce must not error")
	require.Equal(t, 0, aliceSigner.NumLiveSessions())

	// The closee (Bob) does the same on its side.
	bobLocalSig, bobRemoteSig, bobCombineOpts, err :=
		bobCloser.CombineClosingOpts(
			bobPartialSig.ToWireSig().PartialSig,
			alicePartialSig.ToWireSig().PartialSig,
		)
	require.NoError(t, err, "unable to combine bob closing opts")

	_, _, err = bobChan.CompleteCooperativeClose(
		bobLocalSig, bobRemoteSig, bobScript, aliceScript, fee,
		bobCombineOpts...,
	)
	require.NoError(t, err, "unable to complete bob cooperative close")

	require.Equal(t, 0, bobSigner.NumLiveSessions())
	require.NoError(t, bobCloser.InvalidateNonce(),
		"post-round InvalidateNonce must not error")
	require.Equal(t, 0, bobSigner.NumLiveSessions())
}
