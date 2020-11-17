package input

import (
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/wire"
)

const (
	// witnessScaleFactor determines the level of "discount" witness data
	// receives compared to "base" data. A scale factor of 4, denotes that
	// witness data is 1/4 as cheap as regular non-witness data. Value copied
	// here for convenience.
	witnessScaleFactor = blockchain.WitnessScaleFactor

	// The weight(weight), which is different from the !size! (see BIP-141),
	// is calculated as:
	// Weight = 4 * BaseSize + WitnessSize (weight).
	// BaseSize - size of the transaction without witness data (bytes).
	// WitnessSize - witness size (bytes).
	// Weight - the metric for determining the weight of the transaction.

	// P2WPKHSize 22 bytes
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (PublicKeyHASH160 length)
	//	- PublicKeyHASH160: 20 bytes
	P2WPKHSize = 1 + 1 + 20

	// NestedP2WPKHSize 23 bytes
	//      - OP_DATA: 1 byte (P2WPKHSize)
	//      - P2WPKHWitnessProgram: 22 bytes
	NestedP2WPKHSize = 1 + P2WPKHSize

	// P2WSHSize 34 bytes
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (WitnessScriptSHA256 length)
	//	- WitnessScriptSHA256: 32 bytes
	P2WSHSize = 1 + 1 + 32

	// NestedP2WSHSize 35 bytes
	//      - OP_DATA: 1 byte (P2WSHSize)
	//      - P2WSHWitnessProgram: 34 bytes
	NestedP2WSHSize = 1 + P2WSHSize

	// P2PKHOutputSize 34 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2pkh): 25 bytes
	P2PKHOutputSize = 8 + 1 + 25

	// P2WKHOutputSize 31 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2wpkh): 22 bytes
	P2WKHOutputSize = 8 + 1 + P2WPKHSize

	// P2WSHOutputSize 43 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2wsh): 34 bytes
	P2WSHOutputSize = 8 + 1 + P2WSHSize

	// P2SHOutputSize 32 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2sh): 23 bytes
	P2SHOutputSize = 8 + 1 + 23

	// P2PKHScriptSigSize 108 bytes
	//      - OP_DATA: 1 byte (signature length)
	//      - signature
	//      - OP_DATA: 1 byte (pubkey length)
	//      - pubkey
	P2PKHScriptSigSize = 1 + 73 + 1 + 33

	// P2WKHWitnessSize 109 bytes
	//      - number_of_witness_elements: 1 byte
	//      - signature_length: 1 byte
	//      - signature
	//      - pubkey_length: 1 byte
	//      - pubkey
	P2WKHWitnessSize = 1 + 1 + 73 + 1 + 33

	// MultiSigSize 71 bytes
	//	- OP_2: 1 byte
	//	- OP_DATA: 1 byte (pubKeyAlice length)
	//	- pubKeyAlice: 33 bytes
	//	- OP_DATA: 1 byte (pubKeyBob length)
	//	- pubKeyBob: 33 bytes
	//	- OP_2: 1 byte
	//	- OP_CHECKMULTISIG: 1 byte
	MultiSigSize = 1 + 1 + 33 + 1 + 33 + 1 + 1

	// MultiSigWitnessSize 222 bytes
	//	- NumberOfWitnessElements: 1 byte
	//	- NilLength: 1 byte
	//	- sigAliceLength: 1 byte
	//	- sigAlice: 73 bytes
	//	- sigBobLength: 1 byte
	//	- sigBob: 73 bytes
	//	- WitnessScriptLength: 1 byte
	//	- WitnessScript (MultiSig)
	MultiSigWitnessSize = 1 + 1 + 1 + 73 + 1 + 73 + 1 + MultiSigSize

	// InputSize 41 bytes
	//	- PreviousOutPoint:
	//		- Hash: 32 bytes
	//		- Index: 4 bytes
	//	- OP_DATA: 1 byte (ScriptSigLength)
	//	- ScriptSig: 0 bytes
	//	- Witness <----	we use "Witness" instead of "ScriptSig" for
	// 			transaction validation, but "Witness" is stored
	// 			separately and weight for it size is smaller. So
	// 			we separate the calculation of ordinary data
	// 			from witness data.
	//	- Sequence: 4 bytes
	InputSize = 32 + 4 + 1 + 4

	// FundingInputSize represents the size of an input to a funding
	// transaction, and is equivalent to the size of a standard segwit input
	// as calculated above.
	FundingInputSize = InputSize

	// CommitmentDelayOutput 43 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (P2WSH)
	CommitmentDelayOutput = 8 + 1 + P2WSHSize

	// CommitmentKeyHashOutput 31 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (P2WPKH)
	CommitmentKeyHashOutput = 8 + 1 + P2WPKHSize

	// CommitmentAnchorOutput 43 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (P2WSH)
	CommitmentAnchorOutput = 8 + 1 + P2WSHSize

	// HTLCSize 43 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (PW2SH)
	HTLCSize = 8 + 1 + P2WSHSize

	// WitnessHeaderSize 2 bytes
	//	- Flag: 1 byte
	//	- Marker: 1 byte
	WitnessHeaderSize = 1 + 1

	// BaseTxSize 8 bytes
	//      - Version: 4 bytes
	//      - LockTime: 4 bytes
	BaseTxSize = 4 + 4

	// BaseCommitmentTxSize 125 + 43 * num-htlc-outputs bytes
	//	- Version: 4 bytes
	//	- WitnessHeader <---- part of the witness data
	//	- CountTxIn: 1 byte
	//	- TxIn: 41 bytes
	//		FundingInput
	//	- CountTxOut: 1 byte
	//	- TxOut: 74 + 43 * num-htlc-outputs bytes
	//		OutputPayingToThem,
	//		OutputPayingToUs,
	//		....HTLCOutputs...
	//	- LockTime: 4 bytes
	BaseCommitmentTxSize = 4 + 1 + FundingInputSize + 1 +
		CommitmentDelayOutput + CommitmentKeyHashOutput + 4

	// BaseCommitmentTxWeight 500 weight
	BaseCommitmentTxWeight = witnessScaleFactor * BaseCommitmentTxSize

	// WitnessCommitmentTxWeight 224 weight
	WitnessCommitmentTxWeight = WitnessHeaderSize + MultiSigWitnessSize

	// BaseAnchorCommitmentTxSize 225 + 43 * num-htlc-outputs bytes
	//	- Version: 4 bytes
	//	- WitnessHeader <---- part of the witness data
	//	- CountTxIn: 1 byte
	//	- TxIn: 41 bytes
	//		FundingInput
	//	- CountTxOut: 3 byte
	//	- TxOut: 4*43 + 43 * num-htlc-outputs bytes
	//		OutputPayingToThem,
	//		OutputPayingToUs,
	//		AnchorPayingToThem,
	//		AnchorPayingToUs,
	//		....HTLCOutputs...
	//	- LockTime: 4 bytes
	BaseAnchorCommitmentTxSize = 4 + 1 + FundingInputSize + 3 +
		2*CommitmentDelayOutput + 2*CommitmentAnchorOutput + 4

	// BaseAnchorCommitmentTxWeight 900 weight
	BaseAnchorCommitmentTxWeight = witnessScaleFactor * BaseAnchorCommitmentTxSize

	// CommitWeight 724 weight
	CommitWeight = BaseCommitmentTxWeight + WitnessCommitmentTxWeight

	// AnchorCommitWeight 1124 weight
	AnchorCommitWeight = BaseAnchorCommitmentTxWeight + WitnessCommitmentTxWeight

	// HTLCWeight 172 weight
	HTLCWeight = witnessScaleFactor * HTLCSize

	// HtlcTimeoutWeight is the weight of the HTLC timeout transaction
	// which will transition an outgoing HTLC to the delay-and-claim state.
	HtlcTimeoutWeight = 663

	// HtlcSuccessWeight is the weight of the HTLC success transaction
	// which will transition an incoming HTLC to the delay-and-claim state.
	HtlcSuccessWeight = 703

	// HtlcConfirmedScriptOverhead is the extra length of an HTLC script
	// that requires confirmation before it can be spent. These extra bytes
	// is a result of the extra CSV check.
	HtlcConfirmedScriptOverhead = 3

	// HtlcTimeoutWeightConfirmed is the weight of the HTLC timeout
	// transaction which will transition an outgoing HTLC to the
	// delay-and-claim state, for the confirmed HTLC outputs. It is 3 bytes
	// larger because of the additional CSV check in the input script.
	HtlcTimeoutWeightConfirmed = HtlcTimeoutWeight + HtlcConfirmedScriptOverhead

	// HtlcSuccessWeightCOnfirmed is the weight of the HTLC success
	// transaction which will transition an incoming HTLC to the
	// delay-and-claim state, for the confirmed HTLC outputs. It is 3 bytes
	// larger because of the cdditional CSV check in the input script.
	HtlcSuccessWeightConfirmed = HtlcSuccessWeight + HtlcConfirmedScriptOverhead

	// MaxHTLCNumber is the maximum number HTLCs which can be included in a
	// commitment transaction. This limit was chosen such that, in the case
	// of a contract breach, the punishment transaction is able to sweep
	// all the HTLC's yet still remain below the widely used standard
	// weight limits.
	MaxHTLCNumber = 966

	// ToLocalScriptSize 79 bytes
	//      - OP_IF: 1 byte
	//          - OP_DATA: 1 byte
	//          - revoke_key: 33 bytes
	//      - OP_ELSE: 1 byte
	//          - OP_DATA: 1 byte
	//          - csv_delay: 4 bytes
	//          - OP_CHECKSEQUENCEVERIFY: 1 byte
	//          - OP_DROP: 1 byte
	//          - OP_DATA: 1 byte
	//          - delay_key: 33 bytes
	//      - OP_ENDIF: 1 byte
	//      - OP_CHECKSIG: 1 byte
	ToLocalScriptSize = 1 + 1 + 33 + 1 + 1 + 4 + 1 + 1 + 1 + 33 + 1 + 1

	// ToLocalTimeoutWitnessSize 156 bytes
	//      - number_of_witness_elements: 1 byte
	//      - local_delay_sig_length: 1 byte
	//      - local_delay_sig: 73 bytes
	//      - zero_length: 1 byte
	//      - witness_script_length: 1 byte
	//      - witness_script (to_local_script)
	ToLocalTimeoutWitnessSize = 1 + 1 + 73 + 1 + 1 + ToLocalScriptSize

	// ToLocalPenaltyWitnessSize 157 bytes
	//      - number_of_witness_elements: 1 byte
	//      - revocation_sig_length: 1 byte
	//      - revocation_sig: 73 bytes
	//      - OP_TRUE_length: 1 byte
	//      - OP_TRUE: 1 byte
	//      - witness_script_length: 1 byte
	//      - witness_script (to_local_script)
	ToLocalPenaltyWitnessSize = 1 + 1 + 73 + 1 + 1 + 1 + ToLocalScriptSize

	// ToRemoteConfirmedScriptSize 37 bytes
	//      - OP_DATA: 1 byte
	//      - to_remote_key: 33 bytes
	//      - OP_CHECKSIGVERIFY: 1 byte
	//      - OP_1: 1 byte
	//      - OP_CHECKSEQUENCEVERIFY: 1 byte
	ToRemoteConfirmedScriptSize = 1 + 33 + 1 + 1 + 1

	// ToRemoteConfirmedWitnessSize 113 bytes
	//      - number_of_witness_elements: 1 byte
	//      - sig_length: 1 byte
	//      - sig: 73 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (to_remote_delayed_script)
	ToRemoteConfirmedWitnessSize = 1 + 1 + 73 + 1 + ToRemoteConfirmedScriptSize

	// AcceptedHtlcScriptSize 140 bytes
	//      - OP_DUP: 1 byte
	//      - OP_HASH160: 1 byte
	//      - OP_DATA: 1 byte (RIPEMD160(SHA256(revocationkey)) length)
	//      - RIPEMD160(SHA256(revocationkey)): 20 bytes
	//      - OP_EQUAL: 1 byte
	//      - OP_IF: 1 byte
	//              - OP_CHECKSIG: 1 byte
	//      - OP_ELSE: 1 byte
	//              - OP_DATA: 1 byte (remotekey length)
	//              - remotekey: 33 bytes
	//              - OP_SWAP: 1 byte
	//              - OP_SIZE: 1 byte
	//              - OP_DATA: 1 byte (32 length)
	//              - 32: 1 byte
	//              - OP_EQUAL: 1 byte
	//              - OP_IF: 1 byte
	//                      - OP_HASH160: 1 byte
	//                      - OP_DATA: 1 byte (RIPEMD160(payment_hash) length)
	//                      - RIPEMD160(payment_hash): 20 bytes
	//                      - OP_EQUALVERIFY: 1 byte
	//                      - 2: 1 byte
	//                      - OP_SWAP: 1 byte
	//                      - OP_DATA: 1 byte (localkey length)
	//                      - localkey: 33 bytes
	//                      - 2: 1 byte
	//                      - OP_CHECKMULTISIG: 1 byte
	//              - OP_ELSE: 1 byte
	//                      - OP_DROP: 1 byte
	//                      - OP_DATA: 1 byte (cltv_expiry length)
	//                      - cltv_expiry: 4 bytes
	//                      - OP_CHECKLOCKTIMEVERIFY: 1 byte
	//                      - OP_DROP: 1 byte
	//                      - OP_CHECKSIG: 1 byte
	//              - OP_ENDIF: 1 byte
	//              - OP_1: 1 byte		// These 3 extra bytes are only
	//              - OP_CSV: 1 byte	// present for the confirmed
	//              - OP_DROP: 1 byte	// HTLC script types.
	//      - OP_ENDIF: 1 byte
	AcceptedHtlcScriptSize = 3*1 + 20 + 5*1 + 33 + 8*1 + 20 + 4*1 +
		33 + 5*1 + 4 + 5*1

	// AcceptedHtlcScriptSizeConfirmed 143 bytes
	AcceptedHtlcScriptSizeConfirmed = AcceptedHtlcScriptSize +
		HtlcConfirmedScriptOverhead

	// AcceptedHtlcTimeoutWitnessSize 216
	//      - number_of_witness_elements: 1 byte
	//      - sender_sig_length: 1 byte
	//      - sender_sig: 73 bytes
	//      - nil_length: 1 byte
	//      - witness_script_length: 1 byte
	//      - witness_script: (accepted_htlc_script)
	AcceptedHtlcTimeoutWitnessSize = 1 + 1 + 73 + 1 + 1 + AcceptedHtlcScriptSize

	// AcceptedHtlcTimeoutWitnessSizeConfirmed 219 bytes
	AcceptedHtlcTimeoutWitnessSizeConfirmed = 1 + 1 + 73 + 1 + 1 +
		AcceptedHtlcScriptSizeConfirmed

	// AcceptedHtlcPenaltyWitnessSize 249 bytes
	//      - number_of_witness_elements: 1 byte
	//      - revocation_sig_length: 1 byte
	//      - revocation_sig: 73 bytes
	//      - revocation_key_length: 1 byte
	//      - revocation_key: 33 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (accepted_htlc_script)
	AcceptedHtlcPenaltyWitnessSize = 1 + 1 + 73 + 1 + 33 + 1 + AcceptedHtlcScriptSize

	// AcceptedHtlcPenaltyWitnessSizeConfirmed 252 bytes
	AcceptedHtlcPenaltyWitnessSizeConfirmed = 1 + 1 + 73 + 1 + 33 + 1 +
		AcceptedHtlcScriptSizeConfirmed

	// AcceptedHtlcSuccessWitnessSize 319 bytes
	//      - number_of_witness_elements: 1 byte
	//      - nil_length: 1 byte
	//      - sig_alice_length: 1 byte
	//      - sig_alice: 73 bytes
	//      - sig_bob_length: 1 byte
	//      - sig_bob: 73 bytes
	//      - preimage_length: 1 byte
	//      - preimage: 32 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (accepted_htlc_script)
	//
	// Input to second level success tx, spending non-delayed HTLC output.
	AcceptedHtlcSuccessWitnessSize = 1 + 1 + 1 + 73 + 1 + 73 + 1 + 32 + 1 +
		AcceptedHtlcScriptSize

	// AcceptedHtlcSuccessWitnessSizeConfirmed 322 bytes
	//
	// Input to second level success tx, spending 1 CSV delayed HTLC output.
	AcceptedHtlcSuccessWitnessSizeConfirmed = 1 + 1 + 1 + 73 + 1 + 73 + 1 + 32 + 1 +
		AcceptedHtlcScriptSizeConfirmed

	// OfferedHtlcScriptSize 133 bytes
	//      - OP_DUP: 1 byte
	//      - OP_HASH160: 1 byte
	//      - OP_DATA: 1 byte (RIPEMD160(SHA256(revocationkey)) length)
	//      - RIPEMD160(SHA256(revocationkey)): 20 bytes
	//      - OP_EQUAL: 1 byte
	//      - OP_IF: 1 byte
	//              - OP_CHECKSIG: 1 byte
	//      - OP_ELSE: 1 byte
	//              - OP_DATA: 1 byte (remotekey length)
	//              - remotekey: 33 bytes
	//              - OP_SWAP: 1 byte
	//              - OP_SIZE: 1 byte
	//              - OP_DATA: 1 byte (32 length)
	//              - 32: 1 byte
	//              - OP_EQUAL: 1 byte
	//              - OP_NOTIF: 1 byte
	//                      - OP_DROP: 1 byte
	//                      - 2: 1 byte
	//                      - OP_SWAP: 1 byte
	//                      - OP_DATA: 1 byte (localkey length)
	//                      - localkey: 33 bytes
	//                      - 2: 1 byte
	//                      - OP_CHECKMULTISIG: 1 byte
	//              - OP_ELSE: 1 byte
	//                      - OP_HASH160: 1 byte
	//                      - OP_DATA: 1 byte (RIPEMD160(payment_hash) length)
	//                      - RIPEMD160(payment_hash): 20 bytes
	//                      - OP_EQUALVERIFY: 1 byte
	//                      - OP_CHECKSIG: 1 byte
	//              - OP_ENDIF: 1 byte
	//              - OP_1: 1 byte		// These 3 extra bytes are only
	//              - OP_CSV: 1 byte	// present for the confirmed
	//              - OP_DROP: 1 byte	// HTLC script types.
	//      - OP_ENDIF: 1 byte
	OfferedHtlcScriptSize = 3*1 + 20 + 5*1 + 33 + 10*1 + 33 + 5*1 + 20 + 4*1

	// OfferedHtlcScriptSizeConfirmed 136 bytes
	OfferedHtlcScriptSizeConfirmed = OfferedHtlcScriptSize +
		HtlcConfirmedScriptOverhead

	// OfferedHtlcSuccessWitnessSize 242 bytes
	//      - number_of_witness_elements: 1 byte
	//      - receiver_sig_length: 1 byte
	//      - receiver_sig: 73 bytes
	//      - payment_preimage_length: 1 byte
	//      - payment_preimage: 32 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (offered_htlc_script)
	OfferedHtlcSuccessWitnessSize = 1 + 1 + 73 + 1 + 32 + 1 + OfferedHtlcScriptSize

	// OfferedHtlcSuccessWitnessSizeConfirmed 245 bytes
	OfferedHtlcSuccessWitnessSizeConfirmed = 1 + 1 + 73 + 1 + 32 + 1 +
		OfferedHtlcScriptSizeConfirmed

	// OfferedHtlcTimeoutWitnessSize 282 bytes
	//      - number_of_witness_elements: 1 byte
	//      - nil_length: 1 byte
	//      - sig_alice_length: 1 byte
	//      - sig_alice: 73 bytes
	//      - sig_bob_length: 1 byte
	//      - sig_bob: 73 bytes
	//      - nil_length: 1 byte
	//      - witness_script_length: 1 byte
	//      - witness_script (offered_htlc_script)
	//
	// Input to second level timeout tx, spending non-delayed HTLC output.
	OfferedHtlcTimeoutWitnessSize = 1 + 1 + 1 + 73 + 1 + 73 + 1 + 1 +
		OfferedHtlcScriptSize

	// OfferedHtlcTimeoutWitnessSizeConfirmed 285 bytes
	//
	// Input to second level timeout tx, spending 1 CSV delayed HTLC output.
	OfferedHtlcTimeoutWitnessSizeConfirmed = 1 + 1 + 1 + 73 + 1 + 73 + 1 + 1 +
		OfferedHtlcScriptSizeConfirmed

	// OfferedHtlcPenaltyWitnessSize 243 bytes
	//      - number_of_witness_elements: 1 byte
	//      - revocation_sig_length: 1 byte
	//      - revocation_sig: 73 bytes
	//      - revocation_key_length: 1 byte
	//      - revocation_key: 33 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (offered_htlc_script)
	OfferedHtlcPenaltyWitnessSize = 1 + 1 + 73 + 1 + 33 + 1 + OfferedHtlcScriptSize

	// OfferedHtlcPenaltyWitnessSizeConfirmed 246 bytes
	OfferedHtlcPenaltyWitnessSizeConfirmed = 1 + 1 + 73 + 1 + 33 + 1 +
		OfferedHtlcScriptSizeConfirmed

	// AnchorScriptSize 40 bytes
	//      - pubkey_length: 1 byte
	//      - pubkey: 33 bytes
	//      - OP_CHECKSIG: 1 byte
	//      - OP_IFDUP: 1 byte
	//      - OP_NOTIF: 1 byte
	//              - OP_16: 1 byte
	//              - OP_CSV 1 byte
	//      - OP_ENDIF: 1 byte
	AnchorScriptSize = 1 + 33 + 6*1

	// AnchorWitnessSize 116 bytes
	//      - number_of_witnes_elements: 1 byte
	//      - signature_length: 1 byte
	//      - signature: 73 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (anchor_script)
	AnchorWitnessSize = 1 + 1 + 73 + 1 + AnchorScriptSize
)

// EstimateCommitTxWeight estimate commitment transaction weight depending on
// the precalculated weight of base transaction, witness data, which is needed
// for paying for funding tx, and htlc weight multiplied by their count.
func EstimateCommitTxWeight(count int, prediction bool) int64 {
	// Make prediction about the size of commitment transaction with
	// additional HTLC.
	if prediction {
		count++
	}

	htlcWeight := int64(count * HTLCWeight)
	baseWeight := int64(BaseCommitmentTxWeight)
	witnessWeight := int64(WitnessCommitmentTxWeight)

	return htlcWeight + baseWeight + witnessWeight
}

// TxWeightEstimator is able to calculate weight estimates for transactions
// based on the input and output types. For purposes of estimation, all
// signatures are assumed to be of the maximum possible size, 73 bytes. Each
// method of the estimator returns an instance with the estimate applied. This
// allows callers to chain each of the methods
type TxWeightEstimator struct {
	hasWitness       bool
	inputCount       uint32
	outputCount      uint32
	inputSize        int
	inputWitnessSize int
	outputSize       int
}

// AddP2PKHInput updates the weight estimate to account for an additional input
// spending a P2PKH output.
func (twe *TxWeightEstimator) AddP2PKHInput() *TxWeightEstimator {
	twe.inputSize += InputSize + P2PKHScriptSigSize
	twe.inputWitnessSize++
	twe.inputCount++

	return twe
}

// AddP2WKHInput updates the weight estimate to account for an additional input
// spending a native P2PWKH output.
func (twe *TxWeightEstimator) AddP2WKHInput() *TxWeightEstimator {
	twe.AddWitnessInput(P2WKHWitnessSize)

	return twe
}

// AddWitnessInput updates the weight estimate to account for an additional
// input spending a native pay-to-witness output. This accepts the total size
// of the witness as a parameter.
func (twe *TxWeightEstimator) AddWitnessInput(witnessSize int) *TxWeightEstimator {
	twe.inputSize += InputSize
	twe.inputWitnessSize += witnessSize
	twe.inputCount++
	twe.hasWitness = true

	return twe
}

// AddNestedP2WKHInput updates the weight estimate to account for an additional
// input spending a P2SH output with a nested P2WKH redeem script.
func (twe *TxWeightEstimator) AddNestedP2WKHInput() *TxWeightEstimator {
	twe.inputSize += InputSize + NestedP2WPKHSize
	twe.inputWitnessSize += P2WKHWitnessSize
	twe.inputCount++
	twe.hasWitness = true

	return twe
}

// AddNestedP2WSHInput updates the weight estimate to account for an additional
// input spending a P2SH output with a nested P2WSH redeem script.
func (twe *TxWeightEstimator) AddNestedP2WSHInput(witnessSize int) *TxWeightEstimator {
	twe.inputSize += InputSize + NestedP2WSHSize
	twe.inputWitnessSize += witnessSize
	twe.inputCount++
	twe.hasWitness = true

	return twe
}

// AddTxOutput adds a known TxOut to the weight estimator.
func (twe *TxWeightEstimator) AddTxOutput(txOut *wire.TxOut) *TxWeightEstimator {
	twe.outputSize += txOut.SerializeSize()
	twe.outputCount++

	return twe
}

// AddP2PKHOutput updates the weight estimate to account for an additional P2PKH
// output.
func (twe *TxWeightEstimator) AddP2PKHOutput() *TxWeightEstimator {
	twe.outputSize += P2PKHOutputSize
	twe.outputCount++

	return twe
}

// AddP2WKHOutput updates the weight estimate to account for an additional
// native P2WKH output.
func (twe *TxWeightEstimator) AddP2WKHOutput() *TxWeightEstimator {
	twe.outputSize += P2WKHOutputSize
	twe.outputCount++

	return twe
}

// AddP2WSHOutput updates the weight estimate to account for an additional
// native P2WSH output.
func (twe *TxWeightEstimator) AddP2WSHOutput() *TxWeightEstimator {
	twe.outputSize += P2WSHOutputSize
	twe.outputCount++

	return twe
}

// AddP2SHOutput updates the weight estimate to account for an additional P2SH
// output.
func (twe *TxWeightEstimator) AddP2SHOutput() *TxWeightEstimator {
	twe.outputSize += P2SHOutputSize
	twe.outputCount++

	return twe
}

// Weight gets the estimated weight of the transaction.
func (twe *TxWeightEstimator) Weight() int {
	txSizeStripped := BaseTxSize +
		wire.VarIntSerializeSize(uint64(twe.inputCount)) + twe.inputSize +
		wire.VarIntSerializeSize(uint64(twe.outputCount)) + twe.outputSize
	weight := txSizeStripped * witnessScaleFactor
	if twe.hasWitness {
		weight += WitnessHeaderSize + twe.inputWitnessSize
	}
	return weight
}

// VSize gets the estimated virtual size of the transactions, in vbytes.
func (twe *TxWeightEstimator) VSize() int {
	// A tx's vsize is 1/4 of the weight, rounded up.
	return (twe.Weight() + witnessScaleFactor - 1) / witnessScaleFactor
}
