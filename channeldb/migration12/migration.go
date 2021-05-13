package migration12

import (
	"bytes"

	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/kvdb"
)

var emptyFeatures = lnwire.NewFeatureVector(nil, nil)

// MigrateInvoiceTLV migrates all existing invoice bodies over to be serialized
// in a single TLV stream. In the process, we drop the Receipt field and add
// PaymentAddr and Features to the invoice Terms.
func MigrateInvoiceTLV(tx kvdb.RwTx) error {
	log.Infof("Migrating invoice bodies to TLV, " +
		"adding payment addresses and feature vectors.")

	invoiceB := tx.ReadWriteBucket(invoiceBucket)
	if invoiceB == nil {
		return nil
	}

	type keyedInvoice struct {
		key     []byte
		invoice Invoice
	}

	// Read in all existing invoices using the old format.
	var invoices []keyedInvoice
	err := invoiceB.ForEach(func(k, v []byte) error {
		if v == nil {
			return nil
		}

		invoiceReader := bytes.NewReader(v)
		invoice, err := LegacyDeserializeInvoice(invoiceReader)
		if err != nil {
			return err
		}

		// Insert an empty feature vector on all old payments.
		invoice.Terms.Features = emptyFeatures

		invoices = append(invoices, keyedInvoice{
			key:     k,
			invoice: invoice,
		})

		return nil
	})
	if err != nil {
		return err
	}

	// Write out each one under its original key using TLV.
	for _, ki := range invoices {
		var b bytes.Buffer
		err = SerializeInvoice(&b, &ki.invoice)
		if err != nil {
			return err
		}

		err = invoiceB.Put(ki.key, b.Bytes())
		if err != nil {
			return err
		}
	}

	log.Infof("Migration to TLV invoice bodies, " +
		"payment address, and features complete!")

	return nil
}
