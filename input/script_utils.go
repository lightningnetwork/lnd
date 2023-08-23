package input

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnutils"
	"golang.org/x/crypto/ripemd160"
)

var (
	// TODO(roasbeef): remove these and use the one's defined in txscript
	// within testnet-L.

	// SequenceLockTimeSeconds is the 22nd bit which indicates the lock
	// time is in seconds.
	SequenceLockTimeSeconds = uint32(1 << 22)
)

// mustParsePubKey parses a hex encoded public key string into a public key and
// panic if parsing fails.
func mustParsePubKey(pubStr string) btcec.PublicKey {
	pubBytes, err := hex.DecodeString(pubStr)
	if err != nil {
		panic(err)
	}

	pub, err := btcec.ParsePubKey(pubBytes)
	if err != nil {
		panic(err)
	}

	return *pub
}

// TaprootNUMSHex is the hex encoded version of the taproot NUMs key.
const TaprootNUMSHex = "02dca094751109d0bd055d03565874e8276dd53e926b44e3bd1bb" +
	"6bf4bc130a279"

var (
	// TaprootNUMSKey is a NUMS key (nothing up my sleeves number) that has
	// no known private key. This was generated using the following script:
	// https://github.com/lightninglabs/lightning-node-connect/tree/
	// master/mailbox/numsgen, with the seed phrase "Lightning Simple
	// Taproot".
	TaprootNUMSKey = mustParsePubKey(TaprootNUMSHex)
)

// Signature is an interface for objects that can populate signatures during
// witness construction.
type Signature interface {
	// Serialize returns a DER-encoded ECDSA signature.
	Serialize() []byte

	// Verify return true if the ECDSA signature is valid for the passed
	// message digest under the provided public key.
	Verify([]byte, *btcec.PublicKey) bool
}

// ParseSignature parses a raw signature into an input.Signature instance. This
// routine supports parsing normal ECDSA DER encoded signatures, as well as
// schnorr signatures.
func ParseSignature(rawSig []byte) (Signature, error) {
	if len(rawSig) == schnorr.SignatureSize {
		return schnorr.ParseSignature(rawSig)
	}

	return ecdsa.ParseDERSignature(rawSig)
}

// WitnessScriptHash generates a pay-to-witness-script-hash public key script
// paying to a version 0 witness program paying to the passed redeem script.
func WitnessScriptHash(witnessScript []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder(
		txscript.WithScriptAllocSize(P2WSHSize),
	)

	bldr.AddOp(txscript.OP_0)
	scriptHash := sha256.Sum256(witnessScript)
	bldr.AddData(scriptHash[:])
	return bldr.Script()
}

// WitnessPubKeyHash generates a pay-to-witness-pubkey-hash public key script
// paying to a version 0 witness program containing the passed serialized
// public key.
func WitnessPubKeyHash(pubkey []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder(
		txscript.WithScriptAllocSize(P2WPKHSize),
	)

	bldr.AddOp(txscript.OP_0)
	pkhash := btcutil.Hash160(pubkey)
	bldr.AddData(pkhash)
	return bldr.Script()
}

// GenerateP2SH generates a pay-to-script-hash public key script paying to the
// passed redeem script.
func GenerateP2SH(script []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder(
		txscript.WithScriptAllocSize(NestedP2WPKHSize),
	)

	bldr.AddOp(txscript.OP_HASH160)
	scripthash := btcutil.Hash160(script)
	bldr.AddData(scripthash)
	bldr.AddOp(txscript.OP_EQUAL)
	return bldr.Script()
}

// GenerateP2PKH generates a pay-to-public-key-hash public key script paying to
// the passed serialized public key.
func GenerateP2PKH(pubkey []byte) ([]byte, error) {
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

// GenerateUnknownWitness generates the maximum-sized witness public key script
// consisting of a version push and a 40-byte data push.
func GenerateUnknownWitness() ([]byte, error) {
	bldr := txscript.NewScriptBuilder()

	bldr.AddOp(txscript.OP_0)
	witnessScript := make([]byte, 40)
	bldr.AddData(witnessScript)
	return bldr.Script()
}

// GenMultiSigScript generates the non-p2sh'd multisig script for 2 of 2
// pubkeys.
func GenMultiSigScript(aPub, bPub []byte) ([]byte, error) {
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

// GenTaprootFundingScript constructs the taproot-native funding output that
// uses musig2 to create a single aggregated key to anchor the channel.
func GenTaprootFundingScript(aPub, bPub *btcec.PublicKey,
	amt int64) ([]byte, *wire.TxOut, error) {

	// Similar to the existing p2wsh funding script, we'll always make sure
	// we sort the keys before any major operations. In order to ensure
	// that there's no other way this output can be spent, we'll use a BIP
	// 86 tweak here during aggregation.
	//
	// TODO(roasbeef): revisit if BIP 86 is needed here?
	combinedKey, _, _, err := musig2.AggregateKeys(
		[]*btcec.PublicKey{aPub, bPub}, true,
		musig2.WithBIP86KeyTweak(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to combine keys: %w", err)
	}

	// Now that we have the combined key, we can create a taproot pkScript
	// from this, and then make the txout given the amount.
	pkScript, err := PayToTaprootScript(combinedKey.FinalKey)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to make taproot "+
			"pkscript: %w", err)
	}

	txOut := wire.NewTxOut(amt, pkScript)

	// For the "witness program" we just return the raw pkScript since the
	// output we create can _only_ be spent with a musig2 signature.
	return pkScript, txOut, nil
}

// SpendMultiSig generates the witness stack required to redeem the 2-of-2 p2wsh
// multi-sig output.
func SpendMultiSig(witnessScript, pubA []byte, sigA Signature,
	pubB []byte, sigB Signature) [][]byte {

	witness := make([][]byte, 4)

	// When spending a p2wsh multi-sig script, rather than an OP_0, we add
	// a nil stack element to eat the extra pop.
	witness[0] = nil

	// When initially generating the witnessScript, we sorted the serialized
	// public keys in descending order. So we do a quick comparison in order
	// ensure the signatures appear on the Script Virtual Machine stack in
	// the correct order.
	if bytes.Compare(pubA, pubB) == 1 {
		witness[1] = append(sigB.Serialize(), byte(txscript.SigHashAll))
		witness[2] = append(sigA.Serialize(), byte(txscript.SigHashAll))
	} else {
		witness[1] = append(sigA.Serialize(), byte(txscript.SigHashAll))
		witness[2] = append(sigB.Serialize(), byte(txscript.SigHashAll))
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
//   - The sender timing out the HTLC using the second level HTLC timeout
//     transaction.
//   - The receiver of the HTLC claiming the output on-chain with the payment
//     preimage.
//   - The receiver of the HTLC sweeping all the funds in the case that a
//     revoked commitment transaction bearing this HTLC was broadcast.
//
// If confirmedSpend=true, a 1 OP_CSV check will be added to the non-revocation
// cases, to allow sweeping only after confirmation.
//
// Possible Input Scripts:
//
//	SENDR: <0> <sendr sig>  <recvr sig> <0> (spend using HTLC timeout transaction)
//	RECVR: <recvr sig>  <preimage>
//	REVOK: <revoke sig> <revoke key>
//	 * receiver revoke
//
// Offered HTLC Output Script:
//
//	 OP_DUP OP_HASH160 <revocation key hash160> OP_EQUAL
//	 OP_IF
//		OP_CHECKSIG
//	 OP_ELSE
//		<recv htlc key>
//		OP_SWAP OP_SIZE 32 OP_EQUAL
//		OP_NOTIF
//		    OP_DROP 2 OP_SWAP <sender htlc key> 2 OP_CHECKMULTISIG
//		OP_ELSE
//		    OP_HASH160 <ripemd160(payment hash)> OP_EQUALVERIFY
//		    OP_CHECKSIG
//		OP_ENDIF
//		[1 OP_CHECKSEQUENCEVERIFY OP_DROP] <- if allowing confirmed
//		spend only.
//	 OP_ENDIF
func SenderHTLCScript(senderHtlcKey, receiverHtlcKey,
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

// SenderHtlcSpendRevokeWithKey constructs a valid witness allowing the receiver of an
// HTLC to claim the output with knowledge of the revocation private key in the
// scenario that the sender of the HTLC broadcasts a previously revoked
// commitment transaction. A valid spend requires knowledge of the private key
// that corresponds to their revocation base point and also the private key from
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
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
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

	revokeKey, err := deriveRevokePubKey(signDesc)
	if err != nil {
		return nil, err
	}

	return SenderHtlcSpendRevokeWithKey(signer, signDesc, revokeKey, sweepTx)
}

// IsHtlcSpendRevoke is used to determine if the passed spend is spending a
// HTLC output using the revocation key.
func IsHtlcSpendRevoke(txIn *wire.TxIn, signDesc *SignDescriptor) (
	bool, error) {

	// For taproot channels, the revocation path only has a single witness,
	// as that's the key spend path.
	isTaproot := txscript.IsPayToTaproot(signDesc.Output.PkScript)
	if isTaproot {
		return len(txIn.Witness) == 1, nil
	}

	revokeKey, err := deriveRevokePubKey(signDesc)
	if err != nil {
		return false, err
	}

	if len(txIn.Witness) == 3 &&
		bytes.Equal(txIn.Witness[1], revokeKey.SerializeCompressed()) {

		return true, nil
	}

	return false, nil
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
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
	witnessStack[1] = paymentPreimage
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// SenderHtlcSpendTimeout constructs a valid witness allowing the sender of an
// HTLC to activate the time locked covenant clause of a soon to be expired
// HTLC.  This script simply spends the multi-sig output using the
// pre-generated HTLC timeout transaction.
func SenderHtlcSpendTimeout(receiverSig Signature,
	receiverSigHash txscript.SigHashType, signer Signer,
	signDesc *SignDescriptor, htlcTimeoutTx *wire.MsgTx) (
	wire.TxWitness, error) {

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
	witnessStack[1] = append(receiverSig.Serialize(), byte(receiverSigHash))
	witnessStack[2] = append(sweepSig.Serialize(), byte(signDesc.HashType))
	witnessStack[3] = nil
	witnessStack[4] = signDesc.WitnessScript

	return witnessStack, nil
}

// SenderHTLCTapLeafTimeout returns the full tapscript leaf for the timeout
// path of the sender HTLC. This is a small script that allows the sender to
// timeout the HTLC after a period of time:
//
//	<local_key> OP_CHECKSIGVERIFY
//	<remote_key> OP_CHECKSIG
func SenderHTLCTapLeafTimeout(senderHtlcKey,
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

// SenderHTLCTapLeafSuccess returns the full tapscript leaf for the success
// path of the sender HTLC. This is a small script that allows the receiver to
// redeem the HTLC with a pre-image:
//
//	OP_SIZE 32 OP_EQUALVERIFY OP_HASH160
//	<RIPEMD160(payment_hash)> OP_EQUALVERIFY
//	<remote_htlcpubkey> OP_CHECKSIG
//	1 OP_CHECKSEQUENCEVERIFY OP_DROP
func SenderHTLCTapLeafSuccess(receiverHtlcKey *btcec.PublicKey,
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

// htlcType is an enum value that denotes what type of HTLC script this is.
type htlcType uint8

const (
	// htlcLocalIncoming represents an incoming HTLC on the local
	// commitment transaction.
	htlcLocalIncoming htlcType = iota

	// htlcLocalOutgoing represents an outgoing HTLC on the local
	// commitment transaction.
	htlcLocalOutgoing

	// htlcRemoteIncoming represents an incoming HTLC on the remote
	// commitment transaction.
	htlcRemoteIncoming

	// htlcRemoteOutgoing represents an outgoing HTLC on the remote
	// commitment transaction.
	htlcRemoteOutgoing
)

// HtlcScriptTree holds the taproot output key, as well as the two script path
// leaves that every taproot HTLC script depends on.
type HtlcScriptTree struct {
	ScriptTree

	// SuccessTapLeaf is the tapleaf for the redemption path.
	SuccessTapLeaf txscript.TapLeaf

	// TimeoutTapLeaf is the tapleaf for the timeout path.
	TimeoutTapLeaf txscript.TapLeaf

	htlcType htlcType
}

// WitnessScriptToSign returns the witness script that we'll use when signing
// for the remote party, and also verifying signatures on our transactions. As
// an example, when we create an outgoing HTLC for the remote party, we want to
// sign the success path for them, so we'll return the success path leaf.
func (h *HtlcScriptTree) WitnessScriptToSign() []byte {
	switch h.htlcType {
	// For incoming HLTCs on our local commitment, we care about verifying
	// the success path.
	case htlcLocalIncoming:
		return h.SuccessTapLeaf.Script

	// For incoming HTLCs on the remote party's commitment, we want to sign
	// the timeout path for them.
	case htlcRemoteIncoming:
		return h.TimeoutTapLeaf.Script

	// For outgoing HTLCs on our local commitment, we want to verify the
	// timeout path.
	case htlcLocalOutgoing:
		return h.TimeoutTapLeaf.Script

	// For outgoing HTLCs on the remote party's commitment, we want to sign
	// the success path for them.
	case htlcRemoteOutgoing:
		return h.SuccessTapLeaf.Script

	default:
		panic(fmt.Sprintf("unknown htlc type: %v", h.htlcType))
	}
}

// WitnessScriptForPath returns the witness script for the given spending path.
// An error is returned if the path is unknown.
func (h *HtlcScriptTree) WitnessScriptForPath(path ScriptPath) ([]byte, error) {
	switch path {
	case ScriptPathSuccess:
		return h.SuccessTapLeaf.Script, nil
	case ScriptPathTimeout:
		return h.TimeoutTapLeaf.Script, nil
	default:
		return nil, fmt.Errorf("unknown script path: %v", path)
	}
}

// CtrlBlockForPath returns the control block for the given spending path. For
// script types that don't have a control block, nil is returned.
func (h *HtlcScriptTree) CtrlBlockForPath(path ScriptPath,
) (*txscript.ControlBlock, error) {

	switch path {
	case ScriptPathSuccess:
		return lnutils.Ptr(MakeTaprootCtrlBlock(
			h.SuccessTapLeaf.Script, h.InternalKey,
			h.TapscriptTree,
		)), nil
	case ScriptPathTimeout:
		return lnutils.Ptr(MakeTaprootCtrlBlock(
			h.TimeoutTapLeaf.Script, h.InternalKey,
			h.TapscriptTree,
		)), nil
	default:
		return nil, fmt.Errorf("unknown script path: %v", path)
	}
}

// A compile time check to ensure HtlcScriptTree implements the
// TapscriptMultiplexer interface.
var _ TapscriptDescriptor = (*HtlcScriptTree)(nil)

// senderHtlcTapScriptTree builds the tapscript tree which is used to anchor
// the HTLC key for HTLCs on the sender's commitment.
func senderHtlcTapScriptTree(senderHtlcKey, receiverHtlcKey,
	revokeKey *btcec.PublicKey, payHash []byte,
	hType htlcType) (*HtlcScriptTree, error) {

	// First, we'll obtain the tap leaves for both the success and timeout
	// path.
	successTapLeaf, err := SenderHTLCTapLeafSuccess(
		receiverHtlcKey, payHash,
	)
	if err != nil {
		return nil, err
	}
	timeoutTapLeaf, err := SenderHTLCTapLeafTimeout(
		senderHtlcKey, receiverHtlcKey,
	)
	if err != nil {
		return nil, err
	}

	// With the two leaves obtained, we'll now make the tapscript tree,
	// then obtain the root from that
	tapscriptTree := txscript.AssembleTaprootScriptTree(
		successTapLeaf, timeoutTapLeaf,
	)

	tapScriptRoot := tapscriptTree.RootNode.TapHash()

	// With the tapscript root obtained, we'll tweak the revocation key
	// with this value to obtain the key that HTLCs will be sent to.
	htlcKey := txscript.ComputeTaprootOutputKey(
		revokeKey, tapScriptRoot[:],
	)

	return &HtlcScriptTree{
		ScriptTree: ScriptTree{
			TaprootKey:    htlcKey,
			TapscriptTree: tapscriptTree,
			TapscriptRoot: tapScriptRoot[:],
			InternalKey:   revokeKey,
		},
		SuccessTapLeaf: successTapLeaf,
		TimeoutTapLeaf: timeoutTapLeaf,
		htlcType:       hType,
	}, nil
}

// SenderHTLCScriptTaproot constructs the taproot witness program (schnorr key)
// for an outgoing HTLC on the sender's version of the commitment transaction.
// This method returns the top level tweaked public key that commits to both
// the script paths. This is also known as an offered HTLC.
//
// The returned key commits to a tapscript tree with two possible paths:
//
//   - Timeout path:
//     <local_key> OP_CHECKSIGVERIFY
//     <remote_key> OP_CHECKSIG
//
//   - Success path:
//     OP_SIZE 32 OP_EQUALVERIFY
//     OP_HASH160 <RIPEMD160(payment_hash)> OP_EQUALVERIFY
//     <remote_htlcpubkey> OP_CHECKSIG
//     1 OP_CHECKSEQUENCEVERIFY OP_DROP
//
// The timeout path can be spent with a witness of (sender timeout):
//
//	<receiver sig> <local sig> <timeout_script> <control_block>
//
// The success path can be spent with a valid control block, and a witness of
// (receiver redeem):
//
//	<receiver sig> <preimage> <success_script> <control_block>
//
// The top level keyspend key is the revocation key, which allows a defender to
// unilaterally spend the created output.
func SenderHTLCScriptTaproot(senderHtlcKey, receiverHtlcKey,
	revokeKey *btcec.PublicKey, payHash []byte,
	localCommit bool) (*HtlcScriptTree, error) {

	var hType htlcType
	if localCommit {
		hType = htlcLocalOutgoing
	} else {
		hType = htlcRemoteIncoming
	}

	// Given all the necessary parameters, we'll return the HTLC script
	// tree that includes the top level output script, as well as the two
	// tap leaf paths.
	return senderHtlcTapScriptTree(
		senderHtlcKey, receiverHtlcKey, revokeKey, payHash,
		hType,
	)
}

// maybeAppendSighashType appends a sighash type to the end of a signature if
// the sighash type isn't sighash default.
func maybeAppendSighash(sig Signature, sigHash txscript.SigHashType) []byte {
	sigBytes := sig.Serialize()
	if sigHash == txscript.SigHashDefault {
		return sigBytes
	}

	return append(sigBytes, byte(sigHash))
}

// SenderHTLCScriptTaprootRedeem creates a valid witness needed to redeem a
// sender taproot HTLC with the pre-image. The returned witness is valid and
// includes the control block required to spend the output. This is the offered
// HTLC claimed by the remote party.
func SenderHTLCScriptTaprootRedeem(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx, preimage []byte, revokeKey *btcec.PublicKey,
	tapscriptTree *txscript.IndexedTapScriptTree) (wire.TxWitness, error) {

	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// In addition to the signature and the witness/leaf script, we also
	// need to make a control block proof using the tapscript tree.
	var ctrlBlock []byte
	if signDesc.ControlBlock == nil {
		successControlBlock := MakeTaprootCtrlBlock(
			signDesc.WitnessScript, revokeKey, tapscriptTree,
		)

		ctrlBytes, err := successControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		ctrlBlock = ctrlBytes
	} else {
		ctrlBlock = signDesc.ControlBlock
	}

	// The final witness stack is:
	//  <receiver sig> <preimage> <success_script> <control_block>
	witnessStack := make(wire.TxWitness, 4)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)
	witnessStack[1] = preimage
	witnessStack[2] = signDesc.WitnessScript
	witnessStack[3] = ctrlBlock

	return witnessStack, nil
}

// SenderHTLCScriptTaprootTimeout creates a valid witness needed to timeout an
// HTLC on the sender's commitment transaction. The returned witness is valid
// and includes the control block required to spend the output. This is a
// timeout of the offered HTLC by the sender.
func SenderHTLCScriptTaprootTimeout(receiverSig Signature,
	receiverSigHash txscript.SigHashType, signer Signer,
	signDesc *SignDescriptor, htlcTimeoutTx *wire.MsgTx,
	revokeKey *btcec.PublicKey,
	tapscriptTree *txscript.IndexedTapScriptTree) (wire.TxWitness, error) {

	sweepSig, err := signer.SignOutputRaw(htlcTimeoutTx, signDesc)
	if err != nil {
		return nil, err
	}

	// With the sweep signature obtained, we'll obtain the control block
	// proof needed to perform a valid spend for the timeout path.
	var ctrlBlockBytes []byte
	if signDesc.ControlBlock == nil {
		timeoutControlBlock := MakeTaprootCtrlBlock(
			signDesc.WitnessScript, revokeKey, tapscriptTree,
		)
		ctrlBytes, err := timeoutControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		ctrlBlockBytes = ctrlBytes
	} else {
		ctrlBlockBytes = signDesc.ControlBlock
	}

	// The final witness stack is:
	//  <receiver sig> <local sig> <timeout_script> <control_block>
	witnessStack := make(wire.TxWitness, 4)
	witnessStack[0] = maybeAppendSighash(receiverSig, receiverSigHash)
	witnessStack[1] = maybeAppendSighash(sweepSig, signDesc.HashType)
	witnessStack[2] = signDesc.WitnessScript
	witnessStack[3] = ctrlBlockBytes

	return witnessStack, nil
}

// SenderHTLCScriptTaprootRevoke creates a valid witness needed to spend the
// revocation path of the HTLC. This uses a plain keyspend using the specified
// revocation key.
func SenderHTLCScriptTaprootRevoke(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The witness stack in this case is pretty simple: we only need to
	// specify the signature generated.
	witnessStack := make(wire.TxWitness, 1)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)

	return witnessStack, nil
}

// ReceiverHTLCScript constructs the public key script for an incoming HTLC
// output payment for the receiver's version of the commitment transaction. The
// possible execution paths from this script include:
//   - The receiver of the HTLC uses its second level HTLC transaction to
//     advance the state of the HTLC into the delay+claim state.
//   - The sender of the HTLC sweeps all the funds of the HTLC as a breached
//     commitment was broadcast.
//   - The sender of the HTLC sweeps the HTLC on-chain after the timeout period
//     of the HTLC has passed.
//
// If confirmedSpend=true, a 1 OP_CSV check will be added to the non-revocation
// cases, to allow sweeping only after confirmation.
//
// Possible Input Scripts:
//
//	RECVR: <0> <sender sig> <recvr sig> <preimage> (spend using HTLC success transaction)
//	REVOK: <sig> <key>
//	SENDR: <sig> 0
//
// Received HTLC Output Script:
//
//	 OP_DUP OP_HASH160 <revocation key hash160> OP_EQUAL
//	 OP_IF
//	 	OP_CHECKSIG
//	 OP_ELSE
//		<sendr htlc key>
//		OP_SWAP OP_SIZE 32 OP_EQUAL
//		OP_IF
//		    OP_HASH160 <ripemd160(payment hash)> OP_EQUALVERIFY
//		    2 OP_SWAP <recvr htlc key> 2 OP_CHECKMULTISIG
//		OP_ELSE
//		    OP_DROP <cltv expiry> OP_CHECKLOCKTIMEVERIFY OP_DROP
//		    OP_CHECKSIG
//		OP_ENDIF
//		[1 OP_CHECKSEQUENCEVERIFY OP_DROP] <- if allowing confirmed
//		spend only.
//	 OP_ENDIF
func ReceiverHTLCScript(cltvExpiry uint32, senderHtlcKey,
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

// ReceiverHtlcSpendRedeem constructs a valid witness allowing the receiver of
// an HTLC to redeem the conditional payment in the event that their commitment
// transaction is broadcast. This clause transitions the state of the HLTC
// output into the delay+claim state by activating the off-chain covenant bound
// by the 2-of-2 multi-sig output. The HTLC success timeout transaction being
// signed has a relative timelock delay enforced by its sequence number. This
// delay give the sender of the HTLC enough time to revoke the output if this
// is a breach commitment transaction.
func ReceiverHtlcSpendRedeem(senderSig Signature,
	senderSigHash txscript.SigHashType, paymentPreimage []byte,
	signer Signer, signDesc *SignDescriptor, htlcSuccessTx *wire.MsgTx) (
	wire.TxWitness, error) {

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
	witnessStack[1] = append(senderSig.Serialize(), byte(senderSigHash))
	witnessStack[2] = append(sweepSig.Serialize(), byte(signDesc.HashType))
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
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
	witnessStack[1] = revokeKey.SerializeCompressed()
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

func deriveRevokePubKey(signDesc *SignDescriptor) (*btcec.PublicKey, error) {
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

	return revokeKey, nil
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

	revokeKey, err := deriveRevokePubKey(signDesc)
	if err != nil {
		return nil, err
	}

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
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
	witnessStack[1] = nil
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// ReceiverHtlcTapLeafTimeout returns the full tapscript leaf for the timeout
// path of the sender HTLC. This is a small script that allows the sender
// timeout the HTLC after expiry:
//
//	<sender_htlcpubkey> OP_CHECKSIG
//	1 OP_CHECKSEQUENCEVERIFY OP_DROP
//	<cltv_expiry> OP_CHECKLOCKTIMEVERIFY OP_DROP
func ReceiverHtlcTapLeafTimeout(senderHtlcKey *btcec.PublicKey,
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

// ReceiverHtlcTapLeafSuccess returns the full tapscript leaf for the success
// path for an HTLC on the receiver's commitment transaction. This script
// allows the receiver to redeem an HTLC with knowledge of the preimage:
//
//	OP_SIZE 32 OP_EQUALVERIFY OP_HASH160
//	<RIPEMD160(payment_hash)> OP_EQUALVERIFY
//	<receiver_htlcpubkey> OP_CHECKSIGVERIFY
//	<sender_htlcpubkey> OP_CHECKSIG
func ReceiverHtlcTapLeafSuccess(receiverHtlcKey *btcec.PublicKey,
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

// receiverHtlcTapScriptTree builds the tapscript tree which is used to anchor
// the HTLC key for HTLCs on the receiver's commitment.
func receiverHtlcTapScriptTree(senderHtlcKey, receiverHtlcKey,
	revokeKey *btcec.PublicKey, payHash []byte,
	cltvExpiry uint32, hType htlcType) (*HtlcScriptTree, error) {

	// First, we'll obtain the tap leaves for both the success and timeout
	// path.
	successTapLeaf, err := ReceiverHtlcTapLeafSuccess(
		receiverHtlcKey, senderHtlcKey, payHash,
	)
	if err != nil {
		return nil, err
	}
	timeoutTapLeaf, err := ReceiverHtlcTapLeafTimeout(
		senderHtlcKey, cltvExpiry,
	)
	if err != nil {
		return nil, err
	}

	// With the two leaves obtained, we'll now make the tapscript tree,
	// then obtain the root from that
	tapscriptTree := txscript.AssembleTaprootScriptTree(
		timeoutTapLeaf, successTapLeaf,
	)

	tapScriptRoot := tapscriptTree.RootNode.TapHash()

	// With the tapscript root obtained, we'll tweak the revocation key
	// with this value to obtain the key that HTLCs will be sent to.
	htlcKey := txscript.ComputeTaprootOutputKey(
		revokeKey, tapScriptRoot[:],
	)

	return &HtlcScriptTree{
		ScriptTree: ScriptTree{
			TaprootKey:    htlcKey,
			TapscriptTree: tapscriptTree,
			TapscriptRoot: tapScriptRoot[:],
			InternalKey:   revokeKey,
		},
		SuccessTapLeaf: successTapLeaf,
		TimeoutTapLeaf: timeoutTapLeaf,
		htlcType:       hType,
	}, nil
}

// ReceiverHTLCScriptTaproot constructs the taproot witness program (schnor
// key) for an incoming HTLC on the receiver's version of the commitment
// transaction. This method returns the top level tweaked public key that
// commits to both the script paths. From the PoV of the receiver, this is an
// accepted HTLC.
//
// The returned key commits to a tapscript tree with two possible paths:
//
//   - The timeout path:
//     <remote_htlcpubkey> OP_CHECKSIG
//     1 OP_CHECKSEQUENCEVERIFY OP_DROP
//     <cltv_expiry> OP_CHECKLOCKTIMEVERIFY OP_DROP
//
//   - Success path:
//     OP_SIZE 32 OP_EQUALVERIFY
//     OP_HASH160 <RIPEMD160(payment_hash)> OP_EQUALVERIFY
//     <local_htlcpubkey> OP_CHECKSIGVERIFY
//     <remote_htlcpubkey> OP_CHECKSIG
//
// The timeout path can be spent with a witness of:
//   - <sender sig> <timeout_script> <control_block>
//
// The success path can be spent with a witness of:
//   - <sender sig> <receiver sig> <preimage> <success_script> <control_block>
//
// The top level keyspend key is the revocation key, which allows a defender to
// unilaterally spend the created output. Both the final output key as well as
// the tap leaf are returned.
func ReceiverHTLCScriptTaproot(cltvExpiry uint32,
	senderHtlcKey, receiverHtlcKey, revocationKey *btcec.PublicKey,
	payHash []byte, ourCommit bool) (*HtlcScriptTree, error) {

	var hType htlcType
	if ourCommit {
		hType = htlcLocalIncoming
	} else {
		hType = htlcRemoteOutgoing
	}

	// Given all the necessary parameters, we'll return the HTLC script
	// tree that includes the top level output script, as well as the two
	// tap leaf paths.
	return receiverHtlcTapScriptTree(
		senderHtlcKey, receiverHtlcKey, revocationKey, payHash,
		cltvExpiry, hType,
	)
}

// ReceiverHTLCScriptTaprootRedeem creates a valid witness needed to redeem a
// receiver taproot HTLC with the pre-image. The returned witness is valid and
// includes the control block required to spend the output.
func ReceiverHTLCScriptTaprootRedeem(senderSig Signature,
	senderSigHash txscript.SigHashType, paymentPreimage []byte,
	signer Signer, signDesc *SignDescriptor,
	htlcSuccessTx *wire.MsgTx, revokeKey *btcec.PublicKey,
	tapscriptTree *txscript.IndexedTapScriptTree) (wire.TxWitness, error) {

	// First, we'll generate a signature for the HTLC success transaction.
	// The signDesc should be signing with the public key used as the
	// receiver's public key and also the correct single tweak.
	sweepSig, err := signer.SignOutputRaw(htlcSuccessTx, signDesc)
	if err != nil {
		return nil, err
	}

	// In addition to the signature and the witness/leaf script, we also
	// need to make a control block proof using the tapscript tree.
	var ctrlBlock []byte
	if signDesc.ControlBlock == nil {
		redeemControlBlock := MakeTaprootCtrlBlock(
			signDesc.WitnessScript, revokeKey, tapscriptTree,
		)
		ctrlBytes, err := redeemControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		ctrlBlock = ctrlBytes
	} else {
		ctrlBlock = signDesc.ControlBlock
	}

	// The final witness stack is:
	//  * <sender sig> <receiver sig> <preimage> <success_script>
	//    <control_block>
	witnessStack := wire.TxWitness(make([][]byte, 5))
	witnessStack[0] = maybeAppendSighash(senderSig, senderSigHash)
	witnessStack[1] = maybeAppendSighash(sweepSig, signDesc.HashType)
	witnessStack[2] = paymentPreimage
	witnessStack[3] = signDesc.WitnessScript
	witnessStack[4] = ctrlBlock

	return witnessStack, nil
}

// ReceiverHTLCScriptTaprootTimeout creates a valid witness needed to timeout
// an HTLC on the receiver's commitment transaction after the timeout has
// elapsed.
func ReceiverHTLCScriptTaprootTimeout(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx, cltvExpiry int32, revokeKey *btcec.PublicKey,
	tapscriptTree *txscript.IndexedTapScriptTree) (wire.TxWitness, error) {

	// If the caller set a proper timeout value, then we'll apply it
	// directly to the transaction.
	//
	// TODO(roasbeef): helper func
	if cltvExpiry != -1 {
		// The HTLC output has an absolute time period before we are
		// permitted to recover the pending funds. Therefore we need to
		// set the locktime on this sweeping transaction in order to
		// pass Script verification.
		sweepTx.LockTime = uint32(cltvExpiry)
	}

	// With the lock time on the transaction set, we'll now generate a
	// signature for the sweep transaction. The passed sign descriptor
	// should be created using the raw public key of the sender (w/o the
	// single tweak applied), and the single tweak set to the proper value
	// taking into account the current state's point.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// In addition to the signature and the witness/leaf script, we also
	// need to make a control block proof using the tapscript tree.
	var ctrlBlock []byte
	if signDesc.ControlBlock == nil {
		timeoutControlBlock := MakeTaprootCtrlBlock(
			signDesc.WitnessScript, revokeKey, tapscriptTree,
		)
		ctrlBlock, err = timeoutControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}
	} else {
		ctrlBlock = signDesc.ControlBlock
	}

	// The final witness is pretty simple, we just need to present a valid
	// signature for the script, and then provide the control block.
	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)
	witnessStack[1] = signDesc.WitnessScript
	witnessStack[2] = ctrlBlock

	return witnessStack, nil
}

// ReceiverHTLCScriptTaprootRevoke creates a valid witness needed to spend the
// revocation path of the HTLC from the PoV of the sender (offerer) of the
// HTLC. This uses a plain keyspend using the specified revocation key.
func ReceiverHTLCScriptTaprootRevoke(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The witness stack in this case is pretty simple: we only need to
	// specify the signature generated.
	witnessStack := make(wire.TxWitness, 1)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)

	return witnessStack, nil
}

// SecondLevelHtlcScript is the uniform script that's used as the output for
// the second-level HTLC transactions. The second level transaction act as a
// sort of covenant, ensuring that a 2-of-2 multi-sig output can only be
// spent in a particular way, and to a particular output.
//
// Possible Input Scripts:
//
//   - To revoke an HTLC output that has been transitioned to the claim+delay
//     state:
//     <revoke sig> 1
//
//   - To claim and HTLC output, either with a pre-image or due to a timeout:
//     <delay sig> 0
//
// Output Script:
//
//	 OP_IF
//		<revoke key>
//	 OP_ELSE
//		<delay in blocks>
//		OP_CHECKSEQUENCEVERIFY
//		OP_DROP
//		<delay key>
//	 OP_ENDIF
//	 OP_CHECKSIG
//
// TODO(roasbeef): possible renames for second-level
//   - transition?
//   - covenant output
func SecondLevelHtlcScript(revocationKey, delayKey *btcec.PublicKey,
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

// TODO(roasbeef): move all taproot stuff to new file?

// TaprootSecondLevelTapLeaf constructs the tap leaf used as the sole script
// path for a second level HTLC spend.
//
// The final script used is:
//
//	<local_delay_key> OP_CHECKSIG
//	<to_self_delay> OP_CHECKSEQUENCEVERIFY OP_DROP
func TaprootSecondLevelTapLeaf(delayKey *btcec.PublicKey,
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

// SecondLevelHtlcTapscriptTree construct the indexed tapscript tree needed to
// generate the taptweak to create the final output and also control block.
func SecondLevelHtlcTapscriptTree(delayKey *btcec.PublicKey,
	csvDelay uint32) (*txscript.IndexedTapScriptTree, error) {

	// First grab the second level leaf script we need to create the top
	// level output.
	secondLevelTapLeaf, err := TaprootSecondLevelTapLeaf(delayKey, csvDelay)
	if err != nil {
		return nil, err
	}

	// Now that we have the sole second level script, we can create the
	// tapscript tree that commits to both the leaves.
	return txscript.AssembleTaprootScriptTree(secondLevelTapLeaf), nil
}

// TaprootSecondLevelHtlcScript is the uniform script that's used as the output
// for the second-level HTLC transaction. The second level transaction acts as
// an off-chain 2-of-2 covenant that can only be spent a particular way and to
// a particular output.
//
// Possible Input Scripts:
//   - revocation sig
//   - <local_delay_sig>
//
// The script main script lets the broadcaster spend after a delay the script
// path:
//
//	<local_delay_key> OP_CHECKSIG
//	<to_self_delay> OP_CHECKSEQUENCEVERIFY OP_DROP
//
// The keyspend path require knowledge of the top level revocation private key.
func TaprootSecondLevelHtlcScript(revokeKey, delayKey *btcec.PublicKey,
	csvDelay uint32) (*btcec.PublicKey, error) {

	// First, we'll make the tapscript tree that commits to the redemption
	// path.
	tapScriptTree, err := SecondLevelHtlcTapscriptTree(
		delayKey, csvDelay,
	)
	if err != nil {
		return nil, err
	}

	tapScriptRoot := tapScriptTree.RootNode.TapHash()

	// With the tapscript root obtained, we'll tweak the revocation key
	// with this value to obtain the key that the second level spend will
	// create.
	redemptionKey := txscript.ComputeTaprootOutputKey(
		revokeKey, tapScriptRoot[:],
	)

	return redemptionKey, nil
}

// SecondLevelScriptTree is a tapscript tree used to spend the second level
// HTLC output after the CSV delay has passed.
type SecondLevelScriptTree struct {
	ScriptTree

	// SuccessTapLeaf is the tapleaf for the redemption path.
	SuccessTapLeaf txscript.TapLeaf
}

// TaprootSecondLevelScriptTree constructs the tapscript tree used to spend the
// second level HTLC output.
func TaprootSecondLevelScriptTree(revokeKey, delayKey *btcec.PublicKey,
	csvDelay uint32) (*SecondLevelScriptTree, error) {

	// First, we'll make the tapscript tree that commits to the redemption
	// path.
	tapScriptTree, err := SecondLevelHtlcTapscriptTree(
		delayKey, csvDelay,
	)
	if err != nil {
		return nil, err
	}

	// With the tree constructed, we can make the pkscript which is the
	// taproot output key itself.
	tapScriptRoot := tapScriptTree.RootNode.TapHash()
	outputKey := txscript.ComputeTaprootOutputKey(
		revokeKey, tapScriptRoot[:],
	)

	return &SecondLevelScriptTree{
		ScriptTree: ScriptTree{
			TaprootKey:    outputKey,
			TapscriptTree: tapScriptTree,
			TapscriptRoot: tapScriptRoot[:],
			InternalKey:   revokeKey,
		},
		SuccessTapLeaf: tapScriptTree.LeafMerkleProofs[0].TapLeaf,
	}, nil
}

// WitnessScript returns the witness script that we'll use when signing for the
// remote party, and also verifying signatures on our transactions. As an
// example, when we create an outgoing HTLC for the remote party, we want to
// sign their success path.
func (s *SecondLevelScriptTree) WitnessScriptToSign() []byte {
	return s.SuccessTapLeaf.Script
}

// WitnessScriptForPath returns the witness script for the given spending path.
// An error is returned if the path is unknown.
func (s *SecondLevelScriptTree) WitnessScriptForPath(path ScriptPath,
) ([]byte, error) {

	switch path {
	case ScriptPathDelay:
		fallthrough
	case ScriptPathSuccess:
		return s.SuccessTapLeaf.Script, nil

	default:
		return nil, fmt.Errorf("unknown script path: %v", path)
	}
}

// CtrlBlockForPath returns the control block for the given spending path. For
// script types that don't have a control block, nil is returned.
func (s *SecondLevelScriptTree) CtrlBlockForPath(path ScriptPath,
) (*txscript.ControlBlock, error) {

	switch path {
	case ScriptPathDelay:
		fallthrough
	case ScriptPathSuccess:
		return lnutils.Ptr(MakeTaprootCtrlBlock(
			s.SuccessTapLeaf.Script, s.InternalKey,
			s.TapscriptTree,
		)), nil

	default:
		return nil, fmt.Errorf("unknown script path: %v", path)
	}
}

// A compile time check to ensure SecondLevelScriptTree implements the
// TapscriptDescriptor interface.
var _ TapscriptDescriptor = (*SecondLevelScriptTree)(nil)

// TaprootHtlcSpendRevoke spends a second-level HTLC output via the revocation
// path. This uses the top level keyspend path to redeem the contested output.
//
// The passed SignDescriptor MUST have the proper witness script and also the
// proper top-level tweak derived from the tapscript tree for the second level
// output.
func TaprootHtlcSpendRevoke(signer Signer, signDesc *SignDescriptor,
	revokeTx *wire.MsgTx) (wire.TxWitness, error) {

	// We don't need any spacial modifications to the transaction as this
	// is just sweeping a revoked HTLC output. So we'll generate a regular
	// schnorr signature.
	sweepSig, err := signer.SignOutputRaw(revokeTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The witness stack in this case is pretty simple: we only need to
	// specify the signature generated.
	witnessStack := make(wire.TxWitness, 1)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)

	return witnessStack, nil
}

// TaprootHtlcSpendSuccess spends a second-level HTLC output via the redemption
// path. This should be used to sweep funds after the pre-image is known.
//
// NOTE: The caller MUST set the txn version, sequence number, and sign
// descriptor's sig hash cache before invocation.
func TaprootHtlcSpendSuccess(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx, revokeKey *btcec.PublicKey,
	tapscriptTree *txscript.IndexedTapScriptTree) (wire.TxWitness, error) {

	// First, we'll generate the sweep signature based on the populated
	// sign desc. This should give us a valid schnorr signature for the
	// sole script path leaf.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	var ctrlBlock []byte
	if signDesc.ControlBlock == nil {
		// Now that we have the sweep signature, we'll construct the
		// control block needed to spend the script path.
		redeemControlBlock := MakeTaprootCtrlBlock(
			signDesc.WitnessScript, revokeKey, tapscriptTree,
		)

		ctrlBlock, err = redeemControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}
	} else {
		ctrlBlock = signDesc.ControlBlock
	}

	// Now that we have the redeem control block, we can construct the
	// final witness needed to spend the script:
	//
	//  <success sig> <success script> <control_block>
	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)
	witnessStack[1] = signDesc.WitnessScript
	witnessStack[2] = ctrlBlock

	return witnessStack, nil
}

// LeaseSecondLevelHtlcScript is the uniform script that's used as the output
// for the second-level HTLC transactions. The second level transaction acts as
// a sort of covenant, ensuring that a 2-of-2 multi-sig output can only be
// spent in a particular way, and to a particular output.
//
// Possible Input Scripts:
//
//   - To revoke an HTLC output that has been transitioned to the claim+delay
//     state:
//     <revoke sig> 1
//
//   - To claim an HTLC output, either with a pre-image or due to a timeout:
//     <delay sig> 0
//
// Output Script:
//
//	 OP_IF
//		<revoke key>
//	 OP_ELSE
//		<lease maturity in blocks>
//		OP_CHECKLOCKTIMEVERIFY
//		OP_DROP
//		<delay in blocks>
//		OP_CHECKSEQUENCEVERIFY
//		OP_DROP
//		<delay key>
//	 OP_ENDIF
//	 OP_CHECKSIG.
func LeaseSecondLevelHtlcScript(revocationKey, delayKey *btcec.PublicKey,
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
	signDesc.SigHashes = NewTxSigHashesV0Only(sweepTx)

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
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
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
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
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
	witnessStack[0] = append(sweepSig.Serialize(), byte(txscript.SigHashAll))
	witnessStack[1] = nil
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// LockTimeToSequence converts the passed relative locktime to a sequence
// number in accordance to BIP-68.
// See: https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
//   - (Compatibility)
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
//
//	REVOKE:     <sig> 1
//	SENDRSWEEP: <sig> <emptyvector>
//
// Output Script:
//
//	OP_IF
//	    <revokeKey>
//	OP_ELSE
//	    <numRelativeBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP
//	    <timeKey>
//	OP_ENDIF
//	OP_CHECKSIG
func CommitScriptToSelf(csvTimeout uint32, selfKey, revokeKey *btcec.PublicKey) ([]byte, error) {
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

// CommitScriptTree holds the taproot output key (in this case the revocation
// key, or a NUMs point for the remote output) along with the tapscript leaf
// that can spend the output after a delay.
type CommitScriptTree struct {
	ScriptTree

	// SettleLeaf is the leaf used to settle the output after the delay.
	SettleLeaf txscript.TapLeaf

	// RevocationLeaf is the leaf used to spend the output with the
	// revocation key signature.
	RevocationLeaf txscript.TapLeaf
}

// A compile time check to ensure CommitScriptTree implements the
// TapscriptDescriptor interface.
var _ TapscriptDescriptor = (*CommitScriptTree)(nil)

// WitnessScript returns the witness script that we'll use when signing for the
// remote party, and also verifying signatures on our transactions. As an
// example, when we create an outgoing HTLC for the remote party, we want to
// sign their success path.
func (c *CommitScriptTree) WitnessScriptToSign() []byte {
	// TODO(roasbeef): abstraction leak here? always dependent
	return nil
}

// WitnessScriptForPath returns the witness script for the given spending path.
// An error is returned if the path is unknown.
func (c *CommitScriptTree) WitnessScriptForPath(path ScriptPath,
) ([]byte, error) {

	switch path {
	// For the commitment output, the delay and success path are the same,
	// so we'll fall through here to success.
	case ScriptPathDelay:
		fallthrough
	case ScriptPathSuccess:
		return c.SettleLeaf.Script, nil
	case ScriptPathRevocation:
		return c.RevocationLeaf.Script, nil
	default:
		return nil, fmt.Errorf("unknown script path: %v", path)
	}
}

// CtrlBlockForPath returns the control block for the given spending path. For
// script types that don't have a control block, nil is returned.
func (c *CommitScriptTree) CtrlBlockForPath(path ScriptPath,
) (*txscript.ControlBlock, error) {

	switch path {
	case ScriptPathDelay:
		fallthrough
	case ScriptPathSuccess:
		return lnutils.Ptr(MakeTaprootCtrlBlock(
			c.SettleLeaf.Script, c.InternalKey,
			c.TapscriptTree,
		)), nil
	case ScriptPathRevocation:
		return lnutils.Ptr(MakeTaprootCtrlBlock(
			c.RevocationLeaf.Script, c.InternalKey,
			c.TapscriptTree,
		)), nil
	default:
		return nil, fmt.Errorf("unknown script path: %v", path)
	}
}

// NewLocalCommitScriptTree returns a new CommitScript tree that can be used to
// create and spend the commitment output for the local party.
func NewLocalCommitScriptTree(csvTimeout uint32,
	selfKey, revokeKey *btcec.PublicKey) (*CommitScriptTree, error) {

	// First, we'll need to construct the tapLeaf that'll be our delay CSV
	// clause.
	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(selfKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	builder.AddInt64(int64(csvTimeout))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	delayScript, err := builder.Script()
	if err != nil {
		return nil, err
	}

	// Next, we'll need to construct the revocation path, which is just a
	// simple checksig script.
	builder = txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(selfKey))
	builder.AddOp(txscript.OP_DROP)
	builder.AddData(schnorr.SerializePubKey(revokeKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	revokeScript, err := builder.Script()
	if err != nil {
		return nil, err
	}

	// With both scripts computed, we'll now create a tapscript tree with
	// the two leaves, and then obtain a root from that.
	delayTapLeaf := txscript.NewBaseTapLeaf(delayScript)
	revokeTapLeaf := txscript.NewBaseTapLeaf(revokeScript)
	tapScriptTree := txscript.AssembleTaprootScriptTree(
		delayTapLeaf, revokeTapLeaf,
	)
	tapScriptRoot := tapScriptTree.RootNode.TapHash()

	// Now that we have our root, we can arrive at the final output script
	// by tweaking the internal key with this root.
	toLocalOutputKey := txscript.ComputeTaprootOutputKey(
		&TaprootNUMSKey, tapScriptRoot[:],
	)

	return &CommitScriptTree{
		ScriptTree: ScriptTree{
			TaprootKey:    toLocalOutputKey,
			TapscriptTree: tapScriptTree,
			TapscriptRoot: tapScriptRoot[:],
			InternalKey:   &TaprootNUMSKey,
		},
		SettleLeaf:     delayTapLeaf,
		RevocationLeaf: revokeTapLeaf,
	}, nil
}

// TaprootCommitScriptToSelf creates the taproot witness program that commits
// to the revocation (script path) and delay path (script path) in a single
// taproot output key. Both the delay script and the revocation script are part
// of the tapscript tree to ensure that the internal key (the local delay key)
// is always revealed.  This ensures that a 3rd party can always sweep the set
// of anchor outputs.
//
// For the delay path we have the following tapscript leaf script:
//
//	<local_delayedpubkey> OP_CHECKSIG
//	<to_self_delay> OP_CHECKSEQUENCEVERIFY OP_DROP
//
// This can then be spent with just:
//
//	<local_delayedsig> <to_delay_script> <delay_control_block>
//
// Where the to_delay_script is listed above, and the delay_control_block
// computed as:
//
//	delay_control_block = (output_key_y_parity | 0xc0) || taproot_nums_key
//
// The revocation path is simply:
//
//	<local_delayedpubkey> OP_DROP
//	<revocationkey> OP_CHECKSIG
//
// The revocation path can be spent with a control block similar to the above
// (but contains the hash of the other script), and with the following witness:
//
//	<revocation_sig>
//
// We use a noop data push to ensure that the local public key is also revealed
// on chain, which enables the anchor output to be swept.
func TaprootCommitScriptToSelf(csvTimeout uint32,
	selfKey, revokeKey *btcec.PublicKey) (*btcec.PublicKey, error) {

	commitScriptTree, err := NewLocalCommitScriptTree(
		csvTimeout, selfKey, revokeKey,
	)
	if err != nil {
		return nil, err
	}

	return commitScriptTree.TaprootKey, nil
}

// MakeTaprootSCtrlBlock takes a leaf script, the internal key (usually the
// revoke key), and a script tree and creates a valid control block for a spend
// of the leaf.
func MakeTaprootCtrlBlock(leafScript []byte, internalKey *btcec.PublicKey,
	scriptTree *txscript.IndexedTapScriptTree) txscript.ControlBlock {

	tapLeafHash := txscript.NewBaseTapLeaf(leafScript).TapHash()
	scriptIdx := scriptTree.LeafProofIndex[tapLeafHash]
	settleMerkleProof := scriptTree.LeafMerkleProofs[scriptIdx]

	return settleMerkleProof.ToControlBlock(internalKey)
}

// TaprootCommitSpendSuccess constructs a valid witness allowing a node to
// sweep the settled taproot output after the delay has passed for a force
// close.
func TaprootCommitSpendSuccess(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx,
	scriptTree *txscript.IndexedTapScriptTree) (wire.TxWitness, error) {

	// First, we'll need to construct a valid control block to execute the
	// leaf script for sweep settlement.
	//
	// TODO(roasbeef); make into closure instead? only need reovke key and
	// scriptTree to make the ctrl block -- then default version that would
	// take froms ign desc?
	var ctrlBlockBytes []byte
	if signDesc.ControlBlock == nil {
		settleControlBlock := MakeTaprootCtrlBlock(
			signDesc.WitnessScript, &TaprootNUMSKey, scriptTree,
		)
		ctrlBytes, err := settleControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		ctrlBlockBytes = ctrlBytes
	} else {
		ctrlBlockBytes = signDesc.ControlBlock
	}

	// With the control block created, we'll now generate the signature we
	// need to authorize the spend.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The final witness stack will be:
	//
	//  <sweep sig> <sweep script> <control block>
	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)
	witnessStack[1] = signDesc.WitnessScript
	witnessStack[2] = ctrlBlockBytes
	if err != nil {
		return nil, err
	}

	return witnessStack, nil
}

// TaprootCommitSpendRevoke constructs a valid witness allowing a node to sweep
// the revoked taproot output of a malicious peer.
func TaprootCommitSpendRevoke(signer Signer, signDesc *SignDescriptor,
	revokeTx *wire.MsgTx,
	scriptTree *txscript.IndexedTapScriptTree) (wire.TxWitness, error) {

	// First, we'll need to construct a valid control block to execute the
	// leaf script for revocation path.
	var ctrlBlockBytes []byte
	if signDesc.ControlBlock == nil {
		revokeCtrlBlock := MakeTaprootCtrlBlock(
			signDesc.WitnessScript, &TaprootNUMSKey, scriptTree,
		)
		revokeBytes, err := revokeCtrlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		ctrlBlockBytes = revokeBytes
	} else {
		ctrlBlockBytes = signDesc.ControlBlock
	}

	// With the control block created, we'll now generate the signature we
	// need to authorize the spend.
	revokeSig, err := signer.SignOutputRaw(revokeTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The final witness stack will be:
	//
	//  <revoke sig sig> <revoke script> <control block>
	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = maybeAppendSighash(revokeSig, signDesc.HashType)
	witnessStack[1] = signDesc.WitnessScript
	witnessStack[2] = ctrlBlockBytes

	return witnessStack, nil
}

// LeaseCommitScriptToSelf constructs the public key script for the output on the
// commitment transaction paying to the "owner" of said commitment transaction.
// If the other party learns of the preimage to the revocation hash, then they
// can claim all the settled funds in the channel, plus the unsettled funds.
//
// Possible Input Scripts:
//
//	REVOKE:     <sig> 1
//	SENDRSWEEP: <sig> <emptyvector>
//
// Output Script:
//
//	OP_IF
//	    <revokeKey>
//	OP_ELSE
//	    <absoluteLeaseExpiry> OP_CHECKLOCKTIMEVERIFY OP_DROP
//	    <numRelativeBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP
//	    <timeKey>
//	OP_ENDIF
//	OP_CHECKSIG
func LeaseCommitScriptToSelf(selfKey, revokeKey *btcec.PublicKey,
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
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
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
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
	witnessStack[1] = []byte{1}
	witnessStack[2] = signDesc.WitnessScript

	return witnessStack, nil
}

// CommitSpendNoDelay constructs a valid witness allowing a node to spend their
// settled no-delay output on the counterparty's commitment transaction. If the
// tweakless field is true, then we'll omit the set where we tweak the pubkey
// with a random set of bytes, and use it directly in the witness stack.
//
// NOTE: The passed SignDescriptor should include the raw (untweaked) public
// key of the receiver and also the proper single tweak value based on the
// current commitment point.
func CommitSpendNoDelay(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx, tweakless bool) (wire.TxWitness, error) {

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
	witness[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))

	switch tweakless {
	// If we're tweaking the key, then we use the tweaked public key as the
	// last item in the witness stack which was originally used to created
	// the pkScript we're spending.
	case false:
		witness[1] = TweakPubKeyWithTweak(
			signDesc.KeyDesc.PubKey, signDesc.SingleTweak,
		).SerializeCompressed()

	// Otherwise, we can just use the raw pubkey, since there's no random
	// value to be combined.
	case true:
		witness[1] = signDesc.KeyDesc.PubKey.SerializeCompressed()
	}

	return witness, nil
}

// CommitScriptUnencumbered constructs the public key script on the commitment
// transaction paying to the "other" party. The constructed output is a normal
// p2wkh output spendable immediately, requiring no contestation period.
func CommitScriptUnencumbered(key *btcec.PublicKey) ([]byte, error) {
	// This script goes to the "other" party, and is spendable immediately.
	builder := txscript.NewScriptBuilder(txscript.WithScriptAllocSize(
		P2WPKHSize,
	))
	builder.AddOp(txscript.OP_0)
	builder.AddData(btcutil.Hash160(key.SerializeCompressed()))

	return builder.Script()
}

// CommitScriptToRemoteConfirmed constructs the script for the output on the
// commitment transaction paying to the remote party of said commitment
// transaction. The money can only be spend after one confirmation.
//
// Possible Input Scripts:
//
//	SWEEP: <sig>
//
// Output Script:
//
//	<key> OP_CHECKSIGVERIFY
//	1 OP_CHECKSEQUENCEVERIFY
func CommitScriptToRemoteConfirmed(key *btcec.PublicKey) ([]byte, error) {
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

// NewRemoteCommitScriptTree constructs a new script tree for the remote party
// to sweep their funds after a hard coded 1 block delay.
func NewRemoteCommitScriptTree(remoteKey *btcec.PublicKey,
) (*CommitScriptTree, error) {

	// First, construct the remote party's tapscript they'll use to sweep
	// their outputs.
	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(remoteKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	builder.AddOp(txscript.OP_1)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	remoteScript, err := builder.Script()
	if err != nil {
		return nil, err
	}

	// With this script constructed, we'll map that into a tapLeaf, then
	// make a new tapscript root from that.
	tapLeaf := txscript.NewBaseTapLeaf(remoteScript)
	tapScriptTree := txscript.AssembleTaprootScriptTree(tapLeaf)
	tapScriptRoot := tapScriptTree.RootNode.TapHash()

	// Now that we have our root, we can arrive at the final output script
	// by tweaking the internal key with this root.
	toRemoteOutputKey := txscript.ComputeTaprootOutputKey(
		&TaprootNUMSKey, tapScriptRoot[:],
	)

	return &CommitScriptTree{
		ScriptTree: ScriptTree{
			TaprootKey:    toRemoteOutputKey,
			TapscriptTree: tapScriptTree,
			TapscriptRoot: tapScriptRoot[:],
			InternalKey:   &TaprootNUMSKey,
		},
		SettleLeaf: tapLeaf,
	}, nil
}

// TaprootCommitScriptToRemote constructs a taproot witness program for the
// output on the commitment transaction for the remote party. For the top level
// key spend, we'll use a NUMs key to ensure that only the script path can be
// taken. Using a set NUMs key here also means that recovery solutions can scan
// the chain given knowledge of the public key for the remote party. We then
// commit to a single tapscript leaf that holds the normal CSV 1 delay
// script.
//
// Our single tapleaf will use the following script:
//
//	<remotepubkey> OP_CHECKSIG
//	1 OP_CHECKSEQUENCEVERIFY OP_DROP
func TaprootCommitScriptToRemote(remoteKey *btcec.PublicKey,
) (*btcec.PublicKey, error) {

	commitScriptTree, err := NewRemoteCommitScriptTree(remoteKey)
	if err != nil {
		return nil, err
	}

	return commitScriptTree.TaprootKey, nil
}

// TaprootCommitRemoteSpend allows the remote party to sweep their output into
// their wallet after an enforced 1 block delay.
func TaprootCommitRemoteSpend(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx,
	scriptTree *txscript.IndexedTapScriptTree) (wire.TxWitness, error) {

	// First, we'll need to construct a valid control block to execute the
	// leaf script for sweep settlement.
	var ctrlBlockBytes []byte
	if signDesc.ControlBlock == nil {
		settleControlBlock := MakeTaprootCtrlBlock(
			signDesc.WitnessScript, &TaprootNUMSKey, scriptTree,
		)
		ctrlBytes, err := settleControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}

		ctrlBlockBytes = ctrlBytes
	} else {
		ctrlBlockBytes = signDesc.ControlBlock
	}

	// With the control block created, we'll now generate the signature we
	// need to authorize the spend.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The final witness stack will be:
	//
	//  <sweep sig> <sweep script> <control block>
	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)
	witnessStack[1] = signDesc.WitnessScript
	witnessStack[2] = ctrlBlockBytes

	return witnessStack, nil
}

// LeaseCommitScriptToRemoteConfirmed constructs the script for the output on
// the commitment transaction paying to the remote party of said commitment
// transaction. The money can only be spend after one confirmation.
//
// Possible Input Scripts:
//
//	SWEEP: <sig>
//
// Output Script:
//
//		<key> OP_CHECKSIGVERIFY
//	     <lease maturity in blocks> OP_CHECKLOCKTIMEVERIFY OP_DROP
//		1 OP_CHECKSEQUENCEVERIFY
func LeaseCommitScriptToRemoteConfirmed(key *btcec.PublicKey,
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

// CommitSpendToRemoteConfirmed constructs a valid witness allowing a node to
// spend their settled output on the counterparty's commitment transaction when
// it has one confirmetion. This is used for the anchor channel type. The
// spending key will always be non-tweaked for this output type.
func CommitSpendToRemoteConfirmed(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	if signDesc.KeyDesc.PubKey == nil {
		return nil, fmt.Errorf("cannot generate witness with nil " +
			"KeyDesc pubkey")
	}

	// Similar to non delayed output, only a signature is needed.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// Finally, we'll manually craft the witness. The witness here is the
	// signature and the redeem script.
	witnessStack := make([][]byte, 2)
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
	witnessStack[1] = signDesc.WitnessScript

	return witnessStack, nil
}

// CommitScriptAnchor constructs the script for the anchor output spendable by
// the given key immediately, or by anyone after 16 confirmations.
//
// Possible Input Scripts:
//
//	By owner:				<sig>
//	By anyone (after 16 conf):	<emptyvector>
//
// Output Script:
//
//	<funding_pubkey> OP_CHECKSIG OP_IFDUP
//	OP_NOTIF
//	  OP_16 OP_CSV
//	OP_ENDIF
func CommitScriptAnchor(key *btcec.PublicKey) ([]byte, error) {
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

// AnchorScriptTree holds all the contents needed to sweep a taproot anchor
// output on chain.
type AnchorScriptTree struct {
	ScriptTree

	// SweepLeaf is the leaf used to settle the output after the delay.
	SweepLeaf txscript.TapLeaf
}

// NewAnchorScriptTree makes a new script tree for an anchor output with the
// passed anchor key.
func NewAnchorScriptTree(anchorKey *btcec.PublicKey,
) (*AnchorScriptTree, error) {

	// The main script used is just a OP_16 CSV (anyone can sweep after 16
	// blocks).
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_16)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	anchorScript, err := builder.Script()
	if err != nil {
		return nil, err
	}

	// With the script, we can make our sole leaf, then derive the root
	// from that.
	tapLeaf := txscript.NewBaseTapLeaf(anchorScript)
	tapScriptTree := txscript.AssembleTaprootScriptTree(tapLeaf)
	tapScriptRoot := tapScriptTree.RootNode.TapHash()

	// Now that we have our root, we can arrive at the final output script
	// by tweaking the internal key with this root.
	anchorOutputKey := txscript.ComputeTaprootOutputKey(
		anchorKey, tapScriptRoot[:],
	)

	return &AnchorScriptTree{
		ScriptTree: ScriptTree{
			TaprootKey:    anchorOutputKey,
			TapscriptTree: tapScriptTree,
			TapscriptRoot: tapScriptRoot[:],
			InternalKey:   anchorKey,
		},
		SweepLeaf: tapLeaf,
	}, nil
}

// WitnessScript returns the witness script that we'll use when signing for the
// remote party, and also verifying signatures on our transactions. As an
// example, when we create an outgoing HTLC for the remote party, we want to
// sign their success path.
func (a *AnchorScriptTree) WitnessScriptToSign() []byte {
	return a.SweepLeaf.Script
}

// WitnessScriptForPath returns the witness script for the given spending path.
// An error is returned if the path is unknown.
func (a *AnchorScriptTree) WitnessScriptForPath(path ScriptPath,
) ([]byte, error) {

	switch path {
	case ScriptPathDelay:
		fallthrough
	case ScriptPathSuccess:
		return a.SweepLeaf.Script, nil

	default:
		return nil, fmt.Errorf("unknown script path: %v", path)
	}
}

// CtrlBlockForPath returns the control block for the given spending path. For
// script types that don't have a control block, nil is returned.
func (a *AnchorScriptTree) CtrlBlockForPath(path ScriptPath,
) (*txscript.ControlBlock, error) {

	switch path {
	case ScriptPathDelay:
		fallthrough
	case ScriptPathSuccess:
		return lnutils.Ptr(MakeTaprootCtrlBlock(
			a.SweepLeaf.Script, a.InternalKey,
			a.TapscriptTree,
		)), nil

	default:
		return nil, fmt.Errorf("unknown script path: %v", path)
	}
}

// A compile time check to ensure AnchorScriptTree implements the
// TapscriptDescriptor interface.
var _ TapscriptDescriptor = (*AnchorScriptTree)(nil)

// TaprootOutputKeyAnchor returns the segwit v1 (taproot) witness program that
// encodes the anchor output spending conditions: the passed key can be used
// for keyspend, with the OP_CSV 16 clause living within an internal tapscript
// leaf.
//
// Spend paths:
//   - Key spend: <key_signature>
//   - Script spend: OP_16 CSV <control_block>
func TaprootOutputKeyAnchor(key *btcec.PublicKey) (*btcec.PublicKey, error) {
	anchorScriptTree, err := NewAnchorScriptTree(key)
	if err != nil {
		return nil, err
	}

	return anchorScriptTree.TaprootKey, nil
}

// TaprootAnchorSpend constructs a valid witness allowing a node to sweep their
// anchor output.
func TaprootAnchorSpend(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	// For this spend type, we only need a single signature which'll be a
	// keyspend using the anchor private key.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The witness stack in this case is pretty simple: we only need to
	// specify the signature generated.
	witnessStack := make(wire.TxWitness, 1)
	witnessStack[0] = maybeAppendSighash(sweepSig, signDesc.HashType)

	return witnessStack, nil
}

// TaprootAnchorSpendAny constructs a valid witness allowing anyone to sweep
// the anchor output after 16 blocks.
func TaprootAnchorSpendAny(anchorKey *btcec.PublicKey) (wire.TxWitness, error) {
	anchorScriptTree, err := NewAnchorScriptTree(anchorKey)
	if err != nil {
		return nil, err
	}

	// For this spend, the only thing we need to do is create a valid
	// control block. Other than that, there're no restrictions to how the
	// output can be spent.
	scriptTree := anchorScriptTree.TapscriptTree
	sweepLeaf := anchorScriptTree.SweepLeaf
	sweepIdx := scriptTree.LeafProofIndex[sweepLeaf.TapHash()]
	sweepMerkleProof := scriptTree.LeafMerkleProofs[sweepIdx]
	sweepControlBlock := sweepMerkleProof.ToControlBlock(anchorKey)

	// The final witness stack will be:
	//
	//  <sweep script> <control block>
	witnessStack := make(wire.TxWitness, 2)
	witnessStack[0] = sweepLeaf.Script
	witnessStack[1], err = sweepControlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return witnessStack, nil
}

// CommitSpendAnchor constructs a valid witness allowing a node to spend their
// anchor output on the commitment transaction using their funding key. This is
// used for the anchor channel type.
func CommitSpendAnchor(signer Signer, signDesc *SignDescriptor,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	if signDesc.KeyDesc.PubKey == nil {
		return nil, fmt.Errorf("cannot generate witness with nil " +
			"KeyDesc pubkey")
	}

	// Create a signature.
	sweepSig, err := signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// The witness here is just a signature and the redeem script.
	witnessStack := make([][]byte, 2)
	witnessStack[0] = append(sweepSig.Serialize(), byte(signDesc.HashType))
	witnessStack[1] = signDesc.WitnessScript

	return witnessStack, nil
}

// CommitSpendAnchorAnyone constructs a witness allowing anyone to spend the
// anchor output after it has gotten 16 confirmations. Since no signing is
// required, only knowledge of the redeem script is necessary to spend it.
func CommitSpendAnchorAnyone(script []byte) (wire.TxWitness, error) {
	// The witness here is just the redeem script.
	witnessStack := make([][]byte, 2)
	witnessStack[0] = nil
	witnessStack[1] = script

	return witnessStack, nil
}

// SingleTweakBytes computes set of bytes we call the single tweak. The purpose
// of the single tweak is to randomize all regular delay and payment base
// points. To do this, we generate a hash that binds the commitment point to
// the pay/delay base point. The end end results is that the basePoint is
// tweaked as follows:
//
//   - key = basePoint + sha256(commitPoint || basePoint)*G
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
//	tweakPub := basePoint + sha256(commitPoint || basePoint) * G
//	         := G*k + sha256(commitPoint || basePoint)*G
//	         := G*(k + sha256(commitPoint || basePoint))
//
// Therefore, if a party possess the value k, the private key of the base
// point, then they are able to derive the proper private key for the
// revokeKey by computing:
//
//	revokePriv := k + sha256(commitPoint || basePoint) mod N
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
func TweakPubKeyWithTweak(pubKey *btcec.PublicKey,
	tweakBytes []byte) *btcec.PublicKey {

	var (
		pubKeyJacobian btcec.JacobianPoint
		tweakJacobian  btcec.JacobianPoint
		resultJacobian btcec.JacobianPoint
	)
	tweakKey, _ := btcec.PrivKeyFromBytes(tweakBytes)
	btcec.ScalarBaseMultNonConst(&tweakKey.Key, &tweakJacobian)

	pubKey.AsJacobian(&pubKeyJacobian)
	btcec.AddNonConst(&pubKeyJacobian, &tweakJacobian, &resultJacobian)

	resultJacobian.ToAffine()
	return btcec.NewPublicKey(&resultJacobian.X, &resultJacobian.Y)
}

// TweakPrivKey tweaks the private key of a public base point given a per
// commitment point. The per commitment secret is the revealed revocation
// secret for the commitment state in question. This private key will only need
// to be generated in the case that a channel counter party broadcasts a
// revoked state. Precisely, the following operation is used to derive a
// tweaked private key:
//
//   - tweakPriv := basePriv + sha256(commitment || basePub) mod N
//
// Where N is the order of the sub-group.
func TweakPrivKey(basePriv *btcec.PrivateKey,
	commitTweak []byte) *btcec.PrivateKey {

	// tweakInt := sha256(commitPoint || basePub)
	tweakScalar := new(btcec.ModNScalar)
	tweakScalar.SetByteSlice(commitTweak)

	tweakScalar.Add(&basePriv.Key)

	return &btcec.PrivateKey{Key: *tweakScalar}
}

// DeriveRevocationPubkey derives the revocation public key given the
// counterparty's commitment key, and revocation preimage derived via a
// pseudo-random-function. In the event that we (for some reason) broadcast a
// revoked commitment transaction, then if the other party knows the revocation
// preimage, then they'll be able to derive the corresponding private key to
// this private key by exploiting the homomorphism in the elliptic curve group:
//   - https://en.wikipedia.org/wiki/Group_homomorphism#Homomorphisms_of_abelian_groups
//
// The derivation is performed as follows:
//
//	revokeKey := revokeBase * sha256(revocationBase || commitPoint) +
//	             commitPoint * sha256(commitPoint || revocationBase)
//
//	          := G*(revokeBasePriv * sha256(revocationBase || commitPoint)) +
//	             G*(commitSecret * sha256(commitPoint || revocationBase))
//
//	          := G*(revokeBasePriv * sha256(revocationBase || commitPoint) +
//	                commitSecret * sha256(commitPoint || revocationBase))
//
// Therefore, once we divulge the revocation secret, the remote peer is able to
// compute the proper private key for the revokeKey by computing:
//
//	revokePriv := (revokeBasePriv * sha256(revocationBase || commitPoint)) +
//	              (commitSecret * sha256(commitPoint || revocationBase)) mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPubkey(revokeBase,
	commitPoint *btcec.PublicKey) *btcec.PublicKey {

	// R = revokeBase * sha256(revocationBase || commitPoint)
	revokeTweakBytes := SingleTweakBytes(revokeBase, commitPoint)
	revokeTweakScalar := new(btcec.ModNScalar)
	revokeTweakScalar.SetByteSlice(revokeTweakBytes)

	var (
		revokeBaseJacobian btcec.JacobianPoint
		rJacobian          btcec.JacobianPoint
	)
	revokeBase.AsJacobian(&revokeBaseJacobian)
	btcec.ScalarMultNonConst(
		revokeTweakScalar, &revokeBaseJacobian, &rJacobian,
	)

	// C = commitPoint * sha256(commitPoint || revocationBase)
	commitTweakBytes := SingleTweakBytes(commitPoint, revokeBase)
	commitTweakScalar := new(btcec.ModNScalar)
	commitTweakScalar.SetByteSlice(commitTweakBytes)

	var (
		commitPointJacobian btcec.JacobianPoint
		cJacobian           btcec.JacobianPoint
	)
	commitPoint.AsJacobian(&commitPointJacobian)
	btcec.ScalarMultNonConst(
		commitTweakScalar, &commitPointJacobian, &cJacobian,
	)

	// Now that we have the revocation point, we add this to their commitment
	// public key in order to obtain the revocation public key.
	//
	// P = R + C
	var resultJacobian btcec.JacobianPoint
	btcec.AddNonConst(&rJacobian, &cJacobian, &resultJacobian)

	resultJacobian.ToAffine()
	return btcec.NewPublicKey(&resultJacobian.X, &resultJacobian.Y)
}

// DeriveRevocationPrivKey derives the revocation private key given a node's
// commitment private key, and the preimage to a previously seen revocation
// hash. Using this derived private key, a node is able to claim the output
// within the commitment transaction of a node in the case that they broadcast
// a previously revoked commitment transaction.
//
// The private key is derived as follows:
//
//	revokePriv := (revokeBasePriv * sha256(revocationBase || commitPoint)) +
//	              (commitSecret * sha256(commitPoint || revocationBase)) mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPrivKey(revokeBasePriv *btcec.PrivateKey,
	commitSecret *btcec.PrivateKey) *btcec.PrivateKey {

	// r = sha256(revokeBasePub || commitPoint)
	revokeTweakBytes := SingleTweakBytes(
		revokeBasePriv.PubKey(), commitSecret.PubKey(),
	)
	revokeTweakScalar := new(btcec.ModNScalar)
	revokeTweakScalar.SetByteSlice(revokeTweakBytes)

	// c = sha256(commitPoint || revokeBasePub)
	commitTweakBytes := SingleTweakBytes(
		commitSecret.PubKey(), revokeBasePriv.PubKey(),
	)
	commitTweakScalar := new(btcec.ModNScalar)
	commitTweakScalar.SetByteSlice(commitTweakBytes)

	// Finally to derive the revocation secret key we'll perform the
	// following operation:
	//
	//  k = (revocationPriv * r) + (commitSecret * c) mod N
	//
	// This works since:
	//  P = (G*a)*b + (G*c)*d
	//  P = G*(a*b) + G*(c*d)
	//  P = G*(a*b + c*d)
	revokeHalfPriv := revokeTweakScalar.Mul(&revokeBasePriv.Key)
	commitHalfPriv := commitTweakScalar.Mul(&commitSecret.Key)

	revocationPriv := revokeHalfPriv.Add(commitHalfPriv)

	return &btcec.PrivateKey{Key: *revocationPriv}
}

// ComputeCommitmentPoint generates a commitment point given a commitment
// secret. The commitment point for each state is used to randomize each key in
// the key-ring and also to used as a tweak to derive new public+private keys
// for the state.
func ComputeCommitmentPoint(commitSecret []byte) *btcec.PublicKey {
	_, pubKey := btcec.PrivKeyFromBytes(commitSecret)
	return pubKey
}
