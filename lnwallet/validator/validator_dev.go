//go:build dev
// +build dev

package validator

import (
	"context"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// Validator is currently a no-op validator that runs in the production env.
type Validator struct{}

// NewValidator creates a new Validator instance.
func NewValidator() *Validator {
	return &Validator{}
}

// ValidatePSBT always determines that the provided SignPsbtRequest should be
// signed.
func (r *Validator) ValidatePSBT(_ context.Context,
	_ *walletrpc.SignPsbtRequest) (ValidationResult, error) {

	/*
		packet, err := psbt.NewFromRawBytes(
			bytes.NewReader(req.FundedPsbt), false,
		)
		if err != nil {
			return nil, error
		}

		transactionType, err := GetTransactionType(packet)
		if err != nil {
			return nil, error
		}

		switch GetTransactionType(packet) {

		case txType.IsRemoteCommitmentTransaction:
			for _, output := range packet.Outputs {
				switch GetCommitmentOutputType(output) {

				case outputType.ToLocal:
					if !KeyDerivedFromRemoteDelayedBasepoint(output) {
						res := ValidationFailureResult("Incorrect " +
							"public key for to_local " +
							"output in remote commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.ToRemote:
					if !KeyDerivedFromLocalPaymentBasepoint(output) {
						res := ValidationFailureResult("Incorrect " +
							"public key for to_remote " +
							"output in remote commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.OfferedHTLC:
					correctKeys := KeysDerivedFromHTLCsBasepoints(
						v.GetLocalHTLCBasePoint(packet),
						v.GetRemoteHTLCBasePoint(packet),
						output,
					)
					if !correctKeys {
						res := ValidationFailureResult(
							"Public keys in HTLC not derived from " +
							"channel HTLC basepoints in " +
							"remote commitment "+
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.ReceivedHTLC:
					if !PaymentHashIsWhiteListed(output){
						res := ValidationFailureResult(
							"Unauthorized payment_hash " +
							"detected in remote commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

					correctKeys := KeysDerivedFromHTLCsBasepoints(
						v.GetLocalHTLCBasePoint(packet),
						v.GetRemoteHTLCBasePoint(packet),
						output,
					)
					if !correctKeys {
						res := ValidationFailureResult(
							"Public keys in HTLC not derived from " +
							"channel HTLC basepoints in " +
							"remote commitment "+
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.LocalAnchor:
					if !KeyIsRemoteFundingPubkey(output) {
						res := ValidationFailureResult("Public " +
							"key for to_local_anchor isn't" +
							"remote's funding pubkey in " +
							"remote commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.RemoteAnchor:
					if !KeyIsLocalFundingPubkey(output) {
						res := ValidationFailureResult("Public " +
							"key for to_remote_anchor isn't" +
							"local funding pubkey in " +
							"remote commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				default:
					res := ValidationFailureResult("Unexpected " +
						"output in remote commitment " +
						"transaction: %v", output)
					return res, nil
				}

				if !ScriptHashMatch(output) {
					res := ValidationFailureResult("Locking " +
						"script does not match script "+
						"hash for output %v in remote " +
						"commitment transaction", output)
					return res, nil
				}
			}

			return ValidationSuccessResult(), nil

		case txType.IsLocalCommitmentTransaction: // Force closure
			// Checks that the commitment height of the tx is >= than the
			// persisted local commitment height
			if !IsCurrentCommitmentHeight(packet){
				res := ValidationFailureResult("Revoked state detected " +
					"in request to force close channel with local " +
					"commitment transaction: %v", packet,
				)
				return res, nil
			}

			for _, output := range packet.Outputs {
				switch GetCommitmentOutputType(output) {

				case outputType.ToLocal:
					if !KeyDerivedFromLocalDelayedBasepoint(output) {
						res := ValidationFailureResult("Incorrect " +
							"public key for to_local " +
							"output in local commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.ToRemote:
					if !KeyDerivedFromRemotePaymentBasepoint(output) {
						res := ValidationFailureResult("Incorrect " +
							"public key for to_remote " +
							"output in local commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.OfferedHTLC:
					if !PaymentHashIsWhiteListed(output){
						res := ValidationFailureResult(
							"Unauthorized payment_hash " +
							"detected in local commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

					correctKeys := KeysDerivedFromHTLCsBasepoints(
						v.GetRemoteHTLCBasePoint(packet),
						v.GetLocalHTLCBasePoint(packet),
						output,
					)
					if !correctKeys {
						res := ValidationFailureResult(
							"Public keys in HTLC not derived from " +
							"channel HTLC basepoints in " +
							"local commitment "+
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.ReceivedHTLC:
					correctKeys := KeysDerivedFromHTLCsBasepoints(
						v.GetRemoteHTLCBasePoint(packet),
						v.GetLocalHTLCBasePoint(packet),
						output,
					)
					if !correctKeys {
						res := ValidationFailureResult(
							"Public keys in HTLC not derived from " +
							"channel HTLC basepoints in " +
							"local commitment "+
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.LocalAnchor:
					if !KeyIsLocalFundingPubkey(output) {
						res := ValidationFailureResult("Public " +
							"key for to_local_anchor isn't" +
							"local funding pubkey in " +
							"local commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				case outputType.RemoteAnchor:
					if !KeyIsRemoteFundingPubkey(output) {
						res := ValidationFailureResult("Public " +
							"key for to_remote_anchor isn't" +
							"remote's funding pubkey in " +
							"local commitment " +
							"transaction %v for output: %v",
							packet, output,
						)
						return res, nil
					}

				default:
					res := ValidationFailureResult("Unexpected " +
						"output %v in local commitment " +
						"transaction: %v", output, packet)
					return res, nil
				}

				if !ScriptHashMatch(output) {
					res := ValidationFailureResult("Locking " +
						"script does not match script "+
						"hash for output %v in remote " +
						"commitment transaction", output)
					return res, nil
				}
			}

			return ValidationSuccessResult(), nil

		case txType.IsCoopCloseTx:
			// This type of transaction is particularly hard to decide how
			// we should handle, as it will contain the remote party's
			// output, which is an address which cannot be derived from any
			// basepoint.
			// Therefore we're faced with 2 different options.
			// TODO: decide option

			// Option 1, we require that all outputs is either whitelisted,
			// or is an internal address (the default if no DeliveryAddress
			// is set).
			// This WILL require that the remote party's address gets
			// whitelisted somehow on the application layer.
			for _, output := range packet.Outputs {
				switch GetCoopCloseOutputType(output) {

				switch coOpType.IsInternalAddress, coOpType.IsWhiteListedAddress:

				default:
					res := ValidationFailureResult("Unknown " +
						"output %v in cooperative closing "+
						"transaction %v", output, packet)
					return res, nil
				}
			}

			return ValidationSuccessResult(), nil

			// Option 2:
			// We ignore checking the remote output, and instead only check
			// if the transaction contains an output for us. We consider it
			// to be our output if it contains an output address that's
			// either internal in the wallet, or is whitelisted (i.e. a
			// DeliveryAddress has been set for us).
			// NOTE: As our output in the closing tx can be trimmed, if an
			// output has been trimmed we check if that non-trimmed output
			// in the closure tx should be ours or not. We determine that in
			// that scenario if the to_local output amount in our
			// last local commitment tx was above the value for the to_remote
			// output.

			// If an output was trimmed checks if the to_local output value
			// in our last commitment tx was above the to_remote value.
			if len(packet.Outputs) == 1 && !RequireKnownOutput(packet) {
				// If not we won't require that a local output exists in
				// the closing tx.
				return ValidationSuccessResult(), nil
			}

			// Else the tx should have 2 outputs, where one of the outputs
			// should be ours.
			for _, output := range packet.Outputs {
				switch GetCoopCloseOutputType(output) {

				case coOpType.IsInternalAddress, coOpType.IsWhiteListedAddress:
					// Found the local output.
					return ValidationSuccessResult(), nil

				default:
					res := ValidationFailureResult("Unknown " +
						"output %v in funding transaction %v",
						output, packet)
					return res, nil
				}
			}

			res := ValidationFailureResult("Could not find local " +
				"output in cooperative closing "+
				"transaction %v", packet)

			return res, nil

		case txType.IsFundingTransaction:
			// NOTE: we only enter this and sign funding transactions if we
			// are the channel funder.
			// This would need to be updated when support for dualfunding
			// exists, as the outputs may contain the remote's change output

			for _, output := range packet.Outputs {
				switch GetFundingOutputType(output) {

				case fundingType.IsInternalAddress:
					// Our change output

				case fundingType.Taproot:
					// Validates that the output is a MuSig2 output
					// where one of the keys is an internal key.
					if !MuSig2OutputWithInternalKey(output){
						res := ValidationFailureResult("Internal " +
							"key not found in funding "+
							"output %v in funding " +
							"transaction", output)
						return res, nil
					}

					if !KeysMatchTaprootAddress(output) {
						res := ValidationFailureResult("Taproot " +
							"pubkey doesn't match included "+
							"keys for output %v in funding " +
							"transaction", output)
						return res, nil
					}

				case fundingType.P2WSH:
					// Validates that the output is a 2 of 2 output
					// multisig output, where one of the keys is an
					// internal key.
					if !MultiSigOutputWithInternalKey(output){
						res := ValidationFailureResult("Internal " +
							"key not found in funding "+
							"output %v in funding " +
							"transaction", output)
						return res, nil
					}

					if !ScriptHashMatch(output) {
						res := ValidationFailureResult("Locking " +
							"script does not match script "+
							"hash for output %v in funding " +
							"transaction", output)
						return res, nil
					}
				}
			}

			return ValidationSuccessResult(), nil

		case txType.IsSecondStageHTLCTransaction:
			for _, output := range packet.Outputs {
				switch GetSecondStageType(output) {

				case secondStageType.IsInternalAddress:
					// Sweeper change output

				case secondStageType.IsTimeoutOrSuccesOutput:
					// We don't evaluate if this output actually should BE our or
					// the remote's delayed key, as it would require that the
					// channel party controlling the watch-only actually swaps
					// the key, which this validation level does not aim to prevent.
					if !DerivedFromLocalOrRemoteDelayedBasePoint(output) {
						res := ValidationFailureResult("Second " +
							"stage tx output key not derived " +
							"from delayed basepoint for "+
							"output %v in second " +
							"stage transaction", output)
						return res, nil
					}

					if !ScriptHashMatch(output) {
						res := ValidationFailureResult("Locking " +
							"script does not match script "+
							"hash for output %v in second " +
							"stage transaction", output)
						return res, nil
					}

				default:
					res := ValidationFailureResult("Unknown "+
						"output %v in funding transaction %v",
						output, packet)
					return res, nil
				}
			}

			return ValidationSuccessResult(), nil

		default:
			// This is a sweeper tx or an lncli sendcoins/sendmany tx
			for _, output := range packet.Outputs {
				if !IsInternalAddress(output) && IsWhiteListedAddress(output) {
					res := ValidationFailureResult("Unauthorized "+
						"output %v for transaction %v",
						output, packet)
					return res, nil
				}
			}

			return ValidationSuccessResult(), nil
		}
	*/

	return ValidationSuccessResult(), nil
}

// GetFeatures returns the features supported by the Validator
// implementation. This information helps the watch-only node
// decide which types of metadata to send to the remote signer.
func (r *Validator) GetFeatures() string {
	return ""
}

// AddMetadata allows metadata to be passed to the Validator.
// This metadata may be used during a future ValidatePSBT call.
func (r *Validator) AddMetadata(_ []byte) error {
	return nil
}

// A compile time assertion to ensure Validator meets the Validation interface.
var _ Validation = (*Validator)(nil)
