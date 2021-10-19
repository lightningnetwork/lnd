package btcwallet

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// FetchInputInfo queries for the WalletController's knowledge of the passed
// outpoint. If the base wallet determines this output is under its control,
// then the original txout should be returned. Otherwise, a non-nil error value
// of ErrNotMine should be returned instead.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) FetchInputInfo(prevOut *wire.OutPoint) (*lnwallet.Utxo, error) {
	_, txOut, _, confirmations, err := b.wallet.FetchInputInfo(prevOut)
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
	}

	return &lnwallet.Utxo{
		AddressType:   addressType,
		Value:         btcutil.Amount(txOut.Value),
		PkScript:      txOut.PkScript,
		Confirmations: confirmations,
		OutPoint:      *prevOut,
	}, nil
}

// deriveFromKeyLoc attempts to derive a private key using a fully specified
// KeyLocator.
func deriveFromKeyLoc(scopedMgr *waddrmgr.ScopedKeyManager,
	addrmgrNs walletdb.ReadWriteBucket,
	keyLoc keychain.KeyLocator) (*btcec.PrivateKey, error) {

	path := waddrmgr.DerivationPath{
		InternalAccount: uint32(keyLoc.Family),
		Account:         uint32(keyLoc.Family),
		Branch:          0,
		Index:           keyLoc.Index,
	}
	addr, err := scopedMgr.DeriveFromKeyPath(addrmgrNs, path)
	if err != nil {
		return nil, err
	}

	return addr.(waddrmgr.ManagedPubKeyAddress).PrivKey()
}

// deriveKeyByLocator attempts to derive a key stored in the wallet given a
// valid key locator.
func (b *BtcWallet) deriveKeyByLocator(keyLoc keychain.KeyLocator) (*btcec.PrivateKey, error) {
	// We'll assume the special lightning key scope in this case.
	scopedMgr, err := b.wallet.Manager.FetchScopedKeyManager(
		b.chainKeyScope,
	)
	if err != nil {
		return nil, err
	}

	// First try to read the key from the cached store, if this fails, then
	// we'll fall through to the method below that requires a database
	// transaction.
	path := waddrmgr.DerivationPath{
		InternalAccount: uint32(keyLoc.Family),
		Account:         uint32(keyLoc.Family),
		Branch:          0,
		Index:           keyLoc.Index,
	}
	privKey, err := scopedMgr.DeriveFromKeyPathCache(path)
	if err == nil {
		return privKey, nil
	}

	var key *btcec.PrivateKey
	err = walletdb.Update(b.db, func(tx walletdb.ReadWriteTx) error {
		addrmgrNs := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		key, err = deriveFromKeyLoc(scopedMgr, addrmgrNs, keyLoc)
		if waddrmgr.IsError(err, waddrmgr.ErrAccountNotFound) {
			// If we've reached this point, then the account
			// doesn't yet exist, so we'll create it now to ensure
			// we can sign.
			acctErr := scopedMgr.NewRawAccount(
				addrmgrNs, uint32(keyLoc.Family),
			)
			if acctErr != nil {
				return acctErr
			}

			// Now that we know the account exists, we'll attempt
			// to re-derive the private key.
			key, err = deriveFromKeyLoc(
				scopedMgr, addrmgrNs, keyLoc,
			)
			if err != nil {
				return err
			}
		}

		return err
	})
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
	return btcec.ParseDERSignature(sig[:len(sig)-1], btcec.S256())
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
	msg []byte, doubleHash bool) (*btcec.Signature, error) {

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
	sign, err := privKey.Sign(msgDigest)
	if err != nil {
		return nil, errors.Errorf("unable sign the message: %v", err)
	}

	return sign, nil
}

// A compile time check to ensure that BtcWallet implements the MessageSigner
// interface.
var _ lnwallet.MessageSigner = (*BtcWallet)(nil)
