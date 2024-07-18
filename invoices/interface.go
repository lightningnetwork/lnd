package invoices

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

// InvoiceDB is the database that stores the information about invoices.
type InvoiceDB interface {
	// AddInvoice inserts the targeted invoice into the database.
	// If the invoice has *any* payment hashes which already exists within
	// the database, then the insertion will be aborted and rejected due to
	// the strict policy banning any duplicate payment hashes.
	//
	// NOTE: A side effect of this function is that it sets AddIndex on
	// newInvoice.
	AddInvoice(ctx context.Context, invoice *Invoice,
		paymentHash lntypes.Hash) (uint64, error)

	// InvoicesAddedSince can be used by callers to seek into the event
	// time series of all the invoices added in the database. The specified
	// sinceAddIndex should be the highest add index that the caller knows
	// of. This method will return all invoices with an add index greater
	// than the specified sinceAddIndex.
	//
	// NOTE: The index starts from 1, as a result. We enforce that
	// specifying a value below the starting index value is a noop.
	InvoicesAddedSince(ctx context.Context, sinceAddIndex uint64) (
		[]Invoice, error)

	// LookupInvoice attempts to look up an invoice according to its 32 byte
	// payment hash. If an invoice which can settle the HTLC identified by
	// the passed payment hash isn't found, then an error is returned.
	// Otherwise, the full invoice is returned.
	// Before setting the incoming HTLC, the values SHOULD be checked to
	// ensure the payer meets the agreed upon contractual terms of the
	// payment.
	LookupInvoice(ctx context.Context, ref InvoiceRef) (Invoice, error)

	// FetchPendingInvoices returns all invoices that have not yet been
	// settled or canceled.
	FetchPendingInvoices(ctx context.Context) (map[lntypes.Hash]Invoice,
		error)

	// QueryInvoices allows a caller to query the invoice database for
	// invoices within the specified add index range.
	QueryInvoices(ctx context.Context, q InvoiceQuery) (InvoiceSlice, error)

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
	UpdateInvoice(ctx context.Context, ref InvoiceRef, setIDHint *SetID,
		callback InvoiceUpdateCallback) (*Invoice, error)

	// InvoicesSettledSince can be used by callers to catch up any settled
	// invoices they missed within the settled invoice time series. We'll
	// return all known settled invoice that have a settle index higher than
	// the passed sinceSettleIndex.
	//
	// NOTE: The index starts from 1, as a result. We enforce that
	// specifying a value below the starting index value is a noop.
	InvoicesSettledSince(ctx context.Context, sinceSettleIndex uint64) (
		[]Invoice, error)

	// DeleteInvoice attempts to delete the passed invoices from the
	// database in one transaction. The passed delete references hold all
	// keys required to delete the invoices without also needing to
	// deserialize them.
	DeleteInvoice(ctx context.Context,
		invoicesToDelete []InvoiceDeleteRef) error

	// DeleteCanceledInvoices removes all canceled invoices from the
	// database.
	DeleteCanceledInvoices(ctx context.Context) error
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

	// PathID returns the path ID encoded in the payload of a blinded
	// payment.
	PathID() *chainhash.Hash

	// TotalAmtMsat returns the total amount sent to the final hop, as set
	// by the payee.
	TotalAmtMsat() lnwire.MilliSatoshi
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

	// CreationDateStart, expressed in Unix seconds, if set, filters out
	// all invoices with a creation date greater than or equal to it.
	CreationDateStart int64

	// CreationDateEnd, if set, filters out all invoices with a creation
	// date less than or equal to it.
	CreationDateEnd int64
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

// InvoiceUpdater is an interface to abstract away the details of updating an
// invoice in the database. The methods of this interface are called during the
// in-memory update of an invoice when the database needs to be updated or the
// updated state needs to be marked as needing to be written to the database.
type InvoiceUpdater interface {
	// AddHtlc adds a new htlc to the invoice.
	AddHtlc(circuitKey CircuitKey, newHtlc *InvoiceHTLC) error

	// ResolveHtlc marks an htlc as resolved with the given state.
	ResolveHtlc(circuitKey CircuitKey, state HtlcState,
		resolveTime time.Time) error

	// AddAmpHtlcPreimage adds a preimage of an AMP htlc to the AMP invoice
	// identified by the setID.
	AddAmpHtlcPreimage(setID [32]byte, circuitKey CircuitKey,
		preimage lntypes.Preimage) error

	// UpdateInvoiceState updates the invoice state to the new state.
	UpdateInvoiceState(newState ContractState,
		preimage *lntypes.Preimage) error

	// UpdateInvoiceAmtPaid updates the invoice amount paid to the new
	// amount.
	UpdateInvoiceAmtPaid(amtPaid lnwire.MilliSatoshi) error

	// UpdateAmpState updates the state of the AMP invoice identified by
	// the setID.
	UpdateAmpState(setID [32]byte, newState InvoiceStateAMP,
		circuitKey models.CircuitKey) error

	// Finalize finalizes the update before it is written to the database.
	Finalize(updateType UpdateType) error
}
