package btcwallet

import (
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
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
func (b *BtcWallet) FetchInputInfo(prevOut *wire.OutPoint) (*lnwallet.Utxo,
	error) {

	prevTx, txOut, bip32, confirmations, err := b.wallet.FetchInputInfo(
		prevOut,
	)
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
		Derivation:    bip32,
		PrevTx:        prevTx,
	}, nil
}

// ScriptForOutput returns the address, witness program and redeem script for a
// given UTXO. An error is returned if the UTXO does not belong to our wallet or
// it is not a managed pubKey address.
func (b *BtcWallet) ScriptForOutput(output *wire.TxOut) (
	waddrmgr.ManagedPubKeyAddress, []byte, []byte, error) {

	return b.wallet.ScriptForOutput(output)
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
	scopedMgr, err := b.wallet.Manager.FetchScopedKeyManager(scope)
	if err != nil {
		return nil, fmt.Errorf("error fetching manager for scope %v: "+
			"%w", scope, err)
	}

	// Let's see if we can hit the private key cache.
	keyPath := waddrmgr.DerivationPath{
		InternalAccount: account,
		Account:         account,
		Branch:          change,
		Index:           index,
	}
	privKey, err := scopedMgr.DeriveFromKeyPathCache(keyPath)
	if err == nil {
		return privKey, nil
	}

	// The key wasn't in the cache, let's fully derive it now.
	err = walletdb.View(b.db, func(tx walletdb.ReadTx) error {
		addrmgrNs := tx.ReadBucket(waddrmgrNamespaceKey)

		addr, err := scopedMgr.DeriveFromKeyPath(addrmgrNs, keyPath)
		if err != nil {
			return fmt.Errorf("error deriving private key: %w", err)
		}

		privKey, err = addr.(waddrmgr.ManagedPubKeyAddress).PrivKey()
		return err
	})
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

// muSig2State is a struct that holds on to the internal signing session state
// of a MuSig2 session.
type muSig2State struct {
	// MuSig2SessionInfo is the associated meta information of the signing
	// session.
	input.MuSig2SessionInfo

	// context is the signing context responsible for keeping track of the
	// public keys involved in the signing process.
	context input.MuSig2Context

	// session is the signing session responsible for keeping track of the
	// nonces and partial signatures involved in the signing process.
	session input.MuSig2Session
}

// MuSig2CreateSession creates a new MuSig2 signing session using the local
// key identified by the key locator. The complete list of all public keys of
// all signing parties must be provided, including the public key of the local
// signing key. If nonces of other parties are already known, they can be
// submitted as well to reduce the number of method calls necessary later on.
func (b *BtcWallet) MuSig2CreateSession(bipVersion input.MuSig2Version,
	keyLoc keychain.KeyLocator, allSignerPubKeys []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks,
	otherSignerNonces [][musig2.PubNonceSize]byte) (*input.MuSig2SessionInfo,
	error) {

	// We need to derive the private key for signing. In the remote signing
	// setup, this whole RPC call will be forwarded to the signing
	// instance, which requires it to be stateful.
	privKey, err := b.fetchPrivKey(&keychain.KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, fmt.Errorf("error deriving private key: %w", err)
	}

	// Create a signing context and session with the given private key and
	// list of all known signer public keys.
	muSigContext, muSigSession, err := input.MuSig2CreateContext(
		bipVersion, privKey, allSignerPubKeys, tweaks,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating signing context: %w",
			err)
	}

	// Add all nonces we might've learned so far.
	haveAllNonces := false
	for _, otherSignerNonce := range otherSignerNonces {
		haveAllNonces, err = muSigSession.RegisterPubNonce(
			otherSignerNonce,
		)
		if err != nil {
			return nil, fmt.Errorf("error registering other "+
				"signer public nonce: %w", err)
		}
	}

	// Register the new session.
	combinedKey, err := muSigContext.CombinedKey()
	if err != nil {
		return nil, fmt.Errorf("error getting combined key: %w", err)
	}
	session := &muSig2State{
		MuSig2SessionInfo: input.MuSig2SessionInfo{
			SessionID: input.NewMuSig2SessionID(
				combinedKey, muSigSession.PublicNonce(),
			),
			Version:       bipVersion,
			PublicNonce:   muSigSession.PublicNonce(),
			CombinedKey:   combinedKey,
			TaprootTweak:  tweaks.HasTaprootTweak(),
			HaveAllNonces: haveAllNonces,
		},
		context: muSigContext,
		session: muSigSession,
	}

	// The internal key is only calculated if we are using a taproot tweak
	// and need to know it for a potential script spend.
	if tweaks.HasTaprootTweak() {
		internalKey, err := muSigContext.TaprootInternalKey()
		if err != nil {
			return nil, fmt.Errorf("error getting internal key: %w",
				err)
		}
		session.TaprootInternalKey = internalKey
	}

	// Since we generate new nonces for every session, there is no way that
	// a session with the same ID already exists. So even if we call the API
	// twice with the same signers, we still get a new ID.
	b.musig2SessionsMtx.Lock()
	b.musig2Sessions[session.SessionID] = session
	b.musig2SessionsMtx.Unlock()

	return &session.MuSig2SessionInfo, nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID. This method returns true
// once we have all nonces for all other signing participants.
func (b *BtcWallet) MuSig2RegisterNonces(sessionID input.MuSig2SessionID,
	otherSignerNonces [][musig2.PubNonceSize]byte) (bool, error) {

	// We hold the lock during the whole operation, we don't want any
	// interference with calls that might come through in parallel for the
	// same session.
	b.musig2SessionsMtx.Lock()
	defer b.musig2SessionsMtx.Unlock()

	session, ok := b.musig2Sessions[sessionID]
	if !ok {
		return false, fmt.Errorf("session with ID %x not found",
			sessionID[:])
	}

	// Make sure we don't exceed the number of expected nonces as that would
	// indicate something is wrong with the signing setup.
	if session.HaveAllNonces {
		return true, fmt.Errorf("already have all nonces")
	}

	numSigners := len(session.context.SigningKeys())
	remainingNonces := numSigners - session.session.NumRegisteredNonces()
	if len(otherSignerNonces) > remainingNonces {
		return false, fmt.Errorf("only %d other nonces remaining but "+
			"trying to register %d more", remainingNonces,
			len(otherSignerNonces))
	}

	// Add all nonces we've learned so far.
	var err error
	for _, otherSignerNonce := range otherSignerNonces {
		session.HaveAllNonces, err = session.session.RegisterPubNonce(
			otherSignerNonce,
		)
		if err != nil {
			return false, fmt.Errorf("error registering other "+
				"signer public nonce: %w", err)
		}
	}

	return session.HaveAllNonces, nil
}

// MuSig2Sign creates a partial signature using the local signing key
// that was specified when the session was created. This can only be
// called when all public nonces of all participants are known and have
// been registered with the session. If this node isn't responsible for
// combining all the partial signatures, then the cleanup parameter
// should be set, indicating that the session can be removed from memory
// once the signature was produced.
func (b *BtcWallet) MuSig2Sign(sessionID input.MuSig2SessionID,
	msg [sha256.Size]byte, cleanUp bool) (*musig2.PartialSignature, error) {

	// We hold the lock during the whole operation, we don't want any
	// interference with calls that might come through in parallel for the
	// same session.
	b.musig2SessionsMtx.Lock()
	defer b.musig2SessionsMtx.Unlock()

	session, ok := b.musig2Sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session with ID %x not found",
			sessionID[:])
	}

	// We can only sign once we have all other signer's nonces.
	if !session.HaveAllNonces {
		return nil, fmt.Errorf("only have %d of %d required nonces",
			session.session.NumRegisteredNonces(),
			len(session.context.SigningKeys()))
	}

	// Create our own partial signature with the local signing key.
	partialSig, err := input.MuSig2Sign(session.session, msg, true)
	if err != nil {
		return nil, fmt.Errorf("error signing with local key: %w", err)
	}

	// Clean up our local state if requested.
	if cleanUp {
		delete(b.musig2Sessions, sessionID)
	}

	return partialSig, nil
}

// MuSig2CombineSig combines the given partial signature(s) with the
// local one, if it already exists. Once a partial signature of all
// participants is registered, the final signature will be combined and
// returned.
func (b *BtcWallet) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	partialSigs []*musig2.PartialSignature) (*schnorr.Signature, bool,
	error) {

	// We hold the lock during the whole operation, we don't want any
	// interference with calls that might come through in parallel for the
	// same session.
	b.musig2SessionsMtx.Lock()
	defer b.musig2SessionsMtx.Unlock()

	session, ok := b.musig2Sessions[sessionID]
	if !ok {
		return nil, false, fmt.Errorf("session with ID %x not found",
			sessionID[:])
	}

	// Make sure we don't exceed the number of expected partial signatures
	// as that would indicate something is wrong with the signing setup.
	if session.HaveAllSigs {
		return nil, true, fmt.Errorf("already have all partial" +
			"signatures")
	}

	// Add all sigs we got so far.
	var (
		finalSig *schnorr.Signature
		err      error
	)
	for _, otherPartialSig := range partialSigs {
		session.HaveAllSigs, err = input.MuSig2CombineSig(
			session.session, otherPartialSig,
		)
		if err != nil {
			return nil, false, fmt.Errorf("error combining "+
				"partial signature: %w", err)
		}
	}

	// If we have all partial signatures, we should be able to get the
	// complete signature now. We also remove this session from memory since
	// there is nothing more left to do.
	if session.HaveAllSigs {
		finalSig = session.session.FinalSig()
		delete(b.musig2Sessions, sessionID)
	}

	return finalSig, session.HaveAllSigs, nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (b *BtcWallet) MuSig2Cleanup(sessionID input.MuSig2SessionID) error {
	// We hold the lock during the whole operation, we don't want any
	// interference with calls that might come through in parallel for the
	// same session.
	b.musig2SessionsMtx.Lock()
	defer b.musig2SessionsMtx.Unlock()

	_, ok := b.musig2Sessions[sessionID]
	if !ok {
		return fmt.Errorf("session with ID %x not found", sessionID[:])
	}

	delete(b.musig2Sessions, sessionID)

	return nil
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
