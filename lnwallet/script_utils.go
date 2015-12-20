package lnwallet

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// scriptHashPkScript...
func scriptHashPkScript(scriptBytes []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_HASH160)
	bldr.AddData(btcutil.Hash160(scriptBytes))
	bldr.AddOp(txscript.OP_EQUAL)
	return bldr.Script()
}

// getFundingPkScript generates the non-p2sh'd multisig script for 2 of 2
// pubkeys.
func genFundingPkScript(aPub, bPub []byte) ([]byte, error) {
	if len(aPub) != 33 || len(bPub) != 33 {
		return nil, fmt.Errorf("Pubkey size error. Compressed pubkeys only")
	}

	if bytes.Compare(aPub, bPub) == -1 { // swap to sort pubkeys if needed
		aPub, bPub = bPub, aPub
	}

	bldr := txscript.NewScriptBuilder()
	// Require 2 signatures, so from both of the pubkeys
	bldr.AddOp(txscript.OP_2)
	// add both pubkeys (sorted)
	bldr.AddData(aPub)
	bldr.AddData(bPub)
	// 2 keys total.  In case that wasn't obvious.
	bldr.AddOp(txscript.OP_2)
	// Good ol OP_CHECKMULTISIG.  Don't forget the zero!
	bldr.AddOp(txscript.OP_CHECKMULTISIG)
	// get byte slice
	return bldr.Script()
}

// fundMultiSigOut creates a TxOut for the funding transaction.
// Give it the two pubkeys and it'll give you the p2sh'd txout.
// You don't have to remember the p2sh preimage, as long as you remember the
// pubkeys involved.
func fundMultiSigOut(aPub, bPub []byte, amt int64) ([]byte, *wire.TxOut, error) {
	if amt < 0 {
		return nil, nil, fmt.Errorf("Can't create FundTx script with negative coins")
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

// the scriptsig to put on a P2SH input
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
