package btcwallet

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// PsbtKeyTypeInputSignatureTweakSingle is a custom/proprietary PSBT key
	// for an input that specifies what single tweak should be applied to
	// the key before signing the input. The value 51 is leet speak for
	// "si", short for "single".
	PsbtKeyTypeInputSignatureTweakSingle = []byte{0x51}

	// PsbtKeyTypeInputSignatureTweakDouble is a custom/proprietary PSBT key
	// for an input that specifies what double tweak should be applied to
	// the key before signing the input. The value d0 is leet speak for
	// "do", short for "double".
	PsbtKeyTypeInputSignatureTweakDouble = []byte{0xd0}

	// ErrInputMissingUTXOInfo is returned if a PSBT input is supplied that
	// does not specify the witness UTXO info.
	ErrInputMissingUTXOInfo = errors.New(
		"input doesn't specify any UTXO info",
	)

	// ErrScriptSpendFeeEstimationUnsupported is returned if a PSBT input is
	// of a script spend type.
	ErrScriptSpendFeeEstimationUnsupported = errors.New(
		"cannot estimate fee for script spend inputs",
	)

	// ErrUnsupportedScript is returned if a supplied pk script is not
	// known or supported.
	ErrUnsupportedScript = errors.New("unsupported or unknown pk script")
)

// FundPsbt creates a fully populated PSBT packet that contains enough inputs to
// fund the outputs specified in the passed in packet with the specified fee
// rate. If there is change left, a change output from the internal wallet is
// added and the index of the change output is returned. Otherwise no additional
// output is created and the index -1 is returned. If no custom change
// scope is specified, the BIP0084 will be used for default accounts and single
// imported public keys. For custom account, no key scope should be provided
// as the coin selection key scope will always be used to generate the change
// address.
// The function argument `allowUtxo` specifies a filter function for utxos
// during coin selection. It should return true for utxos that can be used and
// false for those that should be excluded.
//
// NOTE: If the packet doesn't contain any inputs, coin selection is performed
// automatically. The account parameter must be non-empty as it determines which
// set of coins are eligible for coin selection. If the packet does contain any
// inputs, it is assumed that full coin selection happened externally and no
// additional inputs are added. If the specified inputs aren't enough to fund
// the outputs with the given fee rate, an error is returned. No lock lease is
// acquired for any of the selected/validated inputs. It is in the caller's
// responsibility to lock the inputs before handing them out.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) FundPsbt(packet *psbt.Packet, minConfs int32,
	feeRate chainfee.SatPerKWeight, accountName string,
	changeScope *waddrmgr.KeyScope,
	strategy wallet.CoinSelectionStrategy,
	allowUtxo func(wtxmgr.Credit) bool) (int32, error) {

	// The fee rate is passed in using units of sat/kw, so we'll convert
	// this to sat/KB as the CreateSimpleTx method requires this unit.
	feeSatPerKB := btcutil.Amount(feeRate.FeePerKVByte())

	var (
		keyScope   *waddrmgr.KeyScope
		accountNum uint32
	)

	switch accountName {
	// For default accounts and single imported public keys, we'll provide a
	// nil key scope to FundPsbt, allowing it to select inputs from all
	// scopes (NP2WKH, P2WKH, P2TR). By default, the change key scope for
	// these accounts will be P2WKH.
	case lnwallet.DefaultAccountName:
		if changeScope == nil {
			changeScope = &waddrmgr.KeyScopeBIP0084
		}

		accountNum = defaultAccount

	case waddrmgr.ImportedAddrAccountName:
		if changeScope == nil {
			changeScope = &waddrmgr.KeyScopeBIP0084
		}

		accountNum = importedAccount

	// Otherwise, map the account name to its key scope and internal account
	// number to only select inputs from said account. No change key scope
	// should have been specified as a custom account should only have one
	// key scope. Providing a change key scope would break this assumption
	// and lead to non-deterministic behavior by using a different change
	// key scope than the custom account key scope. The change key scope
	// will always be the same as the coin selection.
	default:
		if changeScope != nil {
			return 0, fmt.Errorf("couldn't select a " +
				"custom change type for custom accounts")
		}

		scope, account, err := b.lookupFirstCustomAccount(accountName)
		if err != nil {
			return 0, err
		}
		keyScope = &scope
		changeScope = keyScope
		accountNum = account
	}

	var opts []wallet.TxCreateOption
	if changeScope != nil {
		opts = append(opts, wallet.WithCustomChangeScope(changeScope))
	}
	if allowUtxo != nil {
		opts = append(opts, wallet.WithUtxoFilter(allowUtxo))
	}

	// Let the wallet handle coin selection and/or fee estimation based on
	// the partial TX information in the packet.
	return b.wallet.FundPsbt(
		packet, keyScope, minConfs, accountNum, feeSatPerKB,
		strategy, opts...,
	)
}

// SignPsbt expects a partial transaction with all inputs and outputs fully
// declared and tries to sign all unsigned inputs that have all required fields
// (UTXO information, BIP32 derivation information, witness or sig scripts) set.
// If no error is returned, the PSBT is ready to be given to the next signer or
// to be finalized if lnd was the last signer.
//
// NOTE: This method only signs inputs (and only those it can sign), it does not
// perform any other tasks (such as coin selection, UTXO locking or
// input/output/fee value validation, PSBT finalization). Any input that is
// incomplete will be skipped.
func (b *BtcWallet) SignPsbt(packet *psbt.Packet) ([]uint32, error) {
	// In signedInputs we return the indices of psbt inputs that were signed
	// by our wallet. This way the caller can check if any inputs were signed.
	var signedInputs []uint32

	// Let's check that this is actually something we can and want to sign.
	// We need at least one input and one output. In addition each
	// input needs nonWitness Utxo or witness Utxo data specified.
	err := psbt.InputsReadyToSign(packet)
	if err != nil {
		return nil, err
	}

	// Go through each input that doesn't have final witness data attached
	// to it already and try to sign it. If there is nothing more to sign or
	// there are inputs that we don't know how to sign, we won't return any
	// error. So it's possible we're not the final signer.
	tx := packet.UnsignedTx
	prevOutputFetcher := wallet.PsbtPrevOutputFetcher(packet)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutputFetcher)
	for idx := range tx.TxIn {
		in := &packet.Inputs[idx]

		// We can only sign if we have UTXO information available. Since
		// we don't finalize, we just skip over any input that we know
		// we can't do anything with. Since we only support signing
		// witness inputs, we only look at the witness UTXO being set.
		if in.WitnessUtxo == nil {
			continue
		}

		// Skip this input if it's got final witness data attached.
		if len(in.FinalScriptWitness) > 0 {
			continue
		}

		// Skip this input if there is no BIP32 derivation info
		// available.
		if len(in.Bip32Derivation) == 0 {
			continue
		}

		// TODO(guggero): For multisig, we'll need to find out what key
		// to use and there should be multiple derivation paths in the
		// BIP32 derivation field.

		// Let's try and derive the key now. This method will decide if
		// it's a BIP49/84 key for normal on-chain funds or a key of the
		// custom purpose 1017 key scope.
		derivationInfo := in.Bip32Derivation[0]
		privKey, err := b.deriveKeyByBIP32Path(derivationInfo.Bip32Path)
		if err != nil {
			log.Warnf("SignPsbt: Skipping input %d, error "+
				"deriving signing key: %v", idx, err)
			continue
		}

		// We need to make sure we actually derived the key that was
		// expected to be derived.
		pubKeysEqual := bytes.Equal(
			derivationInfo.PubKey,
			privKey.PubKey().SerializeCompressed(),
		)
		if !pubKeysEqual {
			log.Warnf("SignPsbt: Skipping input %d, derived "+
				"public key %x does not match bip32 "+
				"derivation info public key %x", idx,
				privKey.PubKey().SerializeCompressed(),
				derivationInfo.PubKey)
			continue
		}

		// Do we need to tweak anything? Single or double tweaks are
		// sent as custom/proprietary fields in the PSBT input section.
		privKey = maybeTweakPrivKeyPsbt(in.Unknowns, privKey)

		// What kind of signature is expected from us and do we have all
		// information we need?
		signMethod, err := validateSigningMethod(in)
		if err != nil {
			return nil, err
		}

		switch signMethod {
		// For p2wkh, np2wkh and p2wsh.
		case input.WitnessV0SignMethod:
			err = signSegWitV0(in, tx, sigHashes, idx, privKey)

		// For p2tr BIP0086 key spend only.
		case input.TaprootKeySpendBIP0086SignMethod:
			rootHash := make([]byte, 0)
			err = signSegWitV1KeySpend(
				in, tx, sigHashes, idx, privKey, rootHash,
			)

		// For p2tr with script commitment key spend path.
		case input.TaprootKeySpendSignMethod:
			rootHash := in.TaprootMerkleRoot
			err = signSegWitV1KeySpend(
				in, tx, sigHashes, idx, privKey, rootHash,
			)

		// For p2tr script spend path.
		case input.TaprootScriptSpendSignMethod:
			leafScript := in.TaprootLeafScript[0]
			leaf := txscript.TapLeaf{
				LeafVersion: leafScript.LeafVersion,
				Script:      leafScript.Script,
			}
			err = signSegWitV1ScriptSpend(
				in, tx, sigHashes, idx, privKey, leaf,
			)

		default:
			err = fmt.Errorf("unsupported signing method for "+
				"PSBT signing: %v", signMethod)
		}
		if err != nil {
			return nil, err
		}
		signedInputs = append(signedInputs, uint32(idx))
	}
	return signedInputs, nil
}

// validateSigningMethod attempts to detect the signing method that is required
// to sign for the given PSBT input and makes sure all information is available
// to do so.
func validateSigningMethod(in *psbt.PInput) (input.SignMethod, error) {
	script, err := txscript.ParsePkScript(in.WitnessUtxo.PkScript)
	if err != nil {
		return 0, fmt.Errorf("error detecting signing method, "+
			"couldn't parse pkScript: %v", err)
	}

	switch script.Class() {
	case txscript.WitnessV0PubKeyHashTy, txscript.ScriptHashTy,
		txscript.WitnessV0ScriptHashTy:

		return input.WitnessV0SignMethod, nil

	case txscript.WitnessV1TaprootTy:
		if len(in.TaprootBip32Derivation) == 0 {
			return 0, fmt.Errorf("cannot sign for taproot input " +
				"without taproot BIP0032 derivation info")
		}

		// Currently, we only support creating one signature per input.
		//
		// TODO(guggero): Should we support signing multiple paths at
		// the same time? What are the performance and security
		// implications?
		if len(in.TaprootBip32Derivation) > 1 {
			return 0, fmt.Errorf("unsupported multiple taproot " +
				"BIP0032 derivation info found, can only " +
				"sign for one at a time")
		}

		derivation := in.TaprootBip32Derivation[0]
		switch {
		// No leaf hashes means this is the internal key we're signing
		// with, so it's a key spend. And no merkle root means this is
		// a BIP0086 output we're signing for.
		case len(derivation.LeafHashes) == 0 &&
			len(in.TaprootMerkleRoot) == 0:

			return input.TaprootKeySpendBIP0086SignMethod, nil

		// A non-empty merkle root means we committed to a taproot hash
		// that we need to use in the tap tweak.
		case len(derivation.LeafHashes) == 0:
			// Getting here means the merkle root isn't empty, but
			// is it exactly the length we need?
			if len(in.TaprootMerkleRoot) != sha256.Size {
				return 0, fmt.Errorf("invalid taproot merkle "+
					"root length, got %d expected %d",
					len(in.TaprootMerkleRoot), sha256.Size)
			}

			return input.TaprootKeySpendSignMethod, nil

		// Currently, we only support signing for one leaf at a time.
		//
		// TODO(guggero): Should we support signing multiple paths at
		// the same time? What are the performance and security
		// implications?
		case len(derivation.LeafHashes) == 1:
			// If we're supposed to be signing for a leaf hash, we
			// also expect the leaf script that hashes to that hash
			// in the appropriate field.
			if len(in.TaprootLeafScript) != 1 {
				return 0, fmt.Errorf("specified leaf hash in " +
					"taproot BIP0032 derivation but " +
					"missing taproot leaf script")
			}

			leafScript := in.TaprootLeafScript[0]
			leaf := txscript.TapLeaf{
				LeafVersion: leafScript.LeafVersion,
				Script:      leafScript.Script,
			}
			leafHash := leaf.TapHash()
			if !bytes.Equal(leafHash[:], derivation.LeafHashes[0]) {
				return 0, fmt.Errorf("specified leaf hash in" +
					"taproot BIP0032 derivation but " +
					"corresponding taproot leaf script " +
					"was not found")
			}

			return input.TaprootScriptSpendSignMethod, nil

		default:
			return 0, fmt.Errorf("unsupported number of leaf " +
				"hashes in taproot BIP0032 derivation info, " +
				"can only sign for one at a time")
		}

	default:
		return 0, fmt.Errorf("unsupported script class for signing "+
			"PSBT: %v", script.Class())
	}
}

// EstimateInputWeight estimates the weight of a PSBT input and adds it to the
// passed in TxWeightEstimator. It returns an error if the input type is
// unknown or unsupported. Only inputs that have a known witness size are
// supported, which is P2WKH, NP2WKH and P2TR (key spend path).
func EstimateInputWeight(in *psbt.PInput, w *input.TxWeightEstimator) error {
	if in.WitnessUtxo == nil {
		return ErrInputMissingUTXOInfo
	}

	pkScript := in.WitnessUtxo.PkScript
	switch {
	case txscript.IsPayToScriptHash(pkScript):
		w.AddNestedP2WKHInput()

	case txscript.IsPayToWitnessPubKeyHash(pkScript):
		w.AddP2WKHInput()

	case txscript.IsPayToWitnessScriptHash(pkScript):
		return fmt.Errorf("P2WSH inputs are not supported, cannot "+
			"estimate witness size for script spend: %w",
			ErrScriptSpendFeeEstimationUnsupported)

	case txscript.IsPayToTaproot(pkScript):
		signMethod, err := validateSigningMethod(in)
		if err != nil {
			return fmt.Errorf("error determining p2tr signing "+
				"method: %w", err)
		}

		switch signMethod {
		// For p2tr key spend paths.
		case input.TaprootKeySpendBIP0086SignMethod,
			input.TaprootKeySpendSignMethod:

			w.AddTaprootKeySpendInput(in.SighashType)

		// For p2tr script spend path.
		case input.TaprootScriptSpendSignMethod:
			return fmt.Errorf("P2TR inputs are not supported, "+
				"cannot estimate witness size for script "+
				"spend: %w",
				ErrScriptSpendFeeEstimationUnsupported)

		default:
			return fmt.Errorf("unsupported signing method for "+
				"PSBT signing: %v", signMethod)
		}

	default:
		return fmt.Errorf("unknown input type for script %x: %w",
			pkScript, ErrUnsupportedScript)
	}

	return nil
}

// SignSegWitV0 attempts to generate a signature for a SegWit version 0 input
// and stores it in the PartialSigs (and FinalScriptSig for np2wkh addresses)
// field.
func signSegWitV0(in *psbt.PInput, tx *wire.MsgTx,
	sigHashes *txscript.TxSigHashes, idx int,
	privKey *btcec.PrivateKey) error {

	pubKeyBytes := privKey.PubKey().SerializeCompressed()

	// Extract the correct witness and/or legacy scripts now, depending on
	// the type of input we sign. The txscript package has the peculiar
	// requirement that the PkScript of a P2PKH must be given as the witness
	// script in order for it to arrive at the correct sighash. That's why
	// we call it subScript here instead of witness script.
	subScript := prepareScriptsV0(in)

	// We have everything we need for signing the input now.
	sig, err := txscript.RawTxInWitnessSignature(
		tx, sigHashes, idx, in.WitnessUtxo.Value, subScript,
		in.SighashType, privKey,
	)
	if err != nil {
		return fmt.Errorf("error signing input %d: %w", idx, err)
	}
	in.PartialSigs = append(in.PartialSigs, &psbt.PartialSig{
		PubKey:    pubKeyBytes,
		Signature: sig,
	})

	return nil
}

// signSegWitV1KeySpend attempts to generate a signature for a SegWit version 1
// (p2tr) input and stores it in the TaprootKeySpendSig field.
func signSegWitV1KeySpend(in *psbt.PInput, tx *wire.MsgTx,
	sigHashes *txscript.TxSigHashes, idx int, privKey *btcec.PrivateKey,
	tapscriptRootHash []byte) error {

	rawSig, err := txscript.RawTxInTaprootSignature(
		tx, sigHashes, idx, in.WitnessUtxo.Value,
		in.WitnessUtxo.PkScript, tapscriptRootHash, in.SighashType,
		privKey,
	)
	if err != nil {
		return fmt.Errorf("error signing taproot input %d: %w", idx,
			err)
	}

	in.TaprootKeySpendSig = rawSig

	return nil
}

// signSegWitV1ScriptSpend attempts to generate a signature for a SegWit version
// 1 (p2tr) input and stores it in the TaprootScriptSpendSig field.
func signSegWitV1ScriptSpend(in *psbt.PInput, tx *wire.MsgTx,
	sigHashes *txscript.TxSigHashes, idx int, privKey *btcec.PrivateKey,
	leaf txscript.TapLeaf) error {

	rawSig, err := txscript.RawTxInTapscriptSignature(
		tx, sigHashes, idx, in.WitnessUtxo.Value,
		in.WitnessUtxo.PkScript, leaf, in.SighashType, privKey,
	)
	if err != nil {
		return fmt.Errorf("error signing taproot script input %d: %w",
			idx, err)
	}

	leafHash := leaf.TapHash()
	in.TaprootScriptSpendSig = append(
		in.TaprootScriptSpendSig, &psbt.TaprootScriptSpendSig{
			XOnlyPubKey: in.TaprootBip32Derivation[0].XOnlyPubKey,
			LeafHash:    leafHash[:],
			// We snip off the sighash flag from the end (if it was
			// specified in the first place.)
			Signature: rawSig[:schnorr.SignatureSize],
			SigHash:   in.SighashType,
		},
	)

	return nil
}

// prepareScriptsV0 returns the appropriate witness v0 and/or legacy scripts,
// depending on the type of input that should be signed.
func prepareScriptsV0(in *psbt.PInput) []byte {
	switch {
	// It's a NP2WKH input:
	case len(in.RedeemScript) > 0:
		return in.RedeemScript

	// It's a P2WSH input:
	case len(in.WitnessScript) > 0:
		return in.WitnessScript

	// It's a P2WKH input:
	default:
		return in.WitnessUtxo.PkScript
	}
}

// maybeTweakPrivKeyPsbt examines if there are any tweak parameters given in the
// custom/proprietary PSBT fields and may perform a mapping on the passed
// private key in order to utilize the tweaks, if populated.
func maybeTweakPrivKeyPsbt(unknowns []*psbt.Unknown,
	privKey *btcec.PrivateKey) *btcec.PrivateKey {

	// There can be other custom/unknown keys in a PSBT that we just ignore.
	// Key tweaking is optional and only one tweak (single _or_ double) can
	// ever be applied (at least for any use cases described in the BOLT
	// spec).
	for _, u := range unknowns {
		if bytes.Equal(u.Key, PsbtKeyTypeInputSignatureTweakSingle) {
			return input.TweakPrivKey(privKey, u.Value)
		}

		if bytes.Equal(u.Key, PsbtKeyTypeInputSignatureTweakDouble) {
			doubleTweakKey, _ := btcec.PrivKeyFromBytes(
				u.Value,
			)
			return input.DeriveRevocationPrivKey(
				privKey, doubleTweakKey,
			)
		}
	}

	return privKey
}

// FinalizePsbt expects a partial transaction with all inputs and outputs fully
// declared and tries to sign all inputs that belong to the specified account.
// Lnd must be the last signer of the transaction. That means, if there are any
// unsigned non-witness inputs or inputs without UTXO information attached or
// inputs without witness data that do not belong to lnd's wallet, this method
// will fail. If no error is returned, the PSBT is ready to be extracted and the
// final TX within to be broadcast.
//
// NOTE: This method does NOT publish the transaction after it's been
// finalized successfully.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) FinalizePsbt(packet *psbt.Packet, accountName string) error {
	var (
		keyScope   *waddrmgr.KeyScope
		accountNum uint32
	)
	switch accountName {
	// If the default/imported account name was specified, we'll provide a
	// nil key scope to FundPsbt, allowing it to sign inputs from both key
	// scopes (NP2WKH, P2WKH).
	case lnwallet.DefaultAccountName:
		accountNum = defaultAccount

	case waddrmgr.ImportedAddrAccountName:
		accountNum = importedAccount

	// Otherwise, map the account name to its key scope and internal account
	// number to determine if the inputs belonging to this account should be
	// signed.
	default:
		scope, account, err := b.lookupFirstCustomAccount(accountName)
		if err != nil {
			return err
		}
		keyScope = &scope
		accountNum = account
	}

	return b.wallet.FinalizePsbt(keyScope, accountNum, packet)
}

// DecorateInputs fetches the UTXO information of all inputs it can identify and
// adds the required information to the package's inputs. The failOnUnknown
// boolean controls whether the method should return an error if it cannot
// identify an input or if it should just skip it.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) DecorateInputs(packet *psbt.Packet,
	failOnUnknown bool) error {

	return b.wallet.DecorateInputs(packet, failOnUnknown)
}

// lookupFirstCustomAccount returns the first custom account found. In theory,
// there should be only one custom account for the given name. However, due to a
// lack of check, users could have created custom accounts with various key
// scopes. This behaviour has been fixed but, we still need to handle this
// specific case to avoid non-deterministic behaviour implied by LookupAccount.
func (b *BtcWallet) lookupFirstCustomAccount(
	name string) (waddrmgr.KeyScope, uint32, error) {

	var (
		account  *waddrmgr.AccountProperties
		keyScope waddrmgr.KeyScope
	)
	for _, scope := range waddrmgr.DefaultKeyScopes {
		var err error
		account, err = b.wallet.AccountPropertiesByName(scope, name)
		if waddrmgr.IsError(err, waddrmgr.ErrAccountNotFound) {
			continue
		}
		if err != nil {
			return keyScope, 0, err
		}

		keyScope = scope

		break
	}
	if account == nil {
		return waddrmgr.KeyScope{}, 0, newAccountNotFoundError(name)
	}

	return keyScope, account.AccountNumber, nil
}

// Bip32DerivationFromKeyDesc returns the default and Taproot BIP-0032 key
// derivation information from the given key descriptor information.
func Bip32DerivationFromKeyDesc(keyDesc keychain.KeyDescriptor,
	coinType uint32) (*psbt.Bip32Derivation, *psbt.TaprootBip32Derivation,
	string) {

	bip32Derivation := &psbt.Bip32Derivation{
		PubKey: keyDesc.PubKey.SerializeCompressed(),
		Bip32Path: []uint32{
			keychain.BIP0043Purpose + hdkeychain.HardenedKeyStart,
			coinType + hdkeychain.HardenedKeyStart,
			uint32(keyDesc.Family) +
				uint32(hdkeychain.HardenedKeyStart),
			0,
			keyDesc.Index,
		},
	}

	derivationPath := fmt.Sprintf(
		"m/%d'/%d'/%d'/%d/%d", keychain.BIP0043Purpose, coinType,
		keyDesc.Family, 0, keyDesc.Index,
	)

	return bip32Derivation, &psbt.TaprootBip32Derivation{
		XOnlyPubKey:          bip32Derivation.PubKey[1:],
		MasterKeyFingerprint: bip32Derivation.MasterKeyFingerprint,
		Bip32Path:            bip32Derivation.Bip32Path,
		LeafHashes:           make([][]byte, 0),
	}, derivationPath
}

// Bip32DerivationFromAddress returns the default and Taproot BIP-0032 key
// derivation information from the given managed address.
func Bip32DerivationFromAddress(
	addr waddrmgr.ManagedAddress) (*psbt.Bip32Derivation,
	*psbt.TaprootBip32Derivation, string, error) {

	pubKeyAddr, ok := addr.(waddrmgr.ManagedPubKeyAddress)
	if !ok {
		return nil, nil, "", fmt.Errorf("address is not a pubkey " +
			"address")
	}

	scope, derivationInfo, haveInfo := pubKeyAddr.DerivationInfo()
	if !haveInfo {
		return nil, nil, "", fmt.Errorf("address is an imported " +
			"public key, can't derive BIP32 path")
	}

	bip32Derivation := &psbt.Bip32Derivation{
		PubKey: pubKeyAddr.PubKey().SerializeCompressed(),
		Bip32Path: []uint32{
			scope.Purpose + hdkeychain.HardenedKeyStart,
			scope.Coin + hdkeychain.HardenedKeyStart,
			derivationInfo.InternalAccount +
				hdkeychain.HardenedKeyStart,
			derivationInfo.Branch,
			derivationInfo.Index,
		},
	}

	derivationPath := fmt.Sprintf(
		"m/%d'/%d'/%d'/%d/%d", scope.Purpose, scope.Coin,
		derivationInfo.InternalAccount, derivationInfo.Branch,
		derivationInfo.Index,
	)

	return bip32Derivation, &psbt.TaprootBip32Derivation{
		XOnlyPubKey:          bip32Derivation.PubKey[1:],
		MasterKeyFingerprint: bip32Derivation.MasterKeyFingerprint,
		Bip32Path:            bip32Derivation.Bip32Path,
		LeafHashes:           make([][]byte, 0),
	}, derivationPath, nil
}
