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

// PaymentsQuery represents a query to the payments database starting or ending
// at a certain offset index. The number of retrieved records can be limited.
type PaymentsQuery struct {
	// IndexOffset determines the starting point of the payments query and
	// is always exclusive. In normal order, the query starts at the next
	// higher (available) index compared to IndexOffset. In reversed order,
	// the query ends at the next lower (available) index compared to the
	// IndexOffset. In the case of a zero index_offset, the query will start
	// with the oldest payment when paginating forwards, or will end with
	// the most recent payment when paginating backwards.
	IndexOffset uint64

	// MaxPayments is the maximal number of payments returned in the
	// payments query.
	MaxPayments uint64

	// Reversed gives a meaning to the IndexOffset. If reversed is set to
	// true, the query will fetch payments with indices lower than the
	// IndexOffset, otherwise, it will return payments with indices greater
	// than the IndexOffset.
	Reversed bool

	// If IncludeIncomplete is true, then return payments that have not yet
	// fully completed. This means that pending payments, as well as failed
	// payments will show up if this field is set to true.
	IncludeIncomplete bool

	// CountTotal indicates that all payments currently present in the
	// payment index (complete and incomplete) should be counted.
	CountTotal bool

	// CreationDateStart, expressed in Unix seconds, if set, filters out
	// all payments with a creation date greater than or equal to it.
	CreationDateStart int64

	// CreationDateEnd, expressed in Unix seconds, if set, filters out all
	// payments with a creation date less than or equal to it.
	CreationDateEnd int64
}

// PaymentsResponse contains the result of a query to the payments database.
// It includes the set of payments that match the query and integers which
// represent the index of the first and last item returned in the series of
// payments. These integers allow callers to resume their query in the event
// that the query's response exceeds the max number of returnable events.
type PaymentsResponse struct {
	// Payments is the set of payments returned from the database for the
	// PaymentsQuery.
	Payments []*MPPayment

	// FirstIndexOffset is the index of the first element in the set of
	// returned MPPayments. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response. The offset can be used to continue reverse pagination.
	FirstIndexOffset uint64

	// LastIndexOffset is the index of the last element in the set of
	// returned MPPayments. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response. The offset can be used to continue forward pagination.
	LastIndexOffset uint64

	// TotalCount represents the total number of payments that are currently
	// stored in the payment database. This will only be set if the
	// CountTotal field in the query was set to true.
	TotalCount uint64
}
