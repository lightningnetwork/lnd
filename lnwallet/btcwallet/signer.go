package btcwallet

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
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
func (b *BtcWallet) FetchInputInfo(prevOut *wire.OutPoint) (*wire.TxOut, error) {
	var (
		err    error
		output *wire.TxOut
	)

	// First check to see if the output is already within the utxo cache.
	// If so we can return directly saving a disk access.
	b.cacheMtx.RLock()
	if output, ok := b.utxoCache[*prevOut]; ok {
		b.cacheMtx.RUnlock()
		return output, nil
	}
	b.cacheMtx.RUnlock()

	// Otherwise, we manually look up the output within the tx store.
	txid := &prevOut.Hash
	txDetail, err := base.UnstableAPI(b.wallet).TxDetails(txid)
	if err != nil {
		return nil, err
	} else if txDetail == nil {
		return nil, lnwallet.ErrNotMine
	}

	// With the output retrieved, we'll make an additional check to ensure
	// we actually have control of this output. We do this because the check
	// above only guarantees that the transaction is somehow relevant to us,
	// like in the event of us being the sender of the transaction.
	output = txDetail.TxRecord.MsgTx.TxOut[prevOut.Index]
	if _, err := b.fetchOutputAddr(output.PkScript); err != nil {
		return nil, err
	}

	b.cacheMtx.Lock()
	b.utxoCache[*prevOut] = output
	b.cacheMtx.Unlock()

	return output, nil
}

// fetchOutputAddr attempts to fetch the managed address corresponding to the
// passed output script. This function is used to look up the proper key which
// should be used to sign a specified input.
func (b *BtcWallet) fetchOutputAddr(script []byte) (waddrmgr.ManagedAddress, error) {
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, b.netParams)
	if err != nil {
		return nil, err
	}

	// If the case of a multi-sig output, several address may be extracted.
	// Therefore, we simply select the key for the first address we know
	// of.
	for _, addr := range addrs {
		addr, err := b.wallet.AddressInfo(addr)
		if err == nil {
			return addr, nil
		}
	}

	return nil, lnwallet.ErrNotMine
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

	var key *btcec.PrivateKey
	err = walletdb.View(b.db, func(tx walletdb.ReadTx) error {
		addrmgrNs := tx.ReadBucket(waddrmgrNamespaceKey)

		path := waddrmgr.DerivationPath{
			Account: uint32(keyLoc.Family),
			Branch:  0,
			Index:   uint32(keyLoc.Index),
		}
		addr, err := scopedMgr.DeriveFromKeyPath(addrmgrNs, path)
		if err != nil {
			return err
		}

		key, err = addr.(waddrmgr.ManagedPubKeyAddress).PrivKey()
		return err
	})
	if err != nil {
		return nil, err
	}

	return key, nil
}

// fetchPrivKey attempts to retrieve the raw private key corresponding to the
// passed public key if populated, or the key descriptor path (if non-empty).
func (b *BtcWallet) fetchPrivKey(keyDesc *keychain.KeyDescriptor) (*btcec.PrivateKey, error) {
	// If the key locator within the descriptor *isn't* empty, then we can
	// directly derive the keys raw.
	emptyLocator := keyDesc.KeyLocator.IsEmpty()
	if !emptyLocator {
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
	signDesc *input.SignDescriptor) ([]byte, error) {

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
	return sig[:len(sig)-1], nil
}

// ComputeInputScript generates a complete InputScript for the passed
// transaction with the signature as defined within the passed SignDescriptor.
// This method is capable of generating the proper input script for both
// regular p2wkh output and p2wkh outputs nested within a regular p2sh output.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	outputScript := signDesc.Output.PkScript
	walletAddr, err := b.fetchOutputAddr(outputScript)
	if err != nil {
		return nil, err
	}

	pka := walletAddr.(waddrmgr.ManagedPubKeyAddress)
	privKey, err := pka.PrivKey()
	if err != nil {
		return nil, err
	}

	var witnessProgram []byte
	inputScript := &input.Script{}

	switch {

	// If we're spending p2wkh output nested within a p2sh output, then
	// we'll need to attach a sigScript in addition to witness data.
	case pka.AddrType() == waddrmgr.NestedWitnessPubKey:
		pubKey := privKey.PubKey()
		pubKeyHash := btcutil.Hash160(pubKey.SerializeCompressed())

		// Next, we'll generate a valid sigScript that will allow us to
		// spend the p2sh output. The sigScript will contain only a
		// single push of the p2wkh witness program corresponding to
		// the matching public key of this address.
		p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(
			pubKeyHash, b.netParams,
		)
		if err != nil {
			return nil, err
		}
		witnessProgram, err = txscript.PayToAddrScript(p2wkhAddr)
		if err != nil {
			return nil, err
		}

		bldr := txscript.NewScriptBuilder()
		bldr.AddData(witnessProgram)
		sigScript, err := bldr.Script()
		if err != nil {
			return nil, err
		}

		inputScript.SigScript = sigScript

	// Otherwise, this is a regular p2wkh output, so we include the
	// witness program itself as the subscript to generate the proper
	// sighash digest. As part of the new sighash digest algorithm, the
	// p2wkh witness program will be expanded into a regular p2kh
	// script.
	default:
		witnessProgram = outputScript
	}

	// If a tweak (single or double) is specified, then we'll need to use
	// this tweak to derive the final private key to be used for signing
	// this output.
	privKey, err = maybeTweakPrivKey(signDesc, privKey)
	if err != nil {
		return nil, err
	}

	// Generate a valid witness stack for the input.
	// TODO(roasbeef): adhere to passed HashType
	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, witnessProgram,
		signDesc.HashType, privKey, true,
	)
	if err != nil {
		return nil, err
	}

	inputScript.Witness = witnessScript

	return inputScript, nil
}

// A compile time check to ensure that BtcWallet implements the Signer
// interface.
var _ input.Signer = (*BtcWallet)(nil)

// SignMessage attempts to sign a target message with the private key that
// corresponds to the passed public key. If the target private key is unable to
// be found, then an error will be returned. The actual digest signed is the
// double SHA-256 of the passed message.
//
// NOTE: This is a part of the MessageSigner interface.
func (b *BtcWallet) SignMessage(pubKey *btcec.PublicKey,
	msg []byte) (*btcec.Signature, error) {

	// First attempt to fetch the private key which corresponds to the
	// specified public key.
	privKey, err := b.fetchPrivKey(&keychain.KeyDescriptor{
		PubKey: pubKey,
	})
	if err != nil {
		return nil, err
	}

	// Double hash and sign the data.
	msgDigest := chainhash.DoubleHashB(msg)
	sign, err := privKey.Sign(msgDigest)
	if err != nil {
		return nil, errors.Errorf("unable sign the message: %v", err)
	}

	return sign, nil
}

// A compile time check to ensure that BtcWallet implements the MessageSigner
// interface.
var _ lnwallet.MessageSigner = (*BtcWallet)(nil)
