package input

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/internal/musig2v040"
	"github.com/lightningnetwork/lnd/keychain"
)

// MuSig2Version is a type that defines the different versions of the MuSig2
// as defined in the BIP draft:
// (https://github.com/jonasnick/bips/blob/musig2/bip-musig2.mediawiki)
type MuSig2Version uint8

const (
	// MuSig2Version040 is version 0.4.0 of the MuSig2 BIP draft. This will
	// use the lnd internal/musig2v040 package.
	MuSig2Version040 MuSig2Version = 0

	// MuSig2Version100RC2 is version 1.0.0rc2 of the MuSig2 BIP draft. This
	// uses the github.com/btcsuite/btcd/btcec/v2/schnorr/musig2 package
	// at git tag `btcec/v2.3.1`.
	MuSig2Version100RC2 MuSig2Version = 1
)

const (
	// MuSig2PartialSigSize is the size of a MuSig2 partial signature.
	// Because a partial signature is just the s value, this corresponds to
	// the length of a scalar.
	MuSig2PartialSigSize = 32
)

// MuSig2SessionID is a type for a session ID that is just a hash of the MuSig2
// combined key and the local public nonces.
type MuSig2SessionID [sha256.Size]byte

// MuSig2Signer is an interface that declares all methods that a MuSig2
// compatible signer needs to implement.
type MuSig2Signer interface {
	// MuSig2CreateSession creates a new MuSig2 signing session using the
	// local key identified by the key locator. The complete list of all
	// public keys of all signing parties must be provided, including the
	// public key of the local signing key. If nonces of other parties are
	// already known, they can be submitted as well to reduce the number of
	// method calls necessary later on.
	//
	// The localNonces field is optional. If it is set, then the specified
	// nonces will be used instead of generating from scratch.  This is
	// useful in instances where the nonces are generated ahead of time
	// before the set of signers is known.
	MuSig2CreateSession(MuSig2Version, keychain.KeyLocator,
		[]*btcec.PublicKey, *MuSig2Tweaks, [][musig2.PubNonceSize]byte,
		*musig2.Nonces) (*MuSig2SessionInfo, error)

	// MuSig2RegisterNonces registers one or more public nonces of other
	// signing participants for a session identified by its ID. This method
	// returns true once we have all nonces for all other signing
	// participants.
	MuSig2RegisterNonces(MuSig2SessionID,
		[][musig2.PubNonceSize]byte) (bool, error)

	// MuSig2RegisterCombinedNonce registers a pre-aggregated combined nonce
	// for a session identified by its ID. This is an alternative to
	// MuSig2RegisterNonces and is used when a coordinator has already
	// aggregated all individual nonces and wants to distribute the combined
	// nonce to participants.
	//
	// NOTE: This method is mutually exclusive with MuSig2RegisterNonces for
	// the same session. Once this method is called, MuSig2RegisterNonces
	// will return an error if called later for the same session.
	MuSig2RegisterCombinedNonce(MuSig2SessionID,
		[musig2.PubNonceSize]byte) error

	// MuSig2GetCombinedNonce retrieves the combined nonce for a session
	// identified by its ID. This will be available after either all
	// individual nonces have been registered via MuSig2RegisterNonces, or a
	// combined nonce has been registered via MuSig2RegisterCombinedNonce.
	MuSig2GetCombinedNonce(MuSig2SessionID) ([musig2.PubNonceSize]byte,
		error)

	// MuSig2Sign creates a partial signature using the local signing key
	// that was specified when the session was created. This can only be
	// called when all public nonces of all participants are known and have
	// been registered with the session. If this node isn't responsible for
	// combining all the partial signatures, then the cleanup parameter
	// should be set, indicating that the session can be removed from memory
	// once the signature was produced.
	MuSig2Sign(MuSig2SessionID, [sha256.Size]byte,
		bool) (*musig2.PartialSignature, error)

	// MuSig2CombineSig combines the given partial signature(s) with the
	// local one, if it already exists. Once a partial signature of all
	// participants is registered, the final signature will be combined and
	// returned.
	MuSig2CombineSig(MuSig2SessionID,
		[]*musig2.PartialSignature) (*schnorr.Signature, bool, error)

	// MuSig2Cleanup removes a session from memory to free up resources.
	MuSig2Cleanup(MuSig2SessionID) error
}

// MuSig2Context is an interface that is an abstraction over the MuSig2 signing
// context. This interface does not contain all of the methods the underlying
// implementations have because those use package specific types which cannot
// easily be made compatible. Those calls (such as NewSession) are implemented
// in this package instead and do the necessary type switch (see
// MuSig2CreateContext).
type MuSig2Context interface {
	// SigningKeys returns the set of keys used for signing.
	SigningKeys() []*btcec.PublicKey

	// CombinedKey returns the combined public key that will be used to
	// generate multi-signatures  against.
	CombinedKey() (*btcec.PublicKey, error)

	// TaprootInternalKey returns the internal taproot key, which is the
	// aggregated key _before_ the tweak is applied. If a taproot tweak was
	// specified, then CombinedKey() will return the fully tweaked output
	// key, with this method returning the internal key. If a taproot tweak
	// wasn't specified, then this method will return an error.
	TaprootInternalKey() (*btcec.PublicKey, error)
}

// MuSig2Session is an interface that is an abstraction over the MuSig2 signing
// session. This interface does not contain all of the methods the underlying
// implementations have because those use package specific types which cannot
// easily be made compatible. Those calls (such as CombineSig or Sign) are
// implemented in this package instead and do the necessary type switch (see
// MuSig2CombineSig or MuSig2Sign).
type MuSig2Session interface {
	// FinalSig returns the final combined multi-signature, if present.
	FinalSig() *schnorr.Signature

	// PublicNonce returns the public nonce for a signer. This should be
	// sent to other parties before signing begins, so they can compute the
	// aggregated public nonce.
	PublicNonce() [musig2.PubNonceSize]byte

	// NumRegisteredNonces returns the total number of nonces that have been
	// registered so far.
	NumRegisteredNonces() int

	// RegisterPubNonce should be called for each public nonce from the set
	// of signers. This method returns true once all the public nonces have
	// been accounted for.
	RegisterPubNonce(nonce [musig2.PubNonceSize]byte) (bool, error)

	// CombinedNonce returns the combined/aggregated public nonce for the
	// session. This will be available after either all individual nonces
	// have been registered via RegisterPubNonce, or a combined nonce has
	// been registered via RegisterCombinedNonce.
	//
	// If the combined nonce is not yet available, this method returns an
	// error.
	CombinedNonce() ([musig2.PubNonceSize]byte, error)

	// RegisterCombinedNonce allows a caller to directly register a
	// pre-aggregated nonce that was generated externally. This is useful
	// in coordinator-based protocols where the coordinator aggregates all
	// nonces and distributes the combined nonce to participants.
	//
	// NOTE: This method is mutually exclusive with RegisterPubNonce. Once
	// this method is called, RegisterPubNonce will return an error if
	// called later. Similarly, if RegisterPubNonce has already been called,
	// this method will return an error.
	RegisterCombinedNonce(combinedNonce [musig2.PubNonceSize]byte) error
}

// MuSig2SessionInfo is a struct for keeping track of a signing session
// information in memory.
type MuSig2SessionInfo struct {
	// SessionID is the wallet's internal unique ID of this session. The ID
	// is the hash over the combined public key and the local public nonces.
	SessionID [32]byte

	// Version is the version of the MuSig2 BIP this signing session is
	// using.
	Version MuSig2Version

	// PublicNonce contains the public nonce of the local signer session.
	PublicNonce [musig2.PubNonceSize]byte

	// CombinedKey is the combined public key with all tweaks applied to it.
	CombinedKey *btcec.PublicKey

	// TaprootTweak indicates whether a taproot tweak (BIP-0086 or script
	// path) was used. The TaprootInternalKey will only be set if this is
	// set to true.
	TaprootTweak bool

	// TaprootInternalKey is the raw combined public key without any tweaks
	// applied to it. This is only set if TaprootTweak is true.
	TaprootInternalKey *btcec.PublicKey

	// HaveAllNonces indicates whether this session already has all nonces
	// of all other signing participants registered.
	HaveAllNonces bool

	// HaveAllSigs indicates whether this session already has all partial
	// signatures of all other signing participants registered.
	HaveAllSigs bool
}

// MuSig2Tweaks is a struct that contains all tweaks that can be applied to a
// MuSig2 combined public key.
type MuSig2Tweaks struct {
	// GenericTweaks is a list of normal tweaks to apply to the combined
	// public key (and to the private key when signing).
	GenericTweaks []musig2.KeyTweakDesc

	// TaprootBIP0086Tweak indicates that the final key should use the
	// taproot tweak as defined in BIP 341, with the BIP 86 modification:
	//     outputKey = internalKey + h_tapTweak(internalKey)*G.
	// In this case, the aggregated key before the tweak will be used as the
	// internal key. If this is set to true then TaprootTweak will be
	// ignored.
	TaprootBIP0086Tweak bool

	// TaprootTweak specifies that the final key should use the taproot
	// tweak as defined in BIP 341:
	//     outputKey = internalKey + h_tapTweak(internalKey || scriptRoot).
	// In this case, the aggregated key before the tweak will be used as the
	// internal key. Will be ignored if TaprootBIP0086Tweak is set to true.
	TaprootTweak []byte
}

// HasTaprootTweak returns true if either a taproot BIP0086 tweak or a taproot
// script root tweak is set.
func (t *MuSig2Tweaks) HasTaprootTweak() bool {
	return t.TaprootBIP0086Tweak || len(t.TaprootTweak) > 0
}

// ToContextOptions converts the tweak descriptor to context options.
func (t *MuSig2Tweaks) ToContextOptions() []musig2.ContextOption {
	var tweakOpts []musig2.ContextOption
	if len(t.GenericTweaks) > 0 {
		tweakOpts = append(tweakOpts, musig2.WithTweakedContext(
			t.GenericTweaks...,
		))
	}

	// The BIP0086 tweak and the taproot script tweak are mutually
	// exclusive.
	if t.TaprootBIP0086Tweak {
		tweakOpts = append(tweakOpts, musig2.WithBip86TweakCtx())
	} else if len(t.TaprootTweak) > 0 {
		tweakOpts = append(tweakOpts, musig2.WithTaprootTweakCtx(
			t.TaprootTweak,
		))
	}

	return tweakOpts
}

// ToV040ContextOptions converts the tweak descriptor to v0.4.0 context options.
func (t *MuSig2Tweaks) ToV040ContextOptions() []musig2v040.ContextOption {
	var tweakOpts []musig2v040.ContextOption
	if len(t.GenericTweaks) > 0 {
		genericTweaksCopy := make(
			[]musig2v040.KeyTweakDesc, len(t.GenericTweaks),
		)
		for idx := range t.GenericTweaks {
			genericTweaksCopy[idx] = musig2v040.KeyTweakDesc{
				Tweak:   t.GenericTweaks[idx].Tweak,
				IsXOnly: t.GenericTweaks[idx].IsXOnly,
			}
		}
		tweakOpts = append(tweakOpts, musig2v040.WithTweakedContext(
			genericTweaksCopy...,
		))
	}

	// The BIP0086 tweak and the taproot script tweak are mutually
	// exclusive.
	if t.TaprootBIP0086Tweak {
		tweakOpts = append(tweakOpts, musig2v040.WithBip86TweakCtx())
	} else if len(t.TaprootTweak) > 0 {
		tweakOpts = append(tweakOpts, musig2v040.WithTaprootTweakCtx(
			t.TaprootTweak,
		))
	}

	return tweakOpts
}

// MuSig2ParsePubKeys parses a list of raw public keys as the signing keys of a
// MuSig2 signing session.
func MuSig2ParsePubKeys(bipVersion MuSig2Version,
	rawPubKeys [][]byte) ([]*btcec.PublicKey, error) {

	allSignerPubKeys := make([]*btcec.PublicKey, len(rawPubKeys))
	if len(rawPubKeys) < 2 {
		return nil, fmt.Errorf("need at least two signing public keys")
	}

	for idx, pubKeyBytes := range rawPubKeys {
		switch bipVersion {
		case MuSig2Version040:
			pubKey, err := schnorr.ParsePubKey(pubKeyBytes)
			if err != nil {
				return nil, fmt.Errorf("error parsing signer "+
					"public key %d for v0.4.0 (x-only "+
					"format): %v", idx, err)
			}
			allSignerPubKeys[idx] = pubKey

		case MuSig2Version100RC2:
			pubKey, err := btcec.ParsePubKey(pubKeyBytes)
			if err != nil {
				return nil, fmt.Errorf("error parsing signer "+
					"public key %d for v1.0.0rc2 ("+
					"compressed format): %v", idx, err)
			}
			allSignerPubKeys[idx] = pubKey

		default:
			return nil, fmt.Errorf("unknown MuSig2 version: <%d>",
				bipVersion)
		}
	}

	return allSignerPubKeys, nil
}

// MuSig2CombineKeys combines the given set of public keys into a single
// combined MuSig2 combined public key, applying the given tweaks.
func MuSig2CombineKeys(bipVersion MuSig2Version,
	allSignerPubKeys []*btcec.PublicKey, sortKeys bool,
	tweaks *MuSig2Tweaks) (*musig2.AggregateKey, error) {

	switch bipVersion {
	case MuSig2Version040:
		return combineKeysV040(allSignerPubKeys, sortKeys, tweaks)

	case MuSig2Version100RC2:
		return combineKeysV100RC2(allSignerPubKeys, sortKeys, tweaks)

	default:
		return nil, fmt.Errorf("unknown MuSig2 version: <%d>",
			bipVersion)
	}
}

// combineKeysV100rc1 implements the MuSigCombineKeys logic for the MuSig2 BIP
// draft version 1.0.0rc2.
func combineKeysV100RC2(allSignerPubKeys []*btcec.PublicKey, sortKeys bool,
	tweaks *MuSig2Tweaks) (*musig2.AggregateKey, error) {

	// Convert the tweak options into the appropriate MuSig2 API functional
	// options.
	var keyAggOpts []musig2.KeyAggOption
	switch {
	case tweaks.TaprootBIP0086Tweak:
		keyAggOpts = append(keyAggOpts, musig2.WithBIP86KeyTweak())
	case len(tweaks.TaprootTweak) > 0:
		keyAggOpts = append(keyAggOpts, musig2.WithTaprootKeyTweak(
			tweaks.TaprootTweak,
		))
	case len(tweaks.GenericTweaks) > 0:
		keyAggOpts = append(keyAggOpts, musig2.WithKeyTweaks(
			tweaks.GenericTweaks...,
		))
	}

	// Then we'll use this information to compute the aggregated public key.
	combinedKey, _, _, err := musig2.AggregateKeys(
		allSignerPubKeys, sortKeys, keyAggOpts...,
	)
	return combinedKey, err
}

// combineKeysV040 implements the MuSigCombineKeys logic for the MuSig2 BIP
// draft version 0.4.0.
func combineKeysV040(allSignerPubKeys []*btcec.PublicKey, sortKeys bool,
	tweaks *MuSig2Tweaks) (*musig2.AggregateKey, error) {

	// Convert the tweak options into the appropriate MuSig2 API functional
	// options.
	var keyAggOpts []musig2v040.KeyAggOption
	switch {
	case tweaks.TaprootBIP0086Tweak:
		keyAggOpts = append(keyAggOpts, musig2v040.WithBIP86KeyTweak())
	case len(tweaks.TaprootTweak) > 0:
		keyAggOpts = append(keyAggOpts, musig2v040.WithTaprootKeyTweak(
			tweaks.TaprootTweak,
		))
	case len(tweaks.GenericTweaks) > 0:
		genericTweaksCopy := make(
			[]musig2v040.KeyTweakDesc, len(tweaks.GenericTweaks),
		)
		for idx := range tweaks.GenericTweaks {
			genericTweaksCopy[idx] = musig2v040.KeyTweakDesc{
				Tweak:   tweaks.GenericTweaks[idx].Tweak,
				IsXOnly: tweaks.GenericTweaks[idx].IsXOnly,
			}
		}
		keyAggOpts = append(keyAggOpts, musig2v040.WithKeyTweaks(
			genericTweaksCopy...,
		))
	}

	// Then we'll use this information to compute the aggregated public key.
	combinedKey, _, _, err := musig2v040.AggregateKeys(
		allSignerPubKeys, sortKeys, keyAggOpts...,
	)

	// Copy the result back into the default version's native type.
	return &musig2.AggregateKey{
		FinalKey:      combinedKey.FinalKey,
		PreTweakedKey: combinedKey.PreTweakedKey,
	}, err
}

// MuSig2CreateContext creates a new MuSig2 signing context.
func MuSig2CreateContext(bipVersion MuSig2Version, privKey *btcec.PrivateKey,
	allSignerPubKeys []*btcec.PublicKey, tweaks *MuSig2Tweaks,
	localNonces *musig2.Nonces,
) (MuSig2Context, MuSig2Session, error) {

	switch bipVersion {
	case MuSig2Version040:
		return createContextV040(
			privKey, allSignerPubKeys, tweaks, localNonces,
		)

	case MuSig2Version100RC2:
		return createContextV100RC2(
			privKey, allSignerPubKeys, tweaks, localNonces,
		)

	default:
		return nil, nil, fmt.Errorf("unknown MuSig2 version: <%d>",
			bipVersion)
	}
}

// createContextV100RC2 implements the MuSig2CreateContext logic for the MuSig2
// BIP draft version 1.0.0rc2.
func createContextV100RC2(privKey *btcec.PrivateKey,
	allSignerPubKeys []*btcec.PublicKey, tweaks *MuSig2Tweaks,
	localNonces *musig2.Nonces,
) (*musig2.Context, *musig2.Session, error) {

	// The context keeps track of all signing keys and our local key.
	allOpts := append(
		[]musig2.ContextOption{
			musig2.WithKnownSigners(allSignerPubKeys),
		},
		tweaks.ToContextOptions()...,
	)
	muSigContext, err := musig2.NewContext(privKey, true, allOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating MuSig2 signing "+
			"context: %v", err)
	}

	var sessionOpts []musig2.SessionOption
	if localNonces != nil {
		sessionOpts = append(
			sessionOpts, musig2.WithPreGeneratedNonce(localNonces),
		)
	}

	muSigSession, err := muSigContext.NewSession(sessionOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating MuSig2 signing "+
			"session: %v", err)
	}

	return muSigContext, muSigSession, nil
}

// createContextV040 implements the MuSig2CreateContext logic for the MuSig2 BIP
// draft version 0.4.0.
func createContextV040(privKey *btcec.PrivateKey,
	allSignerPubKeys []*btcec.PublicKey, tweaks *MuSig2Tweaks,
	_ *musig2.Nonces,
) (*musig2v040.Context, *musig2v040.Session, error) {

	// The context keeps track of all signing keys and our local key.
	allOpts := append(
		[]musig2v040.ContextOption{
			musig2v040.WithKnownSigners(allSignerPubKeys),
		},
		tweaks.ToV040ContextOptions()...,
	)
	muSigContext, err := musig2v040.NewContext(privKey, true, allOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating MuSig2 signing "+
			"context: %v", err)
	}

	muSigSession, err := muSigContext.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("error creating MuSig2 signing "+
			"session: %v", err)
	}

	return muSigContext, muSigSession, nil
}

// MuSig2Sign calls the Sign() method on the given versioned signing session and
// returns the result in the most recent version of the MuSig2 API.
func MuSig2Sign(session MuSig2Session, msg [32]byte,
	withSortedKeys bool) (*musig2.PartialSignature, error) {

	switch s := session.(type) {
	case *musig2.Session:
		var opts []musig2.SignOption
		if withSortedKeys {
			opts = append(opts, musig2.WithSortedKeys())
		}
		partialSig, err := s.Sign(msg, opts...)
		if err != nil {
			return nil, fmt.Errorf("error signing with local key: "+
				"%v", err)
		}

		return partialSig, nil

	case *musig2v040.Session:
		var opts []musig2v040.SignOption
		if withSortedKeys {
			opts = append(opts, musig2v040.WithSortedKeys())
		}
		partialSig, err := s.Sign(msg, opts...)
		if err != nil {
			return nil, fmt.Errorf("error signing with local key: "+
				"%v", err)
		}

		return &musig2.PartialSignature{
			S: partialSig.S,
			R: partialSig.R,
		}, nil

	default:
		return nil, fmt.Errorf("invalid session type <%T>", s)
	}
}

// MuSig2CombineSig calls the CombineSig() method on the given versioned signing
// session and returns the result in the most recent version of the MuSig2 API.
func MuSig2CombineSig(session MuSig2Session,
	otherPartialSig *musig2.PartialSignature) (bool, error) {

	switch s := session.(type) {
	case *musig2.Session:
		haveAllSigs, err := s.CombineSig(otherPartialSig)
		if err != nil {
			return false, fmt.Errorf("error combining partial "+
				"signature: %v", err)
		}

		return haveAllSigs, nil

	case *musig2v040.Session:
		haveAllSigs, err := s.CombineSig(&musig2v040.PartialSignature{
			S: otherPartialSig.S,
			R: otherPartialSig.R,
		})
		if err != nil {
			return false, fmt.Errorf("error combining partial "+
				"signature: %v", err)
		}

		return haveAllSigs, nil

	default:
		return false, fmt.Errorf("invalid session type <%T>", s)
	}
}

// NewMuSig2SessionID returns the unique ID of a MuSig2 session by using the
// combined key and the local public nonces and hashing that data.
func NewMuSig2SessionID(combinedKey *btcec.PublicKey,
	publicNonces [musig2.PubNonceSize]byte) MuSig2SessionID {

	// We hash the data to save some bytes in memory.
	hash := sha256.New()
	_, _ = hash.Write(combinedKey.SerializeCompressed())
	_, _ = hash.Write(publicNonces[:])

	id := MuSig2SessionID{}
	copy(id[:], hash.Sum(nil))
	return id
}

// SerializePartialSignature encodes the partial signature to a fixed size byte
// array.
func SerializePartialSignature(
	sig *musig2.PartialSignature) ([MuSig2PartialSigSize]byte, error) {

	var (
		buf    bytes.Buffer
		result [MuSig2PartialSigSize]byte
	)
	if err := sig.Encode(&buf); err != nil {
		return result, fmt.Errorf("error encoding partial signature: "+
			"%v", err)
	}

	if buf.Len() != MuSig2PartialSigSize {
		return result, fmt.Errorf("invalid partial signature length, "+
			"got %d wanted %d", buf.Len(), MuSig2PartialSigSize)
	}

	copy(result[:], buf.Bytes())

	return result, nil
}

// DeserializePartialSignature decodes a partial signature from a byte slice.
func DeserializePartialSignature(scalarBytes []byte) (*musig2.PartialSignature,
	error) {

	if len(scalarBytes) != MuSig2PartialSigSize {
		return nil, fmt.Errorf("invalid partial signature length, got "+
			"%d wanted %d", len(scalarBytes), MuSig2PartialSigSize)
	}

	sig := &musig2.PartialSignature{}
	if err := sig.Decode(bytes.NewReader(scalarBytes)); err != nil {
		return nil, fmt.Errorf("error decoding partial signature: %w",
			err)
	}

	return sig, nil
}
