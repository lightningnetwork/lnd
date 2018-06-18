package test

import (
	"bytes"
	"fmt"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	"github.com/lightningnetwork/lnd/lnwallet"
)

// MockSigner is a simple implementation of the Signer interface. Each one has
// a set of private keys in a slice and can sign messages using the appropriate
// one.
type MockSigner struct {
	Privkeys  []*btcec.PrivateKey
	NetParams *chaincfg.Params
}

func (m *MockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) ([]byte, error) {
	pubkey := signDesc.KeyDesc.PubKey
	switch {
	case signDesc.SingleTweak != nil:
		pubkey = lnwallet.TweakPubKeyWithTweak(pubkey, signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		pubkey = lnwallet.DeriveRevocationPubkey(pubkey,
			signDesc.DoubleTweak.PubKey())
	}

	hash160 := btcutil.Hash160(pubkey.SerializeCompressed())
	privKey := m.findKey(hash160, signDesc.SingleTweak, signDesc.DoubleTweak)
	if privKey == nil {
		return nil, fmt.Errorf("mock signer does not have key")
	}

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.WitnessScript,
		txscript.SigHashAll, privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}

func (m *MockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {
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

		scriptSig, err := txscript.SignatureScript(tx, signDesc.InputIndex,
			signDesc.Output.PkScript, txscript.SigHashAll, privKey, true)
		if err != nil {
			return nil, err
		}

		return &lnwallet.InputScript{ScriptSig: scriptSig}, nil

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

		return &lnwallet.InputScript{Witness: witnessScript}, nil

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
		// First check whether public key is directly derived from private key.
		hash160 := btcutil.Hash160(privkey.PubKey().SerializeCompressed())
		if bytes.Equal(hash160, needleHash160) {
			return privkey
		}

		// Otherwise check if public key is derived from tweaked private key.
		switch {
		case singleTweak != nil:
			privkey = lnwallet.TweakPrivKey(privkey, singleTweak)
		case doubleTweak != nil:
			privkey = lnwallet.DeriveRevocationPrivKey(privkey, doubleTweak)
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
