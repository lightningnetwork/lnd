package sweep

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

type FeeBumpStrategy uint8

const (
	// FeeBumpStrategyAggressive indicates that the fee bumping should
	// always use the larger fee rate between the calculated fee rate and
	// the estimated fee rate.
	FeeBumpStrategyAggressive FeeBumpStrategy = iota

	// FeeBumpStrategyConservative indicates that the fee bumping should
	// always use the smaller fee rate between the calculated fee rate and
	// the estimated fee rate.
	FeeBumpStrategyConservative

	// TODO(yy): define more sophisticated strategies.
)

// Bumper defines an interface that can be used by other subsystems for fee
// bumping.
type Bumper interface {
	// Broadcast is used to publish the tx created from the given inputs
	// specified in the request. It handles the tx creation, broadcasts it,
	// and monitors its confirmation status for potential fee bumping. It
	// returns a chan that the caller can use to receive updates about the
	// broadcast result and potential RBF attempts.
	//
	// NOTE: it's expected to have a BumpResult sent immediately once the
	// first broadcast attempt is finished.
	Broadcast(req *BumpRequest) (<-chan *BumpResult, error)
}

// BumpRequest is used by the caller to give the Bumper the necessary info to
// create and manage potential fee bumps for a set of inputs.
type BumpRequest struct {
	// Inputs is the set of inputs to sweep.
	Inputs InputSet

	// ChangeScript is the script to send the change output to.
	ChangeScript []byte

	// MaxFeeRate is the maximum fee rate that can be used for fee bumping.
	MaxFeeRate chainfee.SatPerKWeight

	// Strategy is the fee bumping strategy to use.
	Strategy FeeBumpStrategy
}

// BumpResult is used by the Bumper to send updates about the initial broadcast
// result and future RBF results if attemptted.
type BumpResult struct {
	// Confirmed indicates whether the `CurrentTx` was confirmed or not.
	// This is used as a signal to terminate the subscription of the
	// returned result chan.
	Confirmed bool

	// PreviousTx is the tx that's being replaced. Initially, this is the
	// tx that was broadcasted from the initial request. If a fee bump is
	// attempted, this will be the tx that's being replaced by the new tx.
	PreviousTx *wire.MsgTx

	// CurrentTx is the replacing tx if a fee bump is attempted.
	CurrentTx *wire.MsgTx

	// FeeRate is the fee rate used for the new tx.
	FeeRate chainfee.SatPerKWeight

	// Fee is the fee paid by the new tx.
	Fee btcutil.Amount

	// Err is the error that occurred during the broadcast.
	Err error
}
