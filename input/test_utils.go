package input

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
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

// MockSigner is a simple implementation of the Signer interface. Each one has
// a set of private keys in a slice and can sign messages using the appropriate
// one.
type MockSigner struct {
	Privkeys  []*btcec.PrivateKey
	NetParams *chaincfg.Params
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

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.WitnessScript,
		signDesc.HashType, privKey)
	if err != nil {
		return nil, err
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

// MuSig2CreateSession creates a new MuSig2 signing session using the local
// key identified by the key locator. The complete list of all public keys of
// all signing parties must be provided, including the public key of the local
// signing key. If nonces of other parties are already known, they can be
// submitted as well to reduce the number of method calls necessary later on.
func (m *MockSigner) MuSig2CreateSession(MuSig2Version, keychain.KeyLocator,
	[]*btcec.PublicKey, *MuSig2Tweaks,
	[][musig2.PubNonceSize]byte) (*MuSig2SessionInfo, error) {

	return nil, nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID. This method returns true
// once we have all nonces for all other signing participants.
func (m *MockSigner) MuSig2RegisterNonces(MuSig2SessionID,
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
func (m *MockSigner) MuSig2Sign(MuSig2SessionID,
	[sha256.Size]byte, bool) (*musig2.PartialSignature, error) {

	return nil, nil
}

// MuSig2CombineSig combines the given partial signature(s) with the
// local one, if it already exists. Once a partial signature of all
// participants is registered, the final signature will be combined and
// returned.
func (m *MockSigner) MuSig2CombineSig(MuSig2SessionID,
	[]*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	return nil, false, nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (m *MockSigner) MuSig2Cleanup(MuSig2SessionID) error {
	return nil
}

// findKey searches through all stored private keys and returns one
// corresponding to the hashed pubkey if it can be found. The public key may
// either correspond directly to the private key or to the private key with a
// tweak applied.
func (m *MockSigner) findKey(needleHash160 []byte, singleTweak []byte,
	doubleTweak *btcec.PrivateKey) *btcec.PrivateKey {

	for _, privkey := range m.Privkeys {
		// First check whether public key is directly derived from private key.
		hash160 := btcutil.Hash160(privkey.PubKey().SerializeCompressed())
		if bytes.Equal(hash160, needleHash160) {
			return privkey
		}

		// Otherwise check if public key is derived from tweaked private key.
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
