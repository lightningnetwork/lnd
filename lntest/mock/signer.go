package mock

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec"
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

// SingleSigner is an implementation of the Signer interface that signs
// everything with a single private key.
type SingleSigner struct {
	Privkey *btcec.PrivateKey
	KeyLoc  keychain.KeyLocator
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

	return btcec.ParseDERSignature(sig[:len(sig)-1], btcec.S256())
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
	msg []byte, doubleHash bool) (*btcec.Signature, error) {

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
	sign, err := s.Privkey.Sign(digest)
	if err != nil {
		return nil, fmt.Errorf("can't sign the message: %v", err)
	}

	return sign, nil
}
