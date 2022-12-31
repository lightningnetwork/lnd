package lnwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// MusigPartialSig...
//
// TODO(roasbeef): move to wire package?
type MusigPartialSig struct {
	sig *musig2.PartialSignature

	signerNonce [musig2.PubNonceSize]byte

	combinedNonce [musig2.PubNonceSize]byte

	signerKeys []*btcec.PublicKey
}

// NewMusigPartialSig...
//
// TODO(roasbeef): need version that lets bind the rest later?
func NewMusigPartialSig(sig *musig2.PartialSignature,
	signerNonce, combinedNonce [musig2.PubNonceSize]byte,
	signerKeys []*btcec.PublicKey) *MusigPartialSig {

	return &MusigPartialSig{
		sig:           sig,
		signerNonce:   signerNonce,
		combinedNonce: combinedNonce,
		signerKeys:    signerKeys,
	}
}

// Serialize serializes the musig2 partial signature. The serializing includes
// the combined nonce _and_ the partial signature. The final signature is
// always 64 bytes in length.
func (p *MusigPartialSig) Serialize() []byte {
	var rawSig [schnorr.SignatureSize]byte

	// For the signature, we'll encode only the x-coordinate of the
	// combined nonce point. To do this we'll need to convert the R point
	// in the sig to jacobian coordinate, and then extract the x-coord from
	// that.
	//
	// TODO(roasbeef): test, or can recompute b, then arrive at the
	// combined nonce, given: combinedNonce, combinedKey, msg
	var nonceJ btcec.JacobianPoint
	p.sig.R.AsJacobian(&nonceJ)
	nonceJ.ToAffine()

	nonceX := &nonceJ.X

	nonceX.PutBytesUnchecked(rawSig[:])
	p.sig.S.PutBytesUnchecked(rawSig[32:])

	// TODO(roasbeef): update to 98 byte serialization?

	return rawSig[:]
}

// ToSchnorrShell...
func (p *MusigPartialSig) ToSchnorrShell() *schnorr.Signature {
	var zeroVal btcec.FieldVal
	return schnorr.NewSignature(&zeroVal, p.sig.S)
}

// FromSchnorrShell...
//
// TODO(roasbeef): remove this and the above, pkg w/ nonce instead
func (p *MusigPartialSig) FromSchnorrShell(sig *schnorr.Signature) {
	var (
		partialS      btcec.ModNScalar
		partialSBytes [32]byte
	)
	copy(partialSBytes[:], sig.Serialize()[32:])
	partialS.SetBytes(&partialSBytes)

	p.sig = &musig2.PartialSignature{
		S: &partialS,
	}
}

// TODO(roasbeef): parse method, can recompute the nonce like above?

// Verify...
func (p *MusigPartialSig) Verify(msg []byte, pub *btcec.PublicKey) bool {
	var m [32]byte
	copy(m[:], msg)

	// TODO(roasbeef): need diff nonce here??

	return p.sig.Verify(
		p.signerNonce, p.combinedNonce, p.signerKeys, pub, m,
		musig2.WithSortedKeys(), musig2.WithBip86SignTweak(),
	)
}

// MusigNoncePair...
//
// TODO(roasbeef): rename to nonce1 and nonce2?
//   - or signing nonce and verification nonce
type MusigNoncePair struct {
	// LocalNonce...
	LocalNonce *musig2.Nonces

	// RemoteNonce...
	RemoteNonce *musig2.Nonces
}

// String...
func (n *MusigNoncePair) String() string {
	return fmt.Sprintf("NoncePair(verification_nonce=%x, "+
		"signing_nonce=%x)", n.LocalNonce.PubNonce[:],
		n.RemoteNonce.PubNonce[:])
}

// MusigSession...
type MusigSession struct {
	session *input.MuSig2SessionInfo

	combinedNonce [musig2.PubNonceSize]byte

	// nonces is the set of nonces that'll be used to generate/verify the
	// next commitment.
	nonces MusigNoncePair

	// nextNonces is the next set of nonces to start using once a
	// revocation or new state occurs.
	nextNonces *MusigNoncePair

	// inputTxOut...
	inputTxOut *wire.TxOut

	// signerKeys...
	signerKeys []*btcec.PublicKey

	// remoteKey...
	remoteKey keychain.KeyDescriptor

	// localKey...
	localKey keychain.KeyDescriptor

	// signer...
	signer input.MuSig2Signer

	// remoteCommit tracks if this session is for the remote commitment.
	remoteCommit bool
}

// NewMusigSession...
func NewMusigSession(noncePair MusigNoncePair,
	localKey, remoteKey keychain.KeyDescriptor,
	signer input.MuSig2Signer, inputTxOut *wire.TxOut,
	remoteCommit bool) (*MusigSession, error) {

	var localNonce, remoteNonce *musig2.Nonces

	// If we're making a session for the remote commitment, then the nonce
	// we use to sign is actually our _remote_ nonce, and their
	// verification nonce is the local nonce.
	switch {
	case remoteCommit:
		localNonce = noncePair.RemoteNonce
		remoteNonce = noncePair.LocalNonce

	// Otherwise, we're generating a signature for our local commitment (to
	// broadcast), so we'll use our normal local nonce for signing.
	default:
		localNonce = noncePair.LocalNonce
		remoteNonce = noncePair.RemoteNonce
	}

	signerKeys := []*btcec.PublicKey{localKey.PubKey, remoteKey.PubKey}
	tweakDesc := input.MuSig2Tweaks{
		TaprootBIP0086Tweak: true,
	}
	session, err := signer.MuSig2CreateSession(
		localKey.KeyLocator, signerKeys, &tweakDesc,
		[][musig2.PubNonceSize]byte{remoteNonce.PubNonce},
		musig2.WithPreGeneratedNonce(localNonce),
	)
	if err != nil {
		return nil, err
	}

	// We'll need the raw combined nonces later to be able to verify
	// partial signatures, and also combine partial signatures, so we'll
	// generate it now ourselves.
	combinedNonce, err := musig2.AggregateNonces([][musig2.PubNonceSize]byte{
		noncePair.LocalNonce.PubNonce,
		noncePair.RemoteNonce.PubNonce,
	})
	if err != nil {
		return nil, err
	}

	return &MusigSession{
		nonces:        noncePair,
		remoteKey:     remoteKey,
		localKey:      localKey,
		session:       session,
		combinedNonce: combinedNonce,
		inputTxOut:    inputTxOut,
		signerKeys:    signerKeys,
		signer:        signer,
		remoteCommit:  remoteCommit,
	}, nil
}

// taprootKeyspendSighash...
func taprootKeyspendSighash(tx *wire.MsgTx, pkScript []byte,
	value int64) ([]byte, error) {

	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(
		pkScript, value,
	)

	sigHashes := txscript.NewTxSigHashes(tx, prevOutputFetcher)

	return txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault, tx, 0, prevOutputFetcher,
	)
}

// SignCommit signs the passed commitment w/ the current signing (relative
// remote) nonce. Given nonces should only ever be used once, once the method
// returns a new nonce is returned, w/ the existing nonce blanked out.
func (m *MusigSession) SignCommit(tx *wire.MsgTx,
) (*MusigPartialSig, *[musig2.PubNonceSize]byte, error) {

	// Before we can sign, we'll need to generate the sighash for their
	// commitment transaction.
	sigHash, err := taprootKeyspendSighash(
		tx, m.inputTxOut.PkScript, m.inputTxOut.Value,
	)
	if err != nil {
		return nil, nil, err
	}

	// Now that we have our session created, we'll use it to generate the
	// initial partial signature over our sighash.
	var sigHashMsg [32]byte
	copy(sigHashMsg[:], sigHash)

	walletLog.Infof("Generating new musig2 sig for session=%x, nonces=%s",
		m.session.SessionID[:], m.nonces.String())

	sig, err := m.signer.MuSig2Sign(
		m.session.SessionID, sigHashMsg, false,
	)
	if err != nil {
		return nil, nil, err
	}

	// Now that we've generated a signature with this nonce, we'll generate
	// another nonce for the _next_ commitment. This'll go in the set of
	// nonces for the next state, as we still need the remote party's
	// verification nonce (their relative local nonce).
	nextSigningNonce, err := musig2.GenNonces(
		musig2.WithPublicKey(m.localKey.PubKey),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to gen new nonce: %w", err)
	}

	var nextNonces MusigNoncePair
	switch {
	case m.remoteCommit:
		nextNonces.RemoteNonce = nextSigningNonce
	default:
		nextNonces.LocalNonce = nextSigningNonce
	}

	m.nextNonces = &nextNonces

	// TODO(roasbeef): clean up prior session once new created?

	return NewMusigPartialSig(
		sig, m.session.PublicNonce, m.combinedNonce, m.signerKeys,
	), &nextSigningNonce.PubNonce, nil
}

// Refresh...
func (m *MusigSession) Refresh(nextNonce [musig2.PubNonceSize]byte) error {
	// At this point we should have a next nonce, otherwise this operation
	// is undefined as we haven't yet used our current nonce.
	if m.nextNonces == nil {
		// TODO(roasbeef): proper error
		return fmt.Errorf("no next nonce")
	}

	// Now that we know we have the nonce we need, we can complete the
	// nonce pair.
	if m.remoteCommit {
		m.nextNonces.LocalNonce = &musig2.Nonces{
			PubNonce: nextNonce,
		}
	} else {
		m.nextNonces.RemoteNonce = &musig2.Nonces{
			PubNonce: nextNonce,
		}
	}

	// Now we'll just re-create ourselves entirely given this new
	// information. We'll also clean up the old session since we don't need
	// it any longer.
	//
	// TODO(roasbeef): can't actually clean up here? but need the stateless
	// signer thing?
	defer m.signer.MuSig2Cleanup(m.session.SessionID)

	var err error
	m, err = NewMusigSession(
		*m.nextNonces, m.localKey, m.remoteKey, m.signer, m.inputTxOut,
		m.remoteCommit,
	)
	if err != nil {
		return err
	}

	return nil
}

// VerificationNonce...
func (m *MusigSession) VerificationNonce() [musig2.PubNonceSize]byte {
	if m.remoteCommit {
		return m.nonces.RemoteNonce.PubNonce
	} else {
		return m.nonces.LocalNonce.PubNonce
	}
}

// TODO(roasbeef): re hot signatures, maybe would re-use the state less signing
// thing after all?
//
//   * then able to safely generate nonce deterministically when it comes to
//   signing?

// VerifyCommitSig attempts to verify the passed partial signature against the
// passed commitment transaction. A keyspend sighash is assumed to generate the
// signed message. As we never re-use nonces, a new verification nonce (our
// relative local nonce) returned to transmit to the remote party, which allows
// them to generate another signature.
func (m *MusigSession) VerifyCommitSig(commitTx *wire.MsgTx,
	sig *musig2.PartialSignature) (*[musig2.PubNonceSize]byte, error) {

	// When we verify a commitment signature, we always assume that we're
	// verifying a signature on our local commitment. Therefore, we'll use:
	// their remote nonce, and also public key.
	partialSig := NewMusigPartialSig(
		sig, m.nonces.RemoteNonce.PubNonce, m.combinedNonce,
		m.signerKeys,
	)

	// With the partial sig loaded with the proper context, we'll now
	// generate the sighash that the remote party should have signed.
	sigHash, err := taprootKeyspendSighash(
		commitTx, m.inputTxOut.PkScript, m.inputTxOut.Value,
	)
	if err != nil {
		return nil, err
	}

	if !partialSig.Verify(sigHash, m.remoteKey.PubKey) {
		return nil, fmt.Errorf("invalid partial commit sig")
	}

	// At this point, we know that their signature is valid, so we'll
	// generate another verification nonce for them, so they can generate a
	// new state transition.
	//
	// TODO(roasbeef): do this conditionally?
	nextVerificationNonce, err := musig2.GenNonces()
	if err != nil {
		return nil, fmt.Errorf("unable to gen new nonce: %w", err)
	}

	m.nextNonces = &MusigNoncePair{
		RemoteNonce: nextVerificationNonce,
	}

	return &nextVerificationNonce.PubNonce, nil
}

// MusigSessionCfg...
type MusigSessionCfg struct {
	// LocalKey...
	LocalKey keychain.KeyDescriptor

	// RemoteKey...
	RemoteKey keychain.KeyDescriptor

	// LocalCommitNonces...
	LocalCommitNonces MusigNoncePair

	// RemoteCommitNonces...
	RemoteCommitNonces MusigNoncePair

	// Signer...
	Signer input.MuSig2Signer

	// InputTxOut...
	InputTxOut *wire.TxOut
}

// MusigPairSession...
//
// TODO(roasbeef): split this up into two sessions? then can just make one
// later to be able to sign the txns
//
// TODO(roasbeef): chan session?
type MusigPairSession struct {
	// LocalSession...
	LocalSession *MusigSession

	// RemoteSession...
	RemoteSession *MusigSession

	// signer...
	signer input.MuSig2Signer
}

// TODO(roasbeef): move sig here?

// NewMusigPairSession....
func NewMusigPairSession(cfg *MusigSessionCfg) (*MusigPairSession, error) {
	// Given the config passed in, we'll now create our two sessions: one
	// for the local commit, and one for the remote commit.
	//
	// The session for the local commit uses our local nonce and the remote
	// party's remote nonce. The session for the remote commit uses our
	// remote nonces, and the remote party's local nonce.
	localSession, err := NewMusigSession(
		cfg.LocalCommitNonces, cfg.LocalKey, cfg.RemoteKey,
		cfg.Signer, cfg.InputTxOut, false,
	)
	if err != nil {
		return nil, err
	}
	remoteSession, err := NewMusigSession(
		cfg.RemoteCommitNonces, cfg.LocalKey, cfg.RemoteKey,
		cfg.Signer, cfg.InputTxOut, true,
	)
	if err != nil {
		return nil, err
	}

	return &MusigPairSession{
		LocalSession:  localSession,
		RemoteSession: remoteSession,
		signer:        cfg.Signer,
	}, nil
}

// TODO(roasbeef): chan reest has a late nonce binding
