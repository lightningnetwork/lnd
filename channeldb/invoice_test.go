package channeldb

import (
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/roasbeef/btcutil"
)

func TestInvoiceWorkflow(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}
	defer cleanUp()

	// Create a fake invoice which we'll use several times in the tests
	// below.
	fakeInvoice := &Invoice{
		CreationDate: time.Now(),
	}
	copy(fakeInvoice.Memo[:], []byte("memo"))
	copy(fakeInvoice.Receipt[:], []byte("recipt"))
	copy(fakeInvoice.Terms.PaymentPreimage[:], rev[:])
	fakeInvoice.Terms.Value = btcutil.Amount(10000)

	// Add the invoice to the database, this should suceed as there aren't
	// any existing invoices within the database with the same payment
	// hash.
	if err := db.AddInvoice(fakeInvoice); err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}

	// Attempt to retrieve the invoice which was just added to the
	// database. It should be found, and the invoice returned should be
	// identical to the one created above.
	paymentHash := fastsha256.Sum256(fakeInvoice.Terms.PaymentPreimage[:])
	dbInvoice, err := db.LookupInvoice(paymentHash)
	if err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}
	if !reflect.DeepEqual(fakeInvoice, dbInvoice) {
		t.Fatalf("invoice fetched from db doesn't match original %v vs %v",
			spew.Sdump(fakeInvoice), spew.Sdump(dbInvoice))
	}

	// Settle the invoice, the versin retreived from the database should
	// now have the settled bit toggle to true.
	if err := db.SettleInvoice(paymentHash); err != nil {
		t.Fatalf("unable to settle invoice: %v", err)
	}
	dbInvoice2, err := db.LookupInvoice(paymentHash)
	if err != nil {
		t.Fatalf("unable to fetch invoice: %v", err)
	}
	if !dbInvoice2.Terms.Settled {
		t.Fatalf("invoice should now be settled but isn't")
	}

	// Attempt to insert generated above again, this should fail as
	// duplicates are rejected by the processing logic.
	if err := db.AddInvoice(fakeInvoice); err != ErrDuplicateInvoice {
		t.Fatalf("invoice insertion should fail due to duplication, "+
			"instead %v", err)
	}

	// Attempt to look up a non-existant invoice, this should also fail but
	// with a "not found" error.
	var fakeHash [32]byte
	if _, err := db.LookupInvoice(fakeHash); err != ErrInvoiceNotFound {
		t.Fatalf("lookup should have failed, instead %v", err)
	}
}
