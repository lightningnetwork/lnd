package input

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/big"

	"golang.org/x/crypto/ripemd160"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

var (
	// TODO(roasbeef): remove these and use the one's defined in txscript
	// within testnet-L.

	// SequenceLockTimeSeconds is the 22nd bit which indicates the lock
	// time is in seconds.
	SequenceLockTimeSeconds = uint32(1 << 22)
)

// WitnessScriptHash generates a pay-to-witness-script-hash public key script
// paying to a version 0 witness program paying to the passed redeem script.
func WitnessScriptHash(witnessScript []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()

	bldr.AddOp(txscript.OP_0)
	scriptHash := sha256.Sum256(witnessScript)
	bldr.AddData(scriptHash[:])
	return bldr.Script()
}

// GenMultiSigScript generates the non-p2sh'd multisig script for 2 of 2
// pubkeys.
func GenMultiSigScript(aPub, bPub []byte) ([]byte, error) {
	if len(aPub) != 33 || len(bPub) != 33 {
		return nil, fmt.Errorf("Pubkey size error. Compressed pubkeys only")
	}

	// Swap to sort pubkeys if needed. Keys are sorted in lexicographical
	// order. The signatures within the scriptSig must also adhere to the
	// order, ensuring that the signatures for each public key appears in
	// the proper order on the stack.
	if bytes.Compare(aPub, bPub) == 1 {
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

// GenFundingPkScript creates a redeem script, and its matching p2wsh
// output for the funding transaction.
func GenFundingPkScript(aPub, bPub []byte, amt int64) ([]byte, *wire.TxOut, error) {
	// As a sanity check, ensure that the passed amount is above zero.
	if amt <= 0 {
		return nil, nil, fmt.Errorf("can't create FundTx script with " +
			"zero, or negative coins")
	}

	// First, create the 2-of-2 multi-sig script itself.
	witnessScript, err := GenMultiSigScript(aPub, bPub)
	if err != nil {
		return nil, nil, err
	}

	// With the 2-of-2 script in had, generate a p2wsh script which pays
	// to the funding script.
	pkScript, err := WitnessScriptHash(witnessScript)
	if err != nil {
		return nil, nil, err
	}

	return witnessScript, wire.NewTxOut(amt, pkScript), nil
}

// SpendMultiSig generates the witness stack required to redeem the 2-of-2 p2wsh
// multi-sig output.
func SpendMultiSig(witnessScript, pubA, sigA, pubB, sigB []byte) [][]byte {
	witness := make([][]byte, 4)

	// When spending a p2wsh multi-sig script, rather than an OP_0, we add
	// a nil stack element to eat the extra pop.
	witness[0] = nil

	// When initially generating the witnessScript, we sorted the serialized
	// public keys in descending order. So we do a quick comparison in order
	// ensure the signatures appear on the Script Virtual Machine stack in
	// the correct order.
	if bytes.Compare(pubA, pubB) == 1 {
		witness[1] = sigB
		witness[2] = sigA
	} else {
		witness[1] = sigA
		witness[2] = sigB
	}

	// Finally, add the preimage as the last witness element.
	witness[3] = witnessScript

	return witness
}

// FindScriptOutputIndex finds the index of the public key script output
// matching 'script'. Additionally, a boolean is returned indicating if a
// matching output was found at all.
//
// NOTE: The search stops after the first matching script is found.
func FindScriptOutputIndex(tx *wire.MsgTx, script []byte) (bool, uint32) {
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

// Ripemd160H calculates the ripemd160 of the passed byte slice. This is used to
// calculate the intermediate hash for payment pre-images. Payment hashes are
// the result of ripemd160(sha256(paymentPreimage)). As a result, the value
// passed in should be the sha256 of the payment hash.
func Ripemd160H(d []byte) []byte {
	h := ripemd160.New()
	h.Write(d)
	return h.Sum(nil)
}

// SenderHTLCScript constructs the public key script for an outgoing HTLC
// output payment for the sender's version of the commitment transaction. The
// possible script paths from this output include:
//
//    * The sender timing out the HTLC using the second level HTLC timeout
//      transaction.
//    * The receiver of the HTLC claiming the output on-chain with the payment
//      preimage.
//    * The receiver of the HTLC sweeping all the funds in the case that a
//      revoked commitment transaction bearing this HTLC was broadcast.
//
// Possible Input Scripts:
//    SENDR: <0> <sendr sig>  <recvr sig> <0> (spend using HTLC timeout transaction)
//    RECVR: <recvr sig>  <preimage>
//    REVOK: <revoke sig> <revoke key>
//     * receiver revoke
//
// OP_DUP OP_HASH160 <revocation key hash160> OP_EQUAL
// OP_IF
//     OP_CHECKSIG
// OP_ELSE
//     <recv htlc key>
//     OP_SWAP OP_SIZE 32 OP_EQUAL
//     OP_NOTIF
//         OP_DROP 2 OP_SWAP <sender htlc key> 2 OP_CHECKMULTISIG
//     OP_ELSE
//         OP_HASH160 <ripemd160(payment hash)> OP_EQUALVERIFY
//         OP_CHECKSIG
//     OP_ENDIF
// OP_ENDIF
func SenderHTLCScript(senderHtlcKey, receiverHtlcKey,
	revocationKey *btcec.PublicKey, paymentHash []byte) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	// The opening operations are used to determine if this is the receiver
	// of the HTLC attempting to sweep all the funds due to a contract
	// breach. In this case, they'll place the revocation key at the top of
	// the stack.
	builder.AddOp(txscript.OP_DUP)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(btcutil.Hash160(revocationKey.SerializeCompressed()))
	builder.AddOp(txscript.OP_EQUAL)

	// If the hash matches, then this is the revocation clause. The output
	// can be spent if the check sig operation passes.
	builder.AddOp(txscript.OP_IF)
	builder.AddOp(txscript.OP_CHECKSIG)

	// Otherwise, this may either be the receiver of the HTLC claiming with
	// the pre-image, or the sender of the HTLC sweeping the output after
	// it has timed out.
	builder.AddOp(txscript.OP_ELSE)

	// We'll do a bit of set up by pushing the receiver's key on the top of
	// the stack. This will be needed later if we decide that this is the
	// sender activating the time out clause with the HTLC timeout
	// transaction.
	builder.AddData(receiverHtlcKey.SerializeCompressed())

	// Atm, the top item of the stack is the receiverKey's so we use a swap
	// to expose what is either the payment pre-image or a signature.
	builder.AddOp(txscript.OP_SWAP)

	// With the top item swapped, check if it's 32 bytes. If so, then this
	// *may* be the payment pre-image.
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUAL)

	// If it isn't then this might be the sender of the HTLC activating the
	// time out clause.
	builder.AddOp(txscript.OP_NOTIF)

	// We'll drop the OP_IF return value off the top of the stack so we can
	// reconstruct the multi-sig script used as an off-chain covenant. If
	// two valid signatures are provided, ten then output will be deemed as
	// spendable.
	builder.AddOp(txscript.OP_DROP)
	builder.AddOp(txscript.OP_2)
	builder.AddOp(txscript.OP_SWAP)
	builder.AddData(senderHtlcKey.SerializeCompressed())
	builder.AddOp(txscript.OP_2)
	builder.AddOp(txscript.OP_CHECKMULTISIG)

	// Otherwise, then the only other case is that this is the receiver of
	// the HTLC sweeping it on-chain with the payment pre-image.
	builder.AddOp(txscript.OP_ELSE)

	// Hash the top item of the stack and compare it with the hash160 of
	// the payment hash, which is already the sha256 of the payment
	// pre-image. By using this little trick we're able save space on-chain
	// as the witness includes a 20-byte hash rather than a 32-byte hash.
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(Ripemd160H(paymentHash))
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// This checks the receiver's signature so that a third party with
	// knowledge of the payment preimage still cannot steal the output.
	builder.AddOp(txscript.OP_CHECKSIG)

	// Close out the OP_IF statement above.
	builder.AddOp(txscript.OP_ENDIF)

	// Close out the OP_IF statement at the top of the script.
	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// SenderHtlcSpendRevokeWithKey constructs a valid witness allowing the receiver of an
// HTLC to claim the output with knowledge of the revocation private key in the
// scenario that the sender of the HTLC broadcasts a previously revoked
// commitment transaction. A valid spend requires knowledge of the private key
// that corresponds to their revocation base point and also the private key fro
// the per commitment point, and a valid signature under the combined public
// key.
func SenderHtlcSpendRevokeWithKey(signer Signer, signDesc *SignDescriptor,
	revokeKey *btcec.PublicKey, sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The stack required to sweep a revoke HTLC output consists simply of
	// the exact witness stack as one of a regular p2wkh spend. The only
	// difference is that the keys used were derived in an adversarial
	// manner in order to encode the revocation contract into a sig+key
	// pair.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[1] = revokeKey.SerializeCompressed()
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// SenderHtlcSpendRevoke constructs a valid witness allowing the receiver of an
// HTLC to claim the output with knowledge of the revocation private key in the
// scenario that the sender of the HTLC broadcasts a previously revoked
// commitment transaction.  This method first derives the appropriate revocation
// key, and requires that the provided SignDescriptor has a local revocation
// basepoint and commitment secret in the PubKey and DoubleTweak fields,
// respectively.
func SenderHtlcSpendRevoke(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	if signDesc.KeyDesc.PubKey == nil {
		return nil, fmt.Errorf("cannot generate witness with nil " +
			"KeyDesc pubkey")
	}

	// Derive the revocation key using the local revocation base point and
	// commitment point.
	revokeKey := DeriveRevocationPubkey(
		signDesc.KeyDesc.PubKey,
		signDesc.DoubleTweak.PubKey(),
	)

	return SenderHtlcSpendRevokeWithKey(signer, signDesc, revokeKey, sweepTx)
}

// SenderHtlcSpendRedeem constructs a valid witness allowing the receiver of an
// HTLC to redeem the pending output in the scenario that the sender broadcasts
// their version of the commitment transaction. A valid spend requires
// knowledge of the payment preimage, and a valid signature under the receivers
// public key.
func SenderHtlcSpendRedeem(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx, paymentPreimage []byte) (wire.TxWitness, error) {

	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The stack required to spend this output is simply the signature
	// generated above under the receiver's public key, and the payment
	// pre-image.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[1] = paymentPreimage
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// SenderHtlcSpendTimeout constructs a valid witness allowing the sender of an
// HTLC to activate the time locked covenant clause of a soon to be expired
// HTLC.  This script simply spends the multi-sig output using the
// pre-generated HTLC timeout transaction.
func SenderHtlcSpendTimeout(receiverSig []byte, signer Signer,
	signDesc *SignDescriptor, htlcTimeoutTx *wire.MsgTx) (wire.TxWitness, error) {

	sweepSig, err := signer.SignOutputRaw(htlcTimeoutTx, signDesc)
	if err != nil {
		return nil, err
	}

	// We place a zero as the first item of the evaluated witness stack in
	// order to force Script execution to the HTLC timeout clause. The
	// second zero is required to consume the extra pop due to a bug in the
	// original OP_CHECKMULTISIG.
	witnessStack := wire.TxWitness(make([][]byte, 5))
	witnessStack[0] = nil
	witnessStack[1] = append(receiverSig, byte(txscript.SigHashAll))
	witnessStack[2] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[3] = nil
	witnessStack[4] = signDesc.WitnessScript

	return witnessStack, nil
}

// ReceiverHTLCScript constructs the public key script for an incoming HTLC
// output payment for the receiver's version of the commitment transaction. The
// possible execution paths from this script include:
//   * The receiver of the HTLC uses its second level HTLC transaction to
//     advance the state of the HTLC into the delay+claim state.
//   * The sender of the HTLC sweeps all the funds of the HTLC as a breached
//     commitment was broadcast.
//   * The sender of the HTLC sweeps the HTLC on-chain after the timeout period
//     of the HTLC has passed.
//
// Possible Input Scripts:
//    RECVR: <0> <sender sig> <recvr sig> <preimage> (spend using HTLC success transaction)
//    REVOK: <sig> <key>
//    SENDR: <sig> 0
//
//
// OP_DUP OP_HASH160 <revocation key hash160> OP_EQUAL
// OP_IF
//     OP_CHECKSIG
// OP_ELSE
//     <sendr htlc key>
//     OP_SWAP OP_SIZE 32 OP_EQUAL
//     OP_IF
//         OP_HASH160 <ripemd160(payment hash)> OP_EQUALVERIFY
//         2 OP_SWAP <recvr htlc key> 2 OP_CHECKMULTISIG
//     OP_ELSE
//         OP_DROP <cltv expiry> OP_CHECKLOCKTIMEVERIFY OP_DROP
//         OP_CHECKSIG
//     OP_ENDIF
// OP_ENDIF
func ReceiverHTLCScript(cltvExpiry uint32, senderHtlcKey,
	receiverHtlcKey, revocationKey *btcec.PublicKey,
	paymentHash []byte) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	// The opening operations are used to determine if this is the sender
	// of the HTLC attempting to sweep all the funds due to a contract
	// breach. In this case, they'll place the revocation key at the top of
	// the stack.
	builder.AddOp(txscript.OP_DUP)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(btcutil.Hash160(revocationKey.SerializeCompressed()))
	builder.AddOp(txscript.OP_EQUAL)

	// If the hash matches, then this is the revocation clause. The output
	// can be spent if the check sig operation passes.
	builder.AddOp(txscript.OP_IF)
	builder.AddOp(txscript.OP_CHECKSIG)

	// Otherwise, this may either be the receiver of the HTLC starting the
	// claiming process via the second level HTLC success transaction and
	// the pre-image, or the sender of the HTLC sweeping the output after
	// it has timed out.
	builder.AddOp(txscript.OP_ELSE)

	// We'll do a bit of set up by pushing the sender's key on the top of
	// the stack. This will be needed later if we decide that this is the
	// receiver transitioning the output to the claim state using their
	// second-level HTLC success transaction.
	builder.AddData(senderHtlcKey.SerializeCompressed())

	// Atm, the top item of the stack is the sender's key so we use a swap
	// to expose what is either the payment pre-image or something else.
	builder.AddOp(txscript.OP_SWAP)

	// With the top item swapped, check if it's 32 bytes. If so, then this
	// *may* be the payment pre-image.
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUAL)

	// If the item on the top of the stack is 32-bytes, then it is the
	// proper size, so this indicates that the receiver of the HTLC is
	// attempting to claim the output on-chain by transitioning the state
	// of the HTLC to delay+claim.
	builder.AddOp(txscript.OP_IF)

	// Next we'll hash the item on the top of the stack, if it matches the
	// payment pre-image, then we'll continue. Otherwise, we'll end the
	// script here as this is the invalid payment pre-image.
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(Ripemd160H(paymentHash))
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// If the payment hash matches, then we'll also need to satisfy the
	// multi-sig covenant by providing both signatures of the sender and
	// receiver. If the convenient is met, then we'll allow the spending of
	// this output, but only by the HTLC success transaction.
	builder.AddOp(txscript.OP_2)
	builder.AddOp(txscript.OP_SWAP)
	builder.AddData(receiverHtlcKey.SerializeCompressed())
	builder.AddOp(txscript.OP_2)
	builder.AddOp(txscript.OP_CHECKMULTISIG)

	// Otherwise, this might be the sender of the HTLC attempting to sweep
	// it on-chain after the timeout.
	builder.AddOp(txscript.OP_ELSE)

	// We'll drop the extra item (which is the output from evaluating the
	// OP_EQUAL) above from the stack.
	builder.AddOp(txscript.OP_DROP)

	// With that item dropped off, we can now enforce the absolute
	// lock-time required to timeout the HTLC. If the time has passed, then
	// we'll proceed with a checksig to ensure that this is actually the
	// sender of he original HTLC.
	builder.AddInt64(int64(cltvExpiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddOp(txscript.OP_CHECKSIG)

	// Close out the inner if statement.
	builder.AddOp(txscript.OP_ENDIF)

	// Close out the outer if statement.
	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// ReceiverHtlcSpendRedeem constructs a valid witness allowing the receiver of
// an HTLC to redeem the conditional payment in the event that their commitment
// transaction is broadcast. This clause transitions the state of the HLTC
// output into the delay+claim state by activating the off-chain covenant bound
// by the 2-of-2 multi-sig output. The HTLC success timeout transaction being
// signed has a relative timelock delay enforced by its sequence number. This
// delay give the sender of the HTLC enough time to revoke the output if this
// is a breach commitment transaction.
func ReceiverHtlcSpendRedeem(senderSig, paymentPreimage []byte,
	signer Signer, signDesc *SignDescriptor,
	htlcSuccessTx *wire.MsgTx) (wire.TxWitness, error) {

	// First, we'll generate a signature for the HTLC success transaction.
	// The signDesc should be signing with the public key used as the
	// receiver's public key and also the correct single tweak.
	sweepSig, err := signer.SignOutputRaw(htlcSuccessTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The final witness stack is used the provide the script with the
	// payment pre-image, and also execute the multi-sig clause after the
	// pre-images matches. We add a nil item at the bottom of the stack in
	// order to consume the extra pop within OP_CHECKMULTISIG.
	witnessStack := wire.TxWitness(make([][]byte, 5))
	witnessStack[0] = nil
	witnessStack[1] = append(senderSig, byte(txscript.SigHashAll))
	witnessStack[2] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[3] = paymentPreimage
	witnessStack[4] = signDesc.WitnessScript

	return witnessStack, nil
}

// ReceiverHtlcSpendRevokeWithKey constructs a valid witness allowing the sender of an
// HTLC within a previously revoked commitment transaction to re-claim the
// pending funds in the case that the receiver broadcasts this revoked
// commitment transaction.
func ReceiverHtlcSpendRevokeWithKey(signer Signer, signDesc *SignDescriptor,
	revokeKey *btcec.PublicKey, sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	// First, we'll generate a signature for the sweep transaction.  The
	// signDesc should be signing with the public key used as the fully
	// derived revocation public key and also the correct double tweak
	// value.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// We place a zero, then one as the first items in the evaluated
	// witness stack in order to force script execution to the HTLC
	// revocation clause.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[1] = revokeKey.SerializeCompressed()
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// ReceiverHtlcSpendRevoke constructs a valid witness allowing the sender of an
// HTLC within a previously revoked commitment transaction to re-claim the
// pending funds in the case that the receiver broadcasts this revoked
// commitment transaction. This method first derives the appropriate revocation
// key, and requires that the provided SignDescriptor has a local revocation
// basepoint and commitment secret in the PubKey and DoubleTweak fields,
// respectively.
func ReceiverHtlcSpendRevoke(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	if signDesc.KeyDesc.PubKey == nil {
		return nil, fmt.Errorf("cannot generate witness with nil " +
			"KeyDesc pubkey")
	}

	// Derive the revocation key using the local revocation base point and
	// commitment point.
	revokeKey := DeriveRevocationPubkey(
		signDesc.KeyDesc.PubKey,
		signDesc.DoubleTweak.PubKey(),
	)

	return ReceiverHtlcSpendRevokeWithKey(signer, signDesc, revokeKey, sweepTx)
}

// ReceiverHtlcSpendTimeout constructs a valid witness allowing the sender of
// an HTLC to recover the pending funds after an absolute timeout in the
// scenario that the receiver of the HTLC broadcasts their version of the
// commitment transaction. If the caller has already set the lock time on the
// spending transaction, than a value of -1 can be passed for the cltvExpiry
// value.
//
// NOTE: The target input of the passed transaction MUST NOT have a final
// sequence number. Otherwise, the OP_CHECKLOCKTIMEVERIFY check will fail.
func ReceiverHtlcSpendTimeout(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx, cltvExpiry int32) (wire.TxWitness, error) {

	// If the caller set a proper timeout value, then we'll apply it
	// directly to the transaction.
	if cltvExpiry != -1 {
		// The HTLC output has an absolute time period before we are
		// permitted to recover the pending funds. Therefore we need to
		// set the locktime on this sweeping transaction in order to
		// pass Script verification.
		sweepTx.LockTime = uint32(cltvExpiry)
	}

	// With the lock time on the transaction set, we'll not generate a
	// signature for the sweep transaction. The passed sign descriptor
	// should be created using the raw public key of the sender (w/o the
	// single tweak applied), and the single tweak set to the proper value
	// taking into account the current state's point.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[1] = nil
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// SecondLevelHtlcScript is the uniform script that's used as the output for
// the second-level HTLC transactions. The second level transaction act as a
// sort of covenant, ensuring that a 2-of-2 multi-sig output can only be
// spent in a particular way, and to a particular output.
//
// Possible Input Scripts:
//  * To revoke an HTLC output that has been transitioned to the claim+delay
//    state:
//    * <revoke sig> 1
//
//  * To claim and HTLC output, either with a pre-image or due to a timeout:
//    * <delay sig> 0
//
// OP_IF
//     <revoke key>
// OP_ELSE
//     <delay in blocks>
//     OP_CHECKSEQUENCEVERIFY
//     OP_DROP
//     <delay key>
// OP_ENDIF
// OP_CHECKSIG
//
// TODO(roasbeef): possible renames for second-level
//  * transition?
//  * covenant output
func SecondLevelHtlcScript(revocationKey, delayKey *btcec.PublicKey,
	csvDelay uint32) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	// If this is the revocation clause for this script is to be executed,
	// the spender will push a 1, forcing us to hit the true clause of this
	// if statement.
	builder.AddOp(txscript.OP_IF)

	// If this this is the revocation case, then we'll push the revocation
	// public key on the stack.
	builder.AddData(revocationKey.SerializeCompressed())

	// Otherwise, this is either the sender or receiver of the HTLC
	// attempting to claim the HTLC output.
	builder.AddOp(txscript.OP_ELSE)

	// In order to give the other party time to execute the revocation
	// clause above, we require a relative timeout to pass before the
	// output can be spent.
	builder.AddInt64(int64(csvDelay))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	// If the relative timelock passes, then we'll add the delay key to the
	// stack to ensure that we properly authenticate the spending party.
	builder.AddData(delayKey.SerializeCompressed())

	// Close out the if statement.
	builder.AddOp(txscript.OP_ENDIF)

	// In either case, we'll ensure that only either the party possessing
	// the revocation private key, or the delay private key is able to
	// spend this output.
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// HtlcSpendSuccess spends a second-level HTLC output. This function is to be
// used by the sender of an HTLC to claim the output after a relative timeout
// or the receiver of the HTLC to claim on-chain with the pre-image.
func HtlcSpendSuccess(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx, csvDelay uint32) (wire.TxWitness, error) {

	// We're required to wait a relative period of time before we can sweep
	// the output in order to allow the other party to contest our claim of
	// validity to this version of the commitment transaction.
	sweepTx.TxIn[0].Sequence = LockTimeToSequence(false, csvDelay)

	// Finally, OP_CSV requires that the version of the transaction
	// spending a pkscript with OP_CSV within it *must* be >= 2.
	sweepTx.Version = 2

	// As we mutated the transaction, we'll re-calculate the sighashes for
	// this instance.
	signDesc.SigHashes = txscript.NewTxSigHashes(sweepTx)

	// With the proper sequence and version set, we'll now sign the timeout
	// transaction using the passed signed descriptor. In order to generate
	// a valid signature, then signDesc should be using the base delay
	// public key, and the proper single tweak bytes.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// We set a zero as the first element the witness stack (ignoring the
	// witness script), in order to force execution to the second portion
	// of the if clause.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[1] = nil
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// HtlcSpendRevoke spends a second-level HTLC output. This function is to be
// used by the sender or receiver of an HTLC to claim the HTLC after a revoked
// commitment transaction was broadcast.
func HtlcSpendRevoke(signer Signer, signDesc *SignDescriptor,
	revokeTx *wire.MsgTx) (wire.TxWitness, error) {

	// We don't need any spacial modifications to the transaction as this
	// is just sweeping a revoked HTLC output. So we'll generate a regular
	// witness signature.
	sweepSig, err := signer.SignOutputRaw(revokeTx, signDesc)
	if err != nil {
		return nil, err
	}

	// We set a one as the first element the witness stack (ignoring the
	// witness script), in order to force execution to the revocation
	// clause in the second level HTLC script.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[1] = []byte{1}
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// HtlcSecondLevelSpend exposes the public witness generation function for
// spending an HTLC success transaction, either due to an expiring time lock or
// having had the payment preimage. This method is able to spend any
// second-level HTLC transaction, assuming the caller sets the locktime or
// seqno properly.
//
// NOTE: The caller MUST set the txn version, sequence number, and sign
// descriptor's sig hash cache before invocation.
func HtlcSecondLevelSpend(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	// With the proper sequence and version set, we'll now sign the timeout
	// transaction using the passed signed descriptor. In order to generate
	// a valid signature, then signDesc should be using the base delay
	// public key, and the proper single tweak bytes.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// We set a zero as the first element the witness stack (ignoring the
	// witness script), in order to force execution to the second portion
	// of the if clause.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(txscript.SigHashAll))
	witnessStack[1] = nil
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// LockTimeToSequence converts the passed relative locktime to a sequence
// number in accordance to BIP-68.
// See: https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
//  * (Compatibility)
func LockTimeToSequence(isSeconds bool, locktime uint32) uint32 {
	if !isSeconds {
		// The locktime is to be expressed in confirmations.
		return locktime
	}

	// Set the 22nd bit which indicates the lock time is in seconds, then
	// shift the locktime over by 9 since the time granularity is in
	// 512-second intervals (2^9). This results in a max lock-time of
	// 33,554,431 seconds, or 1.06 years.
	return SequenceLockTimeSeconds | (locktime >> 9)
}

// CommitScriptToSelf constructs the public key script for the output on the
// commitment transaction paying to the "owner" of said commitment transaction.
// If the other party learns of the preimage to the revocation hash, then they
// can claim all the settled funds in the channel, plus the unsettled funds.
//
// Possible Input Scripts:
//     REVOKE:     <sig> 1
//     SENDRSWEEP: <sig> <emptyvector>
//
// Output Script:
//     OP_IF
//         <revokeKey>
//     OP_ELSE
//         <numRelativeBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP
//         <timeKey>
//     OP_ENDIF
//     OP_CHECKSIG
func CommitScriptToSelf(csvTimeout uint32, selfKey, revokeKey *btcec.PublicKey) ([]byte, error) {
	// This script is spendable under two conditions: either the
	// 'csvTimeout' has passed and we can redeem our funds, or they can
	// produce a valid signature with the revocation public key. The
	// revocation public key will *only* be known to the other party if we
	// have divulged the revocation hash, allowing them to homomorphically
	// derive the proper private key which corresponds to the revoke public
	// key.
	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_IF)

	// If a valid signature using the revocation key is presented, then
	// allow an immediate spend provided the proper signature.
	builder.AddData(revokeKey.SerializeCompressed())

	builder.AddOp(txscript.OP_ELSE)

	// Otherwise, we can re-claim our funds after a CSV delay of
	// 'csvTimeout' timeout blocks, and a valid signature.
	builder.AddInt64(int64(csvTimeout))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddData(selfKey.SerializeCompressed())

	builder.AddOp(txscript.OP_ENDIF)

	// Finally, we'll validate the signature against the public key that's
	// left on the top of the stack.
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// CommitScriptUnencumbered constructs the public key script on the commitment
// transaction paying to the "other" party. The constructed output is a normal
// p2wkh output spendable immediately, requiring no contestation period.
func CommitScriptUnencumbered(key *btcec.PublicKey) ([]byte, error) {
	// This script goes to the "other" party, and is spendable immediately.
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_0)
	builder.AddData(btcutil.Hash160(key.SerializeCompressed()))

	return builder.Script()
}

// CommitSpendTimeout constructs a valid witness allowing the owner of a
// particular commitment transaction to spend the output returning settled
// funds back to themselves after a relative block timeout.  In order to
// properly spend the transaction, the target input's sequence number should be
// set accordingly based off of the target relative block timeout within the
// redeem script.  Additionally, OP_CSV requires that the version of the
// transaction spending a pkscript with OP_CSV within it *must* be >= 2.
func CommitSpendTimeout(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	// Ensure the transaction version supports the validation of sequence
	// locks and CSV semantics.
	if sweepTx.Version < 2 {
		return nil, fmt.Errorf("version of passed transaction MUST "+
			"be >= 2, not %v", sweepTx.Version)
	}

	// With the sequence number in place, we're now able to properly sign
	// off on the sweep transaction.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// Place an empty byte as the first item in the evaluated witness stack
	// to force script execution to the timeout spend clause. We need to
	// place an empty byte in order to ensure our script is still valid
	// from the PoV of nodes that are enforcing minimal OP_IF/OP_NOTIF.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[1] = nil
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// CommitSpendRevoke constructs a valid witness allowing a node to sweep the
// settled output of a malicious counterparty who broadcasts a revoked
// commitment transaction.
//
// NOTE: The passed SignDescriptor should include the raw (untweaked)
// revocation base public key of the receiver and also the proper double tweak
// value based on the commitment secret of the revoked commitment.
func CommitSpendRevoke(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// Place a 1 as the first item in the evaluated witness stack to
	// force script execution to the revocation clause.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = append(sweepSig, byte(signDesc.HashType))
	witnessStack[1] = []byte{1}
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// CommitSpendNoDelay constructs a valid witness allowing a node to spend their
// settled no-delay output on the counterparty's commitment transaction. If the
// passed sign descriptor has a nil SingleTweak, then we'll omit the set where
// we tweak the pubkey with a random set of bytes, and use it directly in the
// witness stack.
//
// NOTE: The passed SignDescriptor should include the raw (untweaked) public
// key of the receiver and also the proper single tweak value based on the
// current commitment point.
func CommitSpendNoDelay(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	if signDesc.KeyDesc.PubKey == nil {
		return nil, fmt.Errorf("cannot generate witness with nil " +
			"KeyDesc pubkey")
	}

	// This is just a regular p2wkh spend which looks something like:
	//  * witness: <sig> <pubkey>
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// Finally, we'll manually craft the witness. The witness here is the
	// exact same as a regular p2wkh witness, depending on the value of the
	// tweakless bool.
	witness := make([][]byte, 2)
	witness[0] = append(sweepSig, byte(signDesc.HashType))

	switch {
	// If we're tweaking the key, then we use the tweaked public key as the
	// last item in the witness stack which was originally used to created
	// the pkScript we're spending.
	case signDesc.SingleTweak != nil:
		witness[1] = TweakPubKeyWithTweak(
			signDesc.KeyDesc.PubKey, signDesc.SingleTweak,
		).SerializeCompressed()

	// Otherwise, we can just use the raw pubkey, since there's no random
	// value to be combined.
	default:
		witness[1] = signDesc.KeyDesc.PubKey.SerializeCompressed()
	}

	return witness, nil
}

// SingleTweakBytes computes set of bytes we call the single tweak. The purpose
// of the single tweak is to randomize all regular delay and payment base
// points. To do this, we generate a hash that binds the commitment point to
// the pay/delay base point. The end end results is that the basePoint is
// tweaked as follows:
//
//   * key = basePoint + sha256(commitPoint || basePoint)*G
func SingleTweakBytes(commitPoint, basePoint *btcec.PublicKey) []byte {
	h := sha256.New()
	h.Write(commitPoint.SerializeCompressed())
	h.Write(basePoint.SerializeCompressed())
	return h.Sum(nil)
}

// TweakPubKey tweaks a public base point given a per commitment point. The per
// commitment point is a unique point on our target curve for each commitment
// transaction. When tweaking a local base point for use in a remote commitment
// transaction, the remote party's current per commitment point is to be used.
// The opposite applies for when tweaking remote keys. Precisely, the following
// operation is used to "tweak" public keys:
//
//   tweakPub := basePoint + sha256(commitPoint || basePoint) * G
//            := G*k + sha256(commitPoint || basePoint)*G
//            := G*(k + sha256(commitPoint || basePoint))
//
// Therefore, if a party possess the value k, the private key of the base
// point, then they are able to derive the proper private key for the
// revokeKey by computing:
//
//   revokePriv := k + sha256(commitPoint || basePoint) mod N
//
// Where N is the order of the sub-group.
//
// The rationale for tweaking all public keys used within the commitment
// contracts is to ensure that all keys are properly delinearized to avoid any
// funny business when jointly collaborating to compute public and private
// keys. Additionally, the use of the per commitment point ensures that each
// commitment state houses a unique set of keys which is useful when creating
// blinded channel outsourcing protocols.
//
// TODO(roasbeef): should be using double-scalar mult here
func TweakPubKey(basePoint, commitPoint *btcec.PublicKey) *btcec.PublicKey {
	tweakBytes := SingleTweakBytes(commitPoint, basePoint)
	return TweakPubKeyWithTweak(basePoint, tweakBytes)
}

// TweakPubKeyWithTweak is the exact same as the TweakPubKey function, however
// it accepts the raw tweak bytes directly rather than the commitment point.
func TweakPubKeyWithTweak(pubKey *btcec.PublicKey, tweakBytes []byte) *btcec.PublicKey {
	curve := btcec.S256()
	tweakX, tweakY := curve.ScalarBaseMult(tweakBytes)

	// TODO(roasbeef): check that both passed on curve?
	x, y := curve.Add(pubKey.X, pubKey.Y, tweakX, tweakY)
	return &btcec.PublicKey{
		X:     x,
		Y:     y,
		Curve: curve,
	}
}

// TweakPrivKey tweaks the private key of a public base point given a per
// commitment point. The per commitment secret is the revealed revocation
// secret for the commitment state in question. This private key will only need
// to be generated in the case that a channel counter party broadcasts a
// revoked state. Precisely, the following operation is used to derive a
// tweaked private key:
//
//  * tweakPriv := basePriv + sha256(commitment || basePub) mod N
//
// Where N is the order of the sub-group.
func TweakPrivKey(basePriv *btcec.PrivateKey, commitTweak []byte) *btcec.PrivateKey {
	// tweakInt := sha256(commitPoint || basePub)
	tweakInt := new(big.Int).SetBytes(commitTweak)

	tweakInt = tweakInt.Add(tweakInt, basePriv.D)
	tweakInt = tweakInt.Mod(tweakInt, btcec.S256().N)

	tweakPriv, _ := btcec.PrivKeyFromBytes(btcec.S256(), tweakInt.Bytes())
	return tweakPriv
}

// DeriveRevocationPubkey derives the revocation public key given the
// counterparty's commitment key, and revocation preimage derived via a
// pseudo-random-function. In the event that we (for some reason) broadcast a
// revoked commitment transaction, then if the other party knows the revocation
// preimage, then they'll be able to derive the corresponding private key to
// this private key by exploiting the homomorphism in the elliptic curve group:
//    * https://en.wikipedia.org/wiki/Group_homomorphism#Homomorphisms_of_abelian_groups
//
// The derivation is performed as follows:
//
//   revokeKey := revokeBase * sha256(revocationBase || commitPoint) +
//                commitPoint * sha256(commitPoint || revocationBase)
//
//             := G*(revokeBasePriv * sha256(revocationBase || commitPoint)) +
//                G*(commitSecret * sha256(commitPoint || revocationBase))
//
//             := G*(revokeBasePriv * sha256(revocationBase || commitPoint) +
//                   commitSecret * sha256(commitPoint || revocationBase))
//
// Therefore, once we divulge the revocation secret, the remote peer is able to
// compute the proper private key for the revokeKey by computing:
//
//   revokePriv := (revokeBasePriv * sha256(revocationBase || commitPoint)) +
//                 (commitSecret * sha256(commitPoint || revocationBase)) mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPubkey(revokeBase, commitPoint *btcec.PublicKey) *btcec.PublicKey {

	// R = revokeBase * sha256(revocationBase || commitPoint)
	revokeTweakBytes := SingleTweakBytes(revokeBase, commitPoint)
	rX, rY := btcec.S256().ScalarMult(revokeBase.X, revokeBase.Y,
		revokeTweakBytes)

	// C = commitPoint * sha256(commitPoint || revocationBase)
	commitTweakBytes := SingleTweakBytes(commitPoint, revokeBase)
	cX, cY := btcec.S256().ScalarMult(commitPoint.X, commitPoint.Y,
		commitTweakBytes)

	// Now that we have the revocation point, we add this to their commitment
	// public key in order to obtain the revocation public key.
	//
	// P = R + C
	revX, revY := btcec.S256().Add(rX, rY, cX, cY)
	return &btcec.PublicKey{
		X:     revX,
		Y:     revY,
		Curve: btcec.S256(),
	}
}

// DeriveRevocationPrivKey derives the revocation private key given a node's
// commitment private key, and the preimage to a previously seen revocation
// hash. Using this derived private key, a node is able to claim the output
// within the commitment transaction of a node in the case that they broadcast
// a previously revoked commitment transaction.
//
// The private key is derived as follows:
//   revokePriv := (revokeBasePriv * sha256(revocationBase || commitPoint)) +
//                 (commitSecret * sha256(commitPoint || revocationBase)) mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPrivKey(revokeBasePriv *btcec.PrivateKey,
	commitSecret *btcec.PrivateKey) *btcec.PrivateKey {

	// r = sha256(revokeBasePub || commitPoint)
	revokeTweakBytes := SingleTweakBytes(revokeBasePriv.PubKey(),
		commitSecret.PubKey())
	revokeTweakInt := new(big.Int).SetBytes(revokeTweakBytes)

	// c = sha256(commitPoint || revokeBasePub)
	commitTweakBytes := SingleTweakBytes(commitSecret.PubKey(),
		revokeBasePriv.PubKey())
	commitTweakInt := new(big.Int).SetBytes(commitTweakBytes)

	// Finally to derive the revocation secret key we'll perform the
	// following operation:
	//
	//  k = (revocationPriv * r) + (commitSecret * c) mod N
	//
	// This works since:
	//  P = (G*a)*b + (G*c)*d
	//  P = G*(a*b) + G*(c*d)
	//  P = G*(a*b + c*d)
	revokeHalfPriv := revokeTweakInt.Mul(revokeTweakInt, revokeBasePriv.D)
	commitHalfPriv := commitTweakInt.Mul(commitTweakInt, commitSecret.D)

	revocationPriv := revokeHalfPriv.Add(revokeHalfPriv, commitHalfPriv)
	revocationPriv = revocationPriv.Mod(revocationPriv, btcec.S256().N)

	priv, _ := btcec.PrivKeyFromBytes(btcec.S256(), revocationPriv.Bytes())
	return priv
}

// ComputeCommitmentPoint generates a commitment point given a commitment
// secret. The commitment point for each state is used to randomize each key in
// the key-ring and also to used as a tweak to derive new public+private keys
// for the state.
func ComputeCommitmentPoint(commitSecret []byte) *btcec.PublicKey {
	x, y := btcec.S256().ScalarBaseMult(commitSecret)

	return &btcec.PublicKey{
		X:     x,
		Y:     y,
		Curve: btcec.S256(),
	}
}
