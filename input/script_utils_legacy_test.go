package input

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
)

// legacyWitnessScriptHash generates a pay-to-witness-script-hash public key
// script paying to a version 0 witness program paying to the passed redeem
// script.
func legacyWitnessScriptHash(witnessScript []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder(
		txscript.WithScriptAllocSize(P2WSHSize),
	)

	bldr.AddOp(txscript.OP_0)
	scriptHash := sha256.Sum256(witnessScript)
	bldr.AddData(scriptHash[:])
	return bldr.Script()
}

// legacyWitnessPubKeyHash generates a pay-to-witness-pubkey-hash public key
// script paying to a version 0 witness program containing the passed
// serialized public key.
func legacyWitnessPubKeyHash(pubkey []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder(
		txscript.WithScriptAllocSize(P2WPKHSize),
	)

	bldr.AddOp(txscript.OP_0)
	pkhash := btcutil.Hash160(pubkey)
	bldr.AddData(pkhash)
	return bldr.Script()
}

// legacyGenerateP2SH generates a pay-to-script-hash public key script paying
// to the passed redeem script.
func legacyGenerateP2SH(script []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder(
		txscript.WithScriptAllocSize(NestedP2WPKHSize),
	)

	bldr.AddOp(txscript.OP_HASH160)
	scripthash := btcutil.Hash160(script)
	bldr.AddData(scripthash)
	bldr.AddOp(txscript.OP_EQUAL)
	return bldr.Script()
}

// legacyGenerateP2PKH generates a pay-to-public-key-hash public key script
// paying to the passed serialized public key.
func legacyGenerateP2PKH(pubkey []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder(
		txscript.WithScriptAllocSize(P2PKHSize),
	)

	bldr.AddOp(txscript.OP_DUP)
	bldr.AddOp(txscript.OP_HASH160)
	pkhash := btcutil.Hash160(pubkey)
	bldr.AddData(pkhash)
	bldr.AddOp(txscript.OP_EQUALVERIFY)
	bldr.AddOp(txscript.OP_CHECKSIG)
	return bldr.Script()
}

// legacyGenMultiSigScript generates the non-p2sh'd multisig script for 2 of 2
// pubkeys.
func legacyGenMultiSigScript(aPub, bPub []byte) ([]byte, error) {
	if len(aPub) != 33 || len(bPub) != 33 {
		return nil, fmt.Errorf("pubkey size error: compressed " +
			"pubkeys only")
	}

	// Swap to sort pubkeys if needed. Keys are sorted in lexicographical
	// order. The signatures within the scriptSig must also adhere to the
	// order, ensuring that the signatures for each public key appears in
	// the proper order on the stack.
	if bytes.Compare(aPub, bPub) == 1 {
		aPub, bPub = bPub, aPub
	}

	bldr := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		MultiSigSize,
	))
	bldr.AddOp(txscript.OP_2)
	bldr.AddData(aPub) // Add both pubkeys (sorted).
	bldr.AddData(bPub)
	bldr.AddOp(txscript.OP_2)
	bldr.AddOp(txscript.OP_CHECKMULTISIG)
	return bldr.Script()
}

// legacySenderHTLCScript constructs the public key script for an outgoing HTLC
// output payment for the sender's version of the commitment transaction.
func legacySenderHTLCScript(senderHtlcKey, receiverHtlcKey,
	revocationKey *btcec.PublicKey, paymentHash []byte,
	confirmedSpend bool) ([]byte, error) {

	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		OfferedHtlcScriptSizeConfirmed,
	))

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
	// two valid signatures are provided, then the output will be deemed as
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
	// pre-image. By using this little trick we're able to save space
	// on-chain as the witness includes a 20-byte hash rather than a
	// 32-byte hash.
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(Ripemd160H(paymentHash))
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// This checks the receiver's signature so that a third party with
	// knowledge of the payment preimage still cannot steal the output.
	builder.AddOp(txscript.OP_CHECKSIG)

	// Close out the OP_IF statement above.
	builder.AddOp(txscript.OP_ENDIF)

	// Add 1 block CSV delay if a confirmation is required for the
	// non-revocation clauses.
	if confirmedSpend {
		builder.AddOp(txscript.OP_1)
		builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
		builder.AddOp(txscript.OP_DROP)
	}

	// Close out the OP_IF statement at the top of the script.
	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// legacyReceiverHTLCScript constructs the public key script for an incoming
// HTLC output payment for the receiver's version of the commitment
// transaction.
func legacyReceiverHTLCScript(cltvExpiry uint32, senderHtlcKey,
	receiverHtlcKey, revocationKey *btcec.PublicKey,
	paymentHash []byte, confirmedSpend bool) ([]byte, error) {

	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		AcceptedHtlcScriptSizeConfirmed,
	))

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

	// Add 1 block CSV delay for non-revocation clauses if confirmation is
	// required.
	if confirmedSpend {
		builder.AddOp(txscript.OP_1)
		builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
		builder.AddOp(txscript.OP_DROP)
	}

	// Close out the outer if statement.
	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// legacySecondLevelHtlcScript is the uniform script that's used as the output
// for the second-level HTLC transactions.
func legacySecondLevelHtlcScript(revocationKey, delayKey *btcec.PublicKey,
	csvDelay uint32) ([]byte, error) {

	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		ToLocalScriptSize,
	))

	// If this is the revocation clause for this script is to be executed,
	// the spender will push a 1, forcing us to hit the true clause of this
	// if statement.
	builder.AddOp(txscript.OP_IF)

	// If this is the revocation case, then we'll push the revocation
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

// legacyCommitScriptToSelf constructs the public key script for the output on
// the commitment transaction paying to the "owner" of said commitment
// transaction.
func legacyCommitScriptToSelf(csvTimeout uint32, selfKey, revokeKey *btcec.PublicKey) ([]byte, error) {
	// This script is spendable under two conditions: either the
	// 'csvTimeout' has passed and we can redeem our funds, or they can
	// produce a valid signature with the revocation public key. The
	// revocation public key will *only* be known to the other party if we
	// have divulged the revocation hash, allowing them to homomorphically
	// derive the proper private key which corresponds to the revoke public
	// key.
	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		ToLocalScriptSize,
	))

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

// legacyLeaseCommitScriptToSelf constructs the public key script for the
// output on the commitment transaction paying to the "owner" of said
// commitment transaction, with an additional lease expiry constraint.
func legacyLeaseCommitScriptToSelf(selfKey, revokeKey *btcec.PublicKey,
	csvTimeout, leaseExpiry uint32) ([]byte, error) {

	// This script is spendable under two conditions: either the
	// 'csvTimeout' has passed and we can redeem our funds, or they can
	// produce a valid signature with the revocation public key. The
	// revocation public key will *only* be known to the other party if we
	// have divulged the revocation hash, allowing them to homomorphically
	// derive the proper private key which corresponds to the revoke public
	// key.
	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		ToLocalScriptSize + LeaseWitnessScriptSizeOverhead,
	))

	builder.AddOp(txscript.OP_IF)

	// If a valid signature using the revocation key is presented, then
	// allow an immediate spend provided the proper signature.
	builder.AddData(revokeKey.SerializeCompressed())

	builder.AddOp(txscript.OP_ELSE)

	// Otherwise, we can re-claim our funds after once the CLTV lease
	// maturity has been met, along with the CSV delay of 'csvTimeout'
	// timeout blocks, and a valid signature.
	builder.AddInt64(int64(leaseExpiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)

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

// legacyCommitScriptUnencumbered constructs the public key script on the
// commitment transaction paying to the "other" party. The constructed output
// is a normal p2wkh output spendable immediately, requiring no contestation
// period.
func legacyCommitScriptUnencumbered(key *btcec.PublicKey) ([]byte, error) {
	// This script goes to the "other" party, and is spendable immediately.
	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		P2WPKHSize,
	))
	builder.AddOp(txscript.OP_0)
	builder.AddData(btcutil.Hash160(key.SerializeCompressed()))

	return builder.Script()
}

// legacyCommitScriptToRemoteConfirmed constructs the script for the output on
// the commitment transaction paying to the remote party of said commitment
// transaction. The money can only be spend after one confirmation.
func legacyCommitScriptToRemoteConfirmed(key *btcec.PublicKey) ([]byte, error) {
	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		ToRemoteConfirmedScriptSize,
	))

	// Only the given key can spend the output.
	builder.AddData(key.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	// Check that the it has one confirmation.
	builder.AddOp(txscript.OP_1)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	return builder.Script()
}

// legacyLeaseCommitScriptToRemoteConfirmed constructs the script for the
// output on the commitment transaction paying to the remote party of said
// commitment transaction, with an additional lease expiry constraint.
func legacyLeaseCommitScriptToRemoteConfirmed(key *btcec.PublicKey,
	leaseExpiry uint32) ([]byte, error) {

	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(45))

	// Only the given key can spend the output.
	builder.AddData(key.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	// The channel initiator always has the additional channel lease
	// expiration constraint for outputs that pay to them which must be
	// satisfied.
	builder.AddInt64(int64(leaseExpiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	// Check that it has one confirmation.
	builder.AddOp(txscript.OP_1)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	return builder.Script()
}

// legacyCommitScriptAnchor constructs the script for the anchor output
// spendable by the given key immediately, or by anyone after 16 confirmations.
func legacyCommitScriptAnchor(key *btcec.PublicKey) ([]byte, error) {
	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		AnchorScriptSize,
	))

	// Spend immediately with key.
	builder.AddData(key.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIG)

	// Duplicate the value if true, since it will be consumed by the NOTIF.
	builder.AddOp(txscript.OP_IFDUP)

	// Otherwise spendable by anyone after 16 confirmations.
	builder.AddOp(txscript.OP_NOTIF)
	builder.AddOp(txscript.OP_16)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// legacyLeaseSecondLevelHtlcScript is the uniform script that's used as the
// output for the second-level HTLC transactions with a lease expiry
// constraint.
func legacyLeaseSecondLevelHtlcScript(revocationKey, delayKey *btcec.PublicKey,
	csvDelay, cltvExpiry uint32) ([]byte, error) {

	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		ToLocalScriptSize + LeaseWitnessScriptSizeOverhead,
	))

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

	// The channel initiator always has the additional channel lease
	// expiration constraint for outputs that pay to them which must be
	// satisfied.
	builder.AddInt64(int64(cltvExpiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)

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

// legacySenderHTLCTapLeafTimeout returns the full tapscript leaf for the
// timeout path of the sender HTLC.
func legacySenderHTLCTapLeafTimeout(senderHtlcKey,
	receiverHtlcKey *btcec.PublicKey) (txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	builder.AddData(schnorr.SerializePubKey(senderHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddData(schnorr.SerializePubKey(receiverHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	timeoutLeafScript, err := builder.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(timeoutLeafScript), nil
}

// legacySenderHTLCTapLeafSuccess returns the full tapscript leaf for the
// success path of the sender HTLC.
func legacySenderHTLCTapLeafSuccess(receiverHtlcKey *btcec.PublicKey,
	paymentHash []byte) (txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	// Check that the pre-image is 32 bytes as required.
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// Check that the specified pre-image matches what we hard code into
	// the script.
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(Ripemd160H(paymentHash))
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// Verify the remote party's signature, then make them wait 1 block
	// after confirmation to properly sweep.
	builder.AddData(schnorr.SerializePubKey(receiverHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	builder.AddOp(txscript.OP_1)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	successLeafScript, err := builder.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(successLeafScript), nil
}

// legacyReceiverHtlcTapLeafTimeout returns the full tapscript leaf for the
// timeout path of the receiver HTLC.
func legacyReceiverHtlcTapLeafTimeout(senderHtlcKey *btcec.PublicKey,
	cltvExpiry uint32) (txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	// The first part of the script will verify a signature from the
	// sender authorizing the spend (the timeout).
	builder.AddData(schnorr.SerializePubKey(senderHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	builder.AddOp(txscript.OP_1)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	// The second portion will ensure that the CLTV expiry on the spending
	// transaction is correct.
	builder.AddInt64(int64(cltvExpiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	timeoutLeafScript, err := builder.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(timeoutLeafScript), nil
}

// legacyReceiverHtlcTapLeafSuccess returns the full tapscript leaf for the
// success path for an HTLC on the receiver's commitment transaction.
func legacyReceiverHtlcTapLeafSuccess(receiverHtlcKey *btcec.PublicKey,
	senderHtlcKey *btcec.PublicKey,
	paymentHash []byte) (txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	// Check that the pre-image is 32 bytes as required.
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// Check that the specified pre-image matches what we hard code into
	// the script.
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(Ripemd160H(paymentHash))
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// Verify the "2-of-2" multi-sig that requires both parties to sign
	// off.
	builder.AddData(schnorr.SerializePubKey(receiverHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddData(schnorr.SerializePubKey(senderHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	successLeafScript, err := builder.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(successLeafScript), nil
}

// legacyTaprootSecondLevelTapLeaf constructs the tap leaf used as the sole
// script path for a second level HTLC spend.
func legacyTaprootSecondLevelTapLeaf(delayKey *btcec.PublicKey,
	csvDelay uint32) (txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	// Ensure the proper party can sign for this output.
	builder.AddData(schnorr.SerializePubKey(delayKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	// Assuming the above passes, then we'll now ensure that the CSV delay
	// has been upheld, dropping the int we pushed on. If the sig above is
	// valid, then a 1 will be left on the stack.
	builder.AddInt64(int64(csvDelay))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	secondLevelLeafScript, err := builder.Script()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(secondLevelLeafScript), nil
}

// legacyTaprootLocalCommitDelayScript builds the tap leaf with the CSV delay
// script for the to-local output.
func legacyTaprootLocalCommitDelayScript(csvTimeout uint32,
	selfKey *btcec.PublicKey) ([]byte, error) {

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(selfKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	builder.AddInt64(int64(csvTimeout))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	return builder.Script()
}

// legacyTaprootLocalCommitRevokeScript builds the tap leaf with the revocation
// path for the to-local output.
func legacyTaprootLocalCommitRevokeScript(selfKey, revokeKey *btcec.PublicKey) (
	[]byte, error) {

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(selfKey))
	builder.AddOp(txscript.OP_DROP)
	builder.AddData(schnorr.SerializePubKey(revokeKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}
