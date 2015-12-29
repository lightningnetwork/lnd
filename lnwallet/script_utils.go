package lnwallet

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// scriptHashPkScript generates a pay-to-script-hash public key script paying
// to the hash160 of the passed redeem script.
func scriptHashPkScript(redeemScript []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_HASH160)
	bldr.AddData(btcutil.Hash160(redeemScript))
	bldr.AddOp(txscript.OP_EQUAL)
	return bldr.Script()
}

// getFundingPkScript generates the non-p2sh'd multisig script for 2 of 2
// pubkeys.
func genFundingPkScript(aPub, bPub []byte) ([]byte, error) {
	if len(aPub) != 33 || len(bPub) != 33 {
		return nil, fmt.Errorf("Pubkey size error. Compressed pubkeys only")
	}

	// Swap to sort pubkeys if needed. Keys are sorted in lexicographical
	// order. The signatures within the scriptSig must also adhere to the
	// order, ensuring that the signatures for each public key appears
	// in the proper order on the stack.
	if bytes.Compare(aPub, bPub) == -1 {
		aPub, bPub = bPub, aPub
	}

	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_2)
	bldr.AddData(aPub) // Add both pubkeys (sorted).
	bldr.AddData(bPub)
	bldr.AddOp(txscript.OP_2)
	bldr.AddOp(txscript.OP_CHECKMULTISIG)
	return bldr.Script()
}

// fundMultiSigOut create the redeemScript for the funding transaction, and
// also a TxOut paying to the p2sh of the multi-sig redeemScript. Give it the
// two pubkeys and it'll give you the p2sh'd txout. You don't have to remember
// the p2sh preimage, as long as you remember the pubkeys involved.
func fundMultiSigOut(aPub, bPub []byte, amt int64) ([]byte, *wire.TxOut, error) {
	if amt < 0 {
		return nil, nil, fmt.Errorf("can't create FundTx script with " +
			"negative coins")
	}

	// p2shify
	redeemScript, err := genFundingPkScript(aPub, bPub)
	if err != nil {
		return nil, nil, err
	}
	pkScript, err := scriptHashPkScript(redeemScript)
	if err != nil {
		return nil, nil, err
	}

	return redeemScript, wire.NewTxOut(amt, pkScript), nil
}

// spendMultiSig generates the scriptSig required to redeem the 2-of-2 p2sh
// multi-sig output.
func spendMultiSig(redeemScript, sigA, sigB []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()

	// add a 0 for some multisig fun
	bldr.AddOp(txscript.OP_0)

	// add sigA
	bldr.AddData(sigA)
	// add sigB
	bldr.AddData(sigB)

	// preimage goes on AT THE ENDDDD
	bldr.AddData(redeemScript)

	// that's all, get bytes
	return bldr.Script()
}

// findScriptOutputIndex finds the index of the public key script output
// matching 'script'. Additionally, a boolean is returned indicating if
// a matching output was found at all.
// NOTE: The search stops after the first matching script is found.
func findScriptOutputIndex(tx *wire.MsgTx, script []byte) (bool, uint32) {
	found := false
	index := uint32(0)
	for i, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, script) {
			found = true
			index = uint32(i)
			break
		}
	}

	return found, index
}
