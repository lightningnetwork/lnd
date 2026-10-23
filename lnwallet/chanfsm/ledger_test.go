package chanfsm

import (
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// dance runs one full commitment exchange that starts with the given party
// signing: sign, receive the signature, revoke, receive the revocation. It
// returns both ledgers and what the signer forwarded.
func dance(t *testing.T, signer, receiver Ledger) (Ledger, Ledger,
	[]ForwardRef) {

	t.Helper()

	signer, err := signer.SignCommitment()
	require.NoError(t, err)
	receiver, err = receiver.ReceiveCommitment()
	require.NoError(t, err)
	receiver, err = receiver.RevokeCommitment()
	require.NoError(t, err)
	signer, fwds, err := signer.ReceiveRevocation()
	require.NoError(t, err)

	return signer, receiver, fwds
}

// newPair returns Alice's and Bob's ledgers of a fresh channel Alice opened.
func newPair() (Ledger, Ledger) {
	return Ledger{Initiator: lntypes.Local},
		Ledger{Initiator: lntypes.Remote}
}

// TestLedgerRevocationNeedsPending checks that a revocation with nothing
// pending is refused, and that the refusal leaves the ledger alone.
func TestLedgerRevocationNeedsPending(t *testing.T) {
	t.Parallel()

	alice, _ := newPair()
	next, fwds, err := alice.ReceiveRevocation()
	require.ErrorIs(t, err, ErrUnexpectedRevocation)
	require.Empty(t, fwds)
	require.Equal(t, alice, next)
}

// TestLedgerRemovalRules checks BOLT 2's two removal rules: we may only
// remove an HTLC once it's irrevocably committed, and the peer's removal
// must name an HTLC in our current commitment.
func TestLedgerRemovalRules(t *testing.T) {
	t.Parallel()

	alice, bob := newPair()

	// Bob offers HTLC 0 and signs for it.
	bob, err := bob.AddHtlc(lntypes.Local, 0)
	require.NoError(t, err)
	alice, err = alice.AddHtlc(lntypes.Remote, 0)
	require.NoError(t, err)

	// Alice can't remove it yet: it's in no commitment.
	_, err = alice.RemoveHtlc(lntypes.Local, KindSettle, 0)
	require.ErrorAs(t, err, new(*ErrHtlcNotCommitted))

	// Bob signs, Alice revokes: the add is in Alice's commitment only.
	bob, alice, _ = dance(t, bob, alice)
	_, err = alice.RemoveHtlc(lntypes.Local, KindSettle, 0)
	require.ErrorAs(t, err, new(*ErrHtlcNotCommitted),
		"only in our own commitment")

	// Alice signs back, Bob revokes: now it is irrevocably committed,
	// and forwarded.
	alice, bob, fwds := dance(t, alice, bob)
	require.Equal(t, []ForwardRef{{Kind: KindAdd, ID: 0}}, fwds)
	alice, err = alice.RemoveHtlc(lntypes.Local, KindSettle, 0)
	require.NoError(t, err)

	// A second removal is refused.
	_, err = alice.RemoveHtlc(lntypes.Local, KindFail, 0)
	require.ErrorAs(t, err, new(*ErrHtlcRemoved))

	// Bob's side: the settle of his HTLC 0 is fine, since it's in his
	// current commitment, but a removal of an HTLC Alice offered and
	// that's in no commitment of Bob's is not.
	bob, err = bob.RemoveHtlc(lntypes.Remote, KindSettle, 0)
	require.NoError(t, err)

	alice, err = alice.AddHtlc(lntypes.Local, 0)
	require.NoError(t, err)
	_, err = bob.AddHtlc(lntypes.Remote, 0)
	require.NoError(t, err)
	_, err = alice.RemoveHtlc(lntypes.Remote, KindFail, 0)
	require.ErrorAs(t, err, new(*ErrHtlcNotCommitted))
	_, err = alice.RemoveHtlc(lntypes.Remote, KindFail, 7)
	require.ErrorAs(t, err, new(*ErrUnknownHtlc))
}

// TestLedgerAddID checks that an add must take the sender's next ID.
func TestLedgerAddID(t *testing.T) {
	t.Parallel()

	alice, _ := newPair()
	_, err := alice.AddHtlc(lntypes.Remote, 1)
	require.ErrorAs(t, err, new(*ErrHtlcID))
}

// TestLedgerFeeUpdates checks that only the initiator may update the fee, and
// that an update no commitment includes yet is replaced rather than
// appended.
func TestLedgerFeeUpdates(t *testing.T) {
	t.Parallel()

	alice, bob := newPair()
	_, err := bob.UpdateFee(lntypes.Local)
	require.ErrorIs(t, err, ErrFeeUpdateNotInitiator)
	_, err = alice.UpdateFee(lntypes.Remote)
	require.ErrorIs(t, err, ErrFeeUpdateNotInitiator)

	alice, err = alice.UpdateFee(lntypes.Local)
	require.NoError(t, err)
	alice, err = alice.UpdateFee(lntypes.Local)
	require.NoError(t, err)
	require.EqualValues(t, 1, alice.Logs.Local.LogIndex)

	// Once a commitment includes it, the next one is a new entry. A fee
	// update is never forwarded.
	bob, err = bob.UpdateFee(lntypes.Remote)
	require.NoError(t, err)
	alice, bob, _ = dance(t, alice, bob)
	alice, err = alice.UpdateFee(lntypes.Local)
	require.NoError(t, err)
	require.EqualValues(t, 2, alice.Logs.Local.LogIndex)

	_, _, fwds := dance(t, bob, alice)
	require.Empty(t, fwds)
}

// TestLedgerSignsOnlyAckedRemoteUpdates checks that a commitment we sign
// includes only the remote updates our current commitment includes, and
// that a revocation forwards each update once.
func TestLedgerSignsOnlyAckedRemoteUpdates(t *testing.T) {
	t.Parallel()

	alice, bob := newPair()
	bob, err := bob.AddHtlc(lntypes.Local, 0)
	require.NoError(t, err)
	alice, err = alice.AddHtlc(lntypes.Remote, 0)
	require.NoError(t, err)

	// Alice has received Bob's add but not his signature: a commitment
	// she signs now must not include it.
	next, err := alice.SignCommitment()
	require.NoError(t, err)
	require.True(t, next.Chains.Remote.Pending.IsSome())
	require.Zero(t, next.Chains.Remote.Tip().MsgIdx.Remote)

	// Two full exchanges forward the add exactly once: a third one,
	// carrying only a fee update, forwards nothing.
	bob, alice, _ = dance(t, bob, alice)
	alice, bob, fwds := dance(t, alice, bob)
	require.Equal(t, []ForwardRef{{Kind: KindAdd, ID: 0}}, fwds)

	alice, err = alice.UpdateFee(lntypes.Local)
	require.NoError(t, err)
	bob, err = bob.UpdateFee(lntypes.Remote)
	require.NoError(t, err)
	_, _, fwds = dance(t, alice, bob)
	require.Empty(t, fwds)
}
