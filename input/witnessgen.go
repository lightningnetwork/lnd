package input

import (
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// WitnessGenerator represents a function that is able to generate the final
// witness for a particular public key script. Additionally, if required, this
// function will also return the sigScript for spending nested P2SH witness
// outputs. This function acts as an abstraction layer, hiding the details of
// the underlying script.
type WitnessGenerator func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
	inputIndex int) (*Script, error)

// WitnessType determines how an output's witness will be generated. This
// interface can be implemented to be used for custom sweep scripts if the
// pre-defined StandardWitnessType list doesn't provide a suitable one.
type WitnessType interface {
	// String returns a human readable version of the WitnessType.
	String() string

	// WitnessGenerator will return a WitnessGenerator function that an
	// output uses to generate the witness and optionally the sigScript for
	// a sweep transaction.
	WitnessGenerator(signer Signer,
		descriptor *SignDescriptor) WitnessGenerator

	// SizeUpperBound returns the maximum length of the witness of this
	// WitnessType if it would be included in a tx. It also returns if the
	// output itself is a nested p2sh output, if so then we need to take
	// into account the extra sigScript data size.
	SizeUpperBound() (int, bool, error)

	// AddWeightEstimation adds the estimated size of the witness in bytes
	// to the given weight estimator.
	AddWeightEstimation(e *TxWeightEstimator) error
}

// StandardWitnessType is a numeric representation of standard pre-defined types
// of witness configurations.
type StandardWitnessType uint16

// A compile time check to ensure StandardWitnessType implements the
// WitnessType interface.
var _ WitnessType = (StandardWitnessType)(0)

const (
	// CommitmentTimeLock is a witness that allows us to spend our output
	// on our local commitment transaction after a relative lock-time
	// lockout.
	CommitmentTimeLock StandardWitnessType = 0

	// CommitmentNoDelay is a witness that allows us to spend a settled
	// no-delay output immediately on a counterparty's commitment
	// transaction.
	CommitmentNoDelay StandardWitnessType = 1

	// CommitmentRevoke is a witness that allows us to sweep the settled
	// output of a malicious counterparty's who broadcasts a revoked
	// commitment transaction.
	CommitmentRevoke StandardWitnessType = 2

	// HtlcOfferedRevoke is a witness that allows us to sweep an HTLC which
	// we offered to the remote party in the case that they broadcast a
	// revoked commitment state.
	HtlcOfferedRevoke StandardWitnessType = 3

	// HtlcAcceptedRevoke is a witness that allows us to sweep an HTLC
	// output sent to us in the case that the remote party broadcasts a
	// revoked commitment state.
	HtlcAcceptedRevoke StandardWitnessType = 4

	// HtlcOfferedTimeoutSecondLevel is a witness that allows us to sweep
	// an HTLC output that we extended to a party, but was never fulfilled.
	// This HTLC output isn't directly on the commitment transaction, but
	// is the result of a confirmed second-level HTLC transaction. As a
	// result, we can only spend this after a CSV delay.
	HtlcOfferedTimeoutSecondLevel StandardWitnessType = 5

	// HtlcAcceptedSuccessSecondLevel is a witness that allows us to sweep
	// an HTLC output that was offered to us, and for which we have a
	// payment preimage. This HTLC output isn't directly on our commitment
	// transaction, but is the result of confirmed second-level HTLC
	// transaction. As a result, we can only spend this after a CSV delay.
	HtlcAcceptedSuccessSecondLevel StandardWitnessType = 6

	// HtlcOfferedRemoteTimeout is a witness that allows us to sweep an
	// HTLC that we offered to the remote party which lies in the
	// commitment transaction of the remote party. We can spend this output
	// after the absolute CLTV timeout of the HTLC as passed.
	HtlcOfferedRemoteTimeout StandardWitnessType = 7

	// HtlcAcceptedRemoteSuccess is a witness that allows us to sweep an
	// HTLC that was offered to us by the remote party. We use this witness
	// in the case that the remote party goes to chain, and we know the
	// pre-image to the HTLC. We can sweep this without any additional
	// timeout.
	HtlcAcceptedRemoteSuccess StandardWitnessType = 8

	// HtlcSecondLevelRevoke is a witness that allows us to sweep an HTLC
	// from the remote party's commitment transaction in the case that the
	// broadcast a revoked commitment, but then also immediately attempt to
	// go to the second level to claim the HTLC.
	HtlcSecondLevelRevoke StandardWitnessType = 9

	// WitnessKeyHash is a witness type that allows us to spend a regular
	// p2wkh output that's sent to an output which is under complete
	// control of the backing wallet.
	WitnessKeyHash StandardWitnessType = 10

	// NestedWitnessKeyHash is a witness type that allows us to sweep an
	// output that sends to a nested P2SH script that pays to a key solely
	// under our control. The witness generated needs to include the
	NestedWitnessKeyHash StandardWitnessType = 11

	// CommitSpendNoDelayTweakless is similar to the CommitSpendNoDelay
	// type, but it omits the tweak that randomizes the key we need to
	// spend with a channel peer supplied set of randomness.
	CommitSpendNoDelayTweakless StandardWitnessType = 12

	// CommitmentToRemoteConfirmed is a witness that allows us to spend our
	// output on the counterparty's commitment transaction after a
	// confirmation.
	CommitmentToRemoteConfirmed StandardWitnessType = 13
)

// String returns a human readable version of the target WitnessType.
//
// NOTE: This is part of the WitnessType interface.
func (wt StandardWitnessType) String() string {
	switch wt {
	case CommitmentTimeLock:
		return "CommitmentTimeLock"

	case CommitmentToRemoteConfirmed:
		return "CommitmentToRemoteConfirmed"

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

	case WitnessKeyHash:
		return "WitnessKeyHash"

	case NestedWitnessKeyHash:
		return "NestedWitnessKeyHash"

	default:
		return fmt.Sprintf("Unknown WitnessType: %v", uint32(wt))
	}
}

// WitnessGenerator will return a WitnessGenerator function that an output uses
// to generate the witness and optionally the sigScript for a sweep
// transaction. The sigScript will be generated if the witness type warrants
// one for spending, such as the NestedWitnessKeyHash witness type.
//
// NOTE: This is part of the WitnessType interface.
func (wt StandardWitnessType) WitnessGenerator(signer Signer,
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

		case CommitmentToRemoteConfirmed:
			witness, err := CommitSpendToRemoteConfirmed(
				signer, desc, tx,
			)
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

// SizeUpperBound returns the maximum length of the witness of this witness
// type if it would be included in a tx. We also return if the output itself is
// a nested p2sh output, if so then we need to take into account the extra
// sigScript data size.
//
// NOTE: This is part of the WitnessType interface.
func (wt StandardWitnessType) SizeUpperBound() (int, bool, error) {
	switch wt {

	// Outputs on a remote commitment transaction that pay directly to us.
	case CommitSpendNoDelayTweakless:
		fallthrough
	case WitnessKeyHash:
		fallthrough
	case CommitmentNoDelay:
		return P2WKHWitnessSize, false, nil

	// Outputs on a past commitment transaction that pay directly
	// to us.
	case CommitmentTimeLock:
		return ToLocalTimeoutWitnessSize, false, nil

	// 1 CSV time locked output to us on remote commitment.
	case CommitmentToRemoteConfirmed:
		return ToRemoteConfirmedWitnessSize, false, nil

	// Outgoing second layer HTLC's that have confirmed within the
	// chain, and the output they produced is now mature enough to
	// sweep.
	case HtlcOfferedTimeoutSecondLevel:
		return ToLocalTimeoutWitnessSize, false, nil

	// Incoming second layer HTLC's that have confirmed within the
	// chain, and the output they produced is now mature enough to
	// sweep.
	case HtlcAcceptedSuccessSecondLevel:
		return ToLocalTimeoutWitnessSize, false, nil

	// An HTLC on the commitment transaction of the remote party,
	// that has had its absolute timelock expire.
	case HtlcOfferedRemoteTimeout:
		return AcceptedHtlcTimeoutWitnessSize, false, nil

	// An HTLC on the commitment transaction of the remote party,
	// that can be swept with the preimage.
	case HtlcAcceptedRemoteSuccess:
		return OfferedHtlcSuccessWitnessSize, false, nil

	// A nested P2SH input that has a p2wkh witness script. We'll mark this
	// as nested P2SH so the caller can estimate the weight properly
	// including the sigScript.
	case NestedWitnessKeyHash:
		return P2WKHWitnessSize, true, nil

	// The revocation output on a revoked commitment transaction.
	case CommitmentRevoke:
		return ToLocalPenaltyWitnessSize, false, nil

	// The revocation output on a revoked HTLC that we offered to the remote
	// party.
	case HtlcOfferedRevoke:
		return OfferedHtlcPenaltyWitnessSize, false, nil

	// The revocation output on a revoked HTLC that was sent to us.
	case HtlcAcceptedRevoke:
		return AcceptedHtlcPenaltyWitnessSize, false, nil

	// The revocation output of a second level output of an HTLC.
	case HtlcSecondLevelRevoke:
		return ToLocalPenaltyWitnessSize, false, nil
	}

	return 0, false, fmt.Errorf("unexpected witness type: %v", wt)
}

// AddWeightEstimation adds the estimated size of the witness in bytes to the
// given weight estimator.
//
// NOTE: This is part of the WitnessType interface.
func (wt StandardWitnessType) AddWeightEstimation(e *TxWeightEstimator) error {
	// For fee estimation purposes, we'll now attempt to obtain an
	// upper bound on the weight this input will add when fully
	// populated.
	size, isNestedP2SH, err := wt.SizeUpperBound()
	if err != nil {
		return err
	}

	// If this is a nested P2SH input, then we'll need to factor in
	// the additional data push within the sigScript.
	if isNestedP2SH {
		e.AddNestedP2WSHInput(size)
	} else {
		e.AddWitnessInput(size)
	}

	return nil
}
