package mock

import (
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	idKeyLoc = keychain.KeyLocator{Family: keychain.KeyFamilyNodeKey}
)

// DummySignature is a dummy Signature implementation.
type DummySignature struct{}

// Serialize returns an empty byte slice.
func (d *DummySignature) Serialize() []byte {
	return []byte{}
}

// Verify always returns true.
func (d *DummySignature) Verify(_ []byte, _ *btcec.PublicKey) bool {
	return true
}

// DummySigner is an implementation of the Signer interface that returns
// dummy values when called.
type DummySigner struct{}

// SignOutputRaw returns a dummy signature.
func (d *DummySigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	return &DummySignature{}, nil
}

// ComputeInputScript returns nil for both values.
func (d *DummySigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	return &input.Script{}, nil
}

// MuSig2CreateSession creates a new MuSig2 signing session using the local
// key identified by the key locator. The complete list of all public keys of
// all signing parties must be provided, including the public key of the local
// signing key. If nonces of other parties are already known, they can be
// submitted as well to reduce the number of method calls necessary later on.
func (d *DummySigner) MuSig2CreateSession(input.MuSig2Version,
	keychain.KeyLocator, []*btcec.PublicKey, *input.MuSig2Tweaks,
	[][musig2.PubNonceSize]byte, *musig2.Nonces,
) (*input.MuSig2SessionInfo, error) {

	return nil, nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID. This method returns true
// once we have all nonces for all other signing participants.
func (d *DummySigner) MuSig2RegisterNonces(input.MuSig2SessionID,
	[][musig2.PubNonceSize]byte) (bool, error) {

	return false, nil
}

// MuSig2RegisterCombinedNonce registers a pre-aggregated combined nonce for a
// session identified by its ID.
func (d *DummySigner) MuSig2RegisterCombinedNonce(input.MuSig2SessionID,
	[musig2.PubNonceSize]byte) error {

	return nil
}

// MuSig2GetCombinedNonce retrieves the combined nonce for a session identified
// by its ID.
func (d *DummySigner) MuSig2GetCombinedNonce(input.MuSig2SessionID) (
	[musig2.PubNonceSize]byte, error) {

	return [musig2.PubNonceSize]byte{}, nil
}

// MuSig2Sign creates a partial signature using the local signing key
// that was specified when the session was created. This can only be
// called when all public nonces of all participants are known and have
// been registered with the session. If this node isn't responsible for
// combining all the partial signatures, then the cleanup parameter
// should be set, indicating that the session can be removed from memory
// once the signature was produced.
func (d *DummySigner) MuSig2Sign(input.MuSig2SessionID,
	[sha256.Size]byte, bool) (*musig2.PartialSignature, error) {

	return nil, nil
}

// MuSig2CombineSig combines the given partial signature(s) with the
// local one, if it already exists. Once a partial signature of all
// participants is registered, the final signature will be combined and
// returned.
func (d *DummySigner) MuSig2CombineSig(input.MuSig2SessionID,
	[]*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	return nil, false, nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (d *DummySigner) MuSig2Cleanup(input.MuSig2SessionID) error {
	return nil
}

// SingleSigner is an implementation of the Signer interface that signs
// everything with a single private key.
type SingleSigner struct {
	Privkey *btcec.PrivateKey
	KeyLoc  keychain.KeyLocator

	*input.MusigSessionManager
}

func NewSingleSigner(privkey *btcec.PrivateKey) *SingleSigner {
	signer := &SingleSigner{
		Privkey: privkey,
		KeyLoc:  idKeyLoc,
	}

	keyFetcher := func(*keychain.KeyDescriptor) (*btcec.PrivateKey, error) {
		return signer.Privkey, nil
	}
	signer.MusigSessionManager = input.NewMusigSessionManager(keyFetcher)

	return signer
}

// SignOutputRaw generates a signature for the passed transaction using the
// stored private key.
func (s *SingleSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	amt := signDesc.Output.Value
	witnessScript := signDesc.WitnessScript
	privKey := s.Privkey

	if !privKey.PubKey().IsEqual(signDesc.KeyDesc.PubKey) {
		return nil, fmt.Errorf("incorrect key passed")
	}

	switch {
	case signDesc.SingleTweak != nil:
		privKey = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, signDesc.HashType,
		privKey)
	if err != nil {
		return nil, err
	}

	return ecdsa.ParseDERSignature(sig[:len(sig)-1])
}

// ComputeInputScript computes an input script with the stored private key
// given a transaction and a SignDescriptor.
func (s *SingleSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	privKey := s.Privkey

	switch {
	case signDesc.SingleTweak != nil:
		privKey = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.Output.PkScript,
		signDesc.HashType, privKey, true)
	if err != nil {
		return nil, err
	}

	return &input.Script{
		Witness: witnessScript,
	}, nil
}

// SignMessage takes a public key and a message and only signs the message
// with the stored private key if the public key matches the private key.
func (s *SingleSigner) SignMessage(keyLoc keychain.KeyLocator,
	msg []byte, doubleHash bool) (*ecdsa.Signature, error) {

	mockKeyLoc := s.KeyLoc
	if s.KeyLoc.IsEmpty() {
		mockKeyLoc = idKeyLoc
	}

	if keyLoc != mockKeyLoc {
		return nil, fmt.Errorf("unknown public key")
	}

	var digest []byte
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	} else {
		digest = chainhash.HashB(msg)
	}
	return ecdsa.Sign(s.Privkey, digest), nil
}
