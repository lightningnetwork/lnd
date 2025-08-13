package channeldb

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// FailureReason encodes the reason a payment ultimately failed.
type FailureReason byte

const (
	// FailureReasonTimeout indicates that the payment did timeout before a
	// successful payment attempt was made.
	FailureReasonTimeout FailureReason = 0

	// FailureReasonNoRoute indicates no successful route to the
	// destination was found during path finding.
	FailureReasonNoRoute FailureReason = 1

	// FailureReasonError indicates that an unexpected error happened during
	// payment.
	FailureReasonError FailureReason = 2

	// FailureReasonPaymentDetails indicates that either the hash is unknown
	// or the final cltv delta or amount is incorrect.
	FailureReasonPaymentDetails FailureReason = 3

	// FailureReasonInsufficientBalance indicates that we didn't have enough
	// balance to complete the payment.
	FailureReasonInsufficientBalance FailureReason = 4

	// FailureReasonCanceled indicates that the payment was canceled by the
	// user.
	FailureReasonCanceled FailureReason = 5

	// TODO(joostjager): Add failure reasons for:
	// LocalLiquidityInsufficient, RemoteCapacityInsufficient.
)

// Error returns a human-readable error string for the FailureReason.
func (r FailureReason) Error() string {
	return r.String()
}

// String returns a human-readable FailureReason.
func (r FailureReason) String() string {
	switch r {
	case FailureReasonTimeout:
		return "timeout"
	case FailureReasonNoRoute:
		return "no_route"
	case FailureReasonError:
		return "error"
	case FailureReasonPaymentDetails:
		return "incorrect_payment_details"
	case FailureReasonInsufficientBalance:
		return "insufficient_balance"
	case FailureReasonCanceled:
		return "canceled"
	}

	return "unknown"
}

// PaymentCreationInfo is the information necessary to have ready when
// initiating a payment, moving it into state InFlight.
type PaymentCreationInfo struct {
	// PaymentIdentifier is the hash this payment is paying to in case of
	// non-AMP payments, and the SetID for AMP payments.
	PaymentIdentifier lntypes.Hash

	// Value is the amount we are paying.
	Value lnwire.MilliSatoshi

	// CreationTime is the time when this payment was initiated.
	CreationTime time.Time

	// PaymentRequest is the full payment request, if any.
	PaymentRequest []byte

	// FirstHopCustomRecords are the TLV records that are to be sent to the
	// first hop of this payment. These records will be transmitted via the
	// wire message only and therefore do not affect the onion payload size.
	FirstHopCustomRecords lnwire.CustomRecords
}

// String returns a human-readable description of the payment creation info.
func (p *PaymentCreationInfo) String() string {
	return fmt.Sprintf("payment_id=%v, amount=%v, created_at=%v",
		p.PaymentIdentifier, p.Value, p.CreationTime)
}
