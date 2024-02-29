package sweep

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrInvalidBumpResult is returned when the bump result is invalid.
	ErrInvalidBumpResult = errors.New("invalid bump result")
)

// Bumper defines an interface that can be used by other subsystems for fee
// bumping.
type Bumper interface {
	// Broadcast is used to publish the tx created from the given inputs
	// specified in the request. It handles the tx creation, broadcasts it,
	// and monitors its confirmation status for potential fee bumping. It
	// returns a chan that the caller can use to receive updates about the
	// broadcast result and potential RBF attempts.
	Broadcast(req *BumpRequest) (<-chan *BumpResult, error)
}

// BumpEvent represents the event of a fee bumping attempt.
type BumpEvent uint8

const (
	// TxPublished is sent when the broadcast attempt is finished.
	TxPublished BumpEvent = iota

	// TxFailed is sent when the broadcast attempt fails.
	TxFailed

	// TxReplaced is sent when the original tx is replaced by a new one.
	TxReplaced

	// TxConfirmed is sent when the tx is confirmed.
	TxConfirmed

	// sentinalEvent is used to check if an event is unknown.
	sentinalEvent
)

// String returns a human-readable string for the event.
func (e BumpEvent) String() string {
	switch e {
	case TxPublished:
		return "Published"
	case TxFailed:
		return "Failed"
	case TxReplaced:
		return "Replaced"
	case TxConfirmed:
		return "Confirmed"
	default:
		return "Unknown"
	}
}

// Unknown returns true if the event is unknown.
func (e BumpEvent) Unknown() bool {
	return e >= sentinalEvent
}

// BumpRequest is used by the caller to give the Bumper the necessary info to
// create and manage potential fee bumps for a set of inputs.
type BumpRequest struct {
	// Budget givens the total amount that can be used as fees by these
	// inputs.
	Budget btcutil.Amount

	// Inputs is the set of inputs to sweep.
	Inputs []input.Input

	// DeadlineHeight is the block height at which the tx should be
	// confirmed.
	DeadlineHeight int32

	// DeliveryAddress is the script to send the change output to.
	DeliveryAddress []byte

	// MaxFeeRate is the maximum fee rate that can be used for fee bumping.
	MaxFeeRate chainfee.SatPerKWeight
}

// MaxFeeRateAllowed returns the maximum fee rate allowed for the given
// request. It calculates the feerate using the supplied budget and the weight,
// compares it with the specified MaxFeeRate, and returns the smaller of the
// two.
func (r *BumpRequest) MaxFeeRateAllowed() (chainfee.SatPerKWeight, error) {
	// Get the size of the sweep tx, which will be used to calculate the
	// budget fee rate.
	size, err := calcSweepTxWeight(r.Inputs, r.DeliveryAddress)
	if err != nil {
		return 0, err
	}

	// Use the budget and MaxFeeRate to decide the max allowed fee rate.
	// This is needed as, when the input has a large value and the user
	// sets the budget to be proportional to the input value, the fee rate
	// can be very high and we need to make sure it doesn't exceed the max
	// fee rate.
	maxFeeRateAllowed := chainfee.NewSatPerKWeight(r.Budget, size)
	if maxFeeRateAllowed > r.MaxFeeRate {
		log.Debugf("Budget feerate %v exceeds MaxFeeRate %v, use "+
			"MaxFeeRate instead", maxFeeRateAllowed, r.MaxFeeRate)

		return r.MaxFeeRate, nil
	}

	log.Debugf("Budget feerate %v below MaxFeeRate %v, use budget feerate "+
		"instead", maxFeeRateAllowed, r.MaxFeeRate)

	return maxFeeRateAllowed, nil
}

// calcSweepTxWeight calculates the weight of the sweep tx. It assumes a
// sweeping tx always has a single output(change).
func calcSweepTxWeight(inputs []input.Input,
	outputPkScript []byte) (uint64, error) {

	// Use a const fee rate as we only use the weight estimator to
	// calculate the size.
	const feeRate = 1

	// Initialize the tx weight estimator with,
	// - nil outputs as we only have one single change output.
	// - const fee rate as we don't care about the fees here.
	// - 0 maxfeerate as we don't care about fees here.
	//
	// TODO(yy): we should refactor the weight estimator to not require a
	// fee rate and max fee rate and make it a pure tx weight calculator.
	_, estimator, err := getWeightEstimate(
		inputs, nil, feeRate, 0, outputPkScript,
	)
	if err != nil {
		return 0, err
	}

	return uint64(estimator.weight()), nil
}

// BumpResult is used by the Bumper to send updates about the tx being
// broadcast.
type BumpResult struct {
	// Event is the type of event that the result is for.
	Event BumpEvent

	// Tx is the tx being broadcast.
	Tx *wire.MsgTx

	// ReplacedTx is the old, replaced tx if a fee bump is attempted.
	ReplacedTx *wire.MsgTx

	// FeeRate is the fee rate used for the new tx.
	FeeRate chainfee.SatPerKWeight

	// Fee is the fee paid by the new tx.
	Fee btcutil.Amount

	// Err is the error that occurred during the broadcast.
	Err error
}

// Validate validates the BumpResult so it's safe to use.
func (b *BumpResult) Validate() error {
	// Every result must have a tx.
	if b.Tx == nil {
		return fmt.Errorf("%w: nil tx", ErrInvalidBumpResult)
	}

	// Every result must have a known event.
	if b.Event.Unknown() {
		return fmt.Errorf("%w: unknown event", ErrInvalidBumpResult)
	}

	// If it's a replacing event, it must have a replaced tx.
	if b.Event == TxReplaced && b.ReplacedTx == nil {
		return fmt.Errorf("%w: nil replacing tx", ErrInvalidBumpResult)
	}

	// If it's a failed event, it must have an error.
	if b.Event == TxFailed && b.Err == nil {
		return fmt.Errorf("%w: nil error", ErrInvalidBumpResult)
	}

	// If it's a confirmed event, it must have a fee rate and fee.
	if b.Event == TxConfirmed && (b.FeeRate == 0 || b.Fee == 0) {
		return fmt.Errorf("%w: missing fee rate or fee",
			ErrInvalidBumpResult)
	}

	return nil
}
