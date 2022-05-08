package input

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/keychain"
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
	MuSig2CreateSession(keychain.KeyLocator, []*btcec.PublicKey,
		*MuSig2Tweaks, [][musig2.PubNonceSize]byte) (*MuSig2SessionInfo,
		error)

	// MuSig2RegisterNonces registers one or more public nonces of other
	// signing participants for a session identified by its ID. This method
	// returns true once we have all nonces for all other signing
	// participants.
	MuSig2RegisterNonces(MuSig2SessionID,
		[][musig2.PubNonceSize]byte) (bool, error)

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
}

// MuSig2SessionInfo is a struct for keeping track of a signing session
// information in memory.
type MuSig2SessionInfo struct {
	// SessionID is the wallet's internal unique ID of this session. The ID
	// is the hash over the combined public key and the local public nonces.
	SessionID [32]byte

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

// MuSig2CombineKeys combines the given set of public keys into a single
// combined MuSig2 combined public key, applying the given tweaks.
func MuSig2CombineKeys(allSignerPubKeys []*btcec.PublicKey,
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
		allSignerPubKeys, true, keyAggOpts...,
	)
	return combinedKey, err
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
		return nil, fmt.Errorf("error decoding partial signature: %v",
			err)
	}

	return sig, nil
}
