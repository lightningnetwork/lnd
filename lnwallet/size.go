package lnwallet

import (
	"github.com/roasbeef/btcd/blockchain"
)

const (
	// The weight(cost), which is different from the !size! (see BIP-141),
	// is calculated as:
	// Weight = 4 * BaseSize + WitnessSize (weight).
	// BaseSize - size of the transaction without witness data (bytes).
	// WitnessSize - witness size (bytes).
	// Weight - the metric for determining the cost of the transaction.

	// P2WSHSize 34 bytes
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (WitnessScriptSHA256 length)
	//	- WitnessScriptSHA256: 32 bytes
	P2WSHSize = 1 + 1 + 32

	// P2WPKHSize 22 bytes
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (PublicKeyHASH160 length)
	//	- PublicKeyHASH160: 20 bytes
	P2WPKHSize = 1 + 1 + 20

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

	// FundingInputSize 41 bytes
	//	- PreviousOutPoint:
	//		- Hash: 32 bytes
	//		- Index: 4 bytes
	//	- OP_DATA: 1 byte (ScriptSigLength)
	//	- ScriptSig: 0 bytes
	//	- Witness <----	we use "Witness" instead of "ScriptSig" for
	// 			transaction validation, but "Witness" is stored
	// 			separately and cost for it size is smaller. So
	// 			we separate the calculation of ordinary data
	// 			from witness data.
	//	- Sequence: 4 bytes
	FundingInputSize = 32 + 4 + 1 + 4

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

	// BaseCommitmentTxSize 125 43 * num-htlc-outputs bytes
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

	// BaseCommitmentTxCost 500 weight
	BaseCommitmentTxCost = blockchain.WitnessScaleFactor * BaseCommitmentTxSize

	// WitnessCommitmentTxCost 224 weight
	WitnessCommitmentTxCost = WitnessHeaderSize + WitnessSize

	// HTLCCost 172 weight
	HTLCCost = blockchain.WitnessScaleFactor * HTLCSize

	// MaxHTLCNumber shows as the maximum number HTLCs which can be
	// included in commitment transaction. This numbers was calculated by
	// Rusty Russel in "BOLT #5: Recommendations for On-chain Transaction
	// Handling", based on the fact that we need to sweep all HTLCs within
	// one penalty transaction.
	MaxHTLCNumber = 1253
)

// estimateCommitTxCost estimate commitment transaction cost depending on the
// precalculated cost of base transaction, witness data, which is needed for
// paying for funding tx, and htlc cost multiplied by their count.
func estimateCommitTxCost(count int, prediction bool) int64 {
	// Make prediction about the size of commitment transaction with
	// additional HTLC.
	if prediction {
		count++
	}

	htlcCost := int64(count * HTLCCost)
	baseCost := int64(BaseCommitmentTxCost)
	witnessCost := int64(WitnessCommitmentTxCost)

	return htlcCost + baseCost + witnessCost
}
