package input

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/multimutex"
)

// MuSig2State is a struct that holds on to the internal signing session state
// of a MuSig2 session.
type MuSig2State struct {
	// MuSig2SessionInfo is the associated meta information of the signing
	// session.
	MuSig2SessionInfo

	// context is the signing context responsible for keeping track of the
	// public keys involved in the signing process.
	context MuSig2Context

	// session is the signing session responsible for keeping track of the
	// nonces and partial signatures involved in the signing process.
	session MuSig2Session

	// contextDigest freezes key/tweak inputs that are no longer available
	// from the crypto session when the message is eventually signed.
	contextDigest [sha256.Size]byte
}

// PrivKeyFetcher is used to fetch a private key that matches a given key desc.
type PrivKeyFetcher func(*keychain.KeyDescriptor) (*btcec.PrivateKey, error)

// MusigSessionMusigSessionManager houses the state needed to manage concurrent
// musig sessions. Each session is identified by a unique session ID which is
// used by callers to interact with a given session.
type MusigSessionManager struct {
	keyFetcher PrivKeyFetcher

	// bindNonce protects caller-supplied v1.0.0rc2 nonces across sessions.
	// Legacy sessions generate fresh nonces and enforce one-use signing.
	bindNonce func([musig2.PubNonceSize]byte, [sha256.Size]byte) error

	sessionMtx *multimutex.Mutex[MuSig2SessionID]

	musig2Sessions *lnutils.SyncMap[MuSig2SessionID, *MuSig2State]
}

// NumLiveSessions returns the number of MuSig2 sessions that are currently
// held by this manager. This is used by callers (and tests) to verify that
// sessions are released once they're no longer needed.
func (m *MusigSessionManager) NumLiveSessions() int {
	return m.musig2Sessions.Len()
}

// NewMusigSessionManager creates a new musig manager given an abstract key
// fetcher and durable nonce/transcript binder. A nil binder is intended only
// for test signers; it retains the crypto session's existing one-use guard.
func NewMusigSessionManager(keyFetcher PrivKeyFetcher,
	bindNonce func([musig2.PubNonceSize]byte,
		[sha256.Size]byte) error) *MusigSessionManager {

	return &MusigSessionManager{
		keyFetcher: keyFetcher,
		bindNonce:  bindNonce,
		musig2Sessions: &lnutils.SyncMap[
			MuSig2SessionID, *MuSig2State,
		]{},
		sessionMtx: multimutex.NewMutex[MuSig2SessionID](),
	}
}

// MuSig2CreateSession creates a new MuSig2 signing session using the local key
// identified by the key locator. The complete list of all public keys of all
// signing parties must be provided, including the public key of the local
// signing key. If nonces of other parties are already known, they can be
// submitted as well to reduce the number of method calls necessary later on.
//
// The set of sessionOpts are _optional_ and allow a caller to modify the
// generated sessions. As an example the local nonce might already be generated
// ahead of time.
func (m *MusigSessionManager) MuSig2CreateSession(bipVersion MuSig2Version,
	keyLoc keychain.KeyLocator, allSignerPubKeys []*btcec.PublicKey,
	tweaks *MuSig2Tweaks, otherSignerNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*MuSig2SessionInfo, error) {

	// We need to derive the private key for signing. In the remote signing
	// setup, this whole RPC call will be forwarded to the signing
	// instance, which requires it to be stateful.
	privKey, err := m.keyFetcher(&keychain.KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, fmt.Errorf("error deriving private key: %w", err)
	}

	// Create a signing context and session with the given private key and
	// list of all known signer public keys.
	musigContext, musigSession, err := MuSig2CreateContext(
		bipVersion, privKey, allSignerPubKeys, tweaks, localNonces,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating signing context: %w",
			err)
	}

	// Add all nonces we might've learned so far.
	haveAllNonces := false
	for _, otherSignerNonce := range otherSignerNonces {
		haveAllNonces, err = musigSession.RegisterPubNonce(
			otherSignerNonce,
		)
		if err != nil {
			return nil, fmt.Errorf("error registering other "+
				"signer public nonce: %v", err)
		}
	}

	// Register the new session.
	combinedKey, err := musigContext.CombinedKey()
	if err != nil {
		return nil, fmt.Errorf("error getting combined key: %w", err)
	}
	session := &MuSig2State{
		MuSig2SessionInfo: MuSig2SessionInfo{
			SessionID: NewMuSig2SessionID(
				combinedKey, musigSession.PublicNonce(),
			),
			Version:       bipVersion,
			PublicNonce:   musigSession.PublicNonce(),
			CombinedKey:   combinedKey,
			TaprootTweak:  tweaks.HasTaprootTweak(),
			HaveAllNonces: haveAllNonces,
		},
		context: musigContext,
		session: musigSession,
		contextDigest: muSig2ContextDigest(
			bipVersion, privKey.PubKey(),
			musigContext.SigningKeys(),
			combinedKey, tweaks,
		),
	}

	// The internal key is only calculated if we are using a taproot tweak
	// and need to know it for a potential script spend.
	if tweaks.HasTaprootTweak() {
		internalKey, err := musigContext.TaprootInternalKey()
		if err != nil {
			return nil, fmt.Errorf("error getting internal key: %w",
				err)
		}
		session.TaprootInternalKey = internalKey
	}

	// Since we generate new nonces for every session, there is no way that
	// a session with the same ID already exists. So even if we call the API
	// twice with the same signers, we still get a new ID.
	//
	// We'll use just all zeroes as the session ID for the mutex, as this
	// is a "global" action.
	m.musig2Sessions.Store(session.SessionID, session)

	return &session.MuSig2SessionInfo, nil
}

// MuSig2Sign creates a partial signature using the local signing key
// that was specified when the session was created. This can only be
// called when all public nonces of all participants are known and have
// been registered with the session. If this node isn't responsible for
// combining all the partial signatures, then the cleanup parameter
// should be set, indicating that the session can be removed from memory
// once the signature was produced.
func (m *MusigSessionManager) MuSig2Sign(sessionID MuSig2SessionID,
	msg [sha256.Size]byte, cleanUp bool) (*musig2.PartialSignature, error) {

	// We hold the lock during the whole operation, we don't want any
	// interference with calls that might come through in parallel for the
	// same session.
	m.sessionMtx.Lock(sessionID)
	defer m.sessionMtx.Unlock(sessionID)

	session, ok := m.musig2Sessions.Load(sessionID)
	if !ok {
		return nil, fmt.Errorf("session with ID %x not found",
			sessionID[:])
	}

	// We can only sign once we have all other signer's nonces.
	if !session.HaveAllNonces {
		return nil, fmt.Errorf("only have %d of %d required nonces",
			session.session.NumRegisteredNonces(),
			len(session.context.SigningKeys()))
	}

	// Bind supplied v1.0.0rc2 nonces before signing so reconstructed exact
	// repeats remain safe. Legacy v0.4.0 cannot accept supplied nonces; it
	// generates fresh ones and retains its existing one-use session guard.
	if m.bindNonce != nil && session.Version == MuSig2Version100RC2 {
		combinedNonce, err := session.session.CombinedNonce()
		if err != nil {
			return nil, fmt.Errorf("get combined nonce: %w", err)
		}
		transcript := append([]byte(nil), session.contextDigest[:]...)
		transcript = append(transcript, session.PublicNonce[:]...)
		transcript = append(transcript, combinedNonce[:]...)
		transcript = append(transcript, msg[:]...)
		digest := sha256.Sum256(transcript)
		err = m.bindNonce(session.PublicNonce, digest)
		if err != nil {
			return nil, fmt.Errorf("bind MuSig2 signing "+
				"transcript: %w", err)
		}
	}

	// Create our own partial signature with the local signing key.
	partialSig, err := MuSig2Sign(session.session, msg, true)
	if err != nil {
		return nil, fmt.Errorf("error signing with local key: %w", err)
	}

	// Clean up our local state if requested.
	if cleanUp {
		m.musig2Sessions.Delete(sessionID)
	}

	return partialSig, nil
}

// MuSig2CombineSig combines the given partial signature(s) with the
// local one, if it already exists. Once a partial signature of all
// participants is registered, the final signature will be combined and
// returned.
func (m *MusigSessionManager) MuSig2CombineSig(sessionID MuSig2SessionID,
	partialSigs []*musig2.PartialSignature) (*schnorr.Signature, bool,
	error) {

	// We hold the lock during the whole operation, we don't want any
	// interference with calls that might come through in parallel for the
	// same session.
	m.sessionMtx.Lock(sessionID)
	defer m.sessionMtx.Unlock(sessionID)

	session, ok := m.musig2Sessions.Load(sessionID)
	if !ok {
		return nil, false, fmt.Errorf("session with ID %x not found",
			sessionID[:])
	}

	// Make sure we don't exceed the number of expected partial signatures
	// as that would indicate something is wrong with the signing setup.
	if session.HaveAllSigs {
		return nil, true, fmt.Errorf("already have all partial" +
			"signatures")
	}

	// Add all sigs we got so far.
	var (
		finalSig *schnorr.Signature
		err      error
	)
	for _, otherPartialSig := range partialSigs {
		session.HaveAllSigs, err = MuSig2CombineSig(
			session.session, otherPartialSig,
		)
		if err != nil {
			return nil, false, fmt.Errorf("error combining "+
				"partial signature: %w", err)
		}
	}

	// If we have all partial signatures, we should be able to get the
	// complete signature now. We also remove this session from memory since
	// there is nothing more left to do.
	if session.HaveAllSigs {
		finalSig = session.session.FinalSig()
		m.musig2Sessions.Delete(sessionID)
	}

	return finalSig, session.HaveAllSigs, nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (m *MusigSessionManager) MuSig2Cleanup(sessionID MuSig2SessionID) error {
	// We hold the lock during the whole operation, we don't want any
	// interference with calls that might come through in parallel for the
	// same session.
	m.sessionMtx.Lock(sessionID)
	defer m.sessionMtx.Unlock(sessionID)

	_, ok := m.musig2Sessions.Load(sessionID)
	if !ok {
		return fmt.Errorf("session with ID %x not found", sessionID[:])
	}

	m.musig2Sessions.Delete(sessionID)

	return nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID. This method returns true
// once we have all nonces for all other signing participants.
func (m *MusigSessionManager) MuSig2RegisterNonces(sessionID MuSig2SessionID,
	otherSignerNonces [][musig2.PubNonceSize]byte) (bool, error) {

	// We hold the lock during the whole operation, we don't want any
	// interference with calls that might come through in parallel for the
	// same session.
	m.sessionMtx.Lock(sessionID)
	defer m.sessionMtx.Unlock(sessionID)

	session, ok := m.musig2Sessions.Load(sessionID)
	if !ok {
		return false, fmt.Errorf("session with ID %x not found",
			sessionID[:])
	}

	// Make sure we don't exceed the number of expected nonces as that would
	// indicate something is wrong with the signing setup.
	if session.HaveAllNonces {
		return true, fmt.Errorf("already have all nonces")
	}

	numSigners := len(session.context.SigningKeys())
	remainingNonces := numSigners - session.session.NumRegisteredNonces()
	if len(otherSignerNonces) > remainingNonces {
		return false, fmt.Errorf("only %d other nonces remaining but "+
			"trying to register %d more", remainingNonces,
			len(otherSignerNonces))
	}

	// Add all nonces we've learned so far.
	var err error
	for _, otherSignerNonce := range otherSignerNonces {
		session.HaveAllNonces, err = session.session.RegisterPubNonce(
			otherSignerNonce,
		)
		if err != nil {
			return false, fmt.Errorf("error registering other "+
				"signer public nonce: %v", err)
		}
	}

	return session.HaveAllNonces, nil
}

// MuSig2RegisterCombinedNonce registers a pre-aggregated combined nonce for a
// session identified by its ID. This is an alternative to MuSig2RegisterNonces
// and is used when a coordinator has already aggregated all individual nonces
// and wants to distribute the combined nonce to participants.
//
// NOTE: This method is mutually exclusive with MuSig2RegisterNonces for the
// same session. Once this method is called, MuSig2RegisterNonces will return
// an error if called later for the same session.
func (m *MusigSessionManager) MuSig2RegisterCombinedNonce(
	sessionID MuSig2SessionID,
	combinedNonce [musig2.PubNonceSize]byte) error {

	// Hold the lock during the whole operation.
	m.sessionMtx.Lock(sessionID)
	defer m.sessionMtx.Unlock(sessionID)

	// Load the session.
	session, ok := m.musig2Sessions.Load(sessionID)
	if !ok {
		return fmt.Errorf("session with ID %x not found", sessionID[:])
	}

	// Check if we already have all nonces.
	if session.HaveAllNonces {
		return fmt.Errorf("already have all nonces")
	}

	// Delegate to the version-specific implementation.
	err := session.session.RegisterCombinedNonce(combinedNonce)
	if err != nil {
		return fmt.Errorf("error registering combined nonce: %w", err)
	}

	// Mark that we have all nonces now.
	session.HaveAllNonces = true

	return nil
}

// MuSig2GetCombinedNonce retrieves the combined nonce for a session identified
// by its ID. This will be available after either all individual nonces have
// been registered via MuSig2RegisterNonces, or a combined nonce has been
// registered via MuSig2RegisterCombinedNonce.
func (m *MusigSessionManager) MuSig2GetCombinedNonce(
	sessionID MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	// Hold the lock during the operation.
	m.sessionMtx.Lock(sessionID)
	defer m.sessionMtx.Unlock(sessionID)

	// Load the session.
	session, ok := m.musig2Sessions.Load(sessionID)
	if !ok {
		return [musig2.PubNonceSize]byte{}, fmt.Errorf("session with "+
			"ID %x not found", sessionID[:])
	}

	// Get the combined nonce from the session.
	combinedNonce, err := session.session.CombinedNonce()
	if err != nil {
		return [musig2.PubNonceSize]byte{}, fmt.Errorf("error getting "+
			"combined nonce: %w", err)
	}

	return combinedNonce, nil
}

// muSig2ContextDigest commits to the effective signing context independently of
// mutable request slices. Fixed-width keys/tweaks and explicit list lengths
// prevent distinct contexts from sharing an ambiguous byte representation.
func muSig2ContextDigest(version MuSig2Version, local *btcec.PublicKey,
	keys []*btcec.PublicKey, combined *btcec.PublicKey,
	tweaks *MuSig2Tweaks) [sha256.Size]byte {

	data := append([]byte("lnd-musig2-transcript-v1"), byte(version))
	data = append(data, local.SerializeCompressed()...)
	data = append(data, combined.SerializeCompressed()...)

	// Context creation sorts signers for aggregation. Freeze that same set
	// so equivalent request ordering does not block recovery retries.
	encodedKeys := make([][]byte, len(keys))
	for i, key := range keys {
		encodedKeys[i] = key.SerializeCompressed()
	}
	sort.Slice(encodedKeys, func(i, j int) bool {
		return bytes.Compare(encodedKeys[i], encodedKeys[j]) < 0
	})
	data = binary.BigEndian.AppendUint32(data, uint32(len(encodedKeys)))
	for _, key := range encodedKeys {
		data = append(data, key...)
	}

	// Generic tweaks are ordered; BIP86 takes precedence over a script
	// root, matching ToContextOptions rather than binding ignored input.
	data = binary.BigEndian.AppendUint32(
		data, uint32(len(tweaks.GenericTweaks)),
	)
	for _, tweak := range tweaks.GenericTweaks {
		data = append(data, tweak.Tweak[:]...)
		flag := byte(0)
		if tweak.IsXOnly {
			flag = 1
		}
		data = append(data, flag)
	}
	if tweaks.TaprootBIP0086Tweak {
		data = append(data, 1)
	} else {
		data = append(data, 0)
		data = binary.BigEndian.AppendUint32(
			data, uint32(len(tweaks.TaprootTweak)),
		)
		data = append(data, tweaks.TaprootTweak...)
	}

	return sha256.Sum256(data)
}
