package sqldb

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/clock"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
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

// AddInvoice inserts the targeted invoice into the database.
// If the invoice has *any* payment hashes which already exists within
// the database, then the insertion will be aborted and rejected due to
// the strict policy banning any duplicate payment hashes.
//
// NOTE: A side effect of this function is that it sets AddIndex on
// newInvoice.
func (i *InvoiceStore) AddInvoice(ctx context.Context,
	newInvoice *invpkg.Invoice, paymentHash lntypes.Hash) error {

	// Make sure this is a valid invoice before trying to store it in our
	// DB.
	if err := invpkg.ValidateInvoice(newInvoice, paymentHash); err != nil {
		return err
	}

	var writeTxOpts InvoiceQueriesTxOptions
	err := i.db.ExecTx(ctx, &writeTxOpts, func(db InvoiceQueries) error {
		var fb bytes.Buffer
		err := newInvoice.Terms.Features.EncodeBase256(&fb)
		if err != nil {
			return err
		}

		params := sqlc.InsertInvoiceParams{
			// Mandatory (not nullable).
			AmountMsat:     int64(newInvoice.Terms.Value),
			Expiry:         int32(newInvoice.Terms.Expiry),
			Hash:           paymentHash[:],
			State:          int16(newInvoice.State),
			AmountPaidMsat: int64(newInvoice.AmtPaid),
			IsAmp:          newInvoice.IsAMP(),
			IsHodl:         newInvoice.HodlInvoice,
			IsKeysend:      newInvoice.IsKeysend(),
			CreatedAt:      newInvoice.CreationDate,
			// Optional (nullable).
			Memo: sqlStr(string(newInvoice.Memo)),
			// KeySend invoices don't have a payment request.
			PaymentRequest: sqlStr(string(
				newInvoice.PaymentRequest),
			),
		}

		// BOLT12 invoices don't have a final cltv delta.
		params.CltvDelta = sqlInt32(newInvoice.Terms.FinalCltvDelta)

		// Some invoices may not have a preimage, like in the case of
		// HODL invoices.
		if newInvoice.Terms.PaymentPreimage != nil {
			params.Preimage = newInvoice.Terms.PaymentPreimage[:]
		}

		// Some non MPP payments may have the defaulf (invalid) value.
		if newInvoice.Terms.PaymentAddr != invpkg.BlankPayAddr {
			params.PaymentAddr = newInvoice.Terms.PaymentAddr[:]
		}

		addIdx, err := db.InsertInvoice(ctx, params)
		if err != nil {
			return fmt.Errorf("unable to insert invoice: %w", err)
		}

		// TODO(positiveblue): if invocies do not have custom features
		// maybe just store the "invoice type" and populate the features
		// based on that.
		for feature := range newInvoice.Terms.Features.Features() {
			params := sqlc.InsertInvoiceFeatureParams{
				InvoiceID: addIdx,
				Feature:   int32(feature),
			}

			err := db.InsertInvoiceFeature(ctx, params)
			if err != nil {
				return fmt.Errorf("unable to insert invoice "+
					"feature(%v): %w", feature, err)
			}
		}

		eventParams := sqlc.InsertInvoiceEventParams{
			InvoiceID: addIdx,
			CreatedAt: i.clock.Now(),
			EventType: int32(InvoiceCreated),
		}

		err = db.InsertInvoiceEvent(ctx, eventParams)
		if err != nil {
			return fmt.Errorf("unable to insert invoice event: %w",
				err)
		}

		newInvoice.AddIndex = uint64(addIdx)

		return nil
	})

	if err != nil {
		return fmt.Errorf("unable to add invoice(%v): %w", paymentHash,
			err)
	}

	return nil
}
