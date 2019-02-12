package wtmock

import (
	"sync"

	"github.com/btcsuite/btcd/btcec"
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
	signDesc *input.SignDescriptor) ([]byte, error) {
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

	return sig[:len(sig)-1], nil
}

// ComputeInputScript is not implemented.
func (s *MockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {
	panic("not implemented")
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
