package btcwallet

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/waddrmgr"
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
	// If so we can return directly saving usk a disk access.
	b.cacheMtx.RLock()
	if output, ok := b.utxoCache[*prevOut]; ok {
		b.cacheMtx.RUnlock()
		return output, nil
	}
	b.cacheMtx.RUnlock()

	// Otherwse, we manually look up the output within the tx store.
	txDetail, err := b.wallet.TxStore.TxDetails(&prevOut.Hash)
	if err != nil {
		return nil, err
	} else if txDetail == nil {
		return nil, lnwallet.ErrNotMine
	}

	output = txDetail.TxRecord.MsgTx.TxOut[prevOut.Index]

	b.cacheMtx.Lock()
	b.utxoCache[*prevOut] = output
	b.cacheMtx.Unlock()

	return output, nil
}

// fetchOutputKey attempts to fetch the managed address corresponding to the
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
		wAddr, err := b.wallet.Manager.Address(addr)
		if err == nil {
			return wAddr, nil
		}
	}

	// TODO(roasbeef): use the errors.wrap package
	return nil, fmt.Errorf("address not found")
}

// fetchPrivKey attempts to retrieve the raw private key coresponding to the
// passed public key.
// TODO(roasbeef): alternatively can extract all the data pushes within the
// script, then attempt to match keys one by one
func (b *BtcWallet) fetchPrivKey(pub *btcec.PublicKey) (*btcec.PrivateKey, error) {
	hash160 := btcutil.Hash160(pub.SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(hash160, b.netParams)
	if err != nil {
		return nil, err
	}

	walletddr, err := b.wallet.Manager.Address(addr)
	if err != nil {
		return nil, err
	}

	return walletddr.(waddrmgr.ManagedPubKeyAddress).PrivKey()
}

// SignOutputRaw generates a signature for the passed transaction according to
// the data within the passed SignDescriptor.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SignOutputRaw(tx *wire.MsgTx, signDesc *lnwallet.SignDescriptor) ([]byte, error) {
	redeemScript := signDesc.RedeemScript

	privKey, err := b.fetchPrivKey(signDesc.PubKey)
	if err != nil {
		return nil, err
	}

	amt := signDesc.Output.Value
	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes, 0,
		amt, redeemScript, txscript.SigHashAll, privKey)
	if err != nil {
		return nil, err
	}

	// Chop off the sighash flag at the end of the signature.
	return sig[:len(sig)-1], nil
}

// ComputeInputScript generates a complete InputIndex for the passed
// transaction with the signature as defined within the passed SignDescriptor.
// This method is capable of generating the proper input script for both
// regular p2wkh output and p2wkh outputs nested within a regular p2sh output.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ComputeInputScript(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {

	outputScript := signDesc.Output.PkScript
	walletAddr, err := b.fetchOutputAddr(outputScript)
	if err != nil {
		return nil, nil
	}

	pka := walletAddr.(waddrmgr.ManagedPubKeyAddress)
	privKey, err := pka.PrivKey()
	if err != nil {
		return nil, err
	}

	var witnessProgram []byte
	inputScript := &lnwallet.InputScript{}

	// If we're spending p2wkh output nested within a p2sh output, then
	// we'll need to attach a sigScript in addition to witness data.
	switch {
	case pka.IsNestedWitness():
		pubKey := privKey.PubKey()
		pubKeyHash := btcutil.Hash160(pubKey.SerializeCompressed())

		// Next, we'll generate a valid sigScript that'll allow us to
		// spend the p2sh output. The sigScript will contain only a
		// single push of the p2wkh witness program corresponding to
		// the matching public key of this address.
		p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash,
			b.netParams)
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

		inputScript.ScriptSig = sigScript
	// Otherwise, this is a regular p2wkh output, so we include the
	// witness program itself as the subscript to generate the proper
	// sighash digest. As part of the new sighash digest algorithm, the
	// p2wkh witness program will be expanded into a regular p2kh
	// script.
	default:
		witnessProgram = outputScript
	}

	// Generate a valid witness stack for the input.
	// TODO(roasbeef): adhere to passed HashType
	witnessScript, err := txscript.WitnessScript(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, witnessProgram,
		txscript.SigHashAll, privKey, true)
	if err != nil {
		return nil, err
	}

	inputScript.Witness = witnessScript

	return inputScript, nil
}
