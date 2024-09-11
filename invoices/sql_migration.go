package invoices

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

var (
	// invoiceBucket is the name of the bucket within the database that
	// stores all data related to invoices no matter their final state.
	// Within the invoice bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint32.
	invoiceBucket = []byte("invoices")

	// paymentHashIndexBucket is the name of the sub-bucket within the
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

// createInvoiceHashIndex creates the payment hash index for all invoices in the
// database. This index is used to retrieve invoice payment hashes during the
// migration from KV to SQL as some invoices do not have a deterministic payment
// hash value. For such invoices we still have a payment hash but it is not
// directly queryable from the KV database.
func createInvoiceHashIndex(ctx context.Context, db kvdb.Backend,
	tx SQLInvoiceQueries) error {

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

		// First iterate over all the hashes in the invoice
		// index bucket and store the hash and the invoice key
		// in the payment_hashes table.
		err := invoiceIndex.ForEach(func(k, v []byte) error {
			// Skip the special numInvoicesKey as that does
			// not point to a valid invoice.
			if bytes.Equal(k, numInvoicesKey) {
				return nil
			}

			// The key is the payment hash, and the value
			// is the invoice key.
			var paymentHash lntypes.Hash
			if len(paymentHash) != len(k) {
				return fmt.Errorf("invalid payment "+
					"hash length: expected %v, "+
					"got %v", len(paymentHash),
					len(k))
			}

			copy(paymentHash[:], k)
			invoiceKey := binary.BigEndian.Uint32(v)

			err := tx.InsertInvoicePaymentHashAndKey(ctx,
				sqlc.InsertInvoicePaymentHashAndKeyParams{
					Hash: paymentHash[:],
					ID:   int64(invoiceKey),
				},
			)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return err
		}

		// Next, iterate over all elements in the add index
		// bucket and update the add index value for the
		// corresponding row in the payment_hashes table.
		return addIndex.ForEach(func(k, v []byte) error {
			// The key is the add index, and the value is
			// the invoice key.
			addIndexNo := binary.BigEndian.Uint64(k)
			invoiceKey := binary.BigEndian.Uint32(v)

			return tx.SetInvoicePaymentHashAddIndex(ctx,
				sqlc.SetInvoicePaymentHashAddIndexParams{
					ID: int64(invoiceKey),
					AddIndex: sql.NullInt64{
						Int64: int64(
							addIndexNo,
						),
						Valid: true,
					},
				},
			)
		})
	}, func() {})
}
