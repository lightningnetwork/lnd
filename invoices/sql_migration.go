package invoices

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"golang.org/x/time/rate"
)

var (
	// invoiceBucket is the name of the bucket within the database that
	// stores all data related to invoices no matter their final state.
	// Within the invoice bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint32.
	invoiceBucket = []byte("invoices")

	// invoiceIndexBucket  is the name of the sub-bucket within the
	// invoiceBucket which indexes all invoices by their payment hash. The
	// payment hash is the sha256 of the invoice's payment preimage. This
	// index is used to detect duplicates, and also to provide a fast path
	// for looking up incoming HTLCs to determine if we're able to settle
	// them fully.
	//
	// maps: payHash => invoiceKey
	invoiceIndexBucket = []byte("paymenthashes")

	// numInvoicesKey is the name of key which houses the auto-incrementing
	// invoice ID which is essentially used as a primary key. With each
	// invoice inserted, the primary key is incremented by one. This key is
	// stored within the invoiceIndexBucket. Within the invoiceBucket
	// invoices are uniquely identified by the invoice ID.
	numInvoicesKey = []byte("nik")

	// addIndexBucket is an index bucket that we'll use to create a
	// monotonically increasing set of add indexes. Each time we add a new
	// invoice, this sequence number will be incremented and then populated
	// within the new invoice.
	//
	// In addition to this sequence number, we map:
	//
	//   addIndexNo => invoiceKey
	addIndexBucket = []byte("invoice-add-index")
)

// createInvoiceHashIndex generates a hash index that contains payment hashes
// for each invoice in the database. Retrieving the payment hash for certain
// invoices, such as those created for spontaneous AMP payments, can be
// challenging because the hash is not directly derivable from the invoice's
// parameters and is stored separately in the `paymenthashes` bucket. This
// bucket maps payment hashes to invoice keys, but for migration purposes, we
// need the ability to query in the reverse direction. This function establishes
// a new index in the SQL database that maps each invoice key to its
// corresponding payment hash.
func createInvoiceHashIndex(ctx context.Context, db kvdb.Backend,
	tx *sqlc.Queries) error {

	return db.View(func(kvTx kvdb.RTx) error {
		invoices := kvTx.ReadBucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}

		invoiceIndex := invoices.NestedReadBucket(
			invoiceIndexBucket,
		)
		if invoiceIndex == nil {
			return ErrNoInvoicesCreated
		}

		addIndex := invoices.NestedReadBucket(addIndexBucket)
		if addIndex == nil {
			return ErrNoInvoicesCreated
		}

		// First, iterate over all elements in the add index bucket and
		// insert the add index value for the corresponding invoice key
		// in the payment_hashes table.
		err := addIndex.ForEach(func(k, v []byte) error {
			// The key is the add index, and the value is
			// the invoice key.
			addIndexNo := binary.BigEndian.Uint64(k)
			invoiceKey := binary.BigEndian.Uint32(v)

			return tx.InsertKVInvoiceKeyAndAddIndex(ctx,
				sqlc.InsertKVInvoiceKeyAndAddIndexParams{
					ID:       int64(invoiceKey),
					AddIndex: int64(addIndexNo),
				},
			)
		})
		if err != nil {
			return err
		}

		// Next, iterate over all hashes in the invoice index bucket and
		// set the hash to the corresponding the invoice key in the
		// payment_hashes table.
		return invoiceIndex.ForEach(func(k, v []byte) error {
			// Skip the special numInvoicesKey as that does
			// not point to a valid invoice.
			if bytes.Equal(k, numInvoicesKey) {
				return nil
			}

			// The key is the payment hash, and the value
			// is the invoice key.
			if len(k) != lntypes.HashSize {
				return fmt.Errorf("invalid payment "+
					"hash length: expected %v, "+
					"got %v", lntypes.HashSize,
					len(k))
			}

			invoiceKey := binary.BigEndian.Uint32(v)

			return tx.SetKVInvoicePaymentHash(ctx,
				sqlc.SetKVInvoicePaymentHashParams{
					ID:   int64(invoiceKey),
					Hash: k,
				},
			)
		})
	}, func() {})
}

// toInsertMigratedInvoiceParams creates the parameters for inserting a migrated
// invoice into the SQL database. The parameters are derived from the original
// invoice insert parameters.
func toInsertMigratedInvoiceParams(
	params sqlc.InsertInvoiceParams) sqlc.InsertMigratedInvoiceParams {

	return sqlc.InsertMigratedInvoiceParams{
		Hash:               params.Hash,
		Preimage:           params.Preimage,
		Memo:               params.Memo,
		AmountMsat:         params.AmountMsat,
		CltvDelta:          params.CltvDelta,
		Expiry:             params.Expiry,
		PaymentAddr:        params.PaymentAddr,
		PaymentRequest:     params.PaymentRequest,
		PaymentRequestHash: params.PaymentRequestHash,
		State:              params.State,
		AmountPaidMsat:     params.AmountPaidMsat,
		IsAmp:              params.IsAmp,
		IsHodl:             params.IsHodl,
		IsKeysend:          params.IsKeysend,
		CreatedAt:          params.CreatedAt,
	}
}

// MigrateSingleInvoice migrates a single invoice to the new SQL schema. Note
// that perfect equality between the old and new schemas is not achievable, as
// the invoice's add index cannot be mapped directly to its ID due to SQL’s
// auto-incrementing primary key. The ID returned from the insert will instead
// serve as the add index in the new schema.
func MigrateSingleInvoice(ctx context.Context, tx SQLInvoiceQueries,
	invoice *Invoice, paymentHash lntypes.Hash) error {

	insertInvoiceParams, err := makeInsertInvoiceParams(
		invoice, paymentHash,
	)
	if err != nil {
		return err
	}

	// Convert the insert invoice parameters to the migrated invoice insert
	// parameters.
	insertMigratedInvoiceParams := toInsertMigratedInvoiceParams(
		insertInvoiceParams,
	)

	// If the invoice is settled, we'll also set the timestamp and the index
	// at which it was settled.
	if invoice.State == ContractSettled {
		if invoice.SettleIndex == 0 {
			return fmt.Errorf("settled invoice %s missing settle "+
				"index", paymentHash)
		}

		if invoice.SettleDate.IsZero() {
			return fmt.Errorf("settled invoice %s missing settle "+
				"date", paymentHash)
		}

		insertMigratedInvoiceParams.SettleIndex = sqldb.SQLInt64(
			invoice.SettleIndex,
		)
		insertMigratedInvoiceParams.SettledAt = sqldb.SQLTime(
			invoice.SettleDate.UTC(),
		)
	}

	// First we need to insert the invoice itself so we can use the "add
	// index" which in this case is the auto incrementing primary key that
	// is returned from the insert.
	invoiceID, err := tx.InsertMigratedInvoice(
		ctx, insertMigratedInvoiceParams,
	)
	if err != nil {
		return fmt.Errorf("unable to insert invoice: %w", err)
	}

	// Insert the invoice's features.
	for feature := range invoice.Terms.Features.Features() {
		params := sqlc.InsertInvoiceFeatureParams{
			InvoiceID: invoiceID,
			Feature:   int32(feature),
		}

		err := tx.InsertInvoiceFeature(ctx, params)
		if err != nil {
			return fmt.Errorf("unable to insert invoice "+
				"feature(%v): %w", feature, err)
		}
	}

	sqlHtlcIDs := make(map[models.CircuitKey]int64)

	// Now insert the HTLCs of the invoice. We'll also keep track of the SQL
	// ID of each HTLC so we can use it when inserting the AMP sub invoices.
	for circuitKey, htlc := range invoice.Htlcs {
		htlcParams := sqlc.InsertInvoiceHTLCParams{
			HtlcID: int64(circuitKey.HtlcID),
			ChanID: strconv.FormatUint(
				circuitKey.ChanID.ToUint64(), 10,
			),
			AmountMsat:   int64(htlc.Amt),
			AcceptHeight: int32(htlc.AcceptHeight),
			AcceptTime:   htlc.AcceptTime.UTC(),
			ExpiryHeight: int32(htlc.Expiry),
			State:        int16(htlc.State),
			InvoiceID:    invoiceID,
		}

		// Leave the MPP amount as NULL if the MPP total amount is zero.
		if htlc.MppTotalAmt != 0 {
			htlcParams.TotalMppMsat = sqldb.SQLInt64(
				int64(htlc.MppTotalAmt),
			)
		}

		// Leave the resolve time as NULL if the HTLC is not resolved.
		if !htlc.ResolveTime.IsZero() {
			htlcParams.ResolveTime = sqldb.SQLTime(
				htlc.ResolveTime.UTC(),
			)
		}

		sqlID, err := tx.InsertInvoiceHTLC(ctx, htlcParams)
		if err != nil {
			return fmt.Errorf("unable to insert invoice htlc: %w",
				err)
		}

		sqlHtlcIDs[circuitKey] = sqlID

		// Store custom records.
		for key, value := range htlc.CustomRecords {
			err = tx.InsertInvoiceHTLCCustomRecord(
				ctx, sqlc.InsertInvoiceHTLCCustomRecordParams{
					Key:    int64(key),
					Value:  value,
					HtlcID: sqlID,
				},
			)
			if err != nil {
				return err
			}
		}
	}

	if !invoice.IsAMP() {
		return nil
	}

	for setID, ampState := range invoice.AMPState {
		// Find the earliest HTLC of the AMP invoice, which will
		// be used as the creation date of this sub invoice.
		var createdAt time.Time
		for circuitKey := range ampState.InvoiceKeys {
			htlc := invoice.Htlcs[circuitKey]
			if createdAt.IsZero() {
				createdAt = htlc.AcceptTime.UTC()
				continue
			}

			if createdAt.After(htlc.AcceptTime) {
				createdAt = htlc.AcceptTime.UTC()
			}
		}

		params := sqlc.InsertAMPSubInvoiceParams{
			SetID:     setID[:],
			State:     int16(ampState.State),
			CreatedAt: createdAt,
			InvoiceID: invoiceID,
		}

		if ampState.SettleIndex != 0 {
			if ampState.SettleDate.IsZero() {
				return fmt.Errorf("settled AMP sub invoice %x "+
					"missing settle date", setID)
			}

			params.SettledAt = sqldb.SQLTime(
				ampState.SettleDate.UTC(),
			)

			params.SettleIndex = sqldb.SQLInt64(
				ampState.SettleIndex,
			)
		}

		err := tx.InsertAMPSubInvoice(ctx, params)
		if err != nil {
			return fmt.Errorf("unable to insert AMP sub invoice: "+
				"%w", err)
		}

		// Now we can add the AMP HTLCs to the database.
		for circuitKey := range ampState.InvoiceKeys {
			htlc := invoice.Htlcs[circuitKey]
			rootShare := htlc.AMP.Record.RootShare()

			sqlHtlcID, ok := sqlHtlcIDs[circuitKey]
			if !ok {
				return fmt.Errorf("missing htlc for AMP htlc: "+
					"%v", circuitKey)
			}

			params := sqlc.InsertAMPSubInvoiceHTLCParams{
				InvoiceID:  invoiceID,
				SetID:      setID[:],
				HtlcID:     sqlHtlcID,
				RootShare:  rootShare[:],
				ChildIndex: int64(htlc.AMP.Record.ChildIndex()),
				Hash:       htlc.AMP.Hash[:],
			}

			if htlc.AMP.Preimage != nil {
				params.Preimage = htlc.AMP.Preimage[:]
			}

			err = tx.InsertAMPSubInvoiceHTLC(ctx, params)
			if err != nil {
				return fmt.Errorf("unable to insert AMP sub "+
					"invoice: %w", err)
			}
		}
	}

	return nil
}

// OverrideInvoiceTimeZone overrides the time zone of the invoice to the local
// time zone and chops off the nanosecond part for comparison. This is needed
// because KV database stores times as-is which as an unwanted side effect would
// fail migration due to time comparison expecting both the original and
// migrated invoices to be in the same local time zone and in microsecond
// precision. Note that PostgreSQL stores times in microsecond precision while
// SQLite can store times in nanosecond precision if using TEXT storage class.
func OverrideInvoiceTimeZone(invoice *Invoice) {
	fixTime := func(t time.Time) time.Time {
		return t.In(time.Local).Truncate(time.Microsecond)
	}

	invoice.CreationDate = fixTime(invoice.CreationDate)

	if !invoice.SettleDate.IsZero() {
		invoice.SettleDate = fixTime(invoice.SettleDate)
	}

	if invoice.IsAMP() {
		for setID, ampState := range invoice.AMPState {
			if ampState.SettleDate.IsZero() {
				continue
			}

			ampState.SettleDate = fixTime(ampState.SettleDate)
			invoice.AMPState[setID] = ampState
		}
	}

	for _, htlc := range invoice.Htlcs {
		if !htlc.AcceptTime.IsZero() {
			htlc.AcceptTime = fixTime(htlc.AcceptTime)
		}

		if !htlc.ResolveTime.IsZero() {
			htlc.ResolveTime = fixTime(htlc.ResolveTime)
		}
	}
}

// MigrateInvoicesToSQL runs the migration of all invoices from the KV database
// to the SQL database. The migration is done in a single transaction to ensure
// that all invoices are migrated or none at all. This function can be run
// multiple times without causing any issues as it will check if the migration
// has already been performed.
func MigrateInvoicesToSQL(ctx context.Context, db kvdb.Backend,
	kvStore InvoiceDB, tx *sqlc.Queries, batchSize int) error {

	log.Infof("Starting migration of invoices from KV to SQL")

	offset := uint64(0)
	t0 := time.Now()

	// Create the hash index which we will use to look up invoice
	// payment hashes by their add index during migration.
	err := createInvoiceHashIndex(ctx, db, tx)
	if err != nil && !errors.Is(err, ErrNoInvoicesCreated) {
		log.Errorf("Unable to create invoice hash index: %v",
			err)

		return err
	}
	log.Debugf("Created SQL invoice hash index in %v", time.Since(t0))

	s := rate.Sometimes{
		Interval: 30 * time.Second,
	}

	t0 = time.Now()
	chunk := 0
	total := 0

	// Now we can start migrating the invoices. We'll do this in
	// batches to reduce memory usage.
	for {
		query := InvoiceQuery{
			IndexOffset:    offset,
			NumMaxInvoices: uint64(batchSize),
		}

		queryResult, err := kvStore.QueryInvoices(ctx, query)
		if err != nil && !errors.Is(err, ErrNoInvoicesCreated) {
			return fmt.Errorf("unable to query invoices: %w", err)
		}

		if len(queryResult.Invoices) == 0 {
			log.Infof("All invoices migrated. Total: %d", total)
			break
		}

		err = migrateInvoices(ctx, tx, queryResult.Invoices)
		if err != nil {
			return err
		}

		offset = queryResult.LastIndexOffset
		resultCnt := len(queryResult.Invoices)
		total += resultCnt
		chunk += resultCnt

		s.Do(func() {
			elapsed := time.Since(t0).Seconds()
			ratePerSec := float64(chunk) / elapsed
			log.Debugf("Migrated %d invoices (%.2f invoices/sec)",
				total, ratePerSec)

			t0 = time.Now()
			chunk = 0
		})
	}

	// Clean up the hash index as it's no longer needed.
	err = tx.ClearKVInvoiceHashIndex(ctx)
	if err != nil {
		return fmt.Errorf("unable to clear invoice hash "+
			"index: %w", err)
	}

	log.Infof("Migration of %d invoices from KV to SQL completed", total)

	return nil
}

func migrateInvoices(ctx context.Context, tx *sqlc.Queries,
	invoices []Invoice) error {

	for i, invoice := range invoices {
		var paymentHash lntypes.Hash
		if invoice.Terms.PaymentPreimage != nil {
			paymentHash = invoice.Terms.PaymentPreimage.Hash()
		} else {
			paymentHashBytes, err :=
				tx.GetKVInvoicePaymentHashByAddIndex(
					ctx, int64(invoice.AddIndex),
				)
			if err != nil {
				// This would be an unexpected inconsistency
				// in the kv database. We can't do much here
				// so we'll notify the user and continue.
				log.Warnf("Cannot migrate invoice, unable to "+
					"fetch payment hash (add_index=%v): %v",
					invoice.AddIndex, err)

				continue
			}

			copy(paymentHash[:], paymentHashBytes)
		}

		err := MigrateSingleInvoice(ctx, tx, &invoices[i], paymentHash)
		if err != nil {
			return fmt.Errorf("unable to migrate invoice(%v): %w",
				paymentHash, err)
		}

		migratedInvoice, err := fetchInvoice(
			ctx, tx, InvoiceRefByHash(paymentHash),
		)
		if err != nil {
			return fmt.Errorf("unable to fetch migrated "+
				"invoice(%v): %w", paymentHash, err)
		}

		// Override the time zone for comparison. Note that we need to
		// override both invoices as the original invoice is coming from
		// KV database, it was stored as a binary serialized Go
		// time.Time value which has nanosecond precision but might have
		// been created in a different time zone. The migrated invoice
		// is stored in SQL in UTC and selected in the local time zone,
		// however in PostgreSQL it has microsecond precision while in
		// SQLite it has nanosecond precision if using TEXT storage
		// class.
		OverrideInvoiceTimeZone(&invoice)
		OverrideInvoiceTimeZone(migratedInvoice)

		// Override the add index before checking for equality.
		migratedInvoice.AddIndex = invoice.AddIndex

		err = sqldb.CompareRecords(invoice, *migratedInvoice, "invoice")
		if err != nil {
			return err
		}
	}

	return nil
}
