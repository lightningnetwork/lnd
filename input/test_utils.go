package input

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/keychain"
)

var (

	// For simplicity a single priv key controls all of our test outputs.
	testWalletPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}

	// We're alice :)
	bobsPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	// Use a hard-coded HD seed.
	testHdSeed = chainhash.Hash{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}
)

// SignDescriptorChecker is an interface that allows the MockSigner to check
// the correctness of the SignDescriptor passed to the SignOutputRaw and
// ComputeInputScript methods.
type SignDescriptorChecker interface {
	// CheckSignDescriptor checks the passed SignDescriptor against the
	// passed transaction.
	CheckSignDescriptor(*wire.MsgTx, *SignDescriptor) error
}

// MockSigner is a simple implementation of the Signer interface. Each one has
// a set of private keys in a slice and can sign messages using the appropriate
// one. It also has an optional SignDescriptorChecker to allow packages using
// this signer to check that the scripts they require are annotated correctly.
type MockSigner struct {
	Privkeys  []*btcec.PrivateKey
	NetParams *chaincfg.Params

	*MusigSessionManager

	SignDescriptorChecker SignDescriptorChecker
}

// NewMockSigner returns a new instance of the MockSigner given a set of
// backing private keys.
func NewMockSigner(privKeys []*btcec.PrivateKey,
	netParams *chaincfg.Params) *MockSigner {

	signer := &MockSigner{
		Privkeys:  privKeys,
		NetParams: netParams,
	}

	keyFetcher := func(*keychain.KeyDescriptor) (*btcec.PrivateKey, error) {
		return signer.Privkeys[0], nil
	}
	signer.MusigSessionManager = NewMusigSessionManager(keyFetcher)

	return signer
}

// SignOutputRaw generates a signature for the passed transaction according to
// the data within the passed SignDescriptor.
func (m *MockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *SignDescriptor) (Signature, error) {

	pubkey := signDesc.KeyDesc.PubKey
	switch {
	case signDesc.SingleTweak != nil:
		pubkey = TweakPubKeyWithTweak(pubkey, signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		pubkey = DeriveRevocationPubkey(pubkey, signDesc.DoubleTweak.PubKey())
	}

	hash160 := btcutil.Hash160(pubkey.SerializeCompressed())
	privKey := m.findKey(hash160, signDesc.SingleTweak, signDesc.DoubleTweak)
	if privKey == nil {
		return nil, fmt.Errorf("mock signer does not have key")
	}

	// In case of a taproot output any signature is always a Schnorr
	// signature, based on the new tapscript sighash algorithm.
	if txscript.IsPayToTaproot(signDesc.Output.PkScript) {
		sigHashes := txscript.NewTxSigHashes(
			tx, signDesc.PrevOutputFetcher,
		)

		// Are we spending a script path or the key path? The API is
		// slightly different, so we need to account for that to get
		// the raw signature.
		var (
			rawSig []byte
			err    error
		)
		switch signDesc.SignMethod {
		case TaprootKeySpendBIP0086SignMethod,
			TaprootKeySpendSignMethod:

			// This function tweaks the private key using the tap
			// root key supplied as the tweak.
			rawSig, err = txscript.RawTxInTaprootSignature(
				tx, sigHashes, signDesc.InputIndex,
				signDesc.Output.Value, signDesc.Output.PkScript,
				signDesc.TapTweak, signDesc.HashType,
				privKey,
			)
			if err != nil {
				return nil, err
			}

		case TaprootScriptSpendSignMethod:
			leaf := txscript.TapLeaf{
				LeafVersion: txscript.BaseLeafVersion,
				Script:      signDesc.WitnessScript,
			}
			rawSig, err = txscript.RawTxInTapscriptSignature(
				tx, sigHashes, signDesc.InputIndex,
				signDesc.Output.Value, signDesc.Output.PkScript,
				leaf, signDesc.HashType, privKey,
			)
			if err != nil {
				return nil, err
			}
		}

		// The signature returned above might have a sighash flag
		// attached if a non-default type was used. We'll slice this
		// off if it exists to ensure we can properly parse the raw
		// signature.
		sig, err := schnorr.ParseSignature(
			rawSig[:schnorr.SignatureSize],
		)
		if err != nil {
			return nil, err
		}

		return sig, nil
	}

	sig, err := txscript.RawTxInWitnessSignature(
		tx, signDesc.SigHashes, signDesc.InputIndex,
		signDesc.Output.Value, signDesc.WitnessScript,
		signDesc.HashType, privKey,
	)
	if err != nil {
		return nil, err
	}

	if m.SignDescriptorChecker != nil {
		err = m.SignDescriptorChecker.CheckSignDescriptor(tx, signDesc)
		if err != nil {
			return nil, err
		}
	}

	return ecdsa.ParseDERSignature(sig[:len(sig)-1])
}

// ComputeInputScript generates a complete InputIndex for the passed transaction
// with the signature as defined within the passed SignDescriptor. This method
// should be capable of generating the proper input script for both regular
// p2wkh output and p2wkh outputs nested within a regular p2sh output.
func (m *MockSigner) ComputeInputScript(tx *wire.MsgTx, signDesc *SignDescriptor) (*Script, error) {
	scriptType, addresses, _, err := txscript.ExtractPkScriptAddrs(
		signDesc.Output.PkScript, m.NetParams)
	if err != nil {
		return nil, err
	}

	if m.SignDescriptorChecker != nil {
		err = m.SignDescriptorChecker.CheckSignDescriptor(tx, signDesc)
		if err != nil {
			return nil, err
		}
	}

	switch scriptType {
	case txscript.PubKeyHashTy:
		privKey := m.findKey(addresses[0].ScriptAddress(), signDesc.SingleTweak,
			signDesc.DoubleTweak)
		if privKey == nil {
			return nil, fmt.Errorf("mock signer does not have key for "+
				"address %v", addresses[0])
		}

		sigScript, err := txscript.SignatureScript(
			tx, signDesc.InputIndex, signDesc.Output.PkScript,
			txscript.SigHashAll, privKey, true,
		)
		if err != nil {
			return nil, err
		}

		return &Script{SigScript: sigScript}, nil

	case txscript.WitnessV0PubKeyHashTy:
		privKey := m.findKey(addresses[0].ScriptAddress(), signDesc.SingleTweak,
			signDesc.DoubleTweak)
		if privKey == nil {
			return nil, fmt.Errorf("mock signer does not have key for "+
				"address %v", addresses[0])
		}

		witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
			signDesc.InputIndex, signDesc.Output.Value,
			signDesc.Output.PkScript, txscript.SigHashAll, privKey, true)
		if err != nil {
			return nil, err
		}

		return &Script{Witness: witnessScript}, nil

	default:
		return nil, fmt.Errorf("unexpected script type: %v", scriptType)
	}
}

// findKey searches through all stored private keys and returns one
// corresponding to the hashed pubkey if it can be found. The public key may
// either correspond directly to the private key or to the private key with a
// tweak applied.
func (m *MockSigner) findKey(needleHash160 []byte, singleTweak []byte,
	doubleTweak *btcec.PrivateKey) *btcec.PrivateKey {

	for _, privkey := range m.Privkeys {
		// First check whether public key is directly derived from
		// private key.
		hash160 := btcutil.Hash160(privkey.PubKey().SerializeCompressed())
		if bytes.Equal(hash160, needleHash160) {
			return privkey
		}

		// Otherwise check if public key is derived from tweaked
		// private key.
		switch {
		case singleTweak != nil:
			privkey = TweakPrivKey(privkey, singleTweak)
		case doubleTweak != nil:
			privkey = DeriveRevocationPrivKey(privkey, doubleTweak)
		default:
			continue
		}
		hash160 = btcutil.Hash160(privkey.PubKey().SerializeCompressed())
		if bytes.Equal(hash160, needleHash160) {
			return privkey
		}
	}
	return nil
}

// pubkeyFromHex parses a Bitcoin public key from a hex encoded string.
func pubkeyFromHex(keyHex string) (*btcec.PublicKey, error) {
	bytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	return btcec.ParsePubKey(bytes)
}

// privkeyFromHex parses a Bitcoin private key from a hex encoded string.
func privkeyFromHex(keyHex string) (*btcec.PrivateKey, error) {
	bytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	key, _ := btcec.PrivKeyFromBytes(bytes)
	return key, nil

}

// pubkeyToHex serializes a Bitcoin public key to a hex encoded string.
func pubkeyToHex(key *btcec.PublicKey) string {
	return hex.EncodeToString(key.SerializeCompressed())
}

// privkeyFromHex serializes a Bitcoin private key to a hex encoded string.
func privkeyToHex(key *btcec.PrivateKey) string {
	return hex.EncodeToString(key.Serialize())
}

// DefaultSignDescriptorChecker checks the SignDescriptor against the AllowedOut
// slice of explicitly allowed PkScript and the OutChecker per-TXO validator.
type DefaultSignDescriptorChecker struct {
	OutChecker func(*wire.TxOut, map[string][]byte) error
	AllowedOut [][]byte
}

// CheckSignDescriptor ensures that the SignDescriptor's OutSignInfo, if they
// exist, can be directly used to derive the output scripts. This ensures that
// any policy enforcement done by a remote signer can be done with a full view
// of where the money is going, and match the components against policy.
func (c *DefaultSignDescriptorChecker) CheckSignDescriptor(tx *wire.MsgTx,
	signDesc *SignDescriptor) error {

	if len(signDesc.OutSignInfo) != len(tx.TxOut) {
		return fmt.Errorf("partial output count mismatch: expected "+
			"%d, got %d", len(tx.TxOut), len(signDesc.OutSignInfo))
	}

checkOut:
	for i, out := range tx.TxOut {
		// First, check if we explicitly allow the output script.
		for _, allowed := range c.AllowedOut {
			if bytes.Equal(allowed, out.PkScript) {
				continue checkOut
			}
		}

		// Next, we make a map of the passed unknowns for the output
		// for easy access.
		unkMap := make(map[string][]byte)
		for _, unk := range signDesc.OutSignInfo[i] {
			unkMap[string(unk.Key)] = unk.Value
		}

		// Then we check that we can derive the PkScript from the passed
		// unknowns.
		err := c.OutChecker(out, unkMap)
		if err != nil {
			return fmt.Errorf("got error checking POutput %d: %v"+
				"\nTx: %+v\nUnknowns: %+v", i, err,
				spew.Sdump(tx), unkMap)
		}
	}

	return nil
}

// AddAllowedOut adds an explicitly allowed PkScript for when we fudge it in
// tests. It only works for the MockSigner and does nothing otherwise. It should
// only be called from the same goroutine where the signer is created.
func AddAllowedOut(signer Signer, script []byte) {
	mock, ok := signer.(*MockSigner)
	if !ok {
		return
	}

	checker, ok := mock.
		SignDescriptorChecker.(*DefaultSignDescriptorChecker)
	if !ok {
		return
	}

	checker.AllowedOut = append(checker.AllowedOut, script)
}
