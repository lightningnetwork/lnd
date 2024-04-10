package invoices

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

const (
	// queryPaginationLimit is used in the LIMIT clause of the SQL queries
	// to limit the number of rows returned.
	queryPaginationLimit = 100
)

// SQLInvoiceQueries is an interface that defines the set of operations that can
// be executed against the invoice SQL database.
type SQLInvoiceQueries interface { //nolint:interfacebloat
	InsertInvoice(ctx context.Context, arg sqlc.InsertInvoiceParams) (int64,
		error)

	InsertInvoiceFeature(ctx context.Context,
		arg sqlc.InsertInvoiceFeatureParams) error

	InsertInvoiceHTLC(ctx context.Context,
		arg sqlc.InsertInvoiceHTLCParams) (int64, error)

	InsertInvoiceHTLCCustomRecord(ctx context.Context,
		arg sqlc.InsertInvoiceHTLCCustomRecordParams) error

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

	UpdateInvoiceState(ctx context.Context,
		arg sqlc.UpdateInvoiceStateParams) (sql.Result, error)

	UpdateInvoiceAmountPaid(ctx context.Context,
		arg sqlc.UpdateInvoiceAmountPaidParams) (sql.Result, error)

	NextInvoiceSettleIndex(ctx context.Context) (int64, error)

	UpdateInvoiceHTLC(ctx context.Context,
		arg sqlc.UpdateInvoiceHTLCParams) error

	DeleteInvoice(ctx context.Context, arg sqlc.DeleteInvoiceParams) (
		sql.Result, error)

	DeleteCanceledInvoices(ctx context.Context) (sql.Result, error)

	// AMP sub invoice specific methods.
	UpsertAMPSubInvoice(ctx context.Context,
		arg sqlc.UpsertAMPSubInvoiceParams) (sql.Result, error)

	UpdateAMPSubInvoiceState(ctx context.Context,
		arg sqlc.UpdateAMPSubInvoiceStateParams) error

	InsertAMPSubInvoiceHTLC(ctx context.Context,
		arg sqlc.InsertAMPSubInvoiceHTLCParams) error

	FetchAMPSubInvoices(ctx context.Context,
		arg sqlc.FetchAMPSubInvoicesParams) ([]sqlc.AmpSubInvoice,
		error)

	FetchAMPSubInvoiceHTLCs(ctx context.Context,
		arg sqlc.FetchAMPSubInvoiceHTLCsParams) (
		[]sqlc.FetchAMPSubInvoiceHTLCsRow, error)

	FetchSettledAMPSubInvoices(ctx context.Context,
		arg sqlc.FetchSettledAMPSubInvoicesParams) (
		[]sqlc.FetchSettledAMPSubInvoicesRow, error)

	UpdateAMPSubInvoiceHTLCPreimage(ctx context.Context,
		arg sqlc.UpdateAMPSubInvoiceHTLCPreimageParams) (sql.Result,
		error)

	// Invoice events specific methods.
	OnInvoiceCreated(ctx context.Context,
		arg sqlc.OnInvoiceCreatedParams) error

	OnInvoiceCanceled(ctx context.Context,
		arg sqlc.OnInvoiceCanceledParams) error

	OnInvoiceSettled(ctx context.Context,
		arg sqlc.OnInvoiceSettledParams) error

	OnAMPSubInvoiceCreated(ctx context.Context,
		arg sqlc.OnAMPSubInvoiceCreatedParams) error

	OnAMPSubInvoiceCanceled(ctx context.Context,
		arg sqlc.OnAMPSubInvoiceCanceledParams) error

	OnAMPSubInvoiceSettled(ctx context.Context,
		arg sqlc.OnAMPSubInvoiceSettledParams) error
}

var _ InvoiceDB = (*SQLStore)(nil)

// SQLInvoiceQueriesTxOptions defines the set of db txn options the
// SQLInvoiceQueries understands.
type SQLInvoiceQueriesTxOptions struct {
	// readOnly governs if a read only transaction is needed or not.
	readOnly bool
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This implements the TxOptions.
func (a *SQLInvoiceQueriesTxOptions) ReadOnly() bool {
	return a.readOnly
}

// NewSQLInvoiceQueryReadTx creates a new read transaction option set.
func NewSQLInvoiceQueryReadTx() SQLInvoiceQueriesTxOptions {
	return SQLInvoiceQueriesTxOptions{
		readOnly: true,
	}
}

// BatchedSQLInvoiceQueries is a version of the SQLInvoiceQueries that's capable
// of batched database operations.
type BatchedSQLInvoiceQueries interface {
	SQLInvoiceQueries

	sqldb.BatchedTx[SQLInvoiceQueries]
}

// SQLStore represents a storage backend.
type SQLStore struct {
	db    BatchedSQLInvoiceQueries
	clock clock.Clock
}

// NewSQLStore creates a new SQLStore instance given a open
// BatchedSQLInvoiceQueries storage backend.
func NewSQLStore(db BatchedSQLInvoiceQueries,
	clock clock.Clock) *SQLStore {

	return &SQLStore{
		db:    db,
		clock: clock,
	}
}

// AddInvoice inserts the targeted invoice into the database. If the invoice has
// *any* payment hashes which already exists within the database, then the
// insertion will be aborted and rejected due to the strict policy banning any
// duplicate payment hashes.
//
// NOTE: A side effect of this function is that it sets AddIndex on newInvoice.
func (i *SQLStore) AddInvoice(ctx context.Context,
	newInvoice *Invoice, paymentHash lntypes.Hash) (uint64, error) {

	// Make sure this is a valid invoice before trying to store it in our
	// DB.
	if err := ValidateInvoice(newInvoice, paymentHash); err != nil {
		return 0, err
	}

	var (
		writeTxOpts SQLInvoiceQueriesTxOptions
		invoiceID   int64
	)

	// Precompute the payment request hash so we can use it in the query.
	var paymentRequestHash []byte
	if len(newInvoice.PaymentRequest) > 0 {
		h := sha256.New()
		h.Write(newInvoice.PaymentRequest)
		paymentRequestHash = h.Sum(nil)
	}

	err := i.db.ExecTx(ctx, &writeTxOpts, func(db SQLInvoiceQueries) error {
		params := sqlc.InsertInvoiceParams{
			Hash:       paymentHash[:],
			Memo:       sqldb.SQLStr(string(newInvoice.Memo)),
			AmountMsat: int64(newInvoice.Terms.Value),
			// Note: BOLT12 invoices don't have a final cltv delta.
			CltvDelta: sqldb.SQLInt32(
				newInvoice.Terms.FinalCltvDelta,
			),
			Expiry: int32(newInvoice.Terms.Expiry),
			// Note: keysend invoices don't have a payment request.
			PaymentRequest: sqldb.SQLStr(string(
				newInvoice.PaymentRequest),
			),
			PaymentRequestHash: paymentRequestHash,
			State:              int16(newInvoice.State),
			AmountPaidMsat:     int64(newInvoice.AmtPaid),
			IsAmp:              newInvoice.IsAMP(),
			IsHodl:             newInvoice.HodlInvoice,
			IsKeysend:          newInvoice.IsKeysend(),
			CreatedAt:          newInvoice.CreationDate.UTC(),
		}

		// Some invoices may not have a preimage, like in the case of
		// HODL invoices.
		if newInvoice.Terms.PaymentPreimage != nil {
			preimage := *newInvoice.Terms.PaymentPreimage
			if preimage == UnknownPreimage {
				return errors.New("cannot use all-zeroes " +
					"preimage")
			}
			params.Preimage = preimage[:]
		}

		// Some non MPP payments may have the default (invalid) value.
		if newInvoice.Terms.PaymentAddr != BlankPayAddr {
			params.PaymentAddr = newInvoice.Terms.PaymentAddr[:]
		}

		var err error
		invoiceID, err = db.InsertInvoice(ctx, params)
		if err != nil {
			return fmt.Errorf("unable to insert invoice: %w", err)
		}

		// TODO(positiveblue): if invocies do not have custom features
		// maybe just store the "invoice type" and populate the features
		// based on that.
		for feature := range newInvoice.Terms.Features.Features() {
			params := sqlc.InsertInvoiceFeatureParams{
				InvoiceID: invoiceID,
				Feature:   int32(feature),
			}

			err := db.InsertInvoiceFeature(ctx, params)
			if err != nil {
				return fmt.Errorf("unable to insert invoice "+
					"feature(%v): %w", feature, err)
			}
		}

		// Finally add a new event for this invoice.
		return db.OnInvoiceCreated(ctx, sqlc.OnInvoiceCreatedParams{
			AddedAt:   newInvoice.CreationDate.UTC(),
			InvoiceID: invoiceID,
		})
	}, func() {})
	if err != nil {
		mappedSQLErr := sqldb.MapSQLError(err)
		var uniqueConstraintErr *sqldb.ErrSQLUniqueConstraintViolation
		if errors.As(mappedSQLErr, &uniqueConstraintErr) {
			// Add context to unique constraint errors.
			return 0, ErrDuplicateInvoice
		}

		return 0, fmt.Errorf("unable to add invoice(%v): %w",
			paymentHash, err)
	}

	newInvoice.AddIndex = uint64(invoiceID)

	return newInvoice.AddIndex, nil
}

// fetchInvoice fetches the common invoice data and the AMP state for the
// invoice with the given reference.
func (i *SQLStore) fetchInvoice(ctx context.Context,
	db SQLInvoiceQueries, ref InvoiceRef) (*Invoice, error) {

	if ref.PayHash() == nil && ref.PayAddr() == nil && ref.SetID() == nil {
		return nil, ErrInvoiceNotFound
	}

	var (
		invoice *Invoice
		params  sqlc.GetInvoiceParams
	)

	// Given all invoices are uniquely identified by their payment hash,
	// we can use it to query a specific invoice.
	if ref.PayHash() != nil {
		params.Hash = ref.PayHash()[:]
	}

	// Newer invoices (0.11 and up) are indexed by payment address in
	// addition to payment hash, but pre 0.8 invoices do not have one at
	// all. Only allow lookups for payment address if it is not a blank
	// payment address, which is a special-cased value for legacy keysend
	// invoices.
	if ref.PayAddr() != nil && *ref.PayAddr() != BlankPayAddr {
		params.PaymentAddr = ref.PayAddr()[:]
	}

	// If the reference has a set ID we'll fetch the invoice which has the
	// corresponding AMP sub invoice.
	if ref.SetID() != nil {
		params.SetID = ref.SetID()[:]
	}

	rows, err := db.GetInvoice(ctx, params)
	switch {
	case len(rows) == 0:
		return nil, ErrInvoiceNotFound

	case len(rows) > 1:
		// In case the reference is ambiguous, meaning it matches more
		// than	one invoice, we'll return an error.
		return nil, fmt.Errorf("ambiguous invoice ref: %s",
			ref.String())

	case err != nil:
		return nil, fmt.Errorf("unable to fetch invoice: %w", err)
	}

	var (
		setID         *[32]byte
		fetchAmpHtlcs bool
	)

	// Now that we got the invoice itself, fetch the HTLCs as requested by
	// the modifier.
	switch ref.Modifier() {
	case DefaultModifier:
		// By default we'll fetch all AMP HTLCs.
		setID = nil
		fetchAmpHtlcs = true

	case HtlcSetOnlyModifier:
		// In this case we'll fetch all AMP HTLCs for the
		// specified set id.
		if ref.SetID() == nil {
			return nil, fmt.Errorf("set ID is required to use " +
				"the HTLC set only modifier")
		}

		setID = ref.SetID()
		fetchAmpHtlcs = true

	case HtlcSetBlankModifier:
		// No need to fetch any HTLCs.
		setID = nil
		fetchAmpHtlcs = false

	default:
		return nil, fmt.Errorf("unknown invoice ref modifier: %v",
			ref.Modifier())
	}

	// Fetch the rest of the invoice data and fill the invoice struct.
	_, invoice, err = fetchInvoiceData(
		ctx, db, rows[0], setID, fetchAmpHtlcs,
	)
	if err != nil {
		return nil, err
	}

	return invoice, nil
}

// fetchAmpState fetches the AMP state for the invoice with the given ID.
// Optional setID can be provided to fetch the state for a specific AMP HTLC
// set. If setID is nil then we'll fetch the state for all AMP sub invoices. If
// fetchHtlcs is set to true, the HTLCs for the given set will be fetched as
// well.
//
//nolint:funlen
func fetchAmpState(ctx context.Context, db SQLInvoiceQueries, invoiceID int64,
	setID *[32]byte, fetchHtlcs bool) (AMPInvoiceState,
	HTLCSet, error) {

	var paramSetID []byte
	if setID != nil {
		paramSetID = setID[:]
	}

	// First fetch all the AMP sub invoices for this invoice or the one
	// matching the provided set ID.
	ampInvoiceRows, err := db.FetchAMPSubInvoices(
		ctx, sqlc.FetchAMPSubInvoicesParams{
			InvoiceID: invoiceID,
			SetID:     paramSetID,
		},
	)
	if err != nil {
		return nil, nil, err
	}

	ampState := make(map[SetID]InvoiceStateAMP)
	for _, row := range ampInvoiceRows {
		var rowSetID [32]byte

		if len(row.SetID) != 32 {
			return nil, nil, fmt.Errorf("invalid set id length: %d",
				len(row.SetID))
		}

		var settleDate time.Time
		if row.SettledAt.Valid {
			settleDate = row.SettledAt.Time.Local()
		}

		copy(rowSetID[:], row.SetID)
		ampState[rowSetID] = InvoiceStateAMP{
			State:       HtlcState(row.State),
			SettleIndex: uint64(row.SettleIndex.Int64),
			SettleDate:  settleDate,
			InvoiceKeys: make(map[models.CircuitKey]struct{}),
		}
	}

	if !fetchHtlcs {
		return ampState, nil, nil
	}

	customRecordRows, err := db.GetInvoiceHTLCCustomRecords(ctx, invoiceID)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get custom records for "+
			"invoice HTLCs: %w", err)
	}

	customRecords := make(map[int64]record.CustomSet, len(customRecordRows))
	for _, row := range customRecordRows {
		if _, ok := customRecords[row.HtlcID]; !ok {
			customRecords[row.HtlcID] = make(record.CustomSet)
		}

		value := row.Value
		if value == nil {
			value = []byte{}
		}

		customRecords[row.HtlcID][uint64(row.Key)] = value
	}

	// Now fetch all the AMP HTLCs for this invoice or the one matching the
	// provided set ID.
	ampHtlcRows, err := db.FetchAMPSubInvoiceHTLCs(
		ctx, sqlc.FetchAMPSubInvoiceHTLCsParams{
			InvoiceID: invoiceID,
			SetID:     paramSetID,
		},
	)
	if err != nil {
		return nil, nil, err
	}

	ampHtlcs := make(map[models.CircuitKey]*InvoiceHTLC)
	for _, row := range ampHtlcRows {
		uint64ChanID, err := strconv.ParseUint(row.ChanID, 10, 64)
		if err != nil {
			return nil, nil, err
		}

		chanID := lnwire.NewShortChanIDFromInt(uint64ChanID)

		if row.HtlcID < 0 {
			return nil, nil, fmt.Errorf("invalid HTLC ID "+
				"value: %v", row.HtlcID)
		}

		htlcID := uint64(row.HtlcID)

		circuitKey := CircuitKey{
			ChanID: chanID,
			HtlcID: htlcID,
		}

		htlc := &InvoiceHTLC{
			Amt:          lnwire.MilliSatoshi(row.AmountMsat),
			AcceptHeight: uint32(row.AcceptHeight),
			AcceptTime:   row.AcceptTime.Local(),
			Expiry:       uint32(row.ExpiryHeight),
			State:        HtlcState(row.State),
		}

		if row.TotalMppMsat.Valid {
			htlc.MppTotalAmt = lnwire.MilliSatoshi(
				row.TotalMppMsat.Int64,
			)
		}

		if row.ResolveTime.Valid {
			htlc.ResolveTime = row.ResolveTime.Time.Local()
		}

		var (
			rootShare [32]byte
			setID     [32]byte
		)

		if len(row.RootShare) != 32 {
			return nil, nil, fmt.Errorf("invalid root share "+
				"length: %d", len(row.RootShare))
		}
		copy(rootShare[:], row.RootShare)

		if len(row.SetID) != 32 {
			return nil, nil, fmt.Errorf("invalid set ID length: %d",
				len(row.SetID))
		}
		copy(setID[:], row.SetID)

		if row.ChildIndex < 0 || row.ChildIndex > math.MaxUint32 {
			return nil, nil, fmt.Errorf("invalid child index "+
				"value: %v", row.ChildIndex)
		}

		ampRecord := record.NewAMP(
			rootShare, setID, uint32(row.ChildIndex),
		)

		htlc.AMP = &InvoiceHtlcAMPData{
			Record: *ampRecord,
		}

		if len(row.Hash) != 32 {
			return nil, nil, fmt.Errorf("invalid hash length: %d",
				len(row.Hash))
		}
		copy(htlc.AMP.Hash[:], row.Hash)

		if row.Preimage != nil {
			preimage, err := lntypes.MakePreimage(row.Preimage)
			if err != nil {
				return nil, nil, err
			}

			htlc.AMP.Preimage = &preimage
		}

		if _, ok := customRecords[row.ID]; ok {
			htlc.CustomRecords = customRecords[row.ID]
		} else {
			htlc.CustomRecords = make(record.CustomSet)
		}

		ampHtlcs[circuitKey] = htlc
	}

	if len(ampHtlcs) > 0 {
		for setID := range ampState {
			var amtPaid lnwire.MilliSatoshi
			invoiceKeys := make(
				map[models.CircuitKey]struct{},
			)

			for key, htlc := range ampHtlcs {
				if htlc.AMP.Record.SetID() != setID {
					continue
				}

				invoiceKeys[key] = struct{}{}

				if htlc.State != HtlcStateCanceled { //nolint: lll
					amtPaid += htlc.Amt
				}
			}

			setState := ampState[setID]
			setState.InvoiceKeys = invoiceKeys
			setState.AmtPaid = amtPaid
			ampState[setID] = setState
		}
	}

	return ampState, ampHtlcs, nil
}

// LookupInvoice attempts to look up an invoice corresponding the passed in
// reference. The reference may be a payment hash, a payment address, or a set
// ID for an AMP sub invoice. If the invoice is found, we'll return the complete
// invoice. If the invoice is not found, then we'll return an ErrInvoiceNotFound
// error.
func (i *SQLStore) LookupInvoice(ctx context.Context,
	ref InvoiceRef) (Invoice, error) {

	var (
		invoice *Invoice
		err     error
	)

	readTxOpt := NewSQLInvoiceQueryReadTx()
	txErr := i.db.ExecTx(ctx, &readTxOpt, func(db SQLInvoiceQueries) error {
		invoice, err = i.fetchInvoice(ctx, db, ref)

		return err
	}, func() {})
	if txErr != nil {
		return Invoice{}, txErr
	}

	return *invoice, nil
}

// FetchPendingInvoices returns all the invoices that are currently in a
// "pending" state. An invoice is pending if it has been created but not yet
// settled or canceled.
func (i *SQLStore) FetchPendingInvoices(ctx context.Context) (
	map[lntypes.Hash]Invoice, error) {

	var invoices map[lntypes.Hash]Invoice

	readTxOpt := NewSQLInvoiceQueryReadTx()
	err := i.db.ExecTx(ctx, &readTxOpt, func(db SQLInvoiceQueries) error {
		limit := queryPaginationLimit

		return queryWithLimit(func(offset int) (int, error) {
			params := sqlc.FilterInvoicesParams{
				PendingOnly: true,
				NumOffset:   int32(offset),
				NumLimit:    int32(limit),
				Reverse:     false,
			}

			rows, err := db.FilterInvoices(ctx, params)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("unable to get invoices "+
					"from db: %w", err)
			}

			// Load all the information for the invoices.
			for _, row := range rows {
				hash, invoice, err := fetchInvoiceData(
					ctx, db, row, nil, true,
				)
				if err != nil {
					return 0, err
				}

				invoices[*hash] = *invoice
			}

			return len(rows), nil
		}, limit)
	}, func() {
		invoices = make(map[lntypes.Hash]Invoice)
	})
	if err != nil {
		return nil, fmt.Errorf("unable to fetch pending invoices: %w",
			err)
	}

	return invoices, nil
}

// InvoicesSettledSince can be used by callers to catch up any settled invoices
// they missed within the settled invoice time series. We'll return all known
// settled invoice that have a settle index higher than the passed
// sinceSettleIndex.
//
// NOTE: The index starts from 1. As a result we enforce that specifying a value
// below the starting index value is a noop.
func (i *SQLStore) InvoicesSettledSince(ctx context.Context, idx uint64) (
	[]Invoice, error) {

	var invoices []Invoice

	if idx == 0 {
		return invoices, nil
	}

	readTxOpt := NewSQLInvoiceQueryReadTx()
	err := i.db.ExecTx(ctx, &readTxOpt, func(db SQLInvoiceQueries) error {
		settleIdx := idx
		limit := queryPaginationLimit

		err := queryWithLimit(func(offset int) (int, error) {
			params := sqlc.FilterInvoicesParams{
				SettleIndexGet: sqldb.SQLInt64(settleIdx + 1),
				NumLimit:       int32(limit),
				NumOffset:      int32(offset),
				Reverse:        false,
			}

			rows, err := db.FilterInvoices(ctx, params)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("unable to get invoices "+
					"from db: %w", err)
			}

			// Load all the information for the invoices.
			for _, row := range rows {
				_, invoice, err := fetchInvoiceData(
					ctx, db, row, nil, true,
				)
				if err != nil {
					return 0, fmt.Errorf("unable to fetch "+
						"invoice(id=%d) from db: %w",
						row.ID, err)
				}

				invoices = append(invoices, *invoice)
			}

			settleIdx += uint64(limit)

			return len(rows), nil
		}, limit)
		if err != nil {
			return err
		}

		// Now fetch all the AMP sub invoices that were settled since
		// the provided index.
		ampInvoices, err := i.db.FetchSettledAMPSubInvoices(
			ctx, sqlc.FetchSettledAMPSubInvoicesParams{
				SettleIndexGet: sqldb.SQLInt64(idx + 1),
			},
		)
		if err != nil {
			return err
		}

		for _, ampInvoice := range ampInvoices {
			// Convert the row to a sqlc.Invoice so we can use the
			// existing fetchInvoiceData function.
			sqlInvoice := sqlc.Invoice{
				ID:             ampInvoice.ID,
				Hash:           ampInvoice.Hash,
				Preimage:       ampInvoice.Preimage,
				SettleIndex:    ampInvoice.AmpSettleIndex,
				SettledAt:      ampInvoice.AmpSettledAt,
				Memo:           ampInvoice.Memo,
				AmountMsat:     ampInvoice.AmountMsat,
				CltvDelta:      ampInvoice.CltvDelta,
				Expiry:         ampInvoice.Expiry,
				PaymentAddr:    ampInvoice.PaymentAddr,
				PaymentRequest: ampInvoice.PaymentRequest,
				State:          ampInvoice.State,
				AmountPaidMsat: ampInvoice.AmountPaidMsat,
				IsAmp:          ampInvoice.IsAmp,
				IsHodl:         ampInvoice.IsHodl,
				IsKeysend:      ampInvoice.IsKeysend,
				CreatedAt:      ampInvoice.CreatedAt.UTC(),
			}

			// Fetch the state and HTLCs for this AMP sub invoice.
			_, invoice, err := fetchInvoiceData(
				ctx, db, sqlInvoice,
				(*[32]byte)(ampInvoice.SetID), true,
			)
			if err != nil {
				return fmt.Errorf("unable to fetch "+
					"AMP invoice(id=%d) from db: %w",
					ampInvoice.ID, err)
			}

			invoices = append(invoices, *invoice)
		}

		return nil
	}, func() {
		invoices = nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to get invoices settled since "+
			"index (excluding) %d: %w", idx, err)
	}

	return invoices, nil
}

// InvoicesAddedSince can be used by callers to seek into the event time series
// of all the invoices added in the database. This method will return all
// invoices with an add index greater than the specified sinceAddIndex.
//
// NOTE: The index starts from 1. As a result we enforce that specifying a value
// below the starting index value is a noop.
func (i *SQLStore) InvoicesAddedSince(ctx context.Context, idx uint64) (
	[]Invoice, error) {

	var result []Invoice

	if idx == 0 {
		return result, nil
	}

	readTxOpt := NewSQLInvoiceQueryReadTx()
	err := i.db.ExecTx(ctx, &readTxOpt, func(db SQLInvoiceQueries) error {
		addIdx := idx
		limit := queryPaginationLimit

		return queryWithLimit(func(offset int) (int, error) {
			params := sqlc.FilterInvoicesParams{
				AddIndexGet: sqldb.SQLInt64(addIdx + 1),
				NumLimit:    int32(limit),
				NumOffset:   int32(offset),
				Reverse:     false,
			}

			rows, err := db.FilterInvoices(ctx, params)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("unable to get invoices "+
					"from db: %w", err)
			}

			// Load all the information for the invoices.
			for _, row := range rows {
				_, invoice, err := fetchInvoiceData(
					ctx, db, row, nil, true,
				)
				if err != nil {
					return 0, err
				}

				result = append(result, *invoice)
			}

			addIdx += uint64(limit)

			return len(rows), nil
		}, limit)
	}, func() {
		result = nil
	})

	if err != nil {
		return nil, fmt.Errorf("unable to get invoices added since "+
			"index %d: %w", idx, err)
	}

	return result, nil
}

// QueryInvoices allows a caller to query the invoice database for invoices
// within the specified add index range.
func (i *SQLStore) QueryInvoices(ctx context.Context,
	q InvoiceQuery) (InvoiceSlice, error) {

	var invoices []Invoice

	if q.NumMaxInvoices == 0 {
		return InvoiceSlice{}, fmt.Errorf("max invoices must " +
			"be non-zero")
	}

	readTxOpt := NewSQLInvoiceQueryReadTx()
	err := i.db.ExecTx(ctx, &readTxOpt, func(db SQLInvoiceQueries) error {
		limit := queryPaginationLimit

		return queryWithLimit(func(offset int) (int, error) {
			params := sqlc.FilterInvoicesParams{
				NumOffset:   int32(offset),
				NumLimit:    int32(limit),
				PendingOnly: q.PendingOnly,
			}

			if q.Reversed {
				idx := int32(q.IndexOffset)

				// If the index offset was not set, we want to
				// fetch from the lastest invoice.
				if idx == 0 {
					params.AddIndexLet = sqldb.SQLInt64(
						int64(math.MaxInt64),
					)
				} else {
					// The invoice with index offset id must
					// not be included in the results.
					params.AddIndexLet = sqldb.SQLInt64(
						idx - int32(offset) - 1,
					)
				}

				params.Reverse = true
			} else {
				// The invoice with index offset id must not be
				// included in the results.
				params.AddIndexGet = sqldb.SQLInt64(
					q.IndexOffset + uint64(offset) + 1,
				)

				params.Reverse = false
			}

			if q.CreationDateStart != 0 {
				params.CreatedAfter = sqldb.SQLTime(
					time.Unix(q.CreationDateStart, 0).UTC(),
				)
			}

			if q.CreationDateEnd != 0 {
				params.CreatedBefore = sqldb.SQLTime(
					time.Unix(q.CreationDateEnd, 0).UTC(),
				)
			}

			rows, err := db.FilterInvoices(ctx, params)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("unable to get invoices "+
					"from db: %w", err)
			}

			// Load all the information for the invoices.
			for _, row := range rows {
				_, invoice, err := fetchInvoiceData(
					ctx, db, row, nil, true,
				)
				if err != nil {
					return 0, err
				}

				invoices = append(invoices, *invoice)

				if len(invoices) == int(q.NumMaxInvoices) {
					return 0, nil
				}
			}

			return len(rows), nil
		}, limit)
	}, func() {
		invoices = nil
	})
	if err != nil {
		return InvoiceSlice{}, fmt.Errorf("unable to query "+
			"invoices: %w", err)
	}

	if len(invoices) == 0 {
		return InvoiceSlice{
			InvoiceQuery: q,
		}, nil
	}

	// If we iterated through the add index in reverse order, then
	// we'll need to reverse the slice of invoices to return them in
	// forward order.
	if q.Reversed {
		numInvoices := len(invoices)
		for i := 0; i < numInvoices/2; i++ {
			reverse := numInvoices - i - 1
			invoices[i], invoices[reverse] =
				invoices[reverse], invoices[i]
		}
	}

	res := InvoiceSlice{
		InvoiceQuery:     q,
		Invoices:         invoices,
		FirstIndexOffset: invoices[0].AddIndex,
		LastIndexOffset:  invoices[len(invoices)-1].AddIndex,
	}

	return res, nil
}

// sqlInvoiceUpdater is the implementation of the InvoiceUpdater interface using
// a SQL database as the backend.
type sqlInvoiceUpdater struct {
	db         SQLInvoiceQueries
	ctx        context.Context //nolint:containedctx
	invoice    *Invoice
	updateTime time.Time
}

// AddHtlc adds a new htlc to the invoice.
func (s *sqlInvoiceUpdater) AddHtlc(circuitKey models.CircuitKey,
	newHtlc *InvoiceHTLC) error {

	htlcPrimaryKeyID, err := s.db.InsertInvoiceHTLC(
		s.ctx, sqlc.InsertInvoiceHTLCParams{
			HtlcID: int64(circuitKey.HtlcID),
			ChanID: strconv.FormatUint(
				circuitKey.ChanID.ToUint64(), 10,
			),
			AmountMsat: int64(newHtlc.Amt),
			TotalMppMsat: sql.NullInt64{
				Int64: int64(newHtlc.MppTotalAmt),
				Valid: newHtlc.MppTotalAmt != 0,
			},
			AcceptHeight: int32(newHtlc.AcceptHeight),
			AcceptTime:   newHtlc.AcceptTime.UTC(),
			ExpiryHeight: int32(newHtlc.Expiry),
			State:        int16(newHtlc.State),
			InvoiceID:    int64(s.invoice.AddIndex),
		},
	)
	if err != nil {
		return err
	}

	for key, value := range newHtlc.CustomRecords {
		err = s.db.InsertInvoiceHTLCCustomRecord(
			s.ctx, sqlc.InsertInvoiceHTLCCustomRecordParams{
				// TODO(bhandras): schema might be wrong here
				// as the custom record key is an uint64.
				Key:    int64(key),
				Value:  value,
				HtlcID: htlcPrimaryKeyID,
			},
		)
		if err != nil {
			return err
		}
	}

	if newHtlc.AMP != nil {
		setID := newHtlc.AMP.Record.SetID()

		upsertResult, err := s.db.UpsertAMPSubInvoice(
			s.ctx, sqlc.UpsertAMPSubInvoiceParams{
				SetID:     setID[:],
				CreatedAt: s.updateTime.UTC(),
				InvoiceID: int64(s.invoice.AddIndex),
			},
		)
		if err != nil {
			mappedSQLErr := sqldb.MapSQLError(err)
			var uniqueConstraintErr *sqldb.ErrSQLUniqueConstraintViolation //nolint:lll
			if errors.As(mappedSQLErr, &uniqueConstraintErr) {
				return ErrDuplicateSetID{
					SetID: setID,
				}
			}

			return err
		}

		// If we're just inserting the AMP invoice, we'll get a non
		// zero rows affected count.
		rowsAffected, err := upsertResult.RowsAffected()
		if err != nil {
			return err
		}
		if rowsAffected != 0 {
			// If we're inserting a new AMP invoice, we'll also
			// insert a new invoice event.
			err = s.db.OnAMPSubInvoiceCreated(
				s.ctx, sqlc.OnAMPSubInvoiceCreatedParams{
					AddedAt:   s.updateTime.UTC(),
					InvoiceID: int64(s.invoice.AddIndex),
					SetID:     setID[:],
				},
			)
			if err != nil {
				return err
			}
		}

		rootShare := newHtlc.AMP.Record.RootShare()

		ampHtlcParams := sqlc.InsertAMPSubInvoiceHTLCParams{
			InvoiceID: int64(s.invoice.AddIndex),
			SetID:     setID[:],
			HtlcID:    htlcPrimaryKeyID,
			RootShare: rootShare[:],
			ChildIndex: int64(
				newHtlc.AMP.Record.ChildIndex(),
			),
			Hash: newHtlc.AMP.Hash[:],
		}

		if newHtlc.AMP.Preimage != nil {
			ampHtlcParams.Preimage = newHtlc.AMP.Preimage[:]
		}

		err = s.db.InsertAMPSubInvoiceHTLC(s.ctx, ampHtlcParams)
		if err != nil {
			return err
		}
	}

	return nil
}

// ResolveHtlc marks an htlc as resolved with the given state.
func (s *sqlInvoiceUpdater) ResolveHtlc(circuitKey models.CircuitKey,
	state HtlcState, resolveTime time.Time) error {

	return s.db.UpdateInvoiceHTLC(s.ctx, sqlc.UpdateInvoiceHTLCParams{
		HtlcID: int64(circuitKey.HtlcID),
		ChanID: strconv.FormatUint(
			circuitKey.ChanID.ToUint64(), 10,
		),
		InvoiceID:   int64(s.invoice.AddIndex),
		State:       int16(state),
		ResolveTime: sqldb.SQLTime(resolveTime.UTC()),
	})
}

// AddAmpHtlcPreimage adds a preimage of an AMP htlc to the AMP sub invoice
// identified by the setID.
func (s *sqlInvoiceUpdater) AddAmpHtlcPreimage(setID [32]byte,
	circuitKey models.CircuitKey, preimage lntypes.Preimage) error {

	result, err := s.db.UpdateAMPSubInvoiceHTLCPreimage(
		s.ctx, sqlc.UpdateAMPSubInvoiceHTLCPreimageParams{
			InvoiceID: int64(s.invoice.AddIndex),
			SetID:     setID[:],
			HtlcID:    int64(circuitKey.HtlcID),
			Preimage:  preimage[:],
		},
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrInvoiceNotFound
	}

	return nil
}

// UpdateInvoiceState updates the invoice state to the new state.
func (s *sqlInvoiceUpdater) UpdateInvoiceState(
	newState ContractState, preimage *lntypes.Preimage) error {

	var (
		settleIndex sql.NullInt64
		settledAt   sql.NullTime
	)

	switch newState {
	case ContractSettled:
		nextSettleIndex, err := s.db.NextInvoiceSettleIndex(s.ctx)
		if err != nil {
			return err
		}

		settleIndex = sqldb.SQLInt64(nextSettleIndex)

		// If the invoice is settled, we'll also update the settle time.
		settledAt = sqldb.SQLTime(s.updateTime.UTC())

		err = s.db.OnInvoiceSettled(
			s.ctx, sqlc.OnInvoiceSettledParams{
				AddedAt:   s.updateTime.UTC(),
				InvoiceID: int64(s.invoice.AddIndex),
			},
		)
		if err != nil {
			return err
		}

	case ContractCanceled:
		err := s.db.OnInvoiceCanceled(
			s.ctx, sqlc.OnInvoiceCanceledParams{
				AddedAt:   s.updateTime.UTC(),
				InvoiceID: int64(s.invoice.AddIndex),
			},
		)
		if err != nil {
			return err
		}
	}

	params := sqlc.UpdateInvoiceStateParams{
		ID:          int64(s.invoice.AddIndex),
		State:       int16(newState),
		SettleIndex: settleIndex,
		SettledAt:   settledAt,
	}

	if preimage != nil {
		params.Preimage = preimage[:]
	}

	result, err := s.db.UpdateInvoiceState(s.ctx, params)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return ErrInvoiceNotFound
	}

	if settleIndex.Valid {
		s.invoice.SettleIndex = uint64(settleIndex.Int64)
		s.invoice.SettleDate = s.updateTime
	}

	return nil
}

// UpdateInvoiceAmtPaid updates the invoice amount paid to the new amount.
func (s *sqlInvoiceUpdater) UpdateInvoiceAmtPaid(
	amtPaid lnwire.MilliSatoshi) error {

	_, err := s.db.UpdateInvoiceAmountPaid(
		s.ctx, sqlc.UpdateInvoiceAmountPaidParams{
			ID:             int64(s.invoice.AddIndex),
			AmountPaidMsat: int64(amtPaid),
		},
	)

	return err
}

// UpdateAmpState updates the state of the AMP sub invoice identified by the
// setID.
func (s *sqlInvoiceUpdater) UpdateAmpState(setID [32]byte,
	newState InvoiceStateAMP, _ models.CircuitKey) error {

	var (
		settleIndex sql.NullInt64
		settledAt   sql.NullTime
	)

	switch newState.State {
	case HtlcStateSettled:
		nextSettleIndex, err := s.db.NextInvoiceSettleIndex(s.ctx)
		if err != nil {
			return err
		}

		settleIndex = sqldb.SQLInt64(nextSettleIndex)

		// If the invoice is settled, we'll also update the settle time.
		settledAt = sqldb.SQLTime(s.updateTime.UTC())

		err = s.db.OnAMPSubInvoiceSettled(
			s.ctx, sqlc.OnAMPSubInvoiceSettledParams{
				AddedAt:   s.updateTime.UTC(),
				InvoiceID: int64(s.invoice.AddIndex),
				SetID:     setID[:],
			},
		)
		if err != nil {
			return err
		}

	case HtlcStateCanceled:
		err := s.db.OnAMPSubInvoiceCanceled(
			s.ctx, sqlc.OnAMPSubInvoiceCanceledParams{
				AddedAt:   s.updateTime.UTC(),
				InvoiceID: int64(s.invoice.AddIndex),
				SetID:     setID[:],
			},
		)
		if err != nil {
			return err
		}
	}

	err := s.db.UpdateAMPSubInvoiceState(
		s.ctx, sqlc.UpdateAMPSubInvoiceStateParams{
			SetID:       setID[:],
			State:       int16(newState.State),
			SettleIndex: settleIndex,
			SettledAt:   settledAt,
		},
	)
	if err != nil {
		return err
	}

	return nil
}

// Finalize finalizes the update before it is written to the database. Note that
// we don't use this directly in the SQL implementation, so the function is just
// a stub.
func (s *sqlInvoiceUpdater) Finalize(_ UpdateType) error {
	return nil
}

// UpdateInvoice attempts to update an invoice corresponding to the passed
// reference. If an invoice matching the passed reference doesn't exist within
// the database, then the action will fail with  ErrInvoiceNotFound error.
//
// The update is performed inside the same database transaction that fetches the
// invoice and is therefore atomic. The fields to update are controlled by the
// supplied callback.
func (i *SQLStore) UpdateInvoice(ctx context.Context, ref InvoiceRef,
	_ *SetID, callback InvoiceUpdateCallback) (
	*Invoice, error) {

	var updatedInvoice *Invoice

	txOpt := SQLInvoiceQueriesTxOptions{readOnly: false}
	txErr := i.db.ExecTx(ctx, &txOpt, func(db SQLInvoiceQueries) error {
		invoice, err := i.fetchInvoice(ctx, db, ref)
		if err != nil {
			return err
		}

		updateTime := i.clock.Now()
		updater := &sqlInvoiceUpdater{
			db:         db,
			ctx:        ctx,
			invoice:    invoice,
			updateTime: updateTime,
		}

		payHash := ref.PayHash()
		updatedInvoice, err = UpdateInvoice(
			payHash, invoice, updateTime, callback, updater,
		)

		return err
	}, func() {})
	if txErr != nil {
		// If the invoice is already settled, we'll return the
		// (unchanged) invoice and the ErrInvoiceAlreadySettled error.
		if errors.Is(txErr, ErrInvoiceAlreadySettled) {
			return updatedInvoice, txErr
		}

		return nil, txErr
	}

	return updatedInvoice, nil
}

// DeleteInvoice attempts to delete the passed invoices and all their related
// data from the database in one transaction.
func (i *SQLStore) DeleteInvoice(ctx context.Context,
	invoicesToDelete []InvoiceDeleteRef) error {

	// All the InvoiceDeleteRef instances include the add index of the
	// invoice. The rest was added to ensure that the invoices were deleted
	// properly in the kv database. When we have fully migrated we can
	// remove the rest of the fields.
	for _, ref := range invoicesToDelete {
		if ref.AddIndex == 0 {
			return fmt.Errorf("unable to delete invoice using a "+
				"ref without AddIndex set: %v", ref)
		}
	}

	var writeTxOpt SQLInvoiceQueriesTxOptions
	err := i.db.ExecTx(ctx, &writeTxOpt, func(db SQLInvoiceQueries) error {
		for _, ref := range invoicesToDelete {
			params := sqlc.DeleteInvoiceParams{
				AddIndex: sqldb.SQLInt64(ref.AddIndex),
			}

			if ref.SettleIndex != 0 {
				params.SettleIndex = sqldb.SQLInt64(
					ref.SettleIndex,
				)
			}

			if ref.PayHash != lntypes.ZeroHash {
				params.Hash = ref.PayHash[:]
			}

			result, err := db.DeleteInvoice(ctx, params)
			if err != nil {
				return fmt.Errorf("unable to delete "+
					"invoice(%v): %w", ref.AddIndex, err)
			}
			rowsAffected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("unable to get rows "+
					"affected: %w", err)
			}
			if rowsAffected == 0 {
				return fmt.Errorf("%w: %v",
					ErrInvoiceNotFound, ref.AddIndex)
			}
		}

		return nil
	}, func() {})

	if err != nil {
		return fmt.Errorf("unable to delete invoices: %w", err)
	}

	return nil
}

// DeleteCanceledInvoices removes all canceled invoices from the database.
func (i *SQLStore) DeleteCanceledInvoices(ctx context.Context) error {
	var writeTxOpt SQLInvoiceQueriesTxOptions
	err := i.db.ExecTx(ctx, &writeTxOpt, func(db SQLInvoiceQueries) error {
		_, err := db.DeleteCanceledInvoices(ctx)
		if err != nil {
			return fmt.Errorf("unable to delete canceled "+
				"invoices: %w", err)
		}

		return nil
	}, func() {})
	if err != nil {
		return fmt.Errorf("unable to delete invoices: %w", err)
	}

	return nil
}

// fetchInvoiceData fetches additional data for the given invoice. If the
// invoice is AMP and the setID is not nil, then it will also fetch the AMP
// state and HTLCs for the given setID, otherwise for all AMP sub invoices of
// the invoice. If fetchAmpHtlcs is true, it will also fetch the AMP HTLCs.
func fetchInvoiceData(ctx context.Context, db SQLInvoiceQueries,
	row sqlc.Invoice, setID *[32]byte, fetchAmpHtlcs bool) (*lntypes.Hash,
	*Invoice, error) {

	// Unmarshal the common data.
	hash, invoice, err := unmarshalInvoice(row)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to unmarshal "+
			"invoice(id=%d) from db: %w", row.ID, err)
	}

	// Fetch the invoice features.
	features, err := getInvoiceFeatures(ctx, db, row.ID)
	if err != nil {
		return nil, nil, err
	}

	invoice.Terms.Features = features

	// If this is an AMP invoice, we'll need fetch the AMP state along
	// with the HTLCs (if requested).
	if invoice.IsAMP() {
		invoiceID := int64(invoice.AddIndex)
		ampState, ampHtlcs, err := fetchAmpState(
			ctx, db, invoiceID, setID, fetchAmpHtlcs,
		)
		if err != nil {
			return nil, nil, err
		}

		invoice.AMPState = ampState
		invoice.Htlcs = ampHtlcs

		return hash, invoice, nil
	}

	// Otherwise simply fetch the invoice HTLCs.
	htlcs, err := getInvoiceHtlcs(ctx, db, row.ID)
	if err != nil {
		return nil, nil, err
	}

	if len(htlcs) > 0 {
		invoice.Htlcs = htlcs
		var amountPaid lnwire.MilliSatoshi
		for _, htlc := range htlcs {
			if htlc.State == HtlcStateSettled {
				amountPaid += htlc.Amt
			}
		}
		invoice.AmtPaid = amountPaid
	}

	return hash, invoice, nil
}

// getInvoiceFeatures fetches the invoice features for the given invoice id.
func getInvoiceFeatures(ctx context.Context, db SQLInvoiceQueries,
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
func getInvoiceHtlcs(ctx context.Context, db SQLInvoiceQueries,
	invoiceID int64) (map[CircuitKey]*InvoiceHTLC, error) {

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

		value := row.Value
		if value == nil {
			value = []byte{}
		}
		cr[row.HtlcID][uint64(row.Key)] = value
	}

	htlcs := make(map[CircuitKey]*InvoiceHTLC, len(htlcRows))

	for _, row := range htlcRows {
		circuiteKey, htlc, err := unmarshalInvoiceHTLC(row)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal "+
				"htlc(%d): %w", row.ID, err)
		}

		if customRecords, ok := cr[row.ID]; ok {
			htlc.CustomRecords = customRecords
		} else {
			htlc.CustomRecords = make(record.CustomSet)
		}

		htlcs[circuiteKey] = htlc
	}

	return htlcs, nil
}

// unmarshalInvoice converts an InvoiceRow to an Invoice.
func unmarshalInvoice(row sqlc.Invoice) (*lntypes.Hash, *Invoice,
	error) {

	var (
		settleIndex    int64
		settledAt      time.Time
		memo           []byte
		paymentRequest []byte
		preimage       *lntypes.Preimage
		paymentAddr    [32]byte
	)

	hash, err := lntypes.MakeHash(row.Hash)
	if err != nil {
		return nil, nil, err
	}

	if row.SettleIndex.Valid {
		settleIndex = row.SettleIndex.Int64
	}

	if row.SettledAt.Valid {
		settledAt = row.SettledAt.Time.Local()
	}

	if row.Memo.Valid {
		memo = []byte(row.Memo.String)
	}

	// Keysend payments will have this field empty.
	if row.PaymentRequest.Valid {
		paymentRequest = []byte(row.PaymentRequest.String)
	} else {
		paymentRequest = []byte{}
	}

	// We may not have the preimage if this a hodl invoice.
	if row.Preimage != nil {
		preimage = &lntypes.Preimage{}
		copy(preimage[:], row.Preimage)
	}

	copy(paymentAddr[:], row.PaymentAddr)

	var cltvDelta int32
	if row.CltvDelta.Valid {
		cltvDelta = row.CltvDelta.Int32
	}

	invoice := &Invoice{
		SettleIndex:    uint64(settleIndex),
		SettleDate:     settledAt,
		Memo:           memo,
		PaymentRequest: paymentRequest,
		CreationDate:   row.CreatedAt.Local(),
		Terms: ContractTerm{
			FinalCltvDelta:  cltvDelta,
			Expiry:          time.Duration(row.Expiry),
			PaymentPreimage: preimage,
			Value:           lnwire.MilliSatoshi(row.AmountMsat),
			PaymentAddr:     paymentAddr,
		},
		AddIndex:    uint64(row.ID),
		State:       ContractState(row.State),
		AmtPaid:     lnwire.MilliSatoshi(row.AmountPaidMsat),
		Htlcs:       make(map[models.CircuitKey]*InvoiceHTLC),
		AMPState:    AMPInvoiceState{},
		HodlInvoice: row.IsHodl,
	}

	return &hash, invoice, nil
}

// unmarshalInvoiceHTLC converts an sqlc.InvoiceHtlc to an InvoiceHTLC.
func unmarshalInvoiceHTLC(row sqlc.InvoiceHtlc) (CircuitKey,
	*InvoiceHTLC, error) {

	uint64ChanID, err := strconv.ParseUint(row.ChanID, 10, 64)
	if err != nil {
		return CircuitKey{}, nil, err
	}

	chanID := lnwire.NewShortChanIDFromInt(uint64ChanID)

	if row.HtlcID < 0 {
		return CircuitKey{}, nil, fmt.Errorf("invalid uint64 "+
			"value: %v", row.HtlcID)
	}

	htlcID := uint64(row.HtlcID)

	circuitKey := CircuitKey{
		ChanID: chanID,
		HtlcID: htlcID,
	}

	htlc := &InvoiceHTLC{
		Amt:          lnwire.MilliSatoshi(row.AmountMsat),
		AcceptHeight: uint32(row.AcceptHeight),
		AcceptTime:   row.AcceptTime.Local(),
		Expiry:       uint32(row.ExpiryHeight),
		State:        HtlcState(row.State),
	}

	if row.TotalMppMsat.Valid {
		htlc.MppTotalAmt = lnwire.MilliSatoshi(row.TotalMppMsat.Int64)
	}

	if row.ResolveTime.Valid {
		htlc.ResolveTime = row.ResolveTime.Time.Local()
	}

	return circuitKey, htlc, nil
}

// queryWithLimit is a helper method that can be used to query the database
// using a limit and offset. The passed query function should return the number
// of rows returned and an error if any.
func queryWithLimit(query func(int) (int, error), limit int) error {
	offset := 0
	for {
		rows, err := query(offset)
		if err != nil {
			return err
		}

		if rows < limit {
			return nil
		}

		offset += limit
	}
}
