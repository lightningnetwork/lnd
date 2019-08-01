package input

import (
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// WitnessType determines how an output's witness will be generated. The
// default commitmentTimeLock type will generate a witness that will allow
// spending of a time-locked transaction enforced by CheckSequenceVerify.
type WitnessType uint16

const (
	// CommitmentTimeLock is a witness that allows us to spend the output of
	// a commitment transaction after a relative lock-time lockout.
	CommitmentTimeLock WitnessType = 0

	// CommitmentNoDelay is a witness that allows us to spend a settled
	// no-delay output immediately on a counterparty's commitment
	// transaction.
	CommitmentNoDelay WitnessType = 1

	// CommitmentRevoke is a witness that allows us to sweep the settled
	// output of a malicious counterparty's who broadcasts a revoked
	// commitment transaction.
	CommitmentRevoke WitnessType = 2

	// HtlcOfferedRevoke is a witness that allows us to sweep an HTLC which
	// we offered to the remote party in the case that they broadcast a
	// revoked commitment state.
	HtlcOfferedRevoke WitnessType = 3

	// HtlcAcceptedRevoke is a witness that allows us to sweep an HTLC
	// output sent to us in the case that the remote party broadcasts a
	// revoked commitment state.
	HtlcAcceptedRevoke WitnessType = 4

	// HtlcOfferedTimeoutSecondLevel is a witness that allows us to sweep
	// an HTLC output that we extended to a party, but was never fulfilled.
	// This HTLC output isn't directly on the commitment transaction, but
	// is the result of a confirmed second-level HTLC transaction. As a
	// result, we can only spend this after a CSV delay.
	HtlcOfferedTimeoutSecondLevel WitnessType = 5

	// HtlcAcceptedSuccessSecondLevel is a witness that allows us to sweep
	// an HTLC output that was offered to us, and for which we have a
	// payment preimage. This HTLC output isn't directly on our commitment
	// transaction, but is the result of confirmed second-level HTLC
	// transaction. As a result, we can only spend this after a CSV delay.
	HtlcAcceptedSuccessSecondLevel WitnessType = 6

	// HtlcOfferedRemoteTimeout is a witness that allows us to sweep an
	// HTLC that we offered to the remote party which lies in the
	// commitment transaction of the remote party. We can spend this output
	// after the absolute CLTV timeout of the HTLC as passed.
	HtlcOfferedRemoteTimeout WitnessType = 7

	// HtlcAcceptedRemoteSuccess is a witness that allows us to sweep an
	// HTLC that was offered to us by the remote party. We use this witness
	// in the case that the remote party goes to chain, and we know the
	// pre-image to the HTLC. We can sweep this without any additional
	// timeout.
	HtlcAcceptedRemoteSuccess WitnessType = 8

	// HtlcSecondLevelRevoke is a witness that allows us to sweep an HTLC
	// from the remote party's commitment transaction in the case that the
	// broadcast a revoked commitment, but then also immediately attempt to
	// go to the second level to claim the HTLC.
	HtlcSecondLevelRevoke WitnessType = 9

	// WitnessKeyHash is a witness type that allows us to spend a regular
	// p2wkh output that's sent to an output which is under complete
	// control of the backing wallet.
	WitnessKeyHash WitnessType = 10

	// NestedWitnessKeyHash is a witness type that allows us to sweep an
	// output that sends to a nested P2SH script that pays to a key solely
	// under our control. The witness generated needs to include the
	NestedWitnessKeyHash WitnessType = 11

	// CommitSpendNoDelayTweakless is similar to the CommitSpendNoDelay
	// type, but it omits the tweak that randomizes the key we need to
	// spend with a channel peer supplied set of randomness.
	CommitSpendNoDelayTweakless = 12
)

// Stirng returns a human readable version of the target WitnessType.
func (wt WitnessType) String() string {
	switch wt {
	case CommitmentTimeLock:
		return "CommitmentTimeLock"

	case CommitmentNoDelay:
		return "CommitmentNoDelay"

	case CommitSpendNoDelayTweakless:
		return "CommitmentNoDelayTweakless"

	case CommitmentRevoke:
		return "CommitmentRevoke"

	case HtlcOfferedRevoke:
		return "HtlcOfferedRevoke"

	case HtlcAcceptedRevoke:
		return "HtlcAcceptedRevoke"

	case HtlcOfferedTimeoutSecondLevel:
		return "HtlcOfferedTimeoutSecondLevel"

	case HtlcAcceptedSuccessSecondLevel:
		return "HtlcAcceptedSuccessSecondLevel"

	case HtlcOfferedRemoteTimeout:
		return "HtlcOfferedRemoteTimeout"

	case HtlcAcceptedRemoteSuccess:
		return "HtlcAcceptedRemoteSuccess"

	case HtlcSecondLevelRevoke:
		return "HtlcSecondLevelRevoke"

	default:
		return fmt.Sprintf("Unknown WitnessType: %v", uint32(wt))
	}
}

// WitnessGenerator represents a function which is able to generate the final
// witness for a particular public key script. Additionally, if required, this
// function will also return the sigScript for spending nested P2SH witness
// outputs. This function acts as an abstraction layer, hiding the details of
// the underlying script.
type WitnessGenerator func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
	inputIndex int) (*Script, error)

// GenWitnessFunc will return a WitnessGenerator function that an output uses
// to generate the witness and optionally the sigScript for a sweep
// transaction. The sigScript will be generated if the witness type warrants
// one for spending, such as the NestedWitnessKeyHash witness type.
func (wt WitnessType) GenWitnessFunc(signer Signer,
	descriptor *SignDescriptor) WitnessGenerator {

	return func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
		inputIndex int) (*Script, error) {

		desc := descriptor
		desc.SigHashes = hc
		desc.InputIndex = inputIndex

		switch wt {
		case CommitmentTimeLock:
			witness, err := CommitSpendTimeout(signer, desc, tx)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case CommitmentNoDelay:
			witness, err := CommitSpendNoDelay(signer, desc, tx, false)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case CommitSpendNoDelayTweakless:
			witness, err := CommitSpendNoDelay(signer, desc, tx, true)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case CommitmentRevoke:
			witness, err := CommitSpendRevoke(signer, desc, tx)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case HtlcOfferedRevoke:
			witness, err := ReceiverHtlcSpendRevoke(signer, desc, tx)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case HtlcAcceptedRevoke:
			witness, err := SenderHtlcSpendRevoke(signer, desc, tx)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case HtlcOfferedTimeoutSecondLevel:
			witness, err := HtlcSecondLevelSpend(signer, desc, tx)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case HtlcAcceptedSuccessSecondLevel:
			witness, err := HtlcSecondLevelSpend(signer, desc, tx)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case HtlcOfferedRemoteTimeout:
			// We pass in a value of -1 for the timeout, as we
			// expect the caller to have already set the lock time
			// value.
			witness, err := ReceiverHtlcSpendTimeout(signer, desc, tx, -1)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case HtlcSecondLevelRevoke:
			witness, err := HtlcSpendRevoke(signer, desc, tx)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case WitnessKeyHash:
			fallthrough
		case NestedWitnessKeyHash:
			return signer.ComputeInputScript(tx, desc)

		default:
			return nil, fmt.Errorf("unknown witness type: %v", wt)
		}
	}

}
