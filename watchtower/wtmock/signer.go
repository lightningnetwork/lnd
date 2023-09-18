package wtmock

import (
	"crypto/sha256"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// MockSigner is an input.Signer that allows one to add arbitrary private keys
// and sign messages by passing the assigned keychain.KeyLocator.
type MockSigner struct {
	mu sync.Mutex

	index uint32
	keys  map[keychain.KeyLocator]*btcec.PrivateKey
}

// NewMockSigner returns a fresh MockSigner.
func NewMockSigner() *MockSigner {
	return &MockSigner{
		keys: make(map[keychain.KeyLocator]*btcec.PrivateKey),
	}
}

// SignOutputRaw signs an input on the passed transaction using the input index
// in the sign descriptor. The returned signature is the raw DER-encoded
// signature without the signhash flag.
func (s *MockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	witnessScript := signDesc.WitnessScript
	amt := signDesc.Output.Value

	privKey, ok := s.keys[signDesc.KeyDesc.KeyLocator]
	if !ok {
		panic("cannot sign w/ unknown key")
	}

	sig, err := txscript.RawTxInWitnessSignature(
		tx, signDesc.SigHashes, signDesc.InputIndex, amt,
		witnessScript, signDesc.HashType, privKey,
	)
	if err != nil {
		return nil, err
	}

	return ecdsa.ParseDERSignature(sig[:len(sig)-1])
}

// ComputeInputScript is not implemented.
func (s *MockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {
	panic("not implemented")
}

// MuSig2CreateSession creates a new MuSig2 signing session using the local
// key identified by the key locator. The complete list of all public keys of
// all signing parties must be provided, including the public key of the local
// signing key. If nonces of other parties are already known, they can be
// submitted as well to reduce the number of method calls necessary later on.
func (s *MockSigner) MuSig2CreateSession(input.MuSig2Version,
	keychain.KeyLocator, []*btcec.PublicKey, *input.MuSig2Tweaks,
	[][musig2.PubNonceSize]byte,
	*musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	return nil, nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID. This method returns true
// once we have all nonces for all other signing participants.
func (s *MockSigner) MuSig2RegisterNonces(input.MuSig2SessionID,
	[][musig2.PubNonceSize]byte) (bool, error) {

	return false, nil
}

// MuSig2Sign creates a partial signature using the local signing key
// that was specified when the session was created. This can only be
// called when all public nonces of all participants are known and have
// been registered with the session. If this node isn't responsible for
// combining all the partial signatures, then the cleanup parameter
// should be set, indicating that the session can be removed from memory
// once the signature was produced.
func (s *MockSigner) MuSig2Sign(input.MuSig2SessionID,
	[sha256.Size]byte, bool) (*musig2.PartialSignature, error) {

	return nil, nil
}

// MuSig2CombineSig combines the given partial signature(s) with the
// local one, if it already exists. Once a partial signature of all
// participants is registered, the final signature will be combined and
// returned.
func (s *MockSigner) MuSig2CombineSig(input.MuSig2SessionID,
	[]*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	return nil, false, nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (s *MockSigner) MuSig2Cleanup(input.MuSig2SessionID) error {
	return nil
}

// AddPrivKey records the passed privKey in the MockSigner's registry of keys it
// can sign with in the future. A unique key locator is returned, allowing the
// caller to sign with this key when presented via an input.SignDescriptor.
func (s *MockSigner) AddPrivKey(privKey *btcec.PrivateKey) keychain.KeyLocator {
	s.mu.Lock()
	defer s.mu.Unlock()

	keyLoc := keychain.KeyLocator{
		Index: s.index,
	}
	s.index++

	s.keys[keyLoc] = privKey

	return keyLoc
}

// Compile-time constraint ensuring the MockSigner implements the input.Signer
// interface.
var _ input.Signer = (*MockSigner)(nil)
