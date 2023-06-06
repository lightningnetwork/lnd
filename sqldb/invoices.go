package sqldb

import (
	"context"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// InvoiceQueries is an interface that defines the set of operations that can be
// executed against the invoice database.
type InvoiceQueries interface { //nolint:interfacebloat
	InsertInvoice(ctx context.Context, arg sqlc.InsertInvoiceParams) (int64,
		error)

	InsertInvoiceFeature(ctx context.Context,
		arg sqlc.InsertInvoiceFeatureParams) error

	InsertInvoiceHTLC(ctx context.Context,
		arg sqlc.InsertInvoiceHTLCParams) error

	InsertInvoiceHTLCCustomRecord(ctx context.Context,
		arg sqlc.InsertInvoiceHTLCCustomRecordParams) error

	InsertInvoicePayment(ctx context.Context,
		arg sqlc.InsertInvoicePaymentParams) (int64, error)

	FilterInvoices(ctx context.Context,
		arg sqlc.FilterInvoicesParams) ([]sqlc.Invoice, error)

	GetInvoice(ctx context.Context,
		arg sqlc.GetInvoiceParams) ([]sqlc.Invoice, error)

	GetInvoiceFeatures(ctx context.Context,
		invoiceID int64) ([]sqlc.InvoiceFeature, error)

	GetInvoiceHTLCCustomRecords(ctx context.Context,
		invoiceID int64) ([]sqlc.GetInvoiceHTLCCustomRecordsRow, error)

	GetInvoiceHTLCs(ctx context.Context,
		invoiceID int64) ([]sqlc.InvoiceHtlc, error)

	FilterInvoicePayments(ctx context.Context,
		arg sqlc.FilterInvoicePaymentsParams) ([]sqlc.InvoicePayment,
		error)

	GetInvoicePayments(ctx context.Context,
		invoiceID int64) ([]sqlc.InvoicePayment, error)

	UpdateInvoice(ctx context.Context, arg sqlc.UpdateInvoiceParams) error

	UpdateInvoiceHTLC(ctx context.Context,
		arg sqlc.UpdateInvoiceHTLCParams) error

	UpdateInvoiceHTLCs(ctx context.Context,
		arg sqlc.UpdateInvoiceHTLCsParams) error

	DeleteInvoice(ctx context.Context, arg sqlc.DeleteInvoiceParams) error

	DeleteInvoiceFeatures(ctx context.Context, invoiceID int64) error

	DeleteInvoiceHTLC(ctx context.Context, htlcID string) error

	DeleteInvoiceHTLCCustomRecords(ctx context.Context,
		invoiceID int64) error

	DeleteInvoiceHTLCs(ctx context.Context, invoiceID int64) error

	// AMP specific methods.

	InsertAMPInvoiceHTLC(ctx context.Context,
		arg sqlc.InsertAMPInvoiceHTLCParams) error

	InsertAMPInvoicePayment(ctx context.Context,
		arg sqlc.InsertAMPInvoicePaymentParams) error

	GetAMPInvoiceHTLCsByInvoiceID(ctx context.Context,
		invoiceID int64) ([]sqlc.AmpInvoiceHtlc, error)

	GetAMPInvoiceHTLCsBySetID(ctx context.Context,
		setID []byte) ([]sqlc.AmpInvoiceHtlc, error)

	GetSetIDHTLCsCustomRecords(ctx context.Context,
		setID []byte) ([]sqlc.GetSetIDHTLCsCustomRecordsRow, error)

	SelectAMPInvoicePayments(ctx context.Context,
		arg sqlc.SelectAMPInvoicePaymentsParams) ([]sqlc.SelectAMPInvoicePaymentsRow, error) //nolint:lll

	UpdateAMPInvoiceHTLC(ctx context.Context,
		arg sqlc.UpdateAMPInvoiceHTLCParams) error

	UpdateAMPPayment(ctx context.Context,
		arg sqlc.UpdateAMPPaymentParams) error

	DeleteAMPInvoiceHTLC(ctx context.Context, setID []byte) error

	// Event specific methods.

	InsertInvoiceEvent(ctx context.Context,
		arg sqlc.InsertInvoiceEventParams) error

	SelectInvoiceEvents(ctx context.Context,
		arg sqlc.SelectInvoiceEventsParams) ([]sqlc.InvoiceEvent, error)

	DeleteInvoiceEvents(ctx context.Context, invoiceID int64) error
}

// InvoiceQueriesTxOptions defines the set of db txn options the InvoiceQueries
// understands.
type InvoiceQueriesTxOptions struct {
	// readOnly governs if a read only transaction is needed or not.
	readOnly bool
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This implements the TxOptions.
func (a *InvoiceQueriesTxOptions) ReadOnly() bool {
	return a.readOnly
}

// NewInvoiceQueryReadTx creates a new read transaction option set.
func NewInvoiceQueryReadTx() InvoiceQueriesTxOptions {
	return InvoiceQueriesTxOptions{
		readOnly: true,
	}
}

// BatchedInvoiceQueries is a version of the InvoiceQueries that's capable of
// batched database operations.
type BatchedInvoiceQueries interface {
	InvoiceQueries

	BatchedTx[InvoiceQueries]
}

// InvoiceStore represents a storage backend.
type InvoiceStore struct {
	db    BatchedInvoiceQueries
	clock clock.Clock
}

// InvoiceEventType is the type of an invoice event.
type InvoiceEventType uint16

const (
	// InvoiceCreated is the event type emitted when an invoice is created.
	InvoiceCreated InvoiceEventType = iota

	// InvoiceCanceled is the event type emitted when an invoice is
	// canceled.
	InvoiceCanceled

	// InvoiceSettled is the event type emitted when an invoice is settled.
	InvoiceSettled

	// InvoiceSetIDCreated is the event type emitted when the first htlc of
	// a set id is accepted.
	InvoiceSetIDCreated

	// InvoiceSetIDCanceled is the event type emitted when the set id is
	// canceled.
	InvoiceSetIDCanceled

	// InvoiceSetIDSettled is the event type emitted when the set id is
	// settled.
	InvoiceSetIDSettled
)

// String returns a human-readable description of an event type.
func (i InvoiceEventType) String() string {
	switch i {
	case InvoiceCreated:
		return "invoice created"

	case InvoiceCanceled:
		return "invoice canceled"

	case InvoiceSettled:
		return "invoice settled"

	case InvoiceSetIDCreated:
		return "invoice set id created"

	case InvoiceSetIDCanceled:
		return "invoice set id canceled"

	case InvoiceSetIDSettled:
		return "invoice set id settled"

	default:
		return "unknown invoice event type"
	}
}

// NewInvoiceStore creates a new InvoiceStore instance given a open
// BatchedInvoiceQueries storage backend.
func NewInvoiceStore(db BatchedInvoiceQueries) *InvoiceStore {
	return &InvoiceStore{
		db:    db,
		clock: clock.NewDefaultClock(),
	}
}
