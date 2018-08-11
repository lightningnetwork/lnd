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
	i.Receipt = []byte("receipt")

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
	fakeInvoice.Receipt = []byte("receipt")
	fakeInvoice.PaymentRequest = []byte("")
	copy(fakeInvoice.Terms.PaymentPreimage[:], rev[:])
	fakeInvoice.Terms.Value = lnwire.NewMSatFromSatoshis(10000)

	// Add the invoice to the database, this should succeed as there aren't
	// any existing invoices within the database with the same payment
	// hash.
	if _, err := db.AddInvoice(fakeInvoice); err != nil {
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
	if !reflect.DeepEqual(*fakeInvoice, dbInvoice) {
		t.Fatalf("invoice fetched from db doesn't match original %v vs %v",
			spew.Sdump(fakeInvoice), spew.Sdump(dbInvoice))
	}

	// The add index of the invoice retrieved from the database should now
	// be fully populated. As this is the first index written to the DB,
	// the addIndex should be 1.
	if dbInvoice.AddIndex != 1 {
		t.Fatalf("wrong add index: expected %v, got %v", 1,
			dbInvoice.AddIndex)
	}

	// Settle the invoice, the version retrieved from the database should
	// now have the settled bit toggle to true and a non-default
	// SettledDate
	payAmt := fakeInvoice.Terms.Value * 2
	if _, err := db.SettleInvoice(paymentHash, payAmt); err != nil {
		t.Fatalf("unable to settle invoice: %v", err)
	}
	dbInvoice2, err := db.LookupInvoice(paymentHash)
	if err != nil {
		t.Fatalf("unable to fetch invoice: %v", err)
	}
	if !dbInvoice2.Terms.Settled {
		t.Fatalf("invoice should now be settled but isn't")
	}
	if dbInvoice2.SettleDate.IsZero() {
		t.Fatalf("invoice should have non-zero SettledDate but isn't")
	}

	// Our 2x payment should be reflected, and also the settle index of 1
	// should also have been committed for this index.
	if dbInvoice2.AmtPaid != payAmt {
		t.Fatalf("wrong amt paid: expected %v, got %v", payAmt,
			dbInvoice2.AmtPaid)
	}
	if dbInvoice2.SettleIndex != 1 {
		t.Fatalf("wrong settle index: expected %v, got %v", 1,
			dbInvoice2.SettleIndex)
	}

	// Attempt to insert generated above again, this should fail as
	// duplicates are rejected by the processing logic.
	if _, err := db.AddInvoice(fakeInvoice); err != ErrDuplicateInvoice {
		t.Fatalf("invoice insertion should fail due to duplication, "+
			"instead %v", err)
	}

	// Attempt to look up a non-existent invoice, this should also fail but
	// with a "not found" error.
	var fakeHash [32]byte
	if _, err := db.LookupInvoice(fakeHash); err != ErrInvoiceNotFound {
		t.Fatalf("lookup should have failed, instead %v", err)
	}

	// Add 10 random invoices.
	const numInvoices = 10
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]*Invoice, numInvoices+1)
	invoices[0] = &dbInvoice2
	for i := 1; i < len(invoices)-1; i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		if _, err := db.AddInvoice(invoice); err != nil {
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
	// using big endian, the invoices should be retrieved in ascending
	// order (and the primary key should be incremented with each
	// insertion).
	for i := 0; i < len(invoices)-1; i++ {
		if !reflect.DeepEqual(*invoices[i], dbInvoices[i]) {
			t.Fatalf("retrieved invoices don't match %v vs %v",
				spew.Sdump(invoices[i]),
				spew.Sdump(dbInvoices[i]))
		}
	}
}

// TestInvoiceTimeSeries tests that newly added invoices invoices, as well as
// settled invoices are added to the database are properly placed in the add
// add or settle index which serves as an event time series.
func TestInvoiceAddTimeSeries(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// We'll start off by creating 20 random invoices, and inserting them
	// into the database.
	const numInvoices = 20
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]Invoice, numInvoices)
	for i := 0; i < len(invoices); i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		if _, err := db.AddInvoice(invoice); err != nil {
			t.Fatalf("unable to add invoice %v", err)
		}

		invoices[i] = *invoice
	}

	// With the invoices constructed, we'll now create a series of queries
	// that we'll use to assert expected return values of
	// InvoicesAddedSince.
	addQueries := []struct {
		sinceAddIndex uint64

		resp []Invoice
	}{
		// If we specify a value of zero, we shouldn't get any invoices
		// back.
		{
			sinceAddIndex: 0,
		},

		// If we specify a value well beyond the number of inserted
		// invoices, we shouldn't get any invoices back.
		{
			sinceAddIndex: 99999999,
		},

		// Using an index of 1 should result in all values, but the
		// first one being returned.
		{
			sinceAddIndex: 1,
			resp:          invoices[1:],
		},

		// If we use an index of 10, then we should retrieve the
		// reaming 10 invoices.
		{
			sinceAddIndex: 10,
			resp:          invoices[10:],
		},
	}

	for i, query := range addQueries {
		resp, err := db.InvoicesAddedSince(query.sinceAddIndex)
		if err != nil {
			t.Fatalf("unable to query: %v", err)
		}

		if !reflect.DeepEqual(query.resp, resp) {
			t.Fatalf("test #%v: expected %v, got %v", i,
				spew.Sdump(query.resp), spew.Sdump(resp))
		}
	}

	// We'll now only settle the latter half of each of those invoices.
	for i := 10; i < len(invoices); i++ {
		invoice := &invoices[i]

		paymentHash := sha256.Sum256(
			invoice.Terms.PaymentPreimage[:],
		)

		_, err := db.SettleInvoice(paymentHash, 0)
		if err != nil {
			t.Fatalf("unable to settle invoice: %v", err)
		}
	}

	invoices, err = db.FetchAllInvoices(false)
	if err != nil {
		t.Fatalf("unable to fetch invoices: %v", err)
	}

	// We'll slice off the first 10 invoices, as we only settled the last
	// 10.
	invoices = invoices[10:]

	// We'll now prepare an additional set of queries to ensure the settle
	// time series has properly been maintained in the database.
	settleQueries := []struct {
		sinceSettleIndex uint64

		resp []Invoice
	}{
		// If we specify a value of zero, we shouldn't get any settled
		// invoices back.
		{
			sinceSettleIndex: 0,
		},

		// If we specify a value well beyond the number of settled
		// invoices, we shouldn't get any invoices back.
		{
			sinceSettleIndex: 99999999,
		},

		// Using an index of 1 should result in the final 10 invoices
		// being returned, as we only settled those.
		{
			sinceSettleIndex: 1,
			resp:             invoices[1:],
		},
	}

	for i, query := range settleQueries {
		resp, err := db.InvoicesSettledSince(query.sinceSettleIndex)
		if err != nil {
			t.Fatalf("unable to query: %v", err)
		}

		if !reflect.DeepEqual(query.resp, resp) {
			t.Fatalf("test #%v: expected %v, got %v", i,
				spew.Sdump(query.resp), spew.Sdump(resp))
		}
	}
}

// TestDuplicateSettleInvoice tests that if we add a new invoice and settle it
// twice, then the second time we also receive the invoice that we settled as a
// return argument.
func TestDuplicateSettleInvoice(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// We'll start out by creating an invoice and writing it to the DB.
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoice, err := randInvoice(amt)
	if err != nil {
		t.Fatalf("unable to create invoice: %v", err)
	}

	if _, err := db.AddInvoice(invoice); err != nil {
		t.Fatalf("unable to add invoice %v", err)
	}

	// With the invoice in the DB, we'll now attempt to settle the invoice.
	payHash := sha256.Sum256(invoice.Terms.PaymentPreimage[:])
	dbInvoice, err := db.SettleInvoice(payHash, amt)
	if err != nil {
		t.Fatalf("unable to settle invoice: %v", err)
	}

	// We'll update what we expect the settle invoice to be so that our
	// comparison below has the correct assumption.
	invoice.SettleIndex = 1
	invoice.Terms.Settled = true
	invoice.AmtPaid = amt
	invoice.SettleDate = dbInvoice.SettleDate

	// We should get back the exact same invoice that we just inserted.
	if !reflect.DeepEqual(dbInvoice, invoice) {
		t.Fatalf("wrong invoice after settle, expected %v got %v",
			spew.Sdump(invoice), spew.Sdump(dbInvoice))
	}

	// If we try to settle the invoice again, then we should get the very
	// same invoice back.
	dbInvoice, err = db.SettleInvoice(payHash, amt)
	if err != nil {
		t.Fatalf("unable to settle invoice: %v", err)
	}

	if dbInvoice == nil {
		t.Fatalf("invoice from db is nil after settle!")
	}

	invoice.SettleDate = dbInvoice.SettleDate
	if !reflect.DeepEqual(dbInvoice, invoice) {
		t.Fatalf("wrong invoice after second settle, expected %v got %v",
			spew.Sdump(invoice), spew.Sdump(dbInvoice))
	}
}

// TestQueryInvoices ensures that we can properly query the invoice database for
// invoices between specific time intervals.
func TestQueryInvoices(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// To begin the test, we'll add 100 invoices to the database. We'll
	// assume that the index of the invoice within the database is the same
	// as the amount of the invoice itself.
	const numInvoices = 100
	for i := lnwire.MilliSatoshi(0); i < numInvoices; i++ {
		invoice, err := randInvoice(i)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		if _, err := db.AddInvoice(invoice); err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		// We'll only settle half of all invoices created.
		if i%2 == 0 {
			paymentHash := sha256.Sum256(invoice.Terms.PaymentPreimage[:])
			if _, err := db.SettleInvoice(paymentHash, i); err != nil {
				t.Fatalf("unable to settle invoice: %v", err)
			}
		}
	}

	// With the invoices created, we can begin querying the database. We'll
	// start with a simple query to retrieve all invoices.
	query := InvoiceQuery{
		NumMaxInvoices: numInvoices,
	}
	res, err := db.QueryInvoices(query)
	if err != nil {
		t.Fatalf("unable to query invoices: %v", err)
	}
	if len(res.Invoices) != numInvoices {
		t.Fatalf("expected %d invoices, got %d", numInvoices,
			len(res.Invoices))
	}

	// Now, we'll limit the query to only return the latest 30 invoices.
	query.IndexOffset = 70
	res, err = db.QueryInvoices(query)
	if err != nil {
		t.Fatalf("unable to query invoices: %v", err)
	}
	if uint32(len(res.Invoices)) != numInvoices-query.IndexOffset {
		t.Fatalf("expected %d invoices, got %d",
			numInvoices-query.IndexOffset, len(res.Invoices))
	}
	for _, invoice := range res.Invoices {
		if uint32(invoice.Terms.Value) < query.IndexOffset {
			t.Fatalf("found invoice with index %v before offset %v",
				invoice.Terms.Value, query.IndexOffset)
		}
	}

	// Limit the query from above to return 25 invoices max.
	query.NumMaxInvoices = 25
	res, err = db.QueryInvoices(query)
	if err != nil {
		t.Fatalf("unable to query invoices: %v", err)
	}
	if uint32(len(res.Invoices)) != query.NumMaxInvoices {
		t.Fatalf("expected %d invoices, got %d", query.NumMaxInvoices,
			len(res.Invoices))
	}

	// Reset the query to fetch all unsettled invoices within the time
	// slice.
	query = InvoiceQuery{
		PendingOnly:    true,
		NumMaxInvoices: numInvoices,
	}
	res, err = db.QueryInvoices(query)
	if err != nil {
		t.Fatalf("unable to query invoices: %v", err)
	}
	// Since only invoices with even amounts were settled, we should see
	// that there are 50 invoices within the response.
	if len(res.Invoices) != numInvoices/2 {
		t.Fatalf("expected %d pending invoices, got %d", numInvoices/2,
			len(res.Invoices))
	}
	for _, invoice := range res.Invoices {
		if invoice.Terms.Value%2 == 0 {
			t.Fatal("retrieved unexpected settled invoice")
		}
	}

	// Finally, we'll skip the first 10 invoices from the set of unsettled
	// invoices.
	query.IndexOffset = 10
	res, err = db.QueryInvoices(query)
	if err != nil {
		t.Fatalf("unable to query invoices: %v", err)
	}
	if uint32(len(res.Invoices)) != (numInvoices/2)-query.IndexOffset {
		t.Fatalf("expected %d invoices, got %d",
			(numInvoices/2)-query.IndexOffset, len(res.Invoices))
	}
	// To ensure the correct invoices were returned, we'll make sure each
	// invoice has an odd value (meaning unsettled). Since the 10 invoices
	// skipped should be unsettled, the value of the invoice must be at
	// least the index of the 11th unsettled invoice.
	for _, invoice := range res.Invoices {
		if uint32(invoice.Terms.Value) < query.IndexOffset*2 {
			t.Fatalf("found invoice with index %v before offset %v",
				invoice.Terms.Value, query.IndexOffset*2)
		}
		if invoice.Terms.Value%2 == 0 {
			t.Fatalf("found unexpected settled invoice with index %v",
				invoice.Terms.Value)
		}
	}
}
