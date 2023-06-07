package sqldb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/clock"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
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
		arg sqlc.FilterInvoicePaymentsParams) ([]sqlc.FilterInvoicePaymentsRow, // nolint:lll
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

	DeleteInvoiceHTLC(ctx context.Context, htlcID int64) error

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

// LookupInvoice attempts to look up an invoice according to its 32 byte
// payment hash. If an invoice which can settle the HTLC identified by the
// passed payment hash isn't found, then an error is returned.
// Otherwise, the full invoice is returned.
// Before setting the incoming HTLC, the values SHOULD be checked to ensure the
// payer meets the agreed upon contractual terms of the payment.
func (i *InvoiceStore) LookupInvoice(ctx context.Context,
	ref invpkg.InvoiceRef) (*invpkg.Invoice, error) {

	var (
		invoice *invpkg.Invoice
		err     error
	)

	readTxOpt := InvoiceQueriesTxOptions{readOnly: true}
	err = i.db.ExecTx(ctx, &readTxOpt, func(db InvoiceQueries) error {
		switch {
		// If the reference is a SetID then we'll fetch the AMP invoice
		// and populate the HTLCs for that set id.
		case ref.SetID() != nil:
			return fmt.Errorf("lookup for amp invoices is not " +
				"implemented yet")

		default:
			params := sqlc.GetInvoiceParams{}
			if ref.PayHash() != nil {
				params.Hash = ref.PayHash()[:]
			}
			if ref.PayAddr() != nil {
				params.PaymentAddr = ref.PayAddr()[:]
			}

			rows, err := db.GetInvoice(ctx, params)
			switch {
			case len(rows) == 0:
				return fmt.Errorf("invoice not found")

			case len(rows) > 1:
				return fmt.Errorf("more than one invoice found")

			case err != nil:
				return fmt.Errorf("unable to get invoice from "+
					"db: %w", err)
			}

			// Load the common invoice data.
			invoice, err = fetchInvoiceData(ctx, db, rows[0])
			if err != nil {
				return err
			}

			// Load any extra HTLC data needed for this invoice ref.
			if invoice.IsAMP() {
				switch ref.Modifier() {
				case invpkg.DefaultModifier:
					// TODO(positiveblue): fetch all htlc
					// data.

				case invpkg.HtlcSetOnlyModifier:
					// TODO(positiveblue): should this ever
					// happen?

				case invpkg.HtlcSetBlankModifier:
					// No need to fetch any htlc data.

				default:
					return fmt.Errorf("unknown invoice "+
						"ref modifier: %v",
						ref.Modifier())
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("unable to lookup invoice(%s): %w", ref,
			err)
	}

	return invoice, nil
}

// InvoicesAddedSince can be used by callers to seek into the event time series
// of all the invoices added in the database. The specified sinceAddIndex should
// be the highest add index that the caller knows of. This method will return
// all invoices with an add index greater than the specified sinceAddIndex.
//
// NOTE: The index starts from 1, as a result. We enforce that specifying a
// value below the starting index value is a noop.
func (i *InvoiceStore) InvoicesAddedSince(ctx context.Context,
	id uint64) ([]*invpkg.Invoice, error) {

	return i.invoiceAddedSince(ctx, id, DefaultRowsLimit)
}

// invoiceAddedSince is a helper method that retrieves all invoices added since
// the specified add index. The limit parameter can be used to limit the number
// of invoices fetched at once from the database.
func (i *InvoiceStore) invoiceAddedSince(ctx context.Context,
	id uint64, limit int32) ([]*invpkg.Invoice, error) {

	var newInvoices []*invpkg.Invoice

	if id == 0 {
		return newInvoices, nil
	}

	readTxOpt := InvoiceQueriesTxOptions{readOnly: true}
	err := i.db.ExecTx(ctx, &readTxOpt, func(db InvoiceQueries) error {
		allRows := make([]sqlc.Invoice, 0, 100)

		addIdx := id
		limit := int32(100)
		offset := int32(0)

		for {
			params := sqlc.FilterInvoicesParams{
				// AddIndexGreaterOrEqualThan.
				AddIndexGet: sqlInt64(addIdx + 1),
				NumLimit:    limit,
				NumOffset:   offset,
			}

			rows, err := db.FilterInvoices(ctx, params)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unable to get invoices "+
					"from db: %w", err)
			}

			allRows = append(allRows, rows...)

			if len(rows) < int(limit) {
				break
			}

			addIdx += uint64(limit)
			offset += limit
		}

		if len(allRows) == 0 {
			return nil
		}

		// Load all the information for the invoices.
		for _, row := range allRows {
			// Load the common invoice data.
			invoice, err := fetchInvoiceData(ctx, db, row)
			if err != nil {
				return err
			}

			newInvoices = append(newInvoices, invoice)
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("unable to get invoices added since "+
			"index %d: %w", id, err)
	}

	return newInvoices, nil
}

// fetchInvoiceData fetches the common invoice data for the given params.
func fetchInvoiceData(ctx context.Context, db InvoiceQueries,
	row sqlc.Invoice) (*invpkg.Invoice, error) {

	// Unmarshal the common data.
	invoice, err := unmarshalInvoice(row)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal invoice(id=%d) "+
			"from db: %w", row.ID, err)
	}

	// Fetch the invoice features.
	features, err := getInvoiceFeatures(ctx, db, row.ID)
	if err != nil {
		return nil, err
	}

	invoice.Terms.Features = features

	// Fetch the payment data.
	dbPayments, err := db.GetInvoicePayments(ctx, row.ID)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("unable to get invoice payment: %w", err)

	case len(dbPayments) > 1:
		return nil, fmt.Errorf("more than one payment found")

	case len(dbPayments) == 1:
		dbPayment := dbPayments[0]
		invoice.SettleIndex = uint64(dbPayment.ID)
		invoice.SettleDate = dbPayment.SettledAt
	}

	// Fetch the invoice htlcs.
	htlcs, err := getInvoiceHtlcs(ctx, db, row.ID)
	if err != nil {
		return nil, err
	}

	if len(htlcs) > 0 {
		invoice.Htlcs = htlcs
	}

	return invoice, nil
}

// getInvoiceFeatures fetches the invoice features for the given invoice id.
func getInvoiceFeatures(ctx context.Context, db InvoiceQueries,
	invoiceID int64) (*lnwire.FeatureVector, error) {

	rows, err := db.GetInvoiceFeatures(ctx, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("unable to get invoice features: %w",
			err)
	}

	features := lnwire.EmptyFeatureVector()
	for _, feature := range rows {
		features.Set(lnwire.FeatureBit(feature.Feature))
	}

	return features, nil
}

// getInvoiceHtlcs fetches the invoice htlcs for the given invoice id.
func getInvoiceHtlcs(ctx context.Context, db InvoiceQueries,
	invoiceID int64) (map[invpkg.CircuitKey]*invpkg.InvoiceHTLC, error) {

	htlcRows, err := db.GetInvoiceHTLCs(ctx, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("unable to get invoice htlcs: %w", err)
	}

	// We have no htlcs to unmarshal.
	if len(htlcRows) == 0 {
		return nil, nil
	}

	crRows, err := db.GetInvoiceHTLCCustomRecords(ctx, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("unable to get custom records for "+
			"invoice htlcs: %w", err)
	}

	cr := make(map[int64]record.CustomSet, len(crRows))
	for _, row := range crRows {
		if _, ok := cr[row.HtlcID]; !ok {
			cr[row.HtlcID] = make(record.CustomSet)
		}

		cr[row.HtlcID][uint64(row.Key)] = row.Value
	}

	htlcs := make(map[invpkg.CircuitKey]*invpkg.InvoiceHTLC, len(htlcRows))

	for _, row := range htlcRows {
		circuiteKey, htlc, err := unmarshalInvoiceHTLC(row)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal "+
				"htlc(%d): %w", row.ID, err)

		}

		if customRecords, ok := cr[row.HtlcID]; ok {
			htlc.CustomRecords = customRecords
		}

		htlcs[circuiteKey] = htlc
	}

	return htlcs, nil
}

// unmarshalInvoice converts an InvoiceRow to an Invoice.
func unmarshalInvoice(row sqlc.Invoice) (*invpkg.Invoice, error) {
	var (
		memo           []byte
		paymentRequest = []byte{}

		preimage *lntypes.Preimage

		paymentAddr [32]byte
	)

	if row.Memo.Valid {
		memo = []byte(row.Memo.String)
	}

	// Keysend payments will have this field empty.
	if row.PaymentRequest.Valid {
		paymentRequest = []byte(row.PaymentRequest.String)
	}

	// If this invoice is a hodl invoice we may not have its preimage
	// stored.
	if row.Preimage != nil {
		preimage = &lntypes.Preimage{}
		copy(preimage[:], row.Preimage)
	}

	copy(paymentAddr[:], row.PaymentAddr)

	var cltvDelta int32
	if row.CltvDelta.Valid {
		cltvDelta = row.CltvDelta.Int32
	}

	invoice := &invpkg.Invoice{
		Memo:           memo,
		PaymentRequest: paymentRequest,
		CreationDate:   row.CreatedAt,
		Terms: invpkg.ContractTerm{
			FinalCltvDelta:  cltvDelta,
			Expiry:          time.Duration(row.Expiry),
			PaymentPreimage: preimage,
			Value:           lnwire.MilliSatoshi(row.AmountMsat),
			PaymentAddr:     paymentAddr,
		},
		AddIndex:    uint64(row.ID),
		State:       invpkg.ContractState(row.State),
		AmtPaid:     lnwire.MilliSatoshi(row.AmountPaidMsat),
		Htlcs:       make(map[models.CircuitKey]*invpkg.InvoiceHTLC),
		AMPState:    invpkg.AMPInvoiceState{},
		HodlInvoice: row.IsHodl,
	}

	return invoice, nil
}

// unmarshalInvoiceHTLC converts an sqlc.InvoiceHtlc to an InvoiceHTLC.
func unmarshalInvoiceHTLC(row sqlc.InvoiceHtlc) (invpkg.CircuitKey,
	*invpkg.InvoiceHTLC, error) {

	uint64ChanID, err := strconv.ParseUint(row.ChanID, 10, 64)
	if err != nil {
		return invpkg.CircuitKey{}, nil, err
	}

	chanID := lnwire.NewShortChanIDFromInt(uint64ChanID)

	if row.HtlcID < 0 {
		return invpkg.CircuitKey{}, nil, fmt.Errorf("invalid uint64 "+
			"value: %v", row.HtlcID)
	}

	htlcID := uint64(row.HtlcID)

	circuitKey := invpkg.CircuitKey{
		ChanID: chanID,
		HtlcID: htlcID,
	}

	htlc := &invpkg.InvoiceHTLC{
		Amt:          lnwire.MilliSatoshi(row.AmountMsat),
		AcceptHeight: uint32(row.AcceptHeight),
		AcceptTime:   row.AcceptTime,
		Expiry:       uint32(row.ExpiryHeight),
		State:        invpkg.HtlcState(row.State),
	}

	if row.TotalMppMsat.Valid {
		htlc.MppTotalAmt = lnwire.MilliSatoshi(row.TotalMppMsat.Int64)
	}

	if row.ResolveTime.Valid {
		htlc.ResolveTime = row.ResolveTime.Time
	}

	return circuitKey, htlc, nil
}
