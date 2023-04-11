package invoices

import (
	"context"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/database"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/record"
)

// InvScanFunc is a helper type used to specify the type used in the
// ScanInvoices methods (part of the InvoiceDB interface).
type InvScanFunc func(lntypes.Hash, *Invoice) error

// InvoiceDB is the database that stores the information about invoices.
type InvoiceDB interface {
	// AddInvoice inserts the given invoice into the database.
	//
	// NOTE: If the invoice is added to the database this method will
	// populate the next avaialbe value for the AddIndex field.
	//
	// NOTE: This method will be executed in its own transaction and will
	// use the default timeout.
	AddInvoice(invoice *Invoice) error

	// AddInvoiceWithTx inserts the given invoice into the database.
	//
	// NOTE: If the invoice is added to the database this method will
	// populate the next avaialbe value for the AddIndex field.
	AddInvoiceWithTx(ctx context.Context, dbTx database.DBTx,
		invoice *Invoice) error

	// GetInvoice fetches the invoice identified by the passed InvoiceRef.
	//
	// NOTE: If the ref is invalid this function will return the
	// ErrInvoiceNotFound error.
	//
	// NOTE: This function will be executed in its own transaction and will
	// use the default database timeout.
	GetInvoice(ref InvoiceRef) (*Invoice, error)

	// GetInvoiceWithTx fetches the invoice identified by the passed
	// InvoiceRef.
	//
	// NOTE: If the ref is invalid this function will return the
	// ErrInvoiceNotFound error.
	GetInvoiceWithTx(ctx context.Context, dbTx database.DBTx,
		ref InvoiceRef) (*Invoice, error)

	// InvoicesAddedSince can be used by callers to seek into the event
	// time series of all the invoices added in the database. The specified
	// sinceAddIndex should be the highest add index that the caller knows
	// of. This method will return all invoices with an add index greater
	// than the specified sinceAddIndex.
	//
	// NOTE: The index starts from 1, as a result. We enforce that
	// specifying a value below the starting index value is a noop.
	InvoicesAddedSince(sinceAddIndex uint64) ([]Invoice, error)

	// LookupInvoice attempts to look up an invoice according to its 32 byte
	// payment hash. If an invoice which can settle the HTLC identified by
	// the passed payment hash isn't found, then an error is returned.
	// Otherwise, the full invoice is returned.
	// Before setting the incoming HTLC, the values SHOULD be checked to
	// ensure the payer meets the agreed upon contractual terms of the
	// payment.
	LookupInvoice(ref InvoiceRef) (Invoice, error)

	// ScanInvoices scans through all invoices and calls the passed scanFunc
	// for each invoice with its respective payment hash. Additionally a
	// reset() closure is passed which is used to reset/initialize partial
	// results and also to signal if the kvdb.View transaction has been
	// retried.
	//
	// TODO(positiveblue): abstract this functionality so it makes sense for
	// other backends like sql.
	ScanInvoices(scanFunc InvScanFunc, reset func()) error

	// QueryInvoices allows a caller to query the invoice database for
	// invoices within the specified add index range.
	QueryInvoices(q InvoiceQuery) (InvoiceSlice, error)

	// UpdateInvoice attempts to update an invoice corresponding to the
	// passed payment hash. If an invoice matching the passed payment hash
	// doesn't exist within the database, then the action will fail with a
	// "not found" error.
	//
	// The update is performed inside the same database transaction that
	// fetches the invoice and is therefore atomic. The fields to update
	// are controlled by the supplied callback.
	//
	// TODO(positiveblue): abstract this functionality so it makes sense for
	// other backends like sql.
	UpdateInvoice(ref InvoiceRef, setIDHint *SetID,
		callback InvoiceUpdateCallback) (*Invoice, error)

	// InvoicesSettledSince can be used by callers to catch up any settled
	// invoices they missed within the settled invoice time series. We'll
	// return all known settled invoice that have a settle index higher than
	// the passed sinceSettleIndex.
	//
	// NOTE: The index starts from 1, as a result. We enforce that
	// specifying a value below the starting index value is a noop.
	InvoicesSettledSince(sinceSettleIndex uint64) ([]Invoice, error)

	// DeleteInvoice attempts to delete the passed invoices from the
	// database in one transaction. The passed delete references hold all
	// keys required to delete the invoices without also needing to
	// deserialze them.
	DeleteInvoice(invoicesToDelete []InvoiceDeleteRef) error
}

// InvoiceDB2 is the database that stores the information about invoices.
//
// TODO(positiveblue): remove this interface once the migration is complete.
// This is a temporary interface to allow us to follow better the next changes
// in the commits.
type InvoiceDB2 interface {
	// UpdateInvoice updates the invoice identified by the given InvoiceRef.
	//
	// NOTE: If the ContractState is ContractSettled and the passed invoice
	// does not have a valid settle index this method will populate the
	// SettleIndex field with the next availabe value.
	//
	// NOTE: Unless the invoice is being settled or canceled this method
	// does not update any changes related to the htlc fields.
	//
	// NOTE: This method will be executed in its own transaction and will
	// use the default timeout.
	UpdateInvoice(invoice *Invoice) error

	// UpdateInvoiceWithTx updates the invoice identified by the given
	// InvoiceRef.
	//
	// NOTE: If the ContractState is ContractSettled and the passed invoice
	// does not have a valid settle index this method will populate the
	// SettleIndex field with the next availabe value.
	//
	// NOTE: Unless the invoice is being settled or canceled this method
	// does not update any changes related to the htlc fields.
	UpdateInvoiceWithTx(ctx context.Context, dbTx database.DBTx,
		invoice *Invoice) error

	// DeleteInvoices updates the invoice identified by the given
	// InvoiceRef.
	//
	// NOTE: This method will be executed in its own transaction and will
	// use the default timeout.
	DeleteInvoices(refs []InvoiceRef) error

	// DeleteInvoiceWithTx updates the invoice identified by the given
	// InvoiceRef.
	DeleteInvoicesWithTx(ctx context.Context, dbTx database.DBTx,
		refs []InvoiceRef) error

	// AddHtlc adds new htlc to an existing invoice.
	//
	// NOTE: This method will be executed in its own transaction and will
	// use the default timeout.
	AddHtlc(ref InvoiceRef, htlcID models.CircuitKey,
		htlc *InvoiceHTLC) error

	// AddHtlcWithTx adds new htlc to an existing invoice.
	AddHtlcWithTx(ctx context.Context, dbTx database.DBTx,
		ref InvoiceRef, htlcID models.CircuitKey,
		htlc *InvoiceHTLC) error

	// UpdateInvoiceHTLCs updates the htlcs for the given invoice.
	//
	// NOTE: This function will be executed in its own transaction and will
	// use the default timeout.
	UpdateInvoiceHTLCs(ref InvoiceRef, state HtlcState,
		timestamp time.Time) error

	// UpdateInvoiceHTLCsWithTx updates the htlcs for the given invoice.
	UpdateInvoiceHTLCsWithTx(ctx context.Context, dbTx database.DBTx,
		ref InvoiceRef, state HtlcState, timestamp time.Time) error
}

// Payload abstracts access to any additional fields provided in the final hop's
// TLV onion payload.
type Payload interface {
	// MultiPath returns the record corresponding the option_mpp parsed from
	// the onion payload.
	MultiPath() *record.MPP

	// AMPRecord returns the record corresponding to the option_amp record
	// parsed from the onion payload.
	AMPRecord() *record.AMP

	// CustomRecords returns the custom tlv type records that were parsed
	// from the payload.
	CustomRecords() record.CustomSet

	// Metadata returns the additional data that is sent along with the
	// payment to the payee.
	Metadata() []byte
}

// InvoiceQuery represents a query to the invoice database. The query allows a
// caller to retrieve all invoices starting from a particular add index and
// limit the number of results returned.
type InvoiceQuery struct {
	// IndexOffset is the offset within the add indices to start at. This
	// can be used to start the response at a particular invoice.
	IndexOffset uint64

	// NumMaxInvoices is the maximum number of invoices that should be
	// starting from the add index.
	NumMaxInvoices uint64

	// PendingOnly, if set, returns unsettled invoices starting from the
	// add index.
	PendingOnly bool

	// Reversed, if set, indicates that the invoices returned should start
	// from the IndexOffset and go backwards.
	Reversed bool

	// CreationDateStart, if set, filters out all invoices with a creation
	// date greater than or euqal to it.
	CreationDateStart time.Time

	// CreationDateEnd, if set, filters out all invoices with a creation
	// date less than or euqal to it.
	CreationDateEnd time.Time
}

// InvoiceSlice is the response to a invoice query. It includes the original
// query, the set of invoices that match the query, and an integer which
// represents the offset index of the last item in the set of returned invoices.
// This integer allows callers to resume their query using this offset in the
// event that the query's response exceeds the maximum number of returnable
// invoices.
type InvoiceSlice struct {
	InvoiceQuery

	// Invoices is the set of invoices that matched the query above.
	Invoices []Invoice

	// FirstIndexOffset is the index of the first element in the set of
	// returned Invoices above. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response.
	FirstIndexOffset uint64

	// LastIndexOffset is the index of the last element in the set of
	// returned Invoices above. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response.
	LastIndexOffset uint64
}

// CircuitKey is a tuple of channel ID and HTLC ID, used to uniquely identify
// HTLCs in a circuit.
type CircuitKey = models.CircuitKey
