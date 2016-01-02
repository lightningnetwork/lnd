package lnwallet

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

var (
	// TODO(roasbeef): remove these and use the one's defined in txscript
	// within testnet-L.
	SequenceLockTimeSeconds      = uint32(1 << 22)
	SequenceLockTimeMask         = uint32(0x0000ffff)
	OP_CHECKSEQUENCEVERIFY  byte = txscript.OP_NOP3
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

// senderHTLCScript constructs the public key script for an outgoing HTLC
// output payment for the sender's commitment transaction.
func senderHTLCScript(absoluteTimeout, relativeTimeout uint32, senderKey,
	receiverKey *btcec.PublicKey, revokeHash, paymentHash []byte) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	// Was the pre-image to the payment hash presented?
	builder.AddOp(txscript.OP_HASH160)
	builder.AddOp(txscript.OP_DUP)
	builder.AddData(paymentHash)
	builder.AddOp(txscript.OP_EQUAL)

	// How about the pre-image for our commitment revocation hash?
	builder.AddOp(txscript.OP_SWAP)
	builder.AddData(revokeHash)
	builder.AddOp(txscript.OP_EQUAL)
	builder.AddOp(txscript.OP_SWAP)
	builder.AddOp(txscript.OP_ADD)

	// If either is present, then the receiver can claim immediately.
	builder.AddOp(txscript.OP_IF)
	builder.AddData(receiverKey.SerializeCompressed())

	// Otherwise, we (the sender) need to wait for an absolute HTLC
	// timeout, then afterwards a relative timeout before we claim re-claim
	// the unsettled funds. This delay gives the other party a chance to
	// present the pre-image to the revocation hash in the event that the
	// sender (at this time) broadcasts this commitment transaction after
	// it has been revoked.
	builder.AddOp(txscript.OP_ELSE)
	builder.AddInt64(int64(absoluteTimeout))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddInt64(int64(relativeTimeout))
	builder.AddOp(OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_2DROP)
	builder.AddData(senderKey.SerializeCompressed())
	builder.AddOp(txscript.OP_ENDIF)
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// senderHTLCScript constructs the public key script for an incoming HTLC
// output payment for the receiver's commitment transaction.
func receiverHTLCScript(absoluteTimeout, relativeTimeout uint32, senderKey,
	receiverKey *btcec.PublicKey, revokeHash, paymentHash []byte) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	// Was the pre-image to the payment hash presented?
	builder.AddOp(txscript.OP_HASH160)
	builder.AddOp(txscript.OP_DUP)
	builder.AddData(paymentHash)
	builder.AddOp(txscript.OP_EQUAL)
	builder.AddOp(txscript.OP_IF)

	// If so, let the receiver redeem after a relative timeout. This added
	// delay gives the sender (at this time) an opportunity to re-claim the
	// pending HTLC in the event that the receiver (at this time) broadcasts
	// this old commitment transaction after it has been revoked.
	builder.AddInt64(int64(relativeTimeout))
	builder.AddOp(OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_2DROP)
	builder.AddData(receiverKey.SerializeCompressed())

	// Otherwise, if the sender has the revocation pre-image to the
	// receiver's commitment transactions, then let them claim the
	// funds immediately.
	builder.AddOp(txscript.OP_ELSE)
	builder.AddData(revokeHash)
	builder.AddOp(txscript.OP_EQUAL)

	// If not, then the sender needs to wait for the HTLC timeout. This
	// clause may be executed if the receiver fails to present the r-value
	// in time. This prevents the pending funds from being locked up
	// indefinately.
	builder.AddOp(txscript.OP_NOTIF)
	builder.AddInt64(int64(absoluteTimeout))
	builder.AddOp(OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddOp(txscript.OP_ENDIF)

	builder.AddData(senderKey.SerializeCompressed())

	builder.AddOp(txscript.OP_ENDIF)
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// commitScriptToSelf constructs the public key script for the output on the
// commitment transaction paying to the "owner" of said commitment transaction.
// If the other party learns of the pre-image to the revocation hash, then they
// can claim all the settled funds in the channel, plus the unsettled funds.
func commitScriptToSelf(csvTimeout uint32, selfKey, theirKey *btcec.PublicKey, revokeHash []byte) ([]byte, error) {
	// This script is spendable under two conditions: either the 'csvTimeout'
	// has passed and we can redeem our funds, or they have the pre-image
	// to 'revokeHash'.
	builder := txscript.NewScriptBuilder()

	// If the pre-image for the revocation hash is presented, then allow a
	// spend provided the proper signature.
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(revokeHash)
	builder.AddOp(txscript.OP_EQUAL)
	builder.AddOp(txscript.OP_IF)
	builder.AddData(theirKey.SerializeCompressed())
	builder.AddOp(txscript.OP_ELSE)

	// Otherwise, we can re-claim our funds after a CSV delay of
	// 'csvTimeout' timeout blocks, and a valid signature.
	builder.AddInt64(int64(csvTimeout))
	builder.AddOp(OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddData(selfKey.SerializeCompressed())
	builder.AddOp(txscript.OP_ENDIF)
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// commitScriptUnencumbered constructs the public key script on the commitment
// transaction paying to the "other" party. This output is spendable
// immediately, requiring no contestation period.
func commitScriptUnencumbered(key *btcec.PublicKey) ([]byte, error) {
	// This script goes to the "other" party, and it spendable immediately.
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_DUP)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(btcutil.Hash160(key.SerializeCompressed()))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}
