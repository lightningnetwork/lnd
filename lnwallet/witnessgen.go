package lnwallet

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
)

// WitnessGenerator represents a function which is able to generate the final
// witness for a particular public key script. This function acts as an
// abstraction layer, hiding the details of the underlying script.
type WitnessGenerator func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
	inputIndex int) ([][]byte, error)

// GenWitnessFunc will return a WitnessGenerator function that an output
// uses to generate the witness for a sweep transaction.
func (wt WitnessType) GenWitnessFunc(signer Signer,
	descriptor *SignDescriptor) WitnessGenerator {

	return func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
		inputIndex int) ([][]byte, error) {

		desc := descriptor
		desc.SigHashes = hc
		desc.InputIndex = inputIndex

		switch wt {
		case CommitmentTimeLock:
			return CommitSpendTimeout(signer, desc, tx)

		case CommitmentNoDelay:
			return CommitSpendNoDelay(signer, desc, tx)

		case CommitmentRevoke:
			return CommitSpendRevoke(signer, desc, tx)

		case HtlcOfferedRevoke:
			return ReceiverHtlcSpendRevoke(signer, desc, tx)

		case HtlcAcceptedRevoke:
			return SenderHtlcSpendRevoke(signer, desc, tx)

		case HtlcOfferedTimeoutSecondLevel:
			return HtlcSecondLevelSpend(signer, desc, tx)

		case HtlcAcceptedSuccessSecondLevel:
			return HtlcSecondLevelSpend(signer, desc, tx)

		case HtlcOfferedRemoteTimeout:
			// We pass in a value of -1 for the timeout, as we
			// expect the caller to have already set the lock time
			// value.
			return receiverHtlcSpendTimeout(signer, desc, tx, -1)

		case HtlcSecondLevelRevoke:
			return htlcSpendRevoke(signer, desc, tx)

		default:
			return nil, fmt.Errorf("unknown witness type: %v", wt)
		}
	}

}
