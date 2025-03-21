package btcwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// FetchOutpointInfo queries for the WalletController's knowledge of the passed
// outpoint. If the base wallet determines this output is under its control,
// then the original txout should be returned. Otherwise, a non-nil error value
// of ErrNotMine should be returned instead.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) FetchOutpointInfo(prevOut *wire.OutPoint) (*lnwallet.Utxo,
	error) {

	prevTx, txOut, confirmations, err := b.wallet.FetchOutpointInfo(prevOut)
	if err != nil {
		return nil, err
	}

	// Then, we'll populate all of the information required by the struct.
	addressType := lnwallet.UnknownAddressType
	switch {
	case txscript.IsPayToWitnessPubKeyHash(txOut.PkScript):
		addressType = lnwallet.WitnessPubKey
	case txscript.IsPayToScriptHash(txOut.PkScript):
		addressType = lnwallet.NestedWitnessPubKey
	case txscript.IsPayToTaproot(txOut.PkScript):
		addressType = lnwallet.TaprootPubkey
	}

	return &lnwallet.Utxo{
		AddressType:   addressType,
		Value:         btcutil.Amount(txOut.Value),
		PkScript:      txOut.PkScript,
		Confirmations: confirmations,
		OutPoint:      *prevOut,
		PrevTx:        prevTx,
	}, nil
}

// FetchDerivationInfo queries for the wallet's knowledge of the passed
// pkScript and constructs the derivation info and returns it.
func (b *BtcWallet) FetchDerivationInfo(
	pkScript []byte) (*psbt.Bip32Derivation, error) {

	return b.wallet.FetchDerivationInfo(pkScript)
}

// ScriptForOutput returns the address, witness program and redeem script for a
// given UTXO. An error is returned if the UTXO does not belong to our wallet or
// it is not a managed pubKey address.
func (b *BtcWallet) ScriptForOutput(output *wire.TxOut) (
	waddrmgr.ManagedPubKeyAddress, []byte, []byte, error) {

	return b.wallet.ScriptForOutput(output)
}

// deriveKeyByBIP32Path derives a key described by a BIP32 path. We expect the
// first three elements of the path to be hardened according to BIP44, so they
// must be a number >= 2^31.
func (b *BtcWallet) deriveKeyByBIP32Path(path []uint32) (*btcec.PrivateKey,
	error) {

	// Make sure we get a full path with exactly 5 elements. A path is
	// either custom purpose one with 4 dynamic and one static elements:
	//    m/1017'/coinType'/keyFamily'/0/index
	// Or a default BIP49/89 one with 5 elements:
	//    m/purpose'/coinType'/account'/change/index
	const expectedDerivationPathDepth = 5
	if len(path) != expectedDerivationPathDepth {
		return nil, fmt.Errorf("invalid BIP32 derivation path, "+
			"expected path length %d, instead was %d",
			expectedDerivationPathDepth, len(path))
	}

	// Assert that the first three parts of the path are actually hardened
	// to avoid under-flowing the uint32 type.
	if err := assertHardened(path[0], path[1], path[2]); err != nil {
		return nil, fmt.Errorf("invalid BIP32 derivation path, "+
			"expected first three elements to be hardened: %w", err)
	}

	purpose := path[0] - hdkeychain.HardenedKeyStart
	coinType := path[1] - hdkeychain.HardenedKeyStart
	account := path[2] - hdkeychain.HardenedKeyStart
	change, index := path[3], path[4]

	// Is this a custom lnd internal purpose key?
	switch purpose {
	case keychain.BIP0043Purpose:
		// Make sure it's for the same coin type as our wallet's
		// keychain scope.
		if coinType != b.chainKeyScope.Coin {
			return nil, fmt.Errorf("invalid BIP32 derivation "+
				"path, expected coin type %d, instead was %d",
				b.chainKeyScope.Coin, coinType)
		}

		return b.deriveKeyByLocator(keychain.KeyLocator{
			Family: keychain.KeyFamily(account),
			Index:  index,
		})

	// Is it a standard, BIP defined purpose that the wallet understands?
	case waddrmgr.KeyScopeBIP0044.Purpose,
		waddrmgr.KeyScopeBIP0049Plus.Purpose,
		waddrmgr.KeyScopeBIP0084.Purpose,
		waddrmgr.KeyScopeBIP0086.Purpose:

		// We're going to continue below the switch statement to avoid
		// unnecessary indentation for this default case.

	// Currently, there is no way to import any other key scopes than the
	// one custom purpose or three standard ones into lnd's wallet. So we
	// shouldn't accept any other scopes to sign for.
	default:
		return nil, fmt.Errorf("invalid BIP32 derivation path, "+
			"unknown purpose %d", purpose)
	}

	// Okay, we made sure it's a BIP49/84 key, so we need to derive it now.
	// Interestingly, the btcwallet never actually uses a coin type other
	// than 0 for those keys, so we need to make sure this behavior is
	// replicated here.
	if coinType != 0 {
		return nil, fmt.Errorf("invalid BIP32 derivation path, coin " +
			"type must be 0 for BIP49/84 btcwallet keys")
	}

	// We only expect to be asked to sign with key scopes that we know
	// about. So if the scope doesn't exist, we don't create it.
	scope := waddrmgr.KeyScope{
		Purpose: purpose,
		Coin:    coinType,
	}
	keyPath := waddrmgr.DerivationPath{
		InternalAccount: account,
		Account:         account,
		Branch:          change,
		Index:           index,
	}

	privKey, err := b.wallet.DeriveFromKeyPath(scope, keyPath)
	if err != nil {
		return nil, fmt.Errorf("error deriving key from path %#v: %w",
			keyPath, err)
	}

	return privKey, nil
}

// assertHardened makes sure each given element is >= 2^31.
func assertHardened(elements ...uint32) error {
	for idx, element := range elements {
		if element < hdkeychain.HardenedKeyStart {
			return fmt.Errorf("element at index %d is not hardened",
				idx)
		}
	}

	return nil
}

// deriveKeyByLocator attempts to derive a key stored in the wallet given a
// valid key locator.
func (b *BtcWallet) deriveKeyByLocator(
	keyLoc keychain.KeyLocator) (*btcec.PrivateKey, error) {

	// We'll assume the special lightning key scope in this case.
	scope := b.chainKeyScope
	path := waddrmgr.DerivationPath{
		InternalAccount: uint32(keyLoc.Family),
		Account:         uint32(keyLoc.Family),
		Branch:          0,
		Index:           keyLoc.Index,
	}

	key, err := b.wallet.DeriveFromKeyPathAddAccount(scope, path)
	if err != nil {
		return nil, err
	}

	return key, nil
}

// fetchPrivKey attempts to retrieve the raw private key corresponding to the
// passed public key if populated, or the key descriptor path (if non-empty).
func (b *BtcWallet) fetchPrivKey(
	keyDesc *keychain.KeyDescriptor) (*btcec.PrivateKey, error) {

	// If the key locator within the descriptor *isn't* empty, then we can
	// directly derive the keys raw.
	emptyLocator := keyDesc.KeyLocator.IsEmpty()
	if !emptyLocator || keyDesc.PubKey == nil {
		return b.deriveKeyByLocator(keyDesc.KeyLocator)
	}

	hash160 := btcutil.Hash160(keyDesc.PubKey.SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(hash160, b.netParams)
	if err != nil {
		return nil, err
	}

	// Otherwise, we'll attempt to derive the key based on the address.
	// This will only work if we've already derived this address in the
	// past, since the wallet relies on a mapping of addr -> key.
	key, err := b.wallet.PrivKeyForAddress(addr)
	switch {
	// If we didn't find this key in the wallet, then there's a chance that
	// this is actually an "empty" key locator. The legacy KeyLocator
	// format failed to properly distinguish an empty key locator from the
	// very first in the index (0, 0).IsEmpty() == true.
	case waddrmgr.IsError(err, waddrmgr.ErrAddressNotFound) && emptyLocator:
		return b.deriveKeyByLocator(keyDesc.KeyLocator)

	case err != nil:
		return nil, err

	default:
		return key, nil
	}
}

// maybeTweakPrivKey examines the single and double tweak parameters on the
// passed sign descriptor and may perform a mapping on the passed private key
// in order to utilize the tweaks, if populated.
func maybeTweakPrivKey(signDesc *input.SignDescriptor,
	privKey *btcec.PrivateKey) (*btcec.PrivateKey, error) {

	var retPriv *btcec.PrivateKey
	switch {

	case signDesc.SingleTweak != nil:
		retPriv = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)

	case signDesc.DoubleTweak != nil:
		retPriv = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)

	default:
		retPriv = privKey
	}

	return retPriv, nil
}

// SignOutputRaw generates a signature for the passed transaction according to
// the data within the passed SignDescriptor.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	witnessScript := signDesc.WitnessScript

	// First attempt to fetch the private key which corresponds to the
	// specified public key.
	privKey, err := b.fetchPrivKey(&signDesc.KeyDesc)
	if err != nil {
		return nil, err
	}

	// If a tweak (single or double) is specified, then we'll need to use
	// this tweak to derive the final private key to be used for signing
	// this output.
	privKey, err = maybeTweakPrivKey(signDesc, privKey)
	if err != nil {
		return nil, err
	}

	// In case of a taproot output any signature is always a Schnorr
	// signature, based on the new tapscript sighash algorithm.
	if txscript.IsPayToTaproot(signDesc.Output.PkScript) {
		sigHashes := txscript.NewTxSigHashes(
			tx, signDesc.PrevOutputFetcher,
		)

		// Are we spending a script path or the key path? The API is
		// slightly different, so we need to account for that to get the
		// raw signature.
		var rawSig []byte
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

	// TODO(roasbeef): generate sighash midstate if not present?

	amt := signDesc.Output.Value
	sig, err := txscript.RawTxInWitnessSignature(
		tx, signDesc.SigHashes, signDesc.InputIndex, amt,
		witnessScript, signDesc.HashType, privKey,
	)
	if err != nil {
		return nil, err
	}

	// Chop off the sighash flag at the end of the signature.
	return ecdsa.ParseDERSignature(sig[:len(sig)-1])
}

// ComputeInputScript generates a complete InputScript for the passed
// transaction with the signature as defined within the passed SignDescriptor.
// This method is capable of generating the proper input script for both
// regular p2wkh output and p2wkh outputs nested within a regular p2sh output.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	// If a tweak (single or double) is specified, then we'll need to use
	// this tweak to derive the final private key to be used for signing
	// this output.
	privKeyTweaker := func(k *btcec.PrivateKey) (*btcec.PrivateKey, error) {
		return maybeTweakPrivKey(signDesc, k)
	}

	// Let the wallet compute the input script now.
	witness, sigScript, err := b.wallet.ComputeInputScript(
		tx, signDesc.Output, signDesc.InputIndex, signDesc.SigHashes,
		signDesc.HashType, privKeyTweaker,
	)
	if err != nil {
		return nil, err
	}

	return &input.Script{
		Witness:   witness,
		SigScript: sigScript,
	}, nil
}

// A compile time check to ensure that BtcWallet implements the Signer
// interface.
var _ input.Signer = (*BtcWallet)(nil)

// SignMessage attempts to sign a target message with the private key that
// corresponds to the passed key locator. If the target private key is unable to
// be found, then an error will be returned. The actual digest signed is the
// double SHA-256 of the passed message.
//
// NOTE: This is a part of the MessageSigner interface.
func (b *BtcWallet) SignMessage(keyLoc keychain.KeyLocator,
	msg []byte, doubleHash bool) (*ecdsa.Signature, error) {

	// First attempt to fetch the private key which corresponds to the
	// specified public key.
	privKey, err := b.fetchPrivKey(&keychain.KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, err
	}

	// Double hash and sign the data.
	var msgDigest []byte
	if doubleHash {
		msgDigest = chainhash.DoubleHashB(msg)
	} else {
		msgDigest = chainhash.HashB(msg)
	}
	return ecdsa.Sign(privKey, msgDigest), nil
}

// A compile time check to ensure that BtcWallet implements the MessageSigner
// interface.
var _ lnwallet.MessageSigner = (*BtcWallet)(nil)
