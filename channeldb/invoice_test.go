package channeldb

import (
	"crypto/rand"
	"crypto/sha256"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
)

func randInvoice(value lnwire.MilliSatoshi) (*Invoice, error) {
	var pre [32]byte
	if _, err := rand.Read(pre[:]); err != nil {
		return nil, err
	}

	i := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate: time.Unix(time.Now().Unix(), 0),
		Terms: ContractTerm{
			PaymentPreimage: pre,
			Value:           value,
		},
	}
	i.Memo = []byte("memo")
	i.Receipt = []byte("recipt")

	// Create a random byte slice of MaxPaymentRequestSize bytes to be used
	// as a dummy paymentrequest, and  determine if it should be set based
	// on one of the random bytes.
	var r [MaxPaymentRequestSize]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	if r[0]&1 == 0 {
		i.PaymentRequest = r[:]
	} else {
		i.PaymentRequest = []byte("")
	}

	return i, nil
}

func TestInvoiceWorkflow(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// Create a fake invoice which we'll use several times in the tests
	// below.
	fakeInvoice := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate: time.Unix(time.Now().Unix(), 0),
	}
	fakeInvoice.Memo = []byte("memo")
	fakeInvoice.Receipt = []byte("recipt")
	fakeInvoice.PaymentRequest = []byte("")
	copy(fakeInvoice.Terms.PaymentPreimage[:], rev[:])
	fakeInvoice.Terms.Value = lnwire.NewMSatFromSatoshis(10000)

	// Add the invoice to the database, this should suceed as there aren't
	// any existing invoices within the database with the same payment
	// hash.
	if err := db.AddInvoice(fakeInvoice); err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}

	// Attempt to retrieve the invoice which was just added to the
	// database. It should be found, and the invoice returned should be
	// identical to the one created above.
	paymentHash := sha256.Sum256(fakeInvoice.Terms.PaymentPreimage[:])
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

	// Attempt to look up a non-existent invoice, this should also fail but
	// with a "not found" error.
	var fakeHash [32]byte
	if _, err := db.LookupInvoice(fakeHash); err != ErrInvoiceNotFound {
		t.Fatalf("lookup should have failed, instead %v", err)
	}

	// Add 100 random invoices.
	const numInvoices = 10
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]*Invoice, numInvoices+1)
	invoices[0] = dbInvoice2
	for i := 1; i < len(invoices)-1; i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		if err := db.AddInvoice(invoice); err != nil {
			t.Fatalf("unable to add invoice %v", err)
		}

		invoices[i] = invoice
	}

	// Perform a scan to collect all the active invoices.
	dbInvoices, err := db.FetchAllInvoices(false)
	if err != nil {
		t.Fatalf("unable to fetch all invoices: %v", err)
	}

	// The retrieve list of invoices should be identical as since we're
	// using big endian, the invoices should be retrieved in asecending
	// order (and the primary key should be incremented with each
	// insertion).
	for i := 0; i < len(invoices)-1; i++ {
		if !reflect.DeepEqual(invoices[i], dbInvoices[i]) {
			t.Fatalf("retrived invoices don't match %v vs %v",
				spew.Sdump(invoices[i]),
				spew.Sdump(dbInvoices[i]))
		}
	}
}
