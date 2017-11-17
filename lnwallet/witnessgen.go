package lnwallet

import (
	"fmt"

	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
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

	// HtlcOfferedRevoke is a witness that allows us to sweep an HTLC
	// output that we offered to the counterparty.
	HtlcOfferedRevoke WitnessType = 3

	// HtlcAcceptedRevoke is a witness that allows us to sweep an HTLC
	// output that we accepted from the counterparty.
	HtlcAcceptedRevoke WitnessType = 4

	// HtlcOfferedTimeout is a witness that allows us to sweep an HTLC
	// output that we extended to a party, but was never fulfilled.
	HtlcOfferedTimeout WitnessType = 5

	// HtlcAcceptedSuccess is a witness that allows us to sweep an HTLC
	// output that was offered to us, and for which we have a payment
	// preimage.
	HtlcAcceptedSuccess WitnessType = 6
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
		case HtlcOfferedTimeout:
			return HtlcSpendSuccess(signer, desc, tx)
		default:
			return nil, fmt.Errorf("unknown witness type: %v", wt)
		}
	}

}
