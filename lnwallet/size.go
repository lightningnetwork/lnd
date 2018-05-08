package lnwallet

import (
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/wire"
)

const (
	// CommitWeight is the weight of the base commitment transaction which
	// includes: one p2wsh input, out p2wkh output, and one p2wsh output.
	CommitWeight int64 = 724

	// HtlcWeight is the weight of an HTLC output.
	HtlcWeight int64 = 172
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

	// P2WSHSize 34 bytes
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (WitnessScriptSHA256 length)
	//	- WitnessScriptSHA256: 32 bytes
	P2WSHSize = 1 + 1 + 32

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

	// WitnessSize 222 bytes
	//	- NumberOfWitnessElements: 1 byte
	//	- NilLength: 1 byte
	//	- sigAliceLength: 1 byte
	//	- sigAlice: 73 bytes
	//	- sigBobLength: 1 byte
	//	- sigBob: 73 bytes
	//	- WitnessScriptLength: 1 byte
	//	- WitnessScript (MultiSig)
	WitnessSize = 1 + 1 + 1 + 73 + 1 + 73 + 1 + MultiSigSize

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
	// - Version: 4 bytes
	// - LockTime: 4 bytes
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
	WitnessCommitmentTxWeight = WitnessHeaderSize + WitnessSize

	// HTLCWeight 172 weight
	HTLCWeight = witnessScaleFactor * HTLCSize

	// HtlcTimeoutWeight is the weight of the HTLC timeout transaction
	// which will transition an outgoing HTLC to the delay-and-claim state.
	HtlcTimeoutWeight = 663

	// HtlcSuccessWeight is the weight of the HTLC success transaction
	// which will transition an incoming HTLC to the delay-and-claim state.
	HtlcSuccessWeight = 703

	// MaxHTLCNumber is the maximum number HTLCs which can be included in a
	// commitment transaction. This limit was chosen such that, in the case
	// of a contract breach, the punishment transaction is able to sweep
	// all the HTLC's yet still remain below the widely used standard
	// weight limits.
	MaxHTLCNumber = 966

	// ToLocalScriptSize 83 bytes
	//      - OP_IF: 1 byte
	//              - OP_DATA: 1 byte (revocationkey length)
	//              - revocationkey: 33 bytes
	//              - OP_CHECKSIG: 1 byte
	//      - OP_ELSE: 1 byte
	//              - OP_DATA: 1 byte (localkey length)
	//              - local_delay_key: 33 bytes
	//              - OP_CHECKSIG_VERIFY: 1 byte
	//              - OP_DATA: 1 byte (delay length)
	//              - delay: 8 bytes
	//              -OP_CHECKSEQUENCEVERIFY: 1 byte
	//      - OP_ENDIF: 1 byte
	ToLocalScriptSize = 1 + 1 + 33 + 1 + 1 + 1 + 33 + 1 + 1 + 8 + 1 + 1

	// ToLocalTimeoutWitnessSize x bytes
	//     - number_of_witness_elements: 1 byte
	//     - local_delay_sig_length: 1 byte
	//     - local_delay_sig: 73 bytes
	//     - zero_length: 1 byte
	//     - witness_script_length: 1 byte
	//     - witness_script (to_local_script)
	ToLocalTimeoutWitnessSize = 1 + 1 + 73 + 1 + 1 + ToLocalScriptSize

	// ToLocalPenaltyWitnessSize 160 bytes
	//      - number_of_witness_elements: 1 byte
	//      - revocation_sig_length: 1 byte
	//      - revocation_sig: 73 bytes
	//      - one_length: 1 byte
	//      - witness_script_length: 1 byte
	//      - witness_script (to_local_script)
	ToLocalPenaltyWitnessSize = 1 + 1 + 73 + 1 + 1 + ToLocalScriptSize

	// SecondLevelHtlcScriptSize 73 bytes
	//      - OP_IF: 1 byte
	//          - OP_DATA: 1 byte
	//          - revoke_key: 33 bytes
	//      - OP_ELSE: 1 byte
	//          - csv_delay: 1 byte
	//          - OP_CHECKSEQUENCEVERIFY: 1 byte
	//          - OP_DROP: 1 byte
	//          - delay_key: 33 bytes
	//      - OP_ENDIF: 1 byte
	//      - OP_CHECKSIG: 1 byte
	SecondLevelHtlcScriptSize = 73

	// SecondLevelHtlcPenaltyWitnessSize 149 bytes
	//  - number_of_witness_elements: 1 byte
	//  - revoke_sig_length: 1 byte
	//  - revoke_sig: 73 bytes
	//  - OP_TRUE: 1 byte
	//  - witness_script (second_level_script_size)
	SecondLevelHtlcPenaltyWitnessSize = 1 + 1 + 73 + 1 + SecondLevelHtlcScriptSize

	// SecondLevelHtlcSuccessWitnessSize 149 bytes
	//  - number_of_witness_elements: 1 byte
	//  - success_sig_length: 1 byte
	//  - success_sig: 73 bytes
	//  - nil_length: 1 byte
	//  - witness_script (second_level_script_size)
	SecondLevelHtlcSuccessWitnessSize = 1 + 1 + 73 + 1 + SecondLevelHtlcScriptSize

	// AcceptedHtlcScriptSize 139 bytes
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
	//      - OP_ENDIF: 1 byte
	AcceptedHtlcScriptSize = 3*1 + 20 + 5*1 + 33 + 7*1 + 20 + 4*1 +
		33 + 5*1 + 4 + 5*1

	// AcceptedHtlcTimeoutWitnessSize 214
	//  - number_of_witness_elements: 1 byte
	//  - sender_sig: 73 bytes
	//  - nil_length: 1 byte
	//  - witness_script: (accepted_htlc_script)
	AcceptedHtlcTimeoutWitnessSize = 1 + 73 + 1 + AcceptedHtlcScriptSize

	// AcceptedHtlcSuccessWitnessSize 325 bytes
	//    - number_of_witness_elements: 1 byte
	//    - nil_length: 1 byte
	//    - sig_alice_length: 1 byte
	//    - sig_alice: 73 bytes
	//    - sig_bob_length: 1 byte
	//    - sig_bob: 73 bytes
	//    - preimage_length: 1 byte
	//    - preimage: 32 bytes
	//    - witness_script_length: 1 byte
	//    - witness_script (accepted_htlc_script)
	AcceptedHtlcSuccessWitnessSize = 1 + 1 + 73 + 1 + 73 + 1 + 32 + 1 + AcceptedHtlcScriptSize

	// AcceptedHtlcPenaltyWitnessSize 249 bytes
	//    - number_of_witness_elements: 1 byte
	//    - revocation_sig_length: 1 byte
	//    - revocation_sig: 73 bytes
	//    - revocation_key_length: 1 byte
	//    - revocation_key: 33 bytes
	//    - witness_script_length: 1 byte
	//    - witness_script (accepted_htlc_script)
	AcceptedHtlcPenaltyWitnessSize = 1 + 1 + 73 + 1 + 33 + 1 + AcceptedHtlcScriptSize

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
	//      - OP_ENDIF: 1 byte
	OfferedHtlcScriptSize = 3*1 + 20 + 5*1 + 33 + 10*1 + 33 + 5*1 + 20 + 4*1

	// OfferedHtlcTimeoutWitnessSize 285 bytes
	// - number_of_witness_elements: 1 byte
	// - nil_length: 1 byte
	// - sig_alice_length: 1 byte
	// - sig_alice: 73 bytes
	// - sig_bob_length: 1 byte
	// - sig_bob: 73 bytes
	// - nil_length: 1 byte
	// - witness_script_length: 1 byte
	// - witness_script (offered_htlc_script)
	OfferedHtlcTimeoutWitnessSize = 1 + 1 + 1 + 73 + 1 + 73 + 1 + 1 + OfferedHtlcScriptSize

	// OfferedHtlcSuccessWitnessSize 283 bytes
	// - number_of_witness_elements: 1 byte
	// - nil_length: 1 byte
	// - receiver_sig: 73 bytes
	// - sender_sigs: 73 bytes
	// - payment_preimage: 32 bytes
	// - witness_script_length: 1 byte
	// - witness_script (offered_htlc_script)
	OfferedHtlcSuccessWitnessSize = 1 + 1 + 73 + 73 + 73 + 32 + 1 + OfferedHtlcScriptSize

	// OfferedHtlcPenaltyWitnessSize 243 bytes
	//      - number_of_witness_elements: 1 byte
	//      - revocation_sig_length: 1 byte
	//      - revocation_sig: 73 bytes
	//      - revocation_key_length: 1 byte
	//      - revocation_key: 33 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (offered_htlc_script)
	OfferedHtlcPenaltyWitnessSize = 1 + 1 + 73 + 1 + 1 + OfferedHtlcScriptSize
)

// estimateCommitTxWeight estimate commitment transaction weight depending on
// the precalculated weight of base transaction, witness data, which is needed
// for paying for funding tx, and htlc weight multiplied by their count.
func estimateCommitTxWeight(count int, prediction bool) int64 {
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
	twe.inputSize += InputSize + P2WPKHSize
	twe.inputWitnessSize += P2WKHWitnessSize
	twe.inputSize++
	twe.hasWitness = true

	return twe
}

// AddNestedP2WSHInput updates the weight estimate to account for an additional
// input spending a P2SH output with a nested P2WSH redeem script.
func (twe *TxWeightEstimator) AddNestedP2WSHInput(witnessSize int) *TxWeightEstimator {
	twe.inputSize += InputSize + P2WSHSize
	twe.inputWitnessSize += witnessSize
	twe.inputSize++
	twe.hasWitness = true

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
