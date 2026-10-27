package lnwallet

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

// TestRestoreFeeUpdateBeforeFirstRevocation checks that a fee update the
// peer acknowledged survives a restart that happens before we ever revoked a
// commitment of our own. The peer's next commitment_signed covers the fee
// update, so if we lost it we'd reject a valid signature and force close.
func TestRestoreFeeUpdateBeforeFirstRevocation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	alice, bob, err := CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	// Alice, the initiator, updates the fee and signs. Bob revokes, and
	// Alice receives his revocation. Alice has never revoked, so the
	// fee update is now on Bob's current commitment only.
	fee := alice.CommitFeeRate() + 10
	require.NoError(t, alice.UpdateFee(fee))
	require.NoError(t, bob.ReceiveUpdateFee(fee))
	aliceSig, err := alice.SignNextCommitment(ctx)
	require.NoError(t, err)
	require.NoError(t, bob.ReceiveNewCommitment(aliceSig.CommitSigs))
	bobRevocation, _, _, err := bob.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = alice.ReceiveRevocation(bobRevocation)
	require.NoError(t, err)

	// Both restart before Bob's commitment_signed reaches Alice.
	alice, err = restartChannel(alice)
	require.NoError(t, err)
	bob, err = restartChannel(bob)
	require.NoError(t, err)

	aliceSync, err := alice.channelState.ChanSyncMsg()
	require.NoError(t, err)
	bobSync, err := bob.channelState.ChanSyncMsg()
	require.NoError(t, err)
	_, _, _, err = alice.ProcessChanSyncMsg(ctx, bobSync)
	require.NoError(t, err)
	_, _, _, err = bob.ProcessChanSyncMsg(ctx, aliceSync)
	require.NoError(t, err)

	// Bob signs the commitment he owes Alice, which covers the fee
	// update, and Alice must accept it.
	bobSig, err := bob.SignNextCommitment(ctx)
	require.NoError(t, err)
	require.NoError(t, alice.ReceiveNewCommitment(bobSig.CommitSigs))
	_, _, _, err = alice.RevokeCurrentCommitment()
	require.NoError(t, err)
	require.Equal(t, fee, alice.CommitFeeRate())
}
