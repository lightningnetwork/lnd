package lnwallet

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
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

// FromWireSig...
func (p *MusigPartialSig) FromWireSig(sig *lnwire.PartialSigWithNonce,
) *MusigPartialSig {

	p.sig = &musig2.PartialSignature{
		S: &sig.Sig,
	}
	p.signerNonce = sig.Nonce

	return p
}

// ToWireSig...
func (p *MusigPartialSig) ToWireSig() *lnwire.PartialSigWithNonce {
	return &lnwire.PartialSigWithNonce{
		PartialSig: lnwire.NewPartialSig(*p.sig.S),
		Nonce:      p.signerNonce,
	}
}

// Serialize serializes the musig2 partial signature. The serializing includes
// the signer's public nonce _and_ the partial signature. The final signature
// is always 98 bytes in length.
func (p *MusigPartialSig) Serialize() []byte {
	var b bytes.Buffer

	p.ToWireSig().Encode(&b)

	return b.Bytes()
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

	return p.sig.Verify(
		p.signerNonce, p.combinedNonce, p.signerKeys, pub, m,
		musig2.WithSortedKeys(), musig2.WithBip86SignTweak(),
	)
}

// MusigNoncePair...
type MusigNoncePair struct {
	// SigningNonce...
	SigningNonce musig2.Nonces

	// VerificationNonce...
	VerificationNonce musig2.Nonces
}

// String...
func (n *MusigNoncePair) String() string {
	return fmt.Sprintf("NoncePair(verification_nonce=%x, "+
		"signing_nonce=%x)", n.VerificationNonce.PubNonce[:],
		n.SigningNonce.PubNonce[:])
}

// MusigSession...
type MusigSession struct {
	session *input.MuSig2SessionInfo

	combinedNonce [musig2.PubNonceSize]byte

	// nonces is the set of nonces that'll be used to generate/verify the
	// next commitment.
	nonces MusigNoncePair

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

// NewPartialMusigSession...
func NewPartialMusigSession(verificationNonce musig2.Nonces,
	localKey, remoteKey keychain.KeyDescriptor,
	signer input.MuSig2Signer, inputTxOut *wire.TxOut,
	remoteCommit bool) *MusigSession {

	signerKeys := []*btcec.PublicKey{localKey.PubKey, remoteKey.PubKey}

	nonces := MusigNoncePair{
		VerificationNonce: verificationNonce,
	}

	return &MusigSession{
		nonces:       nonces,
		remoteKey:    remoteKey,
		localKey:     localKey,
		inputTxOut:   inputTxOut,
		signerKeys:   signerKeys,
		signer:       signer,
		remoteCommit: remoteCommit,
	}
}

// finalizeSession...
//
// TODO(roasbeef): make private again, add NewMusigSessionthat calls above then
// calls thisd
func (m *MusigSession) FinalizeSession(signingNonce musig2.Nonces) error {
	var (
		localNonce, remoteNonce musig2.Nonces
		err                     error
	)

	// First, we'll stash the freshly generated signing nonce. Depending on
	// who's commitment we're handling, this'll either be our generated
	// nonce, or the one we just got from the remote party.
	m.nonces.SigningNonce = signingNonce

	switch {

	// If we're making a session for the remote commitment, then the nonce
	// we use to sign is actually will be the signing nonce for the
	// session, and their nonce the verification nonce.
	case m.remoteCommit:
		localNonce = m.nonces.SigningNonce
		remoteNonce = m.nonces.VerificationNonce

	// Otherwise, we're generating/receiving a signature for our local
	// commitment (to broadcast), so now our verification nonce is the one
	// we've already generated, and we want to bind their new signing
	// nonce.
	default:
		localNonce = m.nonces.VerificationNonce
		remoteNonce = m.nonces.SigningNonce
	}

	tweakDesc := input.MuSig2Tweaks{
		TaprootBIP0086Tweak: true,
	}
	m.session, err = m.signer.MuSig2CreateSession(
		m.localKey.KeyLocator, m.signerKeys, &tweakDesc,
		[][musig2.PubNonceSize]byte{remoteNonce.PubNonce},
		musig2.WithPreGeneratedNonce(&localNonce),
	)
	if err != nil {
		return err
	}

	// We'll need the raw combined nonces later to be able to verify
	// partial signatures, and also combine partial signatures, so we'll
	// generate it now ourselves.
	m.combinedNonce, err = musig2.AggregateNonces([][musig2.PubNonceSize]byte{
		m.nonces.SigningNonce.PubNonce,
		m.nonces.VerificationNonce.PubNonce,
	})
	if err != nil {
		return nil
	}

	return nil
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
func (m *MusigSession) SignCommit(tx *wire.MsgTx) (*MusigPartialSig, error) {
	// If we already have a session, then we don't need to finalize as this
	// was done up front (symmetric nonce case, like for co-op close).
	if m.session == nil {
		// Before we can sign a new commitment, we'll need to generate
		// a fresh nonce that'll be sent along side our signature. With
		// the nonce in hand, we can finalize the session.
		//
		// TODO(roasbeef): can also pass in stuff like the sighash to
		// further bind context, etc, etc.
		signingNonce, err := musig2.GenNonces(
			musig2.WithPublicKey(m.localKey.PubKey),
		)
		if err != nil {
			return nil, err
		}
		if err := m.FinalizeSession(*signingNonce); err != nil {
			return nil, err
		}
	}

	// Once we sign with a nonce, we'll never use it again, so it's safe to
	// go ahead and clean up the session right here.
	// defer m.signer.MuSig2Cleanup(m.session.SessionID)
	//
	// TODO(roasbeef): can't clean up here as need to combine sig

	// Next we can sign, we'll need to generate the sighash for their
	// commitment transaction.
	sigHash, err := taprootKeyspendSighash(
		tx, m.inputTxOut.PkScript, m.inputTxOut.Value,
	)
	if err != nil {
		return nil, err
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
		return nil, err
	}

	return NewMusigPartialSig(
		sig, m.session.PublicNonce, m.combinedNonce, m.signerKeys,
	), nil
}

// Refresh is called once we receive a new verification nonce from the remote
// party after sending a signature. This nonce will be coupled within the
// revoke-and-ack message of the remote party.
func (m *MusigSession) Refresh(verificationNonce *musig2.Nonces,
) (*MusigSession, error) {

	return NewPartialMusigSession(
		*verificationNonce, m.localKey, m.remoteKey, m.signer,
		m.inputTxOut, m.remoteCommit,
	), nil
}

// VerificationNonce returns the current verification nonce for the session.
func (m *MusigSession) VerificationNonce() *musig2.Nonces {
	return &m.nonces.VerificationNonce
}

// musigSessionOpts...
type musigSessionOpts struct {
	customRand io.Reader
}

// defaultMusigSessionOpts...
func defaultMusigSessionOpts() *musigSessionOpts {
	return &musigSessionOpts{}
}

// MusigSessionOpt...
type MusigSessionOpt func(*musigSessionOpts)

// WithLocalCounterNonce...
func WithLocalCounterNonce(targetHeight uint64,
	shaGen shachain.Producer) MusigSessionOpt {

	return func(opt *musigSessionOpts) {
		nextPreimage, _ := shaGen.AtIndex(targetHeight)

		opt.customRand = bytes.NewBuffer(nextPreimage[:])
	}
}

// VerifyCommitSig attempts to verify the passed partial signature against the
// passed commitment transaction. A keyspend sighash is assumed to generate the
// signed message. As we never re-use nonces, a new verification nonce (our
// relative local nonce) returned to transmit to the remote party, which allows
// them to generate another signature.
func (m *MusigSession) VerifyCommitSig(commitTx *wire.MsgTx,
	sig *lnwire.PartialSigWithNonce,
	musigOpts ...MusigSessionOpt) (*musig2.Nonces, error) {

	opts := defaultMusigSessionOpts()
	for _, optFunc := range musigOpts {
		optFunc(opts)
	}

	// Before we can verify the signature, we'll need to finalize the
	// session by binding the remote party's provided signing nonce.
	if err := m.FinalizeSession(musig2.Nonces{
		PubNonce: sig.Nonce,
	}); err != nil {
		return nil, err
	}

	// When we verify a commitment signature, we always assume that we're
	// verifying a signature on our local commitment. Therefore, we'll use:
	// their remote nonce, and also public key.
	partialSig := NewMusigPartialSig(
		&musig2.PartialSignature{S: &sig.Sig},
		m.nonces.SigningNonce.PubNonce, m.combinedNonce, m.signerKeys,
	)

	walletLog.Infof("verify partial sig: %v", spew.Sdump(partialSig))

	// With the partial sig loaded with the proper context, we'll now
	// generate the sighash that the remote party should have signed.
	sigHash, err := taprootKeyspendSighash(
		commitTx, m.inputTxOut.PkScript, m.inputTxOut.Value,
	)
	if err != nil {
		return nil, err
	}

	walletLog.Infof("Verfiying new musig2 sig for session=%x, nonce=%s",
		m.session.SessionID[:], m.nonces.String())

	if !partialSig.Verify(sigHash, m.remoteKey.PubKey) {
		return nil, fmt.Errorf("invalid partial commit sig")
	}

	nonceOpts := []musig2.NonceGenOption{
		musig2.WithPublicKey(m.localKey.PubKey),
	}
	if opts.customRand != nil {
		nonceOpts = append(
			nonceOpts, musig2.WithCustomRand(opts.customRand),
		)
	}

	// At this point, we know that their signature is valid, so we'll
	// generate another verification nonce for them, so they can generate a
	// new state transition.
	nextVerificationNonce, err := musig2.GenNonces(nonceOpts...)
	if err != nil {
		return nil, fmt.Errorf("unable to gen new nonce: %w", err)
	}

	return nextVerificationNonce, nil
}

// CombineSigs...
func (m *MusigSession) CombineSigs(sigs ...*musig2.PartialSignature,
) (*schnorr.Signature, error) {

	sig, _, err := m.signer.MuSig2CombineSig(
		m.session.SessionID,
		sigs,
	)
	if err != nil {
		return nil, err
	}

	return sig, nil
}

// MusigSessionCfg...
type MusigSessionCfg struct {
	// LocalKey...
	LocalKey keychain.KeyDescriptor

	// RemoteKey...
	RemoteKey keychain.KeyDescriptor

	// LocalNonce...
	LocalNonce musig2.Nonces

	// RemoteNonce...
	RemoteNonce musig2.Nonces

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
func NewMusigPairSession(cfg *MusigSessionCfg) *MusigPairSession {
	// Given the config passed in, we'll now create our two sessions: one
	// for the local commit, and one for the remote commit.
	//
	// Both sessions will be created using only the verification nonce for
	// the local+remote party.
	localSession := NewPartialMusigSession(
		cfg.LocalNonce, cfg.LocalKey, cfg.RemoteKey,
		cfg.Signer, cfg.InputTxOut, false,
	)
	remoteSession := NewPartialMusigSession(
		cfg.RemoteNonce, cfg.LocalKey, cfg.RemoteKey,
		cfg.Signer, cfg.InputTxOut, true,
	)

	return &MusigPairSession{
		LocalSession:  localSession,
		RemoteSession: remoteSession,
		signer:        cfg.Signer,
	}
}

// TODO(roasbeef): chan reest has a late nonce binding
