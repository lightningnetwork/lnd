package invoices

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/lightningnetwork/lnd/zpay32"
)

// MigrateSingleInvoice migrates a single invoice to the new SQL schema. Note
// that prefect equality between the old and new schema is not possible as the
// add index of the invoice cannot be mapped to the invoice's ID due to SQL's
// auto incrementing primary key. The invoice's ID is returned from the insert
// will be used as the add index in the new schema.
func MigrateSingleInvoice(ctx context.Context, tx SQLInvoiceQueries,
	invoice *Invoice, paymentHash lntypes.Hash) error {

	insertInvoiceParams, err := makeInsertInvoiceParams(
		invoice, paymentHash,
	)
	if err != nil {
		return err
	}

	// If the invoice is settled, we'll also set the timestamp and the index
	// at which it was settled.
	if invoice.State == ContractSettled {
		insertInvoiceParams.SettleIndex = sqldb.SQLInt64(
			invoice.SettleIndex,
		)
		insertInvoiceParams.SettledAt = sqldb.SQLTime(
			invoice.SettleDate.UTC(),
		)
	}

	// First we need to insert the invoice itself so we can use the "add
	// index" which in this case is the auto incrementing primary key that
	// is returned from the insert.
	invoiceID, err := tx.InsertInvoice(ctx, *insertInvoiceParams)
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

		if htlc.MppTotalAmt != 0 {
			htlcParams.TotalMppMsat = sqldb.SQLInt64(
				int64(htlc.MppTotalAmt),
			)
		}

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

	if invoice.IsAMP() {
		for setID, ampState := range invoice.AMPState {
			// Find the first HTLC of the AMP invoice, which will be
			// used as the creation date of this sub invoice.
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
				params.SettledAt = sqldb.SQLTime(
					ampState.SettleDate.UTC(),
				)

				params.SettleIndex = sqldb.SQLInt64(
					ampState.SettleIndex,
				)
			}

			_, err := tx.InsertAMPSubInvoice(ctx, params)
			if err != nil {
				return fmt.Errorf("unable to insert AMP sub "+
					"invoice: %w", err)
			}

			// Now we can add the AMP HTLCs to the database.
			for circuitKey := range ampState.InvoiceKeys {
				htlc := invoice.Htlcs[circuitKey]
				rootShare := htlc.AMP.Record.RootShare()

				sqlHtlcID, ok := sqlHtlcIDs[circuitKey]
				if !ok {
					return fmt.Errorf("missing htlc for "+
						"AMP htlc: %v", circuitKey)
				}

				params := sqlc.InsertAMPSubInvoiceHTLCParams{
					InvoiceID: invoiceID,
					SetID:     setID[:],
					HtlcID:    sqlHtlcID,
					RootShare: rootShare[:],
					ChildIndex: int64(
						htlc.AMP.Record.ChildIndex(),
					),
					Hash: htlc.AMP.Hash[:],
				}

				if htlc.AMP.Preimage != nil {
					params.Preimage = htlc.AMP.Preimage[:]
				}

				err = tx.InsertAMPSubInvoiceHTLC(ctx, params)
				if err != nil {
					return fmt.Errorf("unable to insert "+
						"AMP sub invoice: %w", err)
				}
			}
		}
	}

	return nil
}

// MigrateInvoices migrates all invoices from the old database to the new SQL
// schema. The migration is done in a single transaction to ensure that all
// invoices are migrated or none at all.
func MigrateInvoices(ctx context.Context, db InvoiceDB,
	sqlStore *SQLStore, netParams *chaincfg.Params, batchSize int) error {

	offset := uint64(0)
	var ops SQLInvoiceQueriesTxOptions
	return sqlStore.db.ExecTx(ctx, &ops, func(tx SQLInvoiceQueries) error {
		for {
			query := InvoiceQuery{
				IndexOffset:    offset,
				NumMaxInvoices: uint64(batchSize),
			}

			queryResult, err := db.QueryInvoices(ctx, query)
			if err != nil {
				return fmt.Errorf("unable to query invoices: "+
					"%v", err)
			}

			if len(queryResult.Invoices) == 0 {
				log.Infof("All invoices migrated")

				return nil
			}

			err = migrateInvoices(
				ctx, tx, sqlStore, queryResult.Invoices,
				netParams,
			)
			if err != nil {
				return err
			}

			offset = queryResult.LastIndexOffset
		}
	}, func() {})
}

func migrateInvoices(ctx context.Context, tx SQLInvoiceQueries,
	sqlStore *SQLStore, invoices []Invoice,
	netParams *chaincfg.Params) error {

	for i, invoice := range invoices {
		var paymentHash lntypes.Hash
		if invoice.Terms.PaymentPreimage != nil {
			paymentHash = invoice.Terms.PaymentPreimage.Hash()
		} else {
			paymentRequest, err := zpay32.Decode(
				string(invoice.PaymentRequest),
				netParams,
			)
			if err != nil {
				return fmt.Errorf("unable to decode payment "+
					"request for invoice (add_index=%v): "+
					"%v", invoice.AddIndex, err)
			}

			if paymentRequest.PaymentHash != nil {
				copy(
					paymentHash[:],
					paymentRequest.PaymentHash[:],
				)
			} else {
				log.Warnf("Cannot migrate invoice "+
					"(add_index=%v)", invoice.AddIndex)

				continue
			}
		}
		err := MigrateSingleInvoice(ctx, tx, &invoices[i], paymentHash)
		if err != nil {
			return fmt.Errorf("unable to migrate invoice(%v): %w",
				paymentHash, err)
		}

		migratedInvoice, err := sqlStore.fetchInvoice(
			ctx, tx, InvoiceRefByHash(paymentHash),
		)
		if err != nil {
			return fmt.Errorf("unable to fetch migrated "+
				"invoice(%v): %v", paymentHash, err)
		}

		// Override the add index before checking for equality.
		migratedInvoice.AddIndex = invoice.AddIndex
		if !reflect.DeepEqual(invoice, *migratedInvoice) {
			return fmt.Errorf("migrated invoice does not match "+
				"original invoice: %v", paymentHash)
		}
	}

	return nil
}
