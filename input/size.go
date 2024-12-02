package input

import (
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/lntypes"
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

	// UnknownWitnessSize 42 bytes
	//      - OP_x: 1 byte
	//      - OP_DATA: 1 byte (max-size length)
	//      - max-size: 40 bytes
	UnknownWitnessSize = 1 + 1 + 40

	// BaseOutputSize 9 bytes
	//     - value: 8 bytes
	//     - var_int: 1 byte (pkscript_length)
	BaseOutputSize = 8 + 1

	// P2PKHSize 25 bytes.
	P2PKHSize = 25

	// P2PKHOutputSize 34 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2pkh): 25 bytes
	P2PKHOutputSize = BaseOutputSize + P2PKHSize

	// P2WKHOutputSize 31 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2wpkh): 22 bytes
	P2WKHOutputSize = BaseOutputSize + P2WPKHSize

	// P2WSHOutputSize 43 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2wsh): 34 bytes
	P2WSHOutputSize = BaseOutputSize + P2WSHSize

	// P2SHSize 23 bytes.
	P2SHSize = 23

	// P2SHOutputSize 32 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2sh): 23 bytes
	P2SHOutputSize = BaseOutputSize + P2SHSize

	// P2TRSize 34 bytes
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (x-only public key length)
	//	- x-only public key length: 32 bytes
	P2TRSize = 34

	// P2TROutputSize 43 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2tr): 34 bytes
	P2TROutputSize = BaseOutputSize + P2TRSize

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

	// FundingInputSize 41 bytes
	// FundingInputSize represents the size of an input to a funding
	// transaction, and is equivalent to the size of a standard segwit input
	// as calculated above.
	FundingInputSize = InputSize

	// CommitmentDelayOutput 43 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (P2WSH)
	CommitmentDelayOutput = 8 + 1 + P2WSHSize

	// TaprootCommitmentOutput 43 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (P2TR)
	TaprootCommitmentOutput = 8 + 1 + P2TRSize

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

	// TaprootCommitmentAnchorOutput 43 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (P2TR)
	TaprootCommitmentAnchorOutput = 8 + 1 + P2TRSize

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

	// BaseAnchorCommitmentTxWeight 900 weight.
	BaseAnchorCommitmentTxWeight = witnessScaleFactor * BaseAnchorCommitmentTxSize

	// BaseTaprootCommitmentTxWeight 225 + 43 * num-htlc-outputs bytes
	//	- Version: 4 bytes
	//	- WitnessHeader <---- part of the witness data
	//	- CountTxIn: 1 byte
	//	- TxIn: 41 bytes
	//		FundingInput
	//	- CountTxOut: 3 byte
	//	- TxOut: 172 + 43 * num-htlc-outputs bytes
	//		OutputPayingToThem,
	//		OutputPayingToUs,
	//		....HTLCOutputs...
	//	- LockTime: 4 bytes
	BaseTaprootCommitmentTxWeight = (4 + 1 + FundingInputSize + 3 +
		2*TaprootCommitmentOutput + 2*TaprootCommitmentAnchorOutput +
		4) * witnessScaleFactor

	// CommitWeight 724 weight.
	CommitWeight = BaseCommitmentTxWeight + WitnessCommitmentTxWeight

	// AnchorCommitWeight 1124 weight.
	AnchorCommitWeight = BaseAnchorCommitmentTxWeight + WitnessCommitmentTxWeight

	// TaprootCommitWeight 968 weight.
	TaprootCommitWeight = (BaseTaprootCommitmentTxWeight +
		WitnessHeaderSize + TaprootKeyPathWitnessSize)

	// HTLCWeight 172 weight.
	HTLCWeight = witnessScaleFactor * HTLCSize

	// HtlcTimeoutWeight 663 weight
	// HtlcTimeoutWeight is the weight of the HTLC timeout transaction
	// which will transition an outgoing HTLC to the delay-and-claim state.
	HtlcTimeoutWeight = 663

	// TaprootHtlcTimeoutWeight is the total weight of the taproot HTLC
	// timeout transaction.
	TaprootHtlcTimeoutWeight = 645

	// HtlcSuccessWeight 703 weight
	// HtlcSuccessWeight is the weight of the HTLC success transaction
	// which will transition an incoming HTLC to the delay-and-claim state.
	HtlcSuccessWeight = 703

	// TaprootHtlcSuccessWeight is the total weight of the taproot HTLC
	// success transaction.
	TaprootHtlcSuccessWeight = 705

	// HtlcConfirmedScriptOverhead 3 bytes
	// HtlcConfirmedScriptOverhead is the extra length of an HTLC script
	// that requires confirmation before it can be spent. These extra bytes
	// is a result of the extra CSV check.
	HtlcConfirmedScriptOverhead = 3

	// HtlcTimeoutWeightConfirmed 666 weight
	// HtlcTimeoutWeightConfirmed is the weight of the HTLC timeout
	// transaction which will transition an outgoing HTLC to the
	// delay-and-claim state, for the confirmed HTLC outputs. It is 3 bytes
	// larger because of the additional CSV check in the input script.
	HtlcTimeoutWeightConfirmed = HtlcTimeoutWeight + HtlcConfirmedScriptOverhead

	// HtlcSuccessWeightConfirmed 706 weight
	// HtlcSuccessWeightConfirmed is the weight of the HTLC success
	// transaction which will transition an incoming HTLC to the
	// delay-and-claim state, for the confirmed HTLC outputs. It is 3 bytes
	// larger because of the cdditional CSV check in the input script.
	HtlcSuccessWeightConfirmed = HtlcSuccessWeight + HtlcConfirmedScriptOverhead

	// MaxHTLCNumber 966
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

	// LeaseWitnessScriptSizeOverhead represents the size overhead in bytes
	// of the witness scripts used within script enforced lease commitments.
	// This overhead results from the additional CLTV clause required to
	// spend.
	//
	//	- OP_DATA: 1 byte
	// 	- lease_expiry: 4 bytes
	// 	- OP_CHECKLOCKTIMEVERIFY: 1 byte
	// 	- OP_DROP: 1 byte
	LeaseWitnessScriptSizeOverhead = 1 + 4 + 1 + 1

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

	// AcceptedHtlcScriptSizeConfirmed 143 bytes.
	AcceptedHtlcScriptSizeConfirmed = AcceptedHtlcScriptSize +
		HtlcConfirmedScriptOverhead

	// AcceptedHtlcTimeoutWitnessSize 217 bytes
	//      - number_of_witness_elements: 1 byte
	//      - sender_sig_length: 1 byte
	//      - sender_sig: 73 bytes
	//      - nil_length: 1 byte
	//      - witness_script_length: 1 byte
	//      - witness_script: (accepted_htlc_script)
	AcceptedHtlcTimeoutWitnessSize = 1 + 1 + 73 + 1 + 1 + AcceptedHtlcScriptSize

	// AcceptedHtlcTimeoutWitnessSizeConfirmed 220 bytes.
	AcceptedHtlcTimeoutWitnessSizeConfirmed = 1 + 1 + 73 + 1 + 1 +
		AcceptedHtlcScriptSizeConfirmed

	// AcceptedHtlcPenaltyWitnessSize 250 bytes
	//      - number_of_witness_elements: 1 byte
	//      - revocation_sig_length: 1 byte
	//      - revocation_sig: 73 bytes
	//      - revocation_key_length: 1 byte
	//      - revocation_key: 33 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (accepted_htlc_script)
	AcceptedHtlcPenaltyWitnessSize = 1 + 1 + 73 + 1 + 33 + 1 + AcceptedHtlcScriptSize

	// AcceptedHtlcPenaltyWitnessSizeConfirmed 253 bytes.
	AcceptedHtlcPenaltyWitnessSizeConfirmed = 1 + 1 + 73 + 1 + 33 + 1 +
		AcceptedHtlcScriptSizeConfirmed

	// AcceptedHtlcSuccessWitnessSize 324 bytes
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

	// AcceptedHtlcSuccessWitnessSizeConfirmed 327 bytes
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

	// OfferedHtlcScriptSizeConfirmed 136 bytes.
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

	// OfferedHtlcSuccessWitnessSizeConfirmed 245 bytes.
	OfferedHtlcSuccessWitnessSizeConfirmed = 1 + 1 + 73 + 1 + 32 + 1 +
		OfferedHtlcScriptSizeConfirmed

	// OfferedHtlcTimeoutWitnessSize 285 bytes
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

	// OfferedHtlcTimeoutWitnessSizeConfirmed 288 bytes
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

	// OfferedHtlcPenaltyWitnessSizeConfirmed 246 bytes.
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

	// TaprootSignatureWitnessSize 65 bytes
	//	- sigLength: 1 byte
	//	- sig: 64 bytes
	TaprootSignatureWitnessSize = 1 + 64

	// TaprootKeyPathWitnessSize 66 bytes
	//	- NumberOfWitnessElements: 1 byte
	//	- sigLength: 1 byte
	//	- sig: 64 bytes
	TaprootKeyPathWitnessSize = 1 + TaprootSignatureWitnessSize

	// TaprootKeyPathCustomSighashWitnessSize 67 bytes
	//	- NumberOfWitnessElements: 1 byte
	//	- sigLength: 1 byte
	//	- sig: 64 bytes
	//      - sighashFlag: 1 byte
	TaprootKeyPathCustomSighashWitnessSize = TaprootKeyPathWitnessSize + 1

	// TaprootBaseControlBlockWitnessSize 33 bytes
	//      - leafVersionAndParity: 1 byte
	//      - schnorrPubKey: 32 byte
	TaprootBaseControlBlockWitnessSize = 33

	// TaprootToLocalScriptSize
	//      - OP_DATA: 1 byte (pub key len)
	//      - local_key: 32 bytes
	//      - OP_CHECKSIG: 1 byte
	//      - OP_DATA: 1 byte (csv delay)
	//      - csv_delay: 4 bytes (worst case estimate)
	//      - OP_CSV: 1 byte
	//      - OP_DROP: 1 byte
	TaprootToLocalScriptSize = 41

	// TaprootToLocalWitnessSize: 175 bytes
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - script_len: 1 byte
	//      - taproot_to_local_script_size: 41 bytes
	//      - ctrl_block_len: 1 byte
	//      - base_control_block_size: 33 bytes
	//      - sibling_merkle_hash: 32 bytes
	TaprootToLocalWitnessSize = 1 + 1 + 65 + 1 + TaprootToLocalScriptSize +
		1 + TaprootBaseControlBlockWitnessSize + 32

	// TaprootToLocalRevokeScriptSize: 68 bytes
	//	- OP_DATA: 1 byte
	//	- local key: 32 bytes
	// 	- OP_DROP: 1 byte
	// 	- OP_DATA: 1 byte
	// 	- revocation key: 32 bytes
	// 	- OP_CHECKSIG: 1 byte
	TaprootToLocalRevokeScriptSize = 1 + 32 + 1 + 1 + 32 + 1

	// TaprootToLocalRevokeWitnessSize: 202 bytes
	// 	- NumberOfWitnessElements: 1 byte
	// 	- sigLength: 1 byte
	// 	- sweep sig: 65 bytes
	// 	- script len: 1 byte
	// 	- revocation script size: 68 bytes
	// 	- ctrl block size: 1 byte
	// 	- base control block: 33 bytes
	//	- merkle proof: 32
	TaprootToLocalRevokeWitnessSize = (1 + 1 + 65 + 1 +
		TaprootToLocalRevokeScriptSize + 1 + 33 + 32)

	// TaprootToRemoteScriptSize
	// 	- OP_DATA: 1 byte
	// 	- remote key: 32 bytes
	// 	- OP_CHECKSIG: 1 byte
	// 	- OP_1: 1 byte
	// 	- OP_CHECKSEQUENCEVERIFY: 1 byte
	// 	- OP_DROP: 1 byte
	TaprootToRemoteScriptSize = 1 + 32 + 1 + 1 + 1 + 1

	// TaprootToRemoteWitnessSize:
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - script_len: 1 byte
	//      - taproot_to_local_script_size: 36 bytes
	//      - ctrl_block_len: 1 byte
	//      - base_control_block_size: 33 bytes
	TaprootToRemoteWitnessSize = (1 + 1 + 65 + 1 +
		TaprootToRemoteScriptSize + 1 +
		TaprootBaseControlBlockWitnessSize)

	// TaprootAnchorWitnessSize: 67 bytes
	//
	// In this case, we use the custom sighash size to give the most
	// pessemistic estimate.
	TaprootAnchorWitnessSize = TaprootKeyPathCustomSighashWitnessSize

	// TaprootSecondLevelHtlcScriptSize: 41 bytes
	//      - OP_DATA: 1 byte (pub key len)
	//      - local_key: 32 bytes
	//      - OP_CHECKSIG: 1 byte
	//      - OP_DATA: 1 byte (csv delay)
	//      - csv_delay: 4 bytes (worst case)
	//      - OP_CSV: 1 byte
	//      - OP_DROP: 1 byte
	TaprootSecondLevelHtlcScriptSize = 1 + 32 + 1 + 1 + 4 + 1 + 1

	// TaprootSecondLevelHtlcWitnessSize:
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - script_len: 1 byte
	//      - taproot_second_level_htlc_script_size: 40 bytes
	//      - ctrl_block_len: 1 byte
	//      - base_control_block_size: 33 bytes
	TaprootSecondLevelHtlcWitnessSize = 1 + 1 + 65 + 1 +
		TaprootSecondLevelHtlcScriptSize + 1 +
		TaprootBaseControlBlockWitnessSize

	// TaprootSecondLevelRevokeWitnessSize
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//nolint:ll
	TaprootSecondLevelRevokeWitnessSize = TaprootKeyPathCustomSighashWitnessSize

	// TaprootAcceptedRevokeWitnessSize:
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//nolint:ll
	TaprootAcceptedRevokeWitnessSize = TaprootKeyPathCustomSighashWitnessSize

	// TaprootOfferedRevokeWitnessSize:
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	TaprootOfferedRevokeWitnessSize = TaprootKeyPathCustomSighashWitnessSize

	// TaprootHtlcOfferedRemoteTimeoutScriptSize: 42 bytes
	//	- OP_DATA: 1 byte (pub key len)
	//	- local_key: 32 bytes
	//	- OP_CHECKSIG: 1 byte
	//      - OP_1: 1 byte
	//      - OP_DROP: 1 byte
	//      - OP_CHECKSEQUENCEVERIFY: 1 byte
	//      - OP_DATA: 1 byte (cltv_expiry length)
	//      - cltv_expiry: 4 bytes
	//      - OP_CHECKLOCKTIMEVERIFY: 1 byte
	//      - OP_DROP: 1 byte
	TaprootHtlcOfferedRemoteTimeoutScriptSize = (1 + 32 + 1 + 1 + 1 + 1 +
		1 + 4 + 1 + 1)

	// TaprootHtlcOfferedRemoteTimeoutwitSize: 176 bytes
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - script_len: 1 byte
	//      - taproot_offered_htlc_script_size: 42 bytes
	//      - ctrl_block_len: 1 byte
	//      - base_control_block_size: 33 bytes
	//      - sibilng_merkle_proof: 32 bytes
	TaprootHtlcOfferedRemoteTimeoutWitnessSize = 1 + 1 + 65 + 1 +
		TaprootHtlcOfferedRemoteTimeoutScriptSize + 1 +
		TaprootBaseControlBlockWitnessSize + 32

	// TaprootHtlcOfferedLocalTmeoutScriptSize:
	//	- OP_DATA: 1 byte (pub key len)
	//	- local_key: 32 bytes
	//	- OP_CHECKSIGVERIFY: 1 byte
	//	- OP_DATA: 1 byte (pub key len)
	//	- remote_key: 32 bytes
	//	- OP_CHECKSIG: 1 byte
	TaprootHtlcOfferedLocalTimeoutScriptSize = 1 + 32 + 1 + 1 + 32 + 1

	// TaprootOfferedLocalTimeoutWitnessSize
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - script_len: 1 byte
	//      - taproot_offered_htlc_script_timeout_size:
	//      - ctrl_block_len: 1 byte
	//      - base_control_block_size: 33 bytes
	//      - sibilng_merkle_proof: 32 bytes
	TaprootOfferedLocalTimeoutWitnessSize = 1 + 1 + 65 + 1 + 65 + 1 +
		TaprootHtlcOfferedLocalTimeoutScriptSize + 1 +
		TaprootBaseControlBlockWitnessSize + 32

	// TaprootHtlcAcceptedRemoteSuccessScriptSize:
	//      - OP_SIZE: 1 byte
	//      - OP_DATA: 1 byte
	//      - 32: 1 byte
	//      - OP_EQUALVERIFY: 1 byte
	//      - OP_HASH160: 1 byte
	//      - OP_DATA: 1 byte (RIPEMD160(payment_hash) length)
	//      - RIPEMD160(payment_hash): 20 bytes
	//      - OP_EQUALVERIFY: 1 byte
	//	- OP_DATA: 1 byte (pub key len)
	//	- remote_key: 32 bytes
	//	- OP_CHECKSIG: 1 byte
	//      - OP_1: 1 byte
	//      - OP_CSV: 1 byte
	//      - OP_DROP: 1 byte
	TaprootHtlcAcceptedRemoteSuccessScriptSize = 1 + 1 + 1 + 1 + 1 + 1 +
		1 + 20 + 1 + 32 + 1 + 1 + 1 + 1

	// TaprootHtlcAcceptedRemoteSuccessScriptSize:
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - payment_preimage_length: 1 byte
	//      - payment_preimage: 32 bytes
	//      - script_len: 1 byte
	//      - taproot_offered_htlc_script_success_size:
	//      - ctrl_block_len: 1 byte
	//      - base_control_block_size: 33 bytes
	//      - sibilng_merkle_proof: 32 bytes
	TaprootHtlcAcceptedRemoteSuccessWitnessSize = 1 + 1 + 65 + 1 + 32 + 1 +
		TaprootHtlcAcceptedRemoteSuccessScriptSize + 1 +
		TaprootBaseControlBlockWitnessSize + 32

	// TaprootHtlcAcceptedLocalSuccessScriptSize:
	//      - OP_SIZE: 1 byte
	//      - OP_DATA: 1 byte
	//      - 32: 1 byte
	//      - OP_EQUALVERIFY: 1 byte
	//      - OP_HASH160: 1 byte
	//      - OP_DATA: 1 byte (RIPEMD160(payment_hash) length)
	//      - RIPEMD160(payment_hash): 20 bytes
	//      - OP_EQUALVERIFY: 1 byte
	//	- OP_DATA: 1 byte (pub key len)
	//	- local_key: 32 bytes
	//	- OP_CHECKSIGVERIFY: 1 byte
	//	- OP_DATA: 1 byte (pub key len)
	//	- remote_key: 32 bytes
	//	- OP_CHECKSIG: 1 byte
	TaprootHtlcAcceptedLocalSuccessScriptSize = 1 + 1 + 1 + 1 + 1 + 1 +
		20 + 1 + 1 + 32 + 1 + 1 + 32 + 1

	// TaprootHtlcAcceptedLocalSuccessWitnessSize:
	//      - number_of_witness_elements: 1 byte
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - sig_len: 1 byte
	//      - sweep_sig: 65 bytes (worst case w/o sighash default)
	//      - payment_preimage_length: 1 byte
	//      - payment_preimage: 32 bytes
	//      - script_len: 1 byte
	//      - taproot_accepted_htlc_script_success_size:
	//      - ctrl_block_len: 1 byte
	//      - base_control_block_size: 33 bytes
	//      - sibilng_merkle_proof: 32 bytes
	TaprootHtlcAcceptedLocalSuccessWitnessSize = 1 + 1 + 65 + 1 + 65 + 1 +
		32 + 1 + TaprootHtlcAcceptedLocalSuccessScriptSize + 1 +
		TaprootBaseControlBlockWitnessSize + 32
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

	// TODO(roasbeef): need taproot modifier? also no anchor so wrong?

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
	inputSize        lntypes.VByte
	inputWitnessSize lntypes.WeightUnit
	outputSize       lntypes.VByte
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
func (twe *TxWeightEstimator) AddWitnessInput(
	witnessSize lntypes.WeightUnit) *TxWeightEstimator {

	twe.inputSize += InputSize
	twe.inputWitnessSize += witnessSize
	twe.inputCount++
	twe.hasWitness = true

	return twe
}

// AddTapscriptInput updates the weight estimate to account for an additional
// input spending a segwit v1 pay-to-taproot output using the script path. This
// accepts the total size of the witness for the script leaf that is executed
// and adds the size of the control block to the total witness size.
//
// NOTE: The leaf witness size must be calculated without the byte that accounts
// for the number of witness elements, only the total size of all elements on
// the stack that are consumed by the revealed script should be counted.
func (twe *TxWeightEstimator) AddTapscriptInput(
	leafWitnessSize lntypes.WeightUnit,
	tapscript *waddrmgr.Tapscript) *TxWeightEstimator {

	// We add 1 byte for the total number of witness elements.
	controlBlockWitnessSize := 1 + TaprootBaseControlBlockWitnessSize +
		// 1 byte for the length of the element plus the element itself.
		1 + len(tapscript.RevealedScript) +
		1 + len(tapscript.ControlBlock.InclusionProof)

	twe.inputSize += InputSize
	twe.inputWitnessSize += leafWitnessSize + lntypes.WeightUnit(
		controlBlockWitnessSize,
	)
	twe.inputCount++
	twe.hasWitness = true

	return twe
}

// AddTaprootKeySpendInput updates the weight estimate to account for an
// additional input spending a segwit v1 pay-to-taproot output using the key
// spend path. This accepts the sighash type being used since that has an
// influence on the total size of the signature.
func (twe *TxWeightEstimator) AddTaprootKeySpendInput(
	hashType txscript.SigHashType) *TxWeightEstimator {

	twe.inputSize += InputSize

	if hashType == txscript.SigHashDefault {
		twe.inputWitnessSize += TaprootKeyPathWitnessSize
	} else {
		twe.inputWitnessSize += TaprootKeyPathCustomSighashWitnessSize
	}

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
func (twe *TxWeightEstimator) AddNestedP2WSHInput(
	witnessSize lntypes.WeightUnit) *TxWeightEstimator {

	twe.inputSize += InputSize + NestedP2WSHSize
	twe.inputWitnessSize += witnessSize
	twe.inputCount++
	twe.hasWitness = true

	return twe
}

// AddTxOutput adds a known TxOut to the weight estimator.
func (twe *TxWeightEstimator) AddTxOutput(txOut *wire.TxOut) *TxWeightEstimator {
	twe.outputSize += lntypes.VByte(txOut.SerializeSize())
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

// AddP2TROutput updates the weight estimate to account for an additional native
// SegWit v1 P2TR output.
func (twe *TxWeightEstimator) AddP2TROutput() *TxWeightEstimator {
	twe.outputSize += P2TROutputSize
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

// AddOutput estimates the weight of an output based on the pkScript.
func (twe *TxWeightEstimator) AddOutput(pkScript []byte) *TxWeightEstimator {
	twe.outputSize += BaseOutputSize + lntypes.VByte(len(pkScript))
	twe.outputCount++

	return twe
}

// Weight gets the estimated weight of the transaction.
func (twe *TxWeightEstimator) Weight() lntypes.WeightUnit {
	inputCount := wire.VarIntSerializeSize(uint64(twe.inputCount))
	outputCount := wire.VarIntSerializeSize(uint64(twe.outputCount))
	txSizeStripped := BaseTxSize + lntypes.VByte(inputCount) +
		twe.inputSize + lntypes.VByte(outputCount) + twe.outputSize
	weight := lntypes.WeightUnit(txSizeStripped * witnessScaleFactor)

	if twe.hasWitness {
		weight += WitnessHeaderSize + twe.inputWitnessSize
	}
	return weight
}

// VSize gets the estimated virtual size of the transactions, in vbytes.
func (twe *TxWeightEstimator) VSize() int {
	// A tx's vsize is 1/4 of the weight, rounded up.
	return int(twe.Weight().ToVB())
}
