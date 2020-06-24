package channeldb

import (
	"crypto/rand"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

var (
	emptyFeatures = lnwire.NewFeatureVector(nil, lnwire.Features)
	testNow       = time.Unix(1, 0)
)

func randInvoice(value lnwire.MilliSatoshi) (*Invoice, error) {
	var (
		pre     lntypes.Preimage
		payAddr [32]byte
	)
	if _, err := rand.Read(pre[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(payAddr[:]); err != nil {
		return nil, err
	}

	i := &Invoice{
		CreationDate: testNow,
		Terms: ContractTerm{
			Expiry:          4000,
			PaymentPreimage: &pre,
			PaymentAddr:     payAddr,
			Value:           value,
			Features:        emptyFeatures,
		},
		Htlcs: map[CircuitKey]*InvoiceHTLC{},
	}
	i.Memo = []byte("memo")

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

// settleTestInvoice settles a test invoice.
func settleTestInvoice(invoice *Invoice, settleIndex uint64) {
	invoice.SettleDate = testNow
	invoice.AmtPaid = invoice.Terms.Value
	invoice.State = ContractSettled
	invoice.Htlcs[CircuitKey{}] = &InvoiceHTLC{
		Amt:           invoice.Terms.Value,
		AcceptTime:    testNow,
		ResolveTime:   testNow,
		State:         HtlcStateSettled,
		CustomRecords: make(record.CustomSet),
	}
	invoice.SettleIndex = settleIndex
}

// Tests that pending invoices are those which are either in ContractOpen or
// in ContractAccepted state.
func TestInvoiceIsPending(t *testing.T) {
	contractStates := []ContractState{
		ContractOpen, ContractSettled, ContractCanceled, ContractAccepted,
	}

	for _, state := range contractStates {
		invoice := Invoice{
			State: state,
		}

		// We expect that an invoice is pending if it's either in ContractOpen
		// or ContractAccepted state.
		pending := (state == ContractOpen || state == ContractAccepted)

		if invoice.IsPending() != pending {
			t.Fatalf("expected pending: %v, got: %v, invoice: %v",
				pending, invoice.IsPending(), invoice)
		}
	}
}

type invWorkflowTest struct {
	name         string
	queryPayHash bool
	queryPayAddr bool
}

var invWorkflowTests = []invWorkflowTest{
	{
		name:         "unknown",
		queryPayHash: false,
		queryPayAddr: false,
	},
	{
		name:         "only payhash known",
		queryPayHash: true,
		queryPayAddr: false,
	},
	{
		name:         "payaddr and payhash known",
		queryPayHash: true,
		queryPayAddr: true,
	},
}

// TestInvoiceWorkflow asserts the basic process of inserting, fetching, and
// updating an invoice. We assert that the flow is successful using when
// querying with various combinations of payment hash and payment address.
func TestInvoiceWorkflow(t *testing.T) {
	t.Parallel()

	for _, test := range invWorkflowTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testInvoiceWorkflow(t, test)
		})
	}
}

func testInvoiceWorkflow(t *testing.T, test invWorkflowTest) {
	db, cleanUp, err := MakeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// Create a fake invoice which we'll use several times in the tests
	// below.
	fakeInvoice, err := randInvoice(10000)
	if err != nil {
		t.Fatalf("unable to create invoice: %v", err)
	}
	invPayHash := fakeInvoice.Terms.PaymentPreimage.Hash()

	// Select the payment hash and payment address we will use to lookup or
	// update the invoice for the remainder of the test.
	var (
		payHash lntypes.Hash
		payAddr *[32]byte
		ref     InvoiceRef
	)
	switch {
	case test.queryPayHash && test.queryPayAddr:
		payHash = invPayHash
		payAddr = &fakeInvoice.Terms.PaymentAddr
		ref = InvoiceRefByHashAndAddr(payHash, *payAddr)
	case test.queryPayHash:
		payHash = invPayHash
		ref = InvoiceRefByHash(payHash)
	}

	// Add the invoice to the database, this should succeed as there aren't
	// any existing invoices within the database with the same payment
	// hash.
	if _, err := db.AddInvoice(fakeInvoice, invPayHash); err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}

	// Attempt to retrieve the invoice which was just added to the
	// database. It should be found, and the invoice returned should be
	// identical to the one created above.
	dbInvoice, err := db.LookupInvoice(ref)
	if !test.queryPayAddr && !test.queryPayHash {
		if err != ErrInvoiceNotFound {
			t.Fatalf("invoice should not exist: %v", err)
		}
		return
	}

	require.Equal(t,
		*fakeInvoice, dbInvoice,
		"invoice fetched from db doesn't match original",
	)

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
	_, err = db.UpdateInvoice(ref, getUpdateInvoice(payAmt))
	if err != nil {
		t.Fatalf("unable to settle invoice: %v", err)
	}
	dbInvoice2, err := db.LookupInvoice(ref)
	if err != nil {
		t.Fatalf("unable to fetch invoice: %v", err)
	}
	if dbInvoice2.State != ContractSettled {
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
	if _, err := db.AddInvoice(fakeInvoice, payHash); err != ErrDuplicateInvoice {
		t.Fatalf("invoice insertion should fail due to duplication, "+
			"instead %v", err)
	}

	// Attempt to look up a non-existent invoice, this should also fail but
	// with a "not found" error.
	var fakeHash [32]byte
	fakeRef := InvoiceRefByHash(fakeHash)
	_, err = db.LookupInvoice(fakeRef)
	if err != ErrInvoiceNotFound {
		t.Fatalf("lookup should have failed, instead %v", err)
	}

	// Add 10 random invoices.
	const numInvoices = 10
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]*Invoice, numInvoices+1)
	invoices[0] = &dbInvoice2
	for i := 1; i < len(invoices); i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		hash := invoice.Terms.PaymentPreimage.Hash()
		if _, err := db.AddInvoice(invoice, hash); err != nil {
			t.Fatalf("unable to add invoice %v", err)
		}

		invoices[i] = invoice
	}

	// Perform a scan to collect all the active invoices.
	query := InvoiceQuery{
		IndexOffset:    0,
		NumMaxInvoices: math.MaxUint64,
		PendingOnly:    false,
	}

	response, err := db.QueryInvoices(query)
	if err != nil {
		t.Fatalf("invoice query failed: %v", err)
	}

	// The retrieve list of invoices should be identical as since we're
	// using big endian, the invoices should be retrieved in ascending
	// order (and the primary key should be incremented with each
	// insertion).
	for i := 0; i < len(invoices); i++ {
		require.Equal(t,
			*invoices[i], response.Invoices[i],
			"retrieved invoice doesn't match",
		)
	}
}

// TestAddDuplicatePayAddr asserts that the payment addresses of inserted
// invoices are unique.
func TestAddDuplicatePayAddr(t *testing.T) {
	db, cleanUp, err := MakeTestDB()
	defer cleanUp()
	require.NoError(t, err)

	// Create two invoices with the same payment addr.
	invoice1, err := randInvoice(1000)
	require.NoError(t, err)

	invoice2, err := randInvoice(20000)
	require.NoError(t, err)
	invoice2.Terms.PaymentAddr = invoice1.Terms.PaymentAddr

	// First insert should succeed.
	inv1Hash := invoice1.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(invoice1, inv1Hash)
	require.NoError(t, err)

	// Second insert should fail with duplicate payment addr.
	inv2Hash := invoice2.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(invoice2, inv2Hash)
	require.Error(t, err, ErrDuplicatePayAddr)
}

// TestAddDuplicateKeysendPayAddr asserts that we permit duplicate payment
// addresses to be inserted if they are blank to support JIT legacy keysend
// invoices.
func TestAddDuplicateKeysendPayAddr(t *testing.T) {
	db, cleanUp, err := MakeTestDB()
	defer cleanUp()
	require.NoError(t, err)

	// Create two invoices with the same _blank_ payment addr.
	invoice1, err := randInvoice(1000)
	require.NoError(t, err)
	invoice1.Terms.PaymentAddr = BlankPayAddr

	invoice2, err := randInvoice(20000)
	require.NoError(t, err)
	invoice2.Terms.PaymentAddr = BlankPayAddr

	// Inserting both should succeed without a duplicate payment address
	// failure.
	inv1Hash := invoice1.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(invoice1, inv1Hash)
	require.NoError(t, err)

	inv2Hash := invoice2.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(invoice2, inv2Hash)
	require.NoError(t, err)

	// Querying for each should succeed. Here we use hash+addr refs since
	// the lookup will fail if the hash and addr point to different
	// invoices, so if both succeed we can be assured they aren't included
	// in the payment address index.
	ref1 := InvoiceRefByHashAndAddr(inv1Hash, BlankPayAddr)
	dbInv1, err := db.LookupInvoice(ref1)
	require.NoError(t, err)
	require.Equal(t, invoice1, &dbInv1)

	ref2 := InvoiceRefByHashAndAddr(inv2Hash, BlankPayAddr)
	dbInv2, err := db.LookupInvoice(ref2)
	require.NoError(t, err)
	require.Equal(t, invoice2, &dbInv2)
}

// TestInvRefEquivocation asserts that retrieving or updating an invoice using
// an equivocating InvoiceRef results in ErrInvRefEquivocation.
func TestInvRefEquivocation(t *testing.T) {
	db, cleanUp, err := MakeTestDB()
	defer cleanUp()
	require.NoError(t, err)

	// Add two random invoices.
	invoice1, err := randInvoice(1000)
	require.NoError(t, err)

	inv1Hash := invoice1.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(invoice1, inv1Hash)
	require.NoError(t, err)

	invoice2, err := randInvoice(2000)
	require.NoError(t, err)

	inv2Hash := invoice2.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(invoice2, inv2Hash)
	require.NoError(t, err)

	// Now, query using invoice 1's payment address, but invoice 2's payment
	// hash. We expect an error since the invref points to multiple
	// invoices.
	ref := InvoiceRefByHashAndAddr(inv2Hash, invoice1.Terms.PaymentAddr)
	_, err = db.LookupInvoice(ref)
	require.Error(t, err, ErrInvRefEquivocation)

	// The same error should be returned when updating an equivocating
	// reference.
	nop := func(_ *Invoice) (*InvoiceUpdateDesc, error) {
		return nil, nil
	}
	_, err = db.UpdateInvoice(ref, nop)
	require.Error(t, err, ErrInvRefEquivocation)
}

// TestInvoiceCancelSingleHtlc tests that a single htlc can be canceled on the
// invoice.
func TestInvoiceCancelSingleHtlc(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := MakeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	preimage := lntypes.Preimage{1}
	paymentHash := preimage.Hash()

	testInvoice := &Invoice{
		Htlcs: map[CircuitKey]*InvoiceHTLC{},
		Terms: ContractTerm{
			Value:           lnwire.NewMSatFromSatoshis(10000),
			Features:        emptyFeatures,
			PaymentPreimage: &preimage,
		},
	}

	if _, err := db.AddInvoice(testInvoice, paymentHash); err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}

	// Accept an htlc on this invoice.
	key := CircuitKey{ChanID: lnwire.NewShortChanIDFromInt(1), HtlcID: 4}
	htlc := HtlcAcceptDesc{
		Amt:           500,
		CustomRecords: make(record.CustomSet),
	}

	ref := InvoiceRefByHash(paymentHash)
	invoice, err := db.UpdateInvoice(ref,
		func(invoice *Invoice) (*InvoiceUpdateDesc, error) {
			return &InvoiceUpdateDesc{
				AddHtlcs: map[CircuitKey]*HtlcAcceptDesc{
					key: &htlc,
				},
			}, nil
		})
	if err != nil {
		t.Fatalf("unable to add invoice htlc: %v", err)
	}
	if len(invoice.Htlcs) != 1 {
		t.Fatalf("expected the htlc to be added")
	}
	if invoice.Htlcs[key].State != HtlcStateAccepted {
		t.Fatalf("expected htlc in state accepted")
	}

	// Cancel the htlc again.
	invoice, err = db.UpdateInvoice(ref,
		func(invoice *Invoice) (*InvoiceUpdateDesc, error) {
			return &InvoiceUpdateDesc{
				CancelHtlcs: map[CircuitKey]struct{}{
					key: {},
				},
			}, nil
		})
	if err != nil {
		t.Fatalf("unable to cancel htlc: %v", err)
	}
	if len(invoice.Htlcs) != 1 {
		t.Fatalf("expected the htlc to be present")
	}
	if invoice.Htlcs[key].State != HtlcStateCanceled {
		t.Fatalf("expected htlc in state canceled")
	}
}

// TestInvoiceTimeSeries tests that newly added invoices invoices, as well as
// settled invoices are added to the database are properly placed in the add
// add or settle index which serves as an event time series.
func TestInvoiceAddTimeSeries(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := MakeTestDB(OptionClock(testClock))
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	_, err = db.InvoicesAddedSince(0)
	require.NoError(t, err)

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

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		if _, err := db.AddInvoice(invoice, paymentHash); err != nil {
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

		require.Equal(t, len(query.resp), len(resp))

		for j := 0; j < len(query.resp); j++ {
			require.Equal(t,
				query.resp[j], resp[j],
				fmt.Sprintf("test: #%v, item: #%v", i, j),
			)
		}
	}

	_, err = db.InvoicesSettledSince(0)
	require.NoError(t, err)

	var settledInvoices []Invoice
	var settleIndex uint64 = 1
	// We'll now only settle the latter half of each of those invoices.
	for i := 10; i < len(invoices); i++ {
		invoice := &invoices[i]

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		ref := InvoiceRefByHash(paymentHash)
		_, err := db.UpdateInvoice(
			ref, getUpdateInvoice(invoice.Terms.Value),
		)
		if err != nil {
			t.Fatalf("unable to settle invoice: %v", err)
		}

		// Create the settled invoice for the expectation set.
		settleTestInvoice(invoice, settleIndex)
		settleIndex++

		settledInvoices = append(settledInvoices, *invoice)
	}

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
			resp:             settledInvoices[1:],
		},
	}

	for i, query := range settleQueries {
		resp, err := db.InvoicesSettledSince(query.sinceSettleIndex)
		if err != nil {
			t.Fatalf("unable to query: %v", err)
		}

		require.Equal(t, len(query.resp), len(resp))

		for j := 0; j < len(query.resp); j++ {
			require.Equal(t,
				query.resp[j], resp[j],
				fmt.Sprintf("test: #%v, item: #%v", i, j),
			)
		}
	}
}

// Tests that FetchAllInvoicesWithPaymentHash returns all invoices with their
// corresponding payment hashes.
func TestFetchAllInvoicesWithPaymentHash(t *testing.T) {
	t.Parallel()

	db, cleanup, err := MakeTestDB()
	defer cleanup()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// With an empty DB we expect to return no error and an empty list.
	empty, err := db.FetchAllInvoicesWithPaymentHash(false)
	if err != nil {
		t.Fatalf("failed to call FetchAllInvoicesWithPaymentHash on empty DB: %v",
			err)
	}

	if len(empty) != 0 {
		t.Fatalf("expected empty list as a result, got: %v", empty)
	}

	states := []ContractState{
		ContractOpen, ContractSettled, ContractCanceled, ContractAccepted,
	}

	numInvoices := len(states) * 2
	testPendingInvoices := make(map[lntypes.Hash]*Invoice)
	testAllInvoices := make(map[lntypes.Hash]*Invoice)

	// Now populate the DB and check if we can get all invoices with their
	// payment hashes as expected.
	for i := 1; i <= numInvoices; i++ {
		invoice, err := randInvoice(lnwire.MilliSatoshi(i))
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		// Set the contract state of the next invoice such that there's an equal
		// number for all possbile states.
		invoice.State = states[i%len(states)]
		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		if invoice.IsPending() {
			testPendingInvoices[paymentHash] = invoice
		}

		testAllInvoices[paymentHash] = invoice

		if _, err := db.AddInvoice(invoice, paymentHash); err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}
	}

	pendingInvoices, err := db.FetchAllInvoicesWithPaymentHash(true)
	if err != nil {
		t.Fatalf("can't fetch invoices with payment hash: %v", err)
	}

	if len(testPendingInvoices) != len(pendingInvoices) {
		t.Fatalf("expected %v pending invoices, got: %v",
			len(testPendingInvoices), len(pendingInvoices))
	}

	allInvoices, err := db.FetchAllInvoicesWithPaymentHash(false)
	if err != nil {
		t.Fatalf("can't fetch invoices with payment hash: %v", err)
	}

	if len(testAllInvoices) != len(allInvoices) {
		t.Fatalf("expected %v invoices, got: %v",
			len(testAllInvoices), len(allInvoices))
	}

	for i := range pendingInvoices {
		expected, ok := testPendingInvoices[pendingInvoices[i].PaymentHash]
		if !ok {
			t.Fatalf("coulnd't find invoice with hash: %v",
				pendingInvoices[i].PaymentHash)
		}

		// Zero out add index to not confuse require.Equal.
		pendingInvoices[i].Invoice.AddIndex = 0
		expected.AddIndex = 0

		require.Equal(t, *expected, pendingInvoices[i].Invoice)
	}

	for i := range allInvoices {
		expected, ok := testAllInvoices[allInvoices[i].PaymentHash]
		if !ok {
			t.Fatalf("coulnd't find invoice with hash: %v",
				allInvoices[i].PaymentHash)
		}

		// Zero out add index to not confuse require.Equal.
		allInvoices[i].Invoice.AddIndex = 0
		expected.AddIndex = 0

		require.Equal(t, *expected, allInvoices[i].Invoice)
	}

}

// TestDuplicateSettleInvoice tests that if we add a new invoice and settle it
// twice, then the second time we also receive the invoice that we settled as a
// return argument.
func TestDuplicateSettleInvoice(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := MakeTestDB(OptionClock(testClock))
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

	payHash := invoice.Terms.PaymentPreimage.Hash()

	if _, err := db.AddInvoice(invoice, payHash); err != nil {
		t.Fatalf("unable to add invoice %v", err)
	}

	// With the invoice in the DB, we'll now attempt to settle the invoice.
	ref := InvoiceRefByHash(payHash)
	dbInvoice, err := db.UpdateInvoice(ref, getUpdateInvoice(amt))
	if err != nil {
		t.Fatalf("unable to settle invoice: %v", err)
	}

	// We'll update what we expect the settle invoice to be so that our
	// comparison below has the correct assumption.
	invoice.SettleIndex = 1
	invoice.State = ContractSettled
	invoice.AmtPaid = amt
	invoice.SettleDate = dbInvoice.SettleDate
	invoice.Htlcs = map[CircuitKey]*InvoiceHTLC{
		{}: {
			Amt:           amt,
			AcceptTime:    time.Unix(1, 0),
			ResolveTime:   time.Unix(1, 0),
			State:         HtlcStateSettled,
			CustomRecords: make(record.CustomSet),
		},
	}

	// We should get back the exact same invoice that we just inserted.
	require.Equal(t, invoice, dbInvoice, "wrong invoice after settle")

	// If we try to settle the invoice again, then we should get the very
	// same invoice back, but with an error this time.
	dbInvoice, err = db.UpdateInvoice(ref, getUpdateInvoice(amt))
	if err != ErrInvoiceAlreadySettled {
		t.Fatalf("expected ErrInvoiceAlreadySettled")
	}

	if dbInvoice == nil {
		t.Fatalf("invoice from db is nil after settle!")
	}

	invoice.SettleDate = dbInvoice.SettleDate
	require.Equal(t, invoice, dbInvoice, "wrong invoice after second settle")
}

// TestQueryInvoices ensures that we can properly query the invoice database for
// invoices using different types of queries.
func TestQueryInvoices(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := MakeTestDB(OptionClock(testClock))
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// To begin the test, we'll add 50 invoices to the database. We'll
	// assume that the index of the invoice within the database is the same
	// as the amount of the invoice itself.
	const numInvoices = 50
	var settleIndex uint64 = 1
	var invoices []Invoice
	var pendingInvoices []Invoice

	for i := 1; i <= numInvoices; i++ {
		amt := lnwire.MilliSatoshi(i)
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		if _, err := db.AddInvoice(invoice, paymentHash); err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		// We'll only settle half of all invoices created.
		if i%2 == 0 {
			ref := InvoiceRefByHash(paymentHash)
			_, err := db.UpdateInvoice(ref, getUpdateInvoice(amt))
			if err != nil {
				t.Fatalf("unable to settle invoice: %v", err)
			}

			// Create the settled invoice for the expectation set.
			settleTestInvoice(invoice, settleIndex)
			settleIndex++
		} else {
			pendingInvoices = append(pendingInvoices, *invoice)
		}

		invoices = append(invoices, *invoice)
	}

	// The test will consist of several queries along with their respective
	// expected response. Each query response should match its expected one.
	testCases := []struct {
		query    InvoiceQuery
		expected []Invoice
	}{
		// Fetch all invoices with a single query.
		{
			query: InvoiceQuery{
				NumMaxInvoices: numInvoices,
			},
			expected: invoices,
		},
		// Fetch all invoices with a single query, reversed.
		{
			query: InvoiceQuery{
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices,
		},
		// Fetch the first 25 invoices.
		{
			query: InvoiceQuery{
				NumMaxInvoices: numInvoices / 2,
			},
			expected: invoices[:numInvoices/2],
		},
		// Fetch the first 10 invoices, but this time iterating
		// backwards.
		{
			query: InvoiceQuery{
				IndexOffset:    11,
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices[:10],
		},
		// Fetch the last 40 invoices.
		{
			query: InvoiceQuery{
				IndexOffset:    10,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices[10:],
		},
		// Fetch all but the first invoice.
		{
			query: InvoiceQuery{
				IndexOffset:    1,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices[1:],
		},
		// Fetch one invoice, reversed, with index offset 3. This
		// should give us the second invoice in the array.
		{
			query: InvoiceQuery{
				IndexOffset:    3,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[1:2],
		},
		// Same as above, at index 2.
		{
			query: InvoiceQuery{
				IndexOffset:    2,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[0:1],
		},
		// Fetch one invoice, at index 1, reversed. Since invoice#1 is
		// the very first, there won't be any left in a reverse search,
		// so we expect no invoices to be returned.
		{
			query: InvoiceQuery{
				IndexOffset:    1,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: nil,
		},
		// Same as above, but don't restrict the number of invoices to
		// 1.
		{
			query: InvoiceQuery{
				IndexOffset:    1,
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			expected: nil,
		},
		// Fetch one invoice, reversed, with no offset set. We expect
		// the last invoice in the response.
		{
			query: InvoiceQuery{
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[numInvoices-1:],
		},
		// Fetch one invoice, reversed, the offset set at numInvoices+1.
		// We expect this to return the last invoice.
		{
			query: InvoiceQuery{
				IndexOffset:    numInvoices + 1,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[numInvoices-1:],
		},
		// Same as above, at offset numInvoices.
		{
			query: InvoiceQuery{
				IndexOffset:    numInvoices,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[numInvoices-2 : numInvoices-1],
		},
		// Fetch one invoice, at no offset (same as offset 0). We
		// expect the first invoice only in the response.
		{
			query: InvoiceQuery{
				NumMaxInvoices: 1,
			},
			expected: invoices[:1],
		},
		// Same as above, at offset 1.
		{
			query: InvoiceQuery{
				IndexOffset:    1,
				NumMaxInvoices: 1,
			},
			expected: invoices[1:2],
		},
		// Same as above, at offset 2.
		{
			query: InvoiceQuery{
				IndexOffset:    2,
				NumMaxInvoices: 1,
			},
			expected: invoices[2:3],
		},
		// Same as above, at offset numInvoices-1. Expect the last
		// invoice to be returned.
		{
			query: InvoiceQuery{
				IndexOffset:    numInvoices - 1,
				NumMaxInvoices: 1,
			},
			expected: invoices[numInvoices-1:],
		},
		// Same as above, at offset numInvoices. No invoices should be
		// returned, as there are no invoices after this offset.
		{
			query: InvoiceQuery{
				IndexOffset:    numInvoices,
				NumMaxInvoices: 1,
			},
			expected: nil,
		},
		// Fetch all pending invoices with a single query.
		{
			query: InvoiceQuery{
				PendingOnly:    true,
				NumMaxInvoices: numInvoices,
			},
			expected: pendingInvoices,
		},
		// Fetch the first 12 pending invoices.
		{
			query: InvoiceQuery{
				PendingOnly:    true,
				NumMaxInvoices: numInvoices / 4,
			},
			expected: pendingInvoices[:len(pendingInvoices)/2],
		},
		// Fetch the first 5 pending invoices, but this time iterating
		// backwards.
		{
			query: InvoiceQuery{
				IndexOffset:    10,
				PendingOnly:    true,
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			// Since we seek to the invoice with index 10 and
			// iterate backwards, there should only be 5 pending
			// invoices before it as every other invoice within the
			// index is settled.
			expected: pendingInvoices[:5],
		},
		// Fetch the last 15 invoices.
		{
			query: InvoiceQuery{
				IndexOffset:    20,
				PendingOnly:    true,
				NumMaxInvoices: numInvoices,
			},
			// Since we seek to the invoice with index 20, there are
			// 30 invoices left. From these 30, only 15 of them are
			// still pending.
			expected: pendingInvoices[len(pendingInvoices)-15:],
		},
		// Fetch all invoices paginating backwards, with an index offset
		// that is beyond our last offset. We expect all invoices to be
		// returned.
		{
			query: InvoiceQuery{
				IndexOffset:    numInvoices * 2,
				PendingOnly:    false,
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices,
		},
	}

	for i, testCase := range testCases {
		response, err := db.QueryInvoices(testCase.query)
		if err != nil {
			t.Fatalf("unable to query invoice database: %v", err)
		}

		require.Equal(t, len(testCase.expected), len(response.Invoices))

		for j, expected := range testCase.expected {
			require.Equal(t,
				expected, response.Invoices[j],
				fmt.Sprintf("test: #%v, item: #%v", i, j),
			)
		}
	}
}

// getUpdateInvoice returns an invoice update callback that, when called,
// settles the invoice with the given amount.
func getUpdateInvoice(amt lnwire.MilliSatoshi) InvoiceUpdateCallback {
	return func(invoice *Invoice) (*InvoiceUpdateDesc, error) {
		if invoice.State == ContractSettled {
			return nil, ErrInvoiceAlreadySettled
		}

		noRecords := make(record.CustomSet)

		update := &InvoiceUpdateDesc{
			State: &InvoiceStateUpdateDesc{
				Preimage: invoice.Terms.PaymentPreimage,
				NewState: ContractSettled,
			},
			AddHtlcs: map[CircuitKey]*HtlcAcceptDesc{
				{}: {
					Amt:           amt,
					CustomRecords: noRecords,
				},
			},
		}

		return update, nil
	}
}

// TestCustomRecords tests that custom records are properly recorded in the
// invoice database.
func TestCustomRecords(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := MakeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	preimage := lntypes.Preimage{1}
	paymentHash := preimage.Hash()

	testInvoice := &Invoice{
		Htlcs: map[CircuitKey]*InvoiceHTLC{},
		Terms: ContractTerm{
			Value:           lnwire.NewMSatFromSatoshis(10000),
			Features:        emptyFeatures,
			PaymentPreimage: &preimage,
		},
	}

	if _, err := db.AddInvoice(testInvoice, paymentHash); err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Accept an htlc with custom records on this invoice.
	key := CircuitKey{ChanID: lnwire.NewShortChanIDFromInt(1), HtlcID: 4}

	records := record.CustomSet{
		100000: []byte{},
		100001: []byte{1, 2},
	}

	ref := InvoiceRefByHash(paymentHash)
	_, err = db.UpdateInvoice(ref,
		func(invoice *Invoice) (*InvoiceUpdateDesc, error) {
			return &InvoiceUpdateDesc{
				AddHtlcs: map[CircuitKey]*HtlcAcceptDesc{
					key: {
						Amt:           500,
						CustomRecords: records,
					},
				},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("unable to add invoice htlc: %v", err)
	}

	// Retrieve the invoice from that database and verify that the custom
	// records are present.
	dbInvoice, err := db.LookupInvoice(ref)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}

	if len(dbInvoice.Htlcs) != 1 {
		t.Fatalf("expected the htlc to be added")
	}

	require.Equal(t,
		records, dbInvoice.Htlcs[key].CustomRecords,
		"invalid custom records",
	)
}

// TestInvoiceRef asserts that the proper identifiers are returned from an
// InvoiceRef depending on the constructor used.
func TestInvoiceRef(t *testing.T) {
	payHash := lntypes.Hash{0x01}
	payAddr := [32]byte{0x02}

	// An InvoiceRef by hash should return the provided hash and a nil
	// payment addr.
	refByHash := InvoiceRefByHash(payHash)
	require.Equal(t, payHash, refByHash.PayHash())
	require.Equal(t, (*[32]byte)(nil), refByHash.PayAddr())

	// An InvoiceRef by hash and addr should return the payment hash and
	// payment addr passed to the constructor.
	refByHashAndAddr := InvoiceRefByHashAndAddr(payHash, payAddr)
	require.Equal(t, payHash, refByHashAndAddr.PayHash())
	require.Equal(t, &payAddr, refByHashAndAddr.PayAddr())
}
