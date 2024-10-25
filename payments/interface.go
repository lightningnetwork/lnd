package payments

import (
	"context"

	"github.com/lightningnetwork/lnd/lntypes"
)

// PaymentDB is the database that stores the information about payments.
type PaymentDB interface {
	InitPayment(paymentHash lntypes.Hash,
		info *PaymentCreationInfo) error

	DeleteFailedAttempts(hash lntypes.Hash) error

	RegisterAttempt(paymentHash lntypes.Hash,
		attempt *HTLCAttemptInfo) (*MPPayment, error)

	SettleAttempt(hash lntypes.Hash,
		attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error)

	FailAttempt(hash lntypes.Hash,
		attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error)

	FetchPayment(paymentHash lntypes.Hash) (
		*MPPayment, error)

	FetchInFlightPayments() ([]*MPPayment, error)

	Fail(paymentHash lntypes.Hash,
		reason FailureReason) (*MPPayment, error)

	// QueryInvoices allows a caller to query the invoice database for
	// invoices within the specified add index range.
	QueryPayments(ctx context.Context,
		q PaymentsQuery) (PaymentsSlice, error)

	DeletePayment(paymentHash lntypes.Hash,
		failedHtlcsOnly bool) error

	DeletePayments(failedOnly, failedHtlcsOnly bool) error
}

// DBMPPayment is an interface derived from channeldb.MPPayment that is used by
// the payment lifecycle.
type dBMPPayment interface {
	// GetState returns the current state of the payment.
	GetState() *MPPaymentState

	// Terminated returns true if the payment is in a final state.
	Terminated() bool

	// GetStatus returns the current status of the payment.
	GetStatus() PaymentStatus

	// NeedWaitAttempts specifies whether the payment needs to wait for the
	// outcome of an attempt.
	NeedWaitAttempts() (bool, error)

	// GetHTLCs returns all HTLCs of this payment.
	GetHTLCs() []HTLCAttempt

	// InFlightHTLCs returns all HTLCs that are in flight.
	InFlightHTLCs() []HTLCAttempt

	// AllowMoreAttempts is used to decide whether we can safely attempt
	// more HTLCs for a given payment state. Return an error if the payment
	// is in an unexpected state.
	AllowMoreAttempts() (bool, error)

	// TerminalInfo returns the settled HTLC attempt or the payment's
	// failure reason.
	TerminalInfo() (*HTLCAttempt, *FailureReason)
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

// PaymentsSlice contains the result of a query to the payments database.
// It includes the set of payments that match the query and integers which
// represent the index of the first and last item returned in the series of
// payments. These integers allow callers to resume their query in the event
// that the query's response exceeds the max number of returnable events.
type PaymentsSlice struct {
	PaymentsQuery
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
