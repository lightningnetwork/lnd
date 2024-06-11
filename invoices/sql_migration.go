package invoices

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
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
