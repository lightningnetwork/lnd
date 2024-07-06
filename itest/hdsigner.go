package itest

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

type hdSigner struct {
	*input.MusigSessionManager

	ExtendedKey *hdkeychain.ExtendedKey
	ChainParams *chaincfg.Params
}

func (s *hdSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	// First attempt to fetch the private key which corresponds to the
	// specified public key.
	privKey, err := s.FetchPrivateKey(&signDesc.KeyDesc)
	if err != nil {
		return nil, err
	}

	return s.signOutputRawWithPrivateKey(tx, signDesc, privKey)
}

func (s *hdSigner) signOutputRawWithPrivateKey(tx *wire.MsgTx,
	signDesc *input.SignDescriptor,
	privKey *secp256k1.PrivateKey) (input.Signature, error) {

	witnessScript := signDesc.WitnessScript
	privKey = maybeTweakPrivKey(signDesc, privKey)

	sigHashes := txscript.NewTxSigHashes(tx, signDesc.PrevOutputFetcher)
	if txscript.IsPayToTaproot(signDesc.Output.PkScript) {
		// Are we spending a script path or the key path? The API is
		// slightly different, so we need to account for that to get the
		// raw signature.
		var (
			rawSig []byte
			err    error
		)

		switch signDesc.SignMethod {
		case input.TaprootKeySpendBIP0086SignMethod,
			input.TaprootKeySpendSignMethod:

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

		case input.TaprootScriptSpendSignMethod:
			leaf := txscript.TapLeaf{
				LeafVersion: txscript.BaseLeafVersion,
				Script:      witnessScript,
			}
			rawSig, err = txscript.RawTxInTapscriptSignature(
				tx, sigHashes, signDesc.InputIndex,
				signDesc.Output.Value, signDesc.Output.PkScript,
				leaf, signDesc.HashType, privKey,
			)
			if err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("unknown sign method: %v",
				signDesc.SignMethod)
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

	amt := signDesc.Output.Value
	sig, err := txscript.RawTxInWitnessSignature(
		tx, sigHashes, signDesc.InputIndex, amt,
		witnessScript, signDesc.HashType, privKey,
	)
	if err != nil {
		return nil, err
	}

	// Chop off the sighash flag at the end of the signature.
	return ecdsa.ParseDERSignature(sig[:len(sig)-1])
}

func (s *hdSigner) ComputeInputScript(_ *wire.MsgTx, _ *input.SignDescriptor) (
	*input.Script, error) {

	return nil, errors.New("unimplemented")
}

func (s *hdSigner) FetchPrivateKey(descriptor *keychain.KeyDescriptor) (
	*btcec.PrivateKey, error) {

	const HardenedKeyStart = uint32(hdkeychain.HardenedKeyStart)
	key, err := deriveChildren(s.ExtendedKey, []uint32{
		HardenedKeyStart + uint32(keychain.BIP0043Purpose),
		HardenedKeyStart + s.ChainParams.HDCoinType,
		HardenedKeyStart + uint32(descriptor.Family),
		0,
		descriptor.Index,
	})
	if err != nil {
		return nil, err
	}

	return key.ECPrivKey()
}

// maybeTweakPrivKey examines the single tweak parameters on the passed sign
// descriptor and may perform a mapping on the passed private key in order to
// utilize the tweaks, if populated.
func maybeTweakPrivKey(signDesc *input.SignDescriptor,
	privKey *btcec.PrivateKey) *btcec.PrivateKey {

	if signDesc.SingleTweak != nil {
		return input.TweakPrivKey(privKey, signDesc.SingleTweak)
	}

	return privKey
}

// ECDH performs a scalar multiplication (ECDH-like operation) between
// the target key descriptor and remote public key. The output
// returned will be the sha256 of the resulting shared point serialized
// in compressed format. If k is our private key, and P is the public
// key, we perform the following operation:
//
//	sx := k*P
//	s := sha256(sx.SerializeCompressed())
//
// NOTE: This is part of the keychain.ECDHRing interface.
func (s *hdSigner) ECDH(keyDesc keychain.KeyDescriptor,
	pubKey *btcec.PublicKey) ([32]byte, error) {

	// First, derive the private key.
	privKey, err := s.FetchPrivateKey(&keyDesc)
	if err != nil {
		return [32]byte{}, fmt.Errorf("failed to derive the private "+
			"key: %w", err)
	}

	return (&keychain.PrivKeyECDH{PrivKey: privKey}).ECDH(pubKey)
}
