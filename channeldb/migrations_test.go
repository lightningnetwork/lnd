package channeldb

import (
	"testing"
	"github.com/coreos/bbolt"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/pkg/errors"
	"crypto/sha256"
)

func TestMigration_AddInvoiceWithChannelPoint(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	invoice, err := randInvoice(10)
	if err != nil {
		t.Fatalf("unanle make random invoice: %v", err)
	}

	if err := db.AddInvoice(invoice, nodeAndEdgeUpdateIndexVersion); err != nil {
		t.Fatalf("unble to add invoice: %v", err)
	}

	payment, err := makeRandomFakePayment()
	if err != nil {
		t.Fatalf("unble to make random payment: %v", err)
	}

	if err := db.AddPayment(payment, nodeAndEdgeUpdateIndexVersion); err != nil {
		t.Fatalf("unble to add payment: %v", err)
	}

	hash := sha256.Sum256(invoice.Terms.PaymentPreimage[:][:])
	invoiceHash, _ := chainhash.NewHash(hash[:])

	if _, err := db.LookupInvoice(*invoiceHash, LastVersion); err == nil {
		t.Fatalf("lookup should have fail because we of usage of new" +
			" db version")
	}

	if _, err := db.FetchAllPayments(LastVersion); err == nil {
		t.Fatalf("fetch of payments should have fail because we of usage of" +
			" new db version")
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		if err := migrateAddInvoiceWithChannelPoint(tx); err != nil {
			return errors.Errorf("unable to migrate: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := db.LookupInvoice(*invoiceHash, LastVersion); err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}

	if _, err := db.FetchAllPayments(LastVersion); err != nil {
		t.Fatalf("unable to fetch payments: %v", err)
	}
}
