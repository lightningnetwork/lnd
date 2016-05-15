package lnwallet

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/fastsha256"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// TODO(roasbeef): remove these and use the one's defined in txscript
	// within testnet-L.
	SequenceLockTimeSeconds      = uint32(1 << 22)
	SequenceLockTimeMask         = uint32(0x0000ffff)
	OP_CHECKSEQUENCEVERIFY  byte = txscript.OP_NOP3
)

// witnessScriptHash generates a pay-to-witness-script-hash public key script
// paying to a version 0 witness program paying to the passed redeem script.
func witnessScriptHash(redeemScript []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()

	bldr.AddOp(txscript.OP_0)
	scriptHash := fastsha256.Sum256(redeemScript)
	bldr.AddData(scriptHash[:])
	return bldr.Script()
}

// genMultiSigScript generates the non-p2sh'd multisig script for 2 of 2
// pubkeys.
func genMultiSigScript(aPub, bPub []byte) ([]byte, error) {
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

// genFundingPkScript creates a redeem script, and its matching p2wsh
// output for the funding transaction.
func genFundingPkScript(aPub, bPub []byte, amt int64) ([]byte, *wire.TxOut, error) {
	// As a sanity check, ensure that the passed amount is above zero.
	if amt <= 0 {
		return nil, nil, fmt.Errorf("can't create FundTx script with " +
			"zero, or negative coins")
	}

	// First, create the 2-of-2 multi-sig script itself.
	redeemScript, err := genMultiSigScript(aPub, bPub)
	if err != nil {
		return nil, nil, err
	}

	// With the 2-of-2 script in had, generate a p2wsh script which pays
	// to the funding script.
	pkScript, err := witnessScriptHash(redeemScript)
	if err != nil {
		return nil, nil, err
	}

	return redeemScript, wire.NewTxOut(amt, pkScript), nil
}

// spendMultiSig generates the witness stack required to redeem the 2-of-2 p2wsh
// multi-sig output.
func spendMultiSig(redeemScript, pubA, sigA, pubB, sigB []byte) [][]byte {
	witness := make([][]byte, 4)

	// When spending a p2wsh multi-sig script, rather than an OP_0, we add
	// a nil stack element to eat the extra pop.
	witness[0] = nil

	// When initially generating the redeemScript, we sorted the serialized
	// public keys in descending order. So we do a quick comparison in order
	// ensure the signatures appear on the Script Virual Machine stack in
	// the correct order.
	if bytes.Compare(pubA, pubB) == -1 {
		witness[1] = sigB
		witness[2] = sigA
	} else {
		witness[1] = sigA
		witness[2] = sigB
	}

	// Finally, add the pre-image as the last witness element.
	witness[3] = redeemScript

	return witness
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
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddOp(txscript.OP_ENDIF)

	builder.AddData(senderKey.SerializeCompressed())

	builder.AddOp(txscript.OP_ENDIF)
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// lockTimeToSequence converts the passed relative locktime to a sequence
// number in accordance to BIP-68.
// See: https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
//  * (Compatibility)
func lockTimeToSequence(isSeconds bool, locktime uint32) uint32 {
	if !isSeconds {
		// The locktime is to be expressed in confirmations. Apply the
		// mask to restrict the number of confirmations to 65,535 or
		// 1.25 years.
		return SequenceLockTimeMask & locktime
	}

	// Set the 22nd bit which indicates the lock time is in seconds, then
	// shift the locktime over by 9 since the time granularity is in
	// 512-second intervals (2^9). This results in a max lock-time of
	// 33,554,431 seconds, or 1.06 years.
	return SequenceLockTimeSeconds | (locktime >> 9)
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
