package input

import (
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntypes"
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
	SizeUpperBound() (lntypes.WeightUnit, bool, error)

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

// NOTE: When adding a new `StandardWitnessType`, also update the `WitnessType`
// protobuf enum and the `allWitnessTypes` map in the `walletrpc` package.
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

	// HtlcOfferedTimeoutSecondLevelInputConfirmed is a witness that allows
	// us to sweep an HTLC output that we extended to a party, but was
	// never fulfilled. This _is_ the HTLC output directly on our
	// commitment transaction, and the input to the second-level HTLC
	// timeout transaction. It can only be spent after CLTV expiry, and
	// commitment confirmation.
	HtlcOfferedTimeoutSecondLevelInputConfirmed StandardWitnessType = 15

	// HtlcAcceptedSuccessSecondLevel is a witness that allows us to sweep
	// an HTLC output that was offered to us, and for which we have a
	// payment preimage. This HTLC output isn't directly on our commitment
	// transaction, but is the result of confirmed second-level HTLC
	// transaction. As a result, we can only spend this after a CSV delay.
	HtlcAcceptedSuccessSecondLevel StandardWitnessType = 6

	// HtlcAcceptedSuccessSecondLevelInputConfirmed is a witness that
	// allows us to sweep an HTLC output that was offered to us, and for
	// which we have a payment preimage. This _is_ the HTLC output directly
	// on our commitment transaction, and the input to the second-level
	// HTLC success transaction.  It can only be spent after the commitment
	// has confirmed.
	HtlcAcceptedSuccessSecondLevelInputConfirmed StandardWitnessType = 16

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

	// CommitmentAnchor is a witness that allows us to spend our anchor on
	// the commitment transaction.
	CommitmentAnchor StandardWitnessType = 14

	// LeaseCommitmentTimeLock is a witness that allows us to spend our
	// output on our local commitment transaction after a relative and
	// absolute lock-time lockout as part of the script enforced lease
	// commitment type.
	LeaseCommitmentTimeLock StandardWitnessType = 17

	// LeaseCommitmentToRemoteConfirmed is a witness that allows us to spend
	// our output on the counterparty's commitment transaction after a
	// confirmation and absolute locktime as part of the script enforced
	// lease commitment type.
	LeaseCommitmentToRemoteConfirmed StandardWitnessType = 18

	// LeaseHtlcOfferedTimeoutSecondLevel is a witness that allows us to
	// sweep an HTLC output that we extended to a party, but was never
	// fulfilled. This HTLC output isn't directly on the commitment
	// transaction, but is the result of a confirmed second-level HTLC
	// transaction. As a result, we can only spend this after a CSV delay
	// and CLTV locktime as part of the script enforced lease commitment
	// type.
	LeaseHtlcOfferedTimeoutSecondLevel StandardWitnessType = 19

	// LeaseHtlcAcceptedSuccessSecondLevel is a witness that allows us to
	// sweep an HTLC output that was offered to us, and for which we have a
	// payment preimage. This HTLC output isn't directly on our commitment
	// transaction, but is the result of confirmed second-level HTLC
	// transaction. As a result, we can only spend this after a CSV delay
	// and CLTV locktime as part of the script enforced lease commitment
	// type.
	LeaseHtlcAcceptedSuccessSecondLevel StandardWitnessType = 20

	// TaprootPubKeySpend is a witness type that allows us to spend a
	// regular p2tr output that's sent to an output which is under complete
	// control of the backing wallet.
	TaprootPubKeySpend StandardWitnessType = 21

	// TaprootLocalCommitSpend is a witness type that allows us to spend
	// our settled local commitment after a CSV delay when we force close
	// the channel.
	TaprootLocalCommitSpend StandardWitnessType = 22

	// TaprootRemoteCommitSpend is a witness type that allows us to spend
	// our settled local commitment after a CSV delay when the remote party
	// has force closed the channel.
	TaprootRemoteCommitSpend StandardWitnessType = 23

	// TaprootAnchorSweepSpend is the witness type we'll use for spending
	// our own anchor output.
	TaprootAnchorSweepSpend StandardWitnessType = 24

	// TaprootHtlcOfferedTimeoutSecondLevel is a witness that allows us to
	// timeout an HTLC we offered to the remote party on our commitment
	// transaction. We use this when we need to go on chain to time out an
	// HTLC.
	TaprootHtlcOfferedTimeoutSecondLevel StandardWitnessType = 25

	// TaprootHtlcAcceptedSuccessSecondLevel is a witness that allows us to
	// sweep an HTLC we accepted on our commitment transaction after we go
	// to the second level on chain.
	TaprootHtlcAcceptedSuccessSecondLevel StandardWitnessType = 26

	// TaprootHtlcSecondLevelRevoke is a witness that allows us to sweep an
	// HTLC on the revoked transaction of the remote party that goes to the
	// second level.
	TaprootHtlcSecondLevelRevoke StandardWitnessType = 27

	// TaprootHtlcAcceptedRevoke is a witness that allows us to sweep an
	// HTLC sent to us by the remote party in the event that they broadcast
	// a revoked state.
	TaprootHtlcAcceptedRevoke StandardWitnessType = 28

	// TaprootHtlcOfferedRevoke is a witness that allows us to sweep an
	// HTLC we offered to the remote party if they broadcast a revoked
	// commitment.
	TaprootHtlcOfferedRevoke StandardWitnessType = 29

	// TaprootHtlcOfferedRemoteTimeout is a witness that allows us to sweep
	// an HTLC we offered to the remote party that lies on the commitment
	// transaction for the remote party. We can spend this output after the
	// absolute CLTV timeout of the HTLC as passed.
	TaprootHtlcOfferedRemoteTimeout StandardWitnessType = 30

	// TaprootHtlcLocalOfferedTimeout is a witness type that allows us to
	// sign the second level HTLC timeout transaction when spending from an
	// HTLC residing on our local commitment transaction.
	//
	// This is used by the sweeper to re-sign inputs if it needs to
	// aggregate several second level HTLCs.
	TaprootHtlcLocalOfferedTimeout StandardWitnessType = 31

	// TaprootHtlcAcceptedRemoteSuccess is a witness that allows us to
	// sweep an HTLC that was offered to us by the remote party for a
	// taproot channels. We use this witness in the case that the remote
	// party goes to chain, and we know the pre-image to the HTLC. We can
	// sweep this without any additional timeout.
	TaprootHtlcAcceptedRemoteSuccess StandardWitnessType = 32

	// TaprootHtlcAcceptedLocalSuccess is a witness type that allows us to
	// sweep the HTLC offered to us on our local commitment transaction.
	// We'll use this when we need to go on chain to sweep the HTLC. In
	// this case, this is the second level HTLC success transaction.
	TaprootHtlcAcceptedLocalSuccess StandardWitnessType = 33

	// TaprootCommitmentRevoke is a witness that allows us to sweep the
	// settled output of a malicious counterparty's who broadcasts a
	// revoked taproot commitment transaction.
	TaprootCommitmentRevoke StandardWitnessType = 34
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

	case CommitmentAnchor:
		return "CommitmentAnchor"

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

	case HtlcOfferedTimeoutSecondLevelInputConfirmed:
		return "HtlcOfferedTimeoutSecondLevelInputConfirmed"

	case HtlcAcceptedSuccessSecondLevel:
		return "HtlcAcceptedSuccessSecondLevel"

	case HtlcAcceptedSuccessSecondLevelInputConfirmed:
		return "HtlcAcceptedSuccessSecondLevelInputConfirmed"

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

	case LeaseCommitmentTimeLock:
		return "LeaseCommitmentTimeLock"

	case LeaseCommitmentToRemoteConfirmed:
		return "LeaseCommitmentToRemoteConfirmed"

	case LeaseHtlcOfferedTimeoutSecondLevel:
		return "LeaseHtlcOfferedTimeoutSecondLevel"

	case LeaseHtlcAcceptedSuccessSecondLevel:
		return "LeaseHtlcAcceptedSuccessSecondLevel"

	case TaprootPubKeySpend:
		return "TaprootPubKeySpend"

	case TaprootLocalCommitSpend:
		return "TaprootLocalCommitSpend"

	case TaprootRemoteCommitSpend:
		return "TaprootRemoteCommitSpend"

	case TaprootAnchorSweepSpend:
		return "TaprootAnchorSweepSpend"

	case TaprootHtlcOfferedTimeoutSecondLevel:
		return "TaprootHtlcOfferedTimeoutSecondLevel"

	case TaprootHtlcAcceptedSuccessSecondLevel:
		return "TaprootHtlcAcceptedSuccessSecondLevel"

	case TaprootHtlcSecondLevelRevoke:
		return "TaprootHtlcSecondLevelRevoke"

	case TaprootHtlcAcceptedRevoke:
		return "TaprootHtlcAcceptedRevoke"

	case TaprootHtlcOfferedRevoke:
		return "TaprootHtlcOfferedRevoke"

	case TaprootHtlcOfferedRemoteTimeout:
		return "TaprootHtlcOfferedRemoteTimeout"

	case TaprootHtlcLocalOfferedTimeout:
		return "TaprootHtlcLocalOfferedTimeout"

	case TaprootHtlcAcceptedRemoteSuccess:
		return "TaprootHtlcAcceptedRemoteSuccess"

	case TaprootHtlcAcceptedLocalSuccess:
		return "TaprootHtlcAcceptedLocalSuccess"

	case TaprootCommitmentRevoke:
		return "TaprootCommitmentRevoke"

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

		// TODO(roasbeef): copy the desc?
		desc := descriptor
		desc.SigHashes = hc
		desc.InputIndex = inputIndex

		switch wt {
		case CommitmentTimeLock, LeaseCommitmentTimeLock:
			witness, err := CommitSpendTimeout(signer, desc, tx)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case CommitmentToRemoteConfirmed, LeaseCommitmentToRemoteConfirmed:
			witness, err := CommitSpendToRemoteConfirmed(
				signer, desc, tx,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case CommitmentAnchor:
			witness, err := CommitSpendAnchor(signer, desc, tx)
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

		case HtlcOfferedTimeoutSecondLevel,
			LeaseHtlcOfferedTimeoutSecondLevel,
			HtlcAcceptedSuccessSecondLevel,
			LeaseHtlcAcceptedSuccessSecondLevel:

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
		case TaprootPubKeySpend:
			fallthrough
		case NestedWitnessKeyHash:
			return signer.ComputeInputScript(tx, desc)

		case TaprootLocalCommitSpend:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootScriptSpendSignMethod

			// The control block bytes must be set at this point.
			if desc.ControlBlock == nil {
				return nil, fmt.Errorf("control block must " +
					"be set for taproot spend")
			}

			witness, err := TaprootCommitSpendSuccess(
				signer, desc, tx, nil,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case TaprootRemoteCommitSpend:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootScriptSpendSignMethod

			// The control block bytes must be set at this point.
			if desc.ControlBlock == nil {
				return nil, fmt.Errorf("control block must " +
					"be set for taproot spend")
			}

			witness, err := TaprootCommitRemoteSpend(
				signer, desc, tx, nil,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case TaprootAnchorSweepSpend:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootKeySpendSignMethod

			// The tap tweak must be set at this point.
			if desc.TapTweak == nil {
				return nil, fmt.Errorf("tap tweak must be " +
					"set for keyspend")
			}

			witness, err := TaprootAnchorSpend(
				signer, desc, tx,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case TaprootHtlcOfferedTimeoutSecondLevel,
			TaprootHtlcAcceptedSuccessSecondLevel:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootScriptSpendSignMethod

			// The control block bytes must be set at this point.
			if desc.ControlBlock == nil {
				return nil, fmt.Errorf("control block must " +
					"be set for taproot spend")
			}

			witness, err := TaprootHtlcSpendSuccess(
				signer, desc, tx, nil, nil,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case TaprootHtlcSecondLevelRevoke:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootKeySpendSignMethod

			// The tap tweak must be set at this point.
			if desc.TapTweak == nil {
				return nil, fmt.Errorf("tap tweak must be " +
					"set for keyspend")
			}

			witness, err := TaprootHtlcSpendRevoke(
				signer, desc, tx,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case TaprootHtlcOfferedRevoke:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootKeySpendSignMethod

			// The tap tweak must be set at this point.
			if desc.TapTweak == nil {
				return nil, fmt.Errorf("tap tweak must be " +
					"set for keyspend")
			}

			witness, err := SenderHTLCScriptTaprootRevoke(
				signer, desc, tx,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case TaprootHtlcAcceptedRevoke:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootKeySpendSignMethod

			// The tap tweak must be set at this point.
			if desc.TapTweak == nil {
				return nil, fmt.Errorf("tap tweak must be " +
					"set for keyspend")
			}

			witness, err := ReceiverHTLCScriptTaprootRevoke(
				signer, desc, tx,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case TaprootHtlcOfferedRemoteTimeout:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootScriptSpendSignMethod

			// The control block bytes must be set at this point.
			if desc.ControlBlock == nil {
				return nil, fmt.Errorf("control block " +
					"must be set for taproot spend")
			}

			witness, err := ReceiverHTLCScriptTaprootTimeout(
				signer, desc, tx, -1, nil, nil,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

		case TaprootCommitmentRevoke:
			// Ensure that the sign desc has the proper sign method
			// set, and a valid prev output fetcher.
			desc.SignMethod = TaprootScriptSpendSignMethod

			// The control block bytes must be set at this point.
			if desc.ControlBlock == nil {
				return nil, fmt.Errorf("control block " +
					"must be set for taproot spend")
			}

			witness, err := TaprootCommitSpendRevoke(
				signer, desc, tx, nil,
			)
			if err != nil {
				return nil, err
			}

			return &Script{
				Witness: witness,
			}, nil

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
func (wt StandardWitnessType) SizeUpperBound() (lntypes.WeightUnit,
	bool, error) {

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
	case LeaseCommitmentTimeLock:
		size := ToLocalTimeoutWitnessSize +
			LeaseWitnessScriptSizeOverhead

		return lntypes.WeightUnit(size), false, nil

	// 1 CSV time locked output to us on remote commitment.
	case CommitmentToRemoteConfirmed:
		return ToRemoteConfirmedWitnessSize, false, nil
	case LeaseCommitmentToRemoteConfirmed:
		size := ToRemoteConfirmedWitnessSize +
			LeaseWitnessScriptSizeOverhead

		return lntypes.WeightUnit(size), false, nil

	// Anchor output on the commitment transaction.
	case CommitmentAnchor:
		return AnchorWitnessSize, false, nil

	// Outgoing second layer HTLC's that have confirmed within the
	// chain, and the output they produced is now mature enough to
	// sweep.
	case HtlcOfferedTimeoutSecondLevel:
		return ToLocalTimeoutWitnessSize, false, nil
	case LeaseHtlcOfferedTimeoutSecondLevel:
		size := ToLocalTimeoutWitnessSize +
			LeaseWitnessScriptSizeOverhead

		return lntypes.WeightUnit(size), false, nil

	// Input to the outgoing HTLC second layer timeout transaction.
	case HtlcOfferedTimeoutSecondLevelInputConfirmed:
		return OfferedHtlcTimeoutWitnessSizeConfirmed, false, nil

	// Incoming second layer HTLC's that have confirmed within the
	// chain, and the output they produced is now mature enough to
	// sweep.
	case HtlcAcceptedSuccessSecondLevel:
		return ToLocalTimeoutWitnessSize, false, nil
	case LeaseHtlcAcceptedSuccessSecondLevel:
		size := ToLocalTimeoutWitnessSize +
			LeaseWitnessScriptSizeOverhead

		return lntypes.WeightUnit(size), false, nil

	// Input to the incoming second-layer HTLC success transaction.
	case HtlcAcceptedSuccessSecondLevelInputConfirmed:
		return AcceptedHtlcSuccessWitnessSizeConfirmed, false, nil

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

	case TaprootPubKeySpend:
		return TaprootKeyPathCustomSighashWitnessSize, false, nil

	// Sweeping a self output after a delay for taproot channels.
	case TaprootLocalCommitSpend:
		return TaprootToLocalWitnessSize, false, nil

	// Sweeping a self output after the remote party fro ce closes. Must
	// wait 1 CSV.
	case TaprootRemoteCommitSpend:
		return TaprootToRemoteWitnessSize, false, nil

	// Sweeping our anchor output with a key spend witness.
	case TaprootAnchorSweepSpend:
		return TaprootAnchorWitnessSize, false, nil

	case TaprootHtlcOfferedTimeoutSecondLevel,
		TaprootHtlcAcceptedSuccessSecondLevel:

		return TaprootSecondLevelHtlcWitnessSize, false, nil

	case TaprootHtlcSecondLevelRevoke:
		return TaprootSecondLevelRevokeWitnessSize, false, nil

	case TaprootHtlcAcceptedRevoke:
		return TaprootAcceptedRevokeWitnessSize, false, nil

	case TaprootHtlcOfferedRevoke:
		return TaprootOfferedRevokeWitnessSize, false, nil

	case TaprootHtlcOfferedRemoteTimeout:
		return TaprootHtlcOfferedRemoteTimeoutWitnessSize, false, nil

	case TaprootHtlcLocalOfferedTimeout:
		return TaprootOfferedLocalTimeoutWitnessSize, false, nil

	case TaprootHtlcAcceptedRemoteSuccess:
		return TaprootHtlcAcceptedRemoteSuccessWitnessSize, false, nil

	case TaprootHtlcAcceptedLocalSuccess:
		return TaprootHtlcAcceptedLocalSuccessWitnessSize, false, nil

	case TaprootCommitmentRevoke:
		return TaprootToLocalRevokeWitnessSize, false, nil
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
