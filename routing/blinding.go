package routing

import (
	"errors"
	"fmt"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrNoBlindedPath is returned when the blinded path in a blinded
	// payment is missing.
	ErrNoBlindedPath = errors.New("blinded path required")

	// ErrInsufficientBlindedHops is returned when a blinded path does
	// not have enough blinded hops.
	ErrInsufficientBlindedHops = errors.New("blinded path requires " +
		"at least one hop")

	// ErrHTLCRestrictions is returned when a blinded path has invalid
	// HTLC maximum and minimum values.
	ErrHTLCRestrictions = errors.New("invalid htlc minimum and maximum")
)

// BlindedPayment provides the path and payment parameters required to send a
// payment along a blinded path.
type BlindedPayment struct {
	// BlindedPath contains the unblinded introduction point and blinded
	// hops for the blinded section of the payment.
	BlindedPath *sphinx.BlindedPath

	// BaseFee is the total base fee to be paid for payments made over the
	// blinded path.
	BaseFee uint32

	// ProportionalFee is the aggregated proportional fee for payments
	// made over the blinded path.
	ProportionalFee uint32

	// CltvExpiryDelta is the total expiry delta for the blinded path. Note
	// this does not include the final cltv delta for the receiving node
	// (which should be provided in an invoice).
	CltvExpiryDelta uint16

	// HtlcMinimum is the highest HLTC minimum supported along the blinded
	// path (while some hops may have lower values, we're effectively
	// bounded by the highest minimum).
	HtlcMinimum uint64

	// HtlcMaximum is the lowest HTLC maximum supported along the blinded
	// path (while some hops may have higher values, we're effectively
	// bounded by the lowest maximum).
	HtlcMaximum uint64

	// Features is the set of relay features available for the payment.
	Features *lnwire.FeatureVector
}

// Validate performs validation on a blinded payment.
func (b *BlindedPayment) Validate() error {
	if b.BlindedPath == nil {
		return ErrNoBlindedPath
	}

	// The sphinx library inserts the introduction node as the first hop,
	// so we expect at least one hop.
	if len(b.BlindedPath.BlindedHops) < 1 {
		return fmt.Errorf("%w got: %v", ErrInsufficientBlindedHops,
			len(b.BlindedPath.BlindedHops))
	}

	if b.HtlcMaximum < b.HtlcMinimum {
		return fmt.Errorf("%w: %v < %v", ErrHTLCRestrictions,
			b.HtlcMaximum, b.HtlcMinimum)
	}

	return nil
}
