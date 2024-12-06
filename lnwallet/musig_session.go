package lnwallet

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
)

// MusigCommitType is an enum that denotes if this is the local or remote
// commitment.
type MusigCommitType uint8

const (
	// LocalMusigCommit denotes that this a session for the local
	// commitment.
	LocalMusigCommit MusigCommitType = iota

	// RemoteMusigCommit denotes that this is a session for the remote
	// commitment.
	RemoteMusigCommit
)

var (
	// ErrSessionNotFinalized is returned when the SignCommit method is
	// called for a local commitment, without the session being finalized
	// (missing nonce).
	ErrSessionNotFinalized = fmt.Errorf("musig2 session not finalized")
)

// tapscriptRootToSignOpt is a function that takes a tapscript root and returns
// a MuSig2 sign opt that'll apply the tweak when signing+verifying.
func tapscriptRootToSignOpt(root chainhash.Hash) musig2.SignOption {
	return musig2.WithTaprootSignTweak(root[:])
}

// TapscriptRootToTweak is a helper function that converts a tapscript root
// into a tweak that can be used with the MuSig2 API.
func TapscriptRootToTweak(root chainhash.Hash) input.MuSig2Tweaks {
	return input.MuSig2Tweaks{
		TaprootTweak: root[:],
	}
}

// MusigPartialSig is a wrapper around the base musig2.PartialSignature type
// that also includes information about the set of nonces used, and also the
// signer. This allows us to implement the input.Signature interface, as that
// requires the ability to perform abstract verification based on a public key.
type MusigPartialSig struct {
	// sig is the actual musig2 partial signature.
	sig *musig2.PartialSignature

	// signerNonce is the nonce used by the signer to generate the partial
	// signature.
	signerNonce lnwire.Musig2Nonce

	// combinedNonce is the combined nonce of all signers.
	combinedNonce lnwire.Musig2Nonce

	// signerKeys is the set of public keys of all signers.
	signerKeys []*btcec.PublicKey

	// tapscriptTweak is an optional tweak, that if specified, will be used
	// instead of the normal BIP 86 tweak when validating the signature.
	tapscriptTweak fn.Option[chainhash.Hash]
}

// NewMusigPartialSig creates a new MuSig2 partial signature.
func NewMusigPartialSig(sig *musig2.PartialSignature, signerNonce,
	combinedNonce lnwire.Musig2Nonce, signerKeys []*btcec.PublicKey,
	tapscriptTweak fn.Option[chainhash.Hash]) *MusigPartialSig {

	return &MusigPartialSig{
		sig:            sig,
		signerNonce:    signerNonce,
		combinedNonce:  combinedNonce,
		signerKeys:     signerKeys,
		tapscriptTweak: tapscriptTweak,
	}
}

// FromWireSig maps a wire partial sig to this internal type that we'll use to
// perform signature validation.
func (p *MusigPartialSig) FromWireSig(
	sig *lnwire.PartialSigWithNonce) *MusigPartialSig {

	p.sig = &musig2.PartialSignature{
		S: &sig.Sig,
	}
	p.signerNonce = sig.Nonce

	return p
}

// ToWireSig maps the partial signature to something that we can use to write
// out for the wire protocol.
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

	_ = p.ToWireSig().Encode(&b)

	return b.Bytes()
}

// ToSchnorrShell converts the musig partial signature to a regular schnorr.
// This schnorr signature uses a zero value for the 'r' field, so we're just
// only using the last 32-bytes of the signature. This is useful when we need
// to convert an HTLC schnorr signature into something we can send using the
// existing messages.
func (p *MusigPartialSig) ToSchnorrShell() *schnorr.Signature {
	var zeroVal btcec.FieldVal
	return schnorr.NewSignature(&zeroVal, p.sig.S)
}

// FromSchnorrShell takes a schnorr signature and parses out the last 32 bytes
// as a normal musig2 partial signature.
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

// Verify attempts to verify the partial musig2 signature using the passed
// message and signer public key.
//
// NOTE: This implements the input.Signature interface.
func (p *MusigPartialSig) Verify(msg []byte, pub *btcec.PublicKey) bool {
	var m [32]byte
	copy(m[:], msg)

	// If we have a tapscript tweak, then we'll use that as a tweak
	// otherwise, we'll fall back to the normal BIP 86 sign tweak.
	signOpts := fn.MapOption(tapscriptRootToSignOpt)(
		p.tapscriptTweak,
	).UnwrapOr(musig2.WithBip86SignTweak())

	return p.sig.Verify(
		p.signerNonce, p.combinedNonce, p.signerKeys, pub, m,
		musig2.WithSortedKeys(), signOpts,
	)
}

// MusigNoncePair holds the two nonces needed to sign/verify a new commitment
// state. The signer nonce is the nonce used by the signer (remote nonce), and
// the verification nonce, the nonce used by the verifier (local nonce).
type MusigNoncePair struct {
	// SigningNonce is the nonce used by the signer to sign the commitment.
	SigningNonce musig2.Nonces

	// VerificationNonce is the nonce used by the verifier to verify the
	// commitment.
	VerificationNonce musig2.Nonces
}

// String returns a string representation of the MusigNoncePair.
func (n *MusigNoncePair) String() string {
	return fmt.Sprintf("NoncePair(verification_nonce=%x, "+
		"signing_nonce=%x)", n.VerificationNonce.PubNonce[:],
		n.SigningNonce.PubNonce[:])
}

// TapscriptRootToTweak is a function that takes a MuSig2 taproot tweak and
// returns the root hash of the tapscript tree.
func muSig2TweakToRoot(tweak input.MuSig2Tweaks) chainhash.Hash {
	var root chainhash.Hash
	copy(root[:], tweak.TaprootTweak)
	return root
}

// MusigSession abstracts over the details of a logical musig session. A single
// session is used for each commitment transactions. The sessions use a JIT
// nonce style, wherein part of the session can be created using only the
// verifier nonce. Once a new state is signed, then the signer nonce is
// generated. Similarly, the verifier then uses the received signer nonce to
// complete the session and verify the incoming signature.
type MusigSession struct {
	// session is the backing musig2 session. We'll use this to interact
	// with the musig2 signer.
	session *input.MuSig2SessionInfo

	// combinedNonce is the combined nonce of all signers.
	combinedNonce lnwire.Musig2Nonce

	// nonces is the set of nonces that'll be used to generate/verify the
	// next commitment.
	nonces MusigNoncePair

	// inputTxOut is the funding input.
	inputTxOut *wire.TxOut

	// signerKeys is the set of public keys of all signers.
	signerKeys []*btcec.PublicKey

	// remoteKey is the key desc of the remote key.
	remoteKey keychain.KeyDescriptor

	// localKey is the key desc of the local key.
	localKey keychain.KeyDescriptor

	// signer is the signer that'll be used to interact with the musig
	// session.
	signer input.MuSig2Signer

	// commitType tracks if this is the session for the local or remote
	// commitment.
	commitType MusigCommitType

	// tapscriptTweak is an optional tweak, that if specified, will be used
	// instead of the normal BIP 86 tweak when creating the MuSig2
	// aggregate key and session.
	tapscriptTweak fn.Option[input.MuSig2Tweaks]
}

// NewPartialMusigSession creates a new musig2 session given only the
// verification nonce (local nonce), and the other information that has already
// been bound to the session.
func NewPartialMusigSession(verificationNonce musig2.Nonces,
	localKey, remoteKey keychain.KeyDescriptor, signer input.MuSig2Signer,
	inputTxOut *wire.TxOut, commitType MusigCommitType,
	tapscriptTweak fn.Option[input.MuSig2Tweaks]) *MusigSession {

	signerKeys := []*btcec.PublicKey{localKey.PubKey, remoteKey.PubKey}

	nonces := MusigNoncePair{
		VerificationNonce: verificationNonce,
	}

	return &MusigSession{
		nonces:         nonces,
		remoteKey:      remoteKey,
		localKey:       localKey,
		inputTxOut:     inputTxOut,
		signerKeys:     signerKeys,
		signer:         signer,
		commitType:     commitType,
		tapscriptTweak: tapscriptTweak,
	}
}

// FinalizeSession finalizes the session given the signer nonce.  This is
// called before signing or verifying a new commitment.
func (m *MusigSession) FinalizeSession(signingNonce musig2.Nonces) error {
	var (
		localNonce, remoteNonce musig2.Nonces
		err                     error
	)

	// First, we'll stash the freshly generated signing nonce. Depending on
	// who's commitment we're handling, this'll either be our generated
	// nonce, or the one we just got from the remote party.
	m.nonces.SigningNonce = signingNonce

	switch m.commitType {
	// If we're making a session for the remote commitment, then the nonce
	// we use to sign is actually will be the signing nonce for the
	// session, and their nonce the verification nonce.
	case RemoteMusigCommit:
		localNonce = m.nonces.SigningNonce
		remoteNonce = m.nonces.VerificationNonce

	// Otherwise, we're generating/receiving a signature for our local
	// commitment (to broadcast), so now our verification nonce is the one
	// we've already generated, and we want to bind their new signing
	// nonce.
	case LocalMusigCommit:
		localNonce = m.nonces.VerificationNonce
		remoteNonce = m.nonces.SigningNonce
	}

	tweakDesc := m.tapscriptTweak.UnwrapOr(input.MuSig2Tweaks{
		TaprootBIP0086Tweak: true,
	})
	m.session, err = m.signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, m.localKey.KeyLocator, m.signerKeys,
		&tweakDesc, [][musig2.PubNonceSize]byte{remoteNonce.PubNonce},
		&localNonce,
	)
	if err != nil {
		return err
	}

	// We'll need the raw combined nonces later to be able to verify
	// partial signatures, and also combine partial signatures, so we'll
	// generate it now ourselves.
	aggNonce, err := musig2.AggregateNonces([][musig2.PubNonceSize]byte{
		m.nonces.SigningNonce.PubNonce,
		m.nonces.VerificationNonce.PubNonce,
	})
	if err != nil {
		return nil
	}

	m.combinedNonce = aggNonce

	return nil
}

// taprootKeyspendSighash generates the sighash for a taproot key spend. As
// this is a musig2 channel output, the keyspend is the only path we can take.
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
	switch {
	// If we already have a session, then we don't need to finalize as this
	// was done up front (symmetric nonce case, like for co-op close).
	case m.session == nil && m.commitType == RemoteMusigCommit:
		// Before we can sign a new commitment, we'll need to generate
		// a fresh nonce that'll be sent along side our signature. With
		// the nonce in hand, we can finalize the session.
		txHash := tx.TxHash()
		signingNonce, err := musig2.GenNonces(
			musig2.WithPublicKey(m.localKey.PubKey),
			musig2.WithNonceAuxInput(txHash[:]),
		)
		if err != nil {
			return nil, err
		}
		if err := m.FinalizeSession(*signingNonce); err != nil {
			return nil, err
		}

	// Otherwise, we're trying to make a new commitment transaction without
	// an active session, so we'll error out.
	case m.session == nil:
		return nil, ErrSessionNotFinalized
	}

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

	tapscriptRoot := fn.MapOption(muSig2TweakToRoot)(m.tapscriptTweak)

	return NewMusigPartialSig(
		sig, m.session.PublicNonce, m.combinedNonce, m.signerKeys,
		tapscriptRoot,
	), nil
}

// Refresh is called once we receive a new verification nonce from the remote
// party after sending a signature. This nonce will be coupled within the
// revoke-and-ack message of the remote party.
func (m *MusigSession) Refresh(verificationNonce *musig2.Nonces,
) (*MusigSession, error) {

	return NewPartialMusigSession(
		*verificationNonce, m.localKey, m.remoteKey, m.signer,
		m.inputTxOut, m.commitType, m.tapscriptTweak,
	), nil
}

// VerificationNonce returns the current verification nonce for the session.
func (m *MusigSession) VerificationNonce() *musig2.Nonces {
	return &m.nonces.VerificationNonce
}

// musigSessionOpts is a set of options that can be used to modify calls to the
// musig session.
type musigSessionOpts struct {
	// customRand is an optional custom random source that can be used to
	// generate nonces via a counter scheme.
	customRand io.Reader
}

// defaultMusigSessionOpts returns the default set of options for the musig
// session.
func defaultMusigSessionOpts() *musigSessionOpts {
	return &musigSessionOpts{}
}

// MusigSessionOpt is a functional option that can be used to modify calls to
// the musig session.
type MusigSessionOpt func(*musigSessionOpts)

// WithLocalCounterNonce is used to generate local nonces based on the shachain
// producer and the current height. This allows us to not have to write secret
// nonce state to disk. Instead, we can use this to derive the nonce we need to
// sign and broadcast our own commitment transaction.
func WithLocalCounterNonce(targetHeight uint64,
	shaGen shachain.Producer) MusigSessionOpt {

	return func(opt *musigSessionOpts) {
		nextPreimage, _ := shaGen.AtIndex(targetHeight)

		opt.customRand = bytes.NewBuffer(nextPreimage[:])
	}
}

// invalidPartialSigError is used to return additional debug information to a
// caller that encounters an invalid partial sig.
type invalidPartialSigError struct {
	partialSig        []byte
	sigHash           []byte
	signingNonce      [musig2.PubNonceSize]byte
	verificationNonce [musig2.PubNonceSize]byte
}

// Error returns the error string for the partial sig error.
func (i invalidPartialSigError) Error() string {
	return fmt.Sprintf("invalid partial sig: partial_sig=%x, "+
		"sig_hash=%x, signing_nonce=%x, verification_nonce=%x",
		i.partialSig, i.sigHash, i.signingNonce[:],
		i.verificationNonce[:])
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

	if sig == nil {
		return nil, fmt.Errorf("sig not provided")
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
	tapscriptRoot := fn.MapOption(muSig2TweakToRoot)(m.tapscriptTweak)
	partialSig := NewMusigPartialSig(
		&musig2.PartialSignature{S: &sig.Sig},
		m.nonces.SigningNonce.PubNonce, m.combinedNonce, m.signerKeys,
		tapscriptRoot,
	)

	// With the partial sig loaded with the proper context, we'll now
	// generate the sighash that the remote party should have signed.
	sigHash, err := taprootKeyspendSighash(
		commitTx, m.inputTxOut.PkScript, m.inputTxOut.Value,
	)
	if err != nil {
		return nil, err
	}

	walletLog.Infof("Verifying new musig2 sig for session=%x, nonce=%s",
		m.session.SessionID[:], m.nonces.String())

	if partialSig == nil {
		return nil, fmt.Errorf("partial sig not set")
	}

	if !partialSig.Verify(sigHash, m.remoteKey.PubKey) {
		return nil, &invalidPartialSigError{
			partialSig:        partialSig.Serialize(),
			sigHash:           sigHash,
			verificationNonce: m.nonces.VerificationNonce.PubNonce,
			signingNonce:      m.nonces.SigningNonce.PubNonce,
		}
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

// CombineSigs combines the passed partial signatures into a valid schnorr
// signature.
func (m *MusigSession) CombineSigs(sigs ...*musig2.PartialSignature,
) (*schnorr.Signature, error) {

	sig, _, err := m.signer.MuSig2CombineSig(
		m.session.SessionID, sigs,
	)
	if err != nil {
		return nil, err
	}

	return sig, nil
}

// MusigSessionCfg is used to create a new musig2 pair session. It contains the
// keys for both parties, as well as their initial verification nonces.
type MusigSessionCfg struct {
	// LocalKey is a key desc for the local key.
	LocalKey keychain.KeyDescriptor

	// RemoteKey is a key desc for the remote key.
	RemoteKey keychain.KeyDescriptor

	// LocalNonce is the local party's initial verification nonce.
	LocalNonce musig2.Nonces

	// RemoteNonce is the remote party's initial verification nonce.
	RemoteNonce musig2.Nonces

	// Signer is the signer that will be used to generate the session.
	Signer input.MuSig2Signer

	// InputTxOut is the output that we're signing for. This will be the
	// funding input.
	InputTxOut *wire.TxOut

	// TapscriptTweak is an optional tweak that can be used to modify the
	// MuSig2 public key used in the session.
	TapscriptTweak fn.Option[chainhash.Hash]
}

// MusigPairSession houses the two musig2 sessions needed to do funding and
// drive forward the state machine.  The local session is used to verify
// incoming commitment states. The remote session is used to propose new
// commitment states to the remote party.
type MusigPairSession struct {
	// LocalSession is the local party's musig2 session.
	LocalSession *MusigSession

	// RemoteSession is the remote party's musig2 session.
	RemoteSession *MusigSession

	// signer is the signer that will be used to drive the session.
	signer input.MuSig2Signer
}

// NewMusigPairSession creates a new musig2 pair session.
func NewMusigPairSession(cfg *MusigSessionCfg) *MusigPairSession {
	// Given the config passed in, we'll now create our two sessions: one
	// for the local commit, and one for the remote commit.
	//
	// Both sessions will be created using only the verification nonce for
	// the local+remote party.
	tapscriptTweak := fn.MapOption(TapscriptRootToTweak)(cfg.TapscriptTweak)
	localSession := NewPartialMusigSession(
		cfg.LocalNonce, cfg.LocalKey, cfg.RemoteKey, cfg.Signer,
		cfg.InputTxOut, LocalMusigCommit, tapscriptTweak,
	)
	remoteSession := NewPartialMusigSession(
		cfg.RemoteNonce, cfg.LocalKey, cfg.RemoteKey, cfg.Signer,
		cfg.InputTxOut, RemoteMusigCommit, tapscriptTweak,
	)

	return &MusigPairSession{
		LocalSession:  localSession,
		RemoteSession: remoteSession,
		signer:        cfg.Signer,
	}
}
