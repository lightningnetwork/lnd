package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// nodeType is an enum that represents the two nodes in our test harness.
type nodeType uint8

const (
	// nodeAlice is the node that initiates the session.
	nodeAlice nodeType = iota

	// nodeBob is the node that responds to the session.
	nodeBob
)

type muSessionHarness struct {
	aliceCommit *wire.MsgTx
	bobCommit   *wire.MsgTx

	aliceSession *MusigPairSession

	bobSession *MusigPairSession

	t *testing.T
}

func (h *muSessionHarness) selectSession(nodeName nodeType) *MusigPairSession {
	var targetSession *MusigPairSession
	switch nodeName {
	case nodeAlice:
		targetSession = h.aliceSession
	case nodeBob:
		targetSession = h.bobSession
	}

	return targetSession
}

func (h *muSessionHarness) refreshSession(nodeName nodeType,
	nextNonce *musig2.Nonces, revoke bool) {

	var session *MusigPairSession
	switch nodeName {
	case nodeAlice:
		session = h.aliceSession
	case nodeBob:
		session = h.bobSession
	}

	var err error

	// If this isn't in response to a revoke, then we just signed, so we'll
	// refresh our local session with the newly generated verification
	// nonce.
	if !revoke {
		session.LocalSession, err = session.LocalSession.Refresh(
			nextNonce,
		)
	} else {
		session.RemoteSession, err = session.RemoteSession.Refresh(
			nextNonce,
		)
	}
	require.NoError(h.t, err)
}

// SignCommitment signs a new remote commitment. This is equivalent to sending
// a CommitSig message on the normal LN protocol.
func (h *muSessionHarness) SignCommitment(nodeName nodeType) *MusigPartialSig {
	targetSession := h.selectSession(nodeName)

	sig, err := targetSession.RemoteSession.SignCommit(h.bobCommit)
	require.NoError(h.t, err)

	return sig
}

// VerifyAndSignCommitment verifies a remote commitment, then signs a new
// commitment. This combines receiving a signature, then sending a revoke
// message.
func (h *muSessionHarness) VerifyAndSignCommitment(nodeName nodeType,
	sig *MusigPartialSig) (*MusigPartialSig, *musig2.Nonces) {

	muSession := h.selectSession(nodeName)

	// Verify the commitment transaction from the remote party. The nonce
	// returned will be sent along side the "revoke and ack" message in the
	// actual p2p protocol.
	nextVerificationNonce, err := muSession.LocalSession.VerifyCommitSig(
		h.bobCommit, sig.ToWireSig(),
	)
	require.NoError(h.t, err)

	// As we've just used our verification nonce to verify the remote sign,
	// we'll refresh our local session with the new nonce.
	h.refreshSession(nodeName, nextVerificationNonce, false)

	// Next, sign a new version of the commitment for the remote party.
	// This uses a JIT nonce that'll be sent along side the signature, and
	// consumes the verification nonce of the remote party.
	remoteSig, err := muSession.RemoteSession.SignCommit(h.aliceCommit)
	require.NoError(h.t, err)

	return remoteSig, nextVerificationNonce
}

// VerifyCommitment verifies a remote commitment, then sends a nonce. This is
// equivalent to verifying a new incoming commitment, then sending a revoke
// message.
func (h *muSessionHarness) VerifyCommitment(nodeName nodeType,
	sig *MusigPartialSig, nextNonce *musig2.Nonces) *musig2.Nonces {

	muSession := h.selectSession(nodeName)

	// We'll now verify the incoming signature, then refresh our local
	// session as we've used up our prior verification nonce.
	nextVerificationNonce, err := muSession.LocalSession.VerifyCommitSig(
		h.aliceCommit, sig.ToWireSig(),
	)
	require.NoError(h.t, err)
	h.refreshSession(nodeName, nextVerificationNonce, false)

	// The packaged nonce is the remote party's new verification nonce, so
	// we'll refresh their remote commitment: we just got the revocation
	// and the sig in the same message.
	h.refreshSession(nodeName, nextNonce, true)

	return nextVerificationNonce
}

// ProcessVerificationNonce processes a verification nonce from the remote.
// This is equivalent to receiving the revoke from a remote party after you
// kicked off the commitment dance.
func (h *muSessionHarness) ProcessVerificationNonce(nodeName nodeType,
	nextNonce *musig2.Nonces) {

	h.refreshSession(nodeName, nextNonce, true)
}

func newMuSessionHarness(t *testing.T) *muSessionHarness {
	aliceCommit := wire.NewMsgTx(2)
	aliceCommit.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 1,
		},
	})

	bobCommit := wire.NewMsgTx(2)
	bobCommit.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 2,
		},
	})

	alicePriv, alicePub := btcec.PrivKeyFromBytes(testWalletPrivKey)
	aliceSigner := input.NewMockSigner([]*btcec.PrivateKey{alicePriv}, nil)

	aliceVerificationNonce, err := musig2.GenNonces(
		musig2.WithPublicKey(alicePub),
	)
	require.NoError(t, err)

	bobPriv, bobPub := btcec.PrivKeyFromBytes(bobsPrivKey)
	bobSigner := input.NewMockSigner([]*btcec.PrivateKey{bobPriv}, nil)

	bobVerificationNonce, err := musig2.GenNonces(
		musig2.WithPublicKey(bobPub),
	)
	require.NoError(t, err)

	inputTxOut := &wire.TxOut{
		Value:    1000,
		PkScript: testHdSeed[:],
	}

	aliceSession := NewMusigPairSession(&MusigSessionCfg{
		LocalKey: keychain.KeyDescriptor{
			PubKey: alicePub,
		},
		RemoteKey: keychain.KeyDescriptor{
			PubKey: bobPub,
		},
		LocalNonce:  *aliceVerificationNonce,
		RemoteNonce: *bobVerificationNonce,
		Signer:      aliceSigner,
		InputTxOut:  inputTxOut,
	})

	bobSession := NewMusigPairSession(&MusigSessionCfg{
		LocalKey: keychain.KeyDescriptor{
			PubKey: bobPub,
		},
		RemoteKey: keychain.KeyDescriptor{
			PubKey: alicePub,
		},
		LocalNonce:  *bobVerificationNonce,
		RemoteNonce: *aliceVerificationNonce,
		Signer:      bobSigner,
		InputTxOut:  inputTxOut,
	})

	return &muSessionHarness{
		aliceCommit:  aliceCommit,
		aliceSession: aliceSession,
		bobCommit:    bobCommit,
		bobSession:   bobSession,
		t:            t,
	}
}

// TestMusigSession tests that we're able to send and receive signatures for
// the set of asymmetric musig sessions. This tests proper nonce rotation and
// signature verification.
func TestMusigSesssion(t *testing.T) {
	t.Parallel()

	// First, we'll make a new musig session between Alice and Bob. This is
	// 4 sessions total, as both sides maintain a session for their local
	// commitment, and one for the remote commitment.
	muSessions := newMuSessionHarness(t)

	t.Run("session_round_trips", func(t *testing.T) {
		const numRounds = 10
		for i := 0; i < numRounds; i++ {
			// We'll now simulate a full commitment dance.
			//
			// To start, Alice will sign a new commitment for Bob's
			// remote commitment.
			aliceSig := muSessions.SignCommitment(nodeAlice)

			// Bob will then verify Alice's signature, and sign a
			// new commitment for Alice.
			bobSig, bobNonce := muSessions.VerifyAndSignCommitment(
				nodeBob, aliceSig,
			)

			// Next Alice will process Bob's signature, and then
			// generate a new verification nonce to he can sign the
			// next commitment.
			aliceNonce := muSessions.VerifyCommitment(
				nodeAlice, bobSig, bobNonce,
			)

			// To conclude the commitment dance, Bob will process
			// Alice's new verification nonce.
			muSessions.ProcessVerificationNonce(nodeBob, aliceNonce)

			// Modify the commitments after each round to simulate
			// the LN protocol commitment randomness structure
			// (sequence+locktime change each state, etc).
			muSessions.aliceCommit.TxIn[0].PreviousOutPoint.Index++
			muSessions.bobCommit.TxIn[0].PreviousOutPoint.Index++
		}
	})

	t.Run("no_finalize_error", func(t *testing.T) {
		// If a local party attempts to sign for their local commitment
		// without finalizing first, they'll get this error.
		_, err := muSessions.aliceSession.LocalSession.SignCommit(
			muSessions.aliceCommit,
		)
		require.ErrorIs(t, err, ErrSessionNotFinalized)
	})
}
