package invoices_test

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/graph/db/models"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

var (
	emptyFeatures = lnwire.NewFeatureVector(nil, lnwire.Features)

	ampFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrOptional,
			lnwire.AMPRequired,
		),
		lnwire.Features,
	)

	testNow = time.Unix(1, 0)
)

// randBytesToString will return a "safe" string from a byte slice. This is
// used to generate random strings for the invoice payment request.
func randBytesToString(buf []byte) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	var stringBuilder strings.Builder

	stringBuilder.Grow(len(buf))
	for i := 0; i < len(buf); i++ {
		ch := charset[int(buf[i])%len(charset)]
		if err := stringBuilder.WriteByte(ch); err != nil {
			return "", err
		}
	}

	return stringBuilder.String(), nil
}

func randInvoice(value lnwire.MilliSatoshi) (*invpkg.Invoice, error) {
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

	i := &invpkg.Invoice{
		CreationDate: testNow,
		Terms: invpkg.ContractTerm{
			Expiry:          time.Duration(4000) * time.Second,
			PaymentPreimage: &pre,
			PaymentAddr:     payAddr,
			Value:           value,
			Features:        emptyFeatures,
		},
		Htlcs:    map[models.CircuitKey]*invpkg.InvoiceHTLC{},
		AMPState: map[invpkg.SetID]invpkg.InvoiceStateAMP{},
	}
	i.Memo = []byte("memo")

	// Create a random byte slice of MaxPaymentRequestSize bytes to be used
	// as a dummy paymentrequest, and  determine if it should be set based
	// on one of the random bytes.
	var r [invpkg.MaxPaymentRequestSize]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	if r[0]&1 == 0 {
		paymentReq, err := randBytesToString(r[:])
		if err != nil {
			return nil, err
		}
		i.PaymentRequest = []byte(paymentReq)
	} else {
		i.PaymentRequest = []byte("")
	}

	return i, nil
}

// settleTestInvoice settles a test invoice.
func settleTestInvoice(invoice *invpkg.Invoice, htlcID uint64,
	settleIndex uint64) {

	invoice.SettleDate = testNow
	invoice.AmtPaid = invoice.Terms.Value
	invoice.State = invpkg.ContractSettled
	invoice.Htlcs[models.CircuitKey{HtlcID: htlcID}] = &invpkg.InvoiceHTLC{
		Amt:           invoice.Terms.Value,
		AcceptTime:    testNow,
		ResolveTime:   testNow,
		State:         invpkg.HtlcStateSettled,
		CustomRecords: make(record.CustomSet),
	}
	invoice.SettleIndex = settleIndex
}

// TestInvoices is a master test which encompasses all tests using an InvoiceDB
// instance. The purpose of this test is to be able to run all tests with a
// custom DB instance, so that we can test the same logic with different DB
// implementations.
func TestInvoices(t *testing.T) {
	testList := []struct {
		name string
		test func(t *testing.T,
			makeDB func(t *testing.T) invpkg.InvoiceDB)
	}{
		{
			name: "InvoiceWorkflow",
			test: testInvoiceWorkflow,
		},
		{
			name: "AddDuplicatePayAddr",
			test: testAddDuplicatePayAddr,
		},
		{
			name: "AddDuplicateKeysendPayAddr",
			test: testAddDuplicateKeysendPayAddr,
		},
		{
			name: "FailInvoiceLookupMPPPayAddrOnly",
			test: testFailInvoiceLookupMPPPayAddrOnly,
		},
		{
			name: "InvRefEquivocation",
			test: testInvRefEquivocation,
		},
		{
			name: "InvoiceCancelSingleHtlc",
			test: testInvoiceCancelSingleHtlc,
		},
		{
			name: "InvoiceCancelSingleHtlcAMP",
			test: testInvoiceCancelSingleHtlcAMP,
		},
		{
			name: "InvoiceAddTimeSeries",
			test: testInvoiceAddTimeSeries,
		},
		{
			name: "SettleIndexAmpPayments",
			test: testSettleIndexAmpPayments,
		},
		{
			name: "FetchPendingInvoices",
			test: testFetchPendingInvoices,
		},
		{
			name: "DuplicateSettleInvoice",
			test: testDuplicateSettleInvoice,
		},
		{
			name: "QueryInvoices",
			test: testQueryInvoices,
		},
		{
			name: "OutWireCustomRecords",
			test: testCustomRecords,
		},
		{
			name: "InvoiceHtlcAMPFields",
			test: testInvoiceHtlcAMPFields,
		},
		{
			name: "AddInvoiceWithHTLCs",
			test: testAddInvoiceWithHTLCs,
		},
		{
			name: "SetIDIndex",
			test: testSetIDIndex,
		},
		{
			name: "UnexpectedInvoicePreimage",
			test: testUnexpectedInvoicePreimage,
		},
		{
			name: "UpdateHTLCPreimages",
			test: testUpdateHTLCPreimages,
		},
		{
			name: "DeleteInvoices",
			test: testDeleteInvoices,
		},
		{
			name: "DeleteCanceledInvoices",
			test: testDeleteCanceledInvoices,
		},
		{
			name: "AddInvoiceInvalidFeatureDeps",
			test: testAddInvoiceInvalidFeatureDeps,
		},
	}

	makeKeyValueDB := func(t *testing.T) invpkg.InvoiceDB {
		db, err := channeldb.MakeTestInvoiceDB(
			t, channeldb.OptionClock(clock.NewTestClock(testNow)),
		)
		require.NoError(t, err, "unable to make test db")

		return db
	}

	// First create a shared Postgres instance so we don't spawn a new
	// docker container for each test.
	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	makeSQLDB := func(t *testing.T, sqlite bool) invpkg.InvoiceDB {
		var db *sqldb.BaseDB
		if sqlite {
			db = sqldb.NewTestSqliteDB(t).BaseDB
		} else {
			db = sqldb.NewTestPostgresDB(t, pgFixture).BaseDB
		}

		executor := sqldb.NewTransactionExecutor(
			db, func(tx *sql.Tx) invpkg.SQLInvoiceQueries {
				return db.WithTx(tx)
			},
		)

		testClock := clock.NewTestClock(testNow)

		// We'll use a pagination limit of 3 for all tests to ensure
		// that we also cover query pagination.
		const testPaginationLimit = 3

		return invpkg.NewSQLStore(
			executor, testClock,
			invpkg.WithPaginationLimit(testPaginationLimit),
		)
	}

	for _, test := range testList {
		test := test
		t.Run(test.name+"_KV", func(t *testing.T) {
			test.test(t, makeKeyValueDB)
		})

		t.Run(test.name+"_SQLite", func(t *testing.T) {
			test.test(t,
				func(t *testing.T) invpkg.InvoiceDB {
					return makeSQLDB(t, true)
				})
		})

		t.Run(test.name+"_Postgres", func(t *testing.T) {
			test.test(t,
				func(t *testing.T) invpkg.InvoiceDB {
					return makeSQLDB(t, false)
				})
		})
	}
}

// TestInvoiceIsPending tests that pending invoices are those which are either
// in ContractOpen or in ContractAccepted state.
func TestInvoiceIsPending(t *testing.T) {
	t.Parallel()

	contractStates := []invpkg.ContractState{
		invpkg.ContractOpen, invpkg.ContractSettled,
		invpkg.ContractCanceled, invpkg.ContractAccepted,
	}

	for _, state := range contractStates {
		invoice := invpkg.Invoice{
			State: state,
		}

		// We expect that an invoice is pending if it's either in
		// ContractOpen or ContractAccepted state.
		open := invpkg.ContractOpen
		accepted := invpkg.ContractAccepted
		pending := (state == open || state == accepted)

		require.Equal(t, pending, invoice.IsPending())
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
func testInvoiceWorkflow(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()

	for _, test := range invWorkflowTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testInvoiceWorkflowImpl(t, test, makeDB)
		})
	}
}

func testInvoiceWorkflowImpl(t *testing.T, test invWorkflowTest,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	db := makeDB(t)

	// Create a fake invoice which we'll use several times in the tests
	// below.
	fakeInvoice, err := randInvoice(10000)
	require.NoError(t, err, "unable to create invoice")
	invPayHash := fakeInvoice.Terms.PaymentPreimage.Hash()

	// Select the payment hash and payment address we will use to lookup or
	// update the invoice for the remainder of the test.
	var (
		payHash lntypes.Hash
		payAddr *[32]byte
		ref     invpkg.InvoiceRef
	)
	switch {
	case test.queryPayHash && test.queryPayAddr:
		payHash = invPayHash
		payAddr = &fakeInvoice.Terms.PaymentAddr
		ref = invpkg.InvoiceRefByHashAndAddr(payHash, *payAddr)
	case test.queryPayHash:
		payHash = invPayHash
		ref = invpkg.InvoiceRefByHash(payHash)
	}

	ctxb := t.Context()
	// Add the invoice to the database, this should succeed as there aren't
	// any existing invoices within the database with the same payment
	// hash.
	if _, err := db.AddInvoice(ctxb, fakeInvoice, invPayHash); err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}

	// Attempt to retrieve the invoice which was just added to the
	// database. It should be found, and the invoice returned should be
	// identical to the one created above.
	dbInvoice, err := db.LookupInvoice(ctxb, ref)
	if !test.queryPayAddr && !test.queryPayHash {
		require.ErrorIs(t, err, invpkg.ErrInvoiceNotFound)
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
	_, err = db.UpdateInvoice(ctxb, ref, nil, getUpdateInvoice(0, payAmt))
	require.NoError(t, err, "unable to settle invoice")
	dbInvoice2, err := db.LookupInvoice(ctxb, ref)
	require.NoError(t, err, "unable to fetch invoice")
	if dbInvoice2.State != invpkg.ContractSettled {
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
	_, err = db.AddInvoice(ctxb, fakeInvoice, payHash)
	require.ErrorIs(t, err, invpkg.ErrDuplicateInvoice)

	// Attempt to look up a non-existent invoice, this should also fail but
	// with a "not found" error.
	var fakeHash [32]byte
	fakeRef := invpkg.InvoiceRefByHash(fakeHash)
	_, err = db.LookupInvoice(ctxb, fakeRef)
	require.ErrorIs(t, err, invpkg.ErrInvoiceNotFound)

	// Add 10 random invoices.
	const numInvoices = 10
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]*invpkg.Invoice, numInvoices+1)
	invoices[0] = &dbInvoice2
	for i := 1; i < len(invoices); i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		hash := invoice.Terms.PaymentPreimage.Hash()
		if _, err := db.AddInvoice(ctxb, invoice, hash); err != nil {
			t.Fatalf("unable to add invoice %v", err)
		}

		invoices[i] = invoice
	}

	// Perform a scan to collect all the active invoices.
	query := invpkg.InvoiceQuery{
		IndexOffset:    0,
		NumMaxInvoices: math.MaxUint64,
		PendingOnly:    false,
	}

	response, err := db.QueryInvoices(ctxb, query)
	require.NoError(t, err, "invoice query failed")

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

// testAddDuplicatePayAddr asserts that the payment addresses of inserted
// invoices are unique.
func testAddDuplicatePayAddr(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// Create two invoices with the same payment addr.
	invoice1, err := randInvoice(1000)
	require.NoError(t, err)

	invoice2, err := randInvoice(20000)
	require.NoError(t, err)
	invoice2.Terms.PaymentAddr = invoice1.Terms.PaymentAddr

	ctxb := t.Context()

	// First insert should succeed.
	inv1Hash := invoice1.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice1, inv1Hash)
	require.NoError(t, err)

	// Second insert should fail with duplicate payment addr.
	inv2Hash := invoice2.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice2, inv2Hash)
	require.Error(t, err, invpkg.ErrDuplicatePayAddr)
}

// testAddDuplicateKeysendPayAddr asserts that we permit duplicate payment
// addresses to be inserted if they are blank to support JIT legacy keysend
// invoices.
func testAddDuplicateKeysendPayAddr(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// Create two invoices with the same _blank_ payment addr.
	invoice1, err := randInvoice(1000)
	require.NoError(t, err)
	invoice1.Terms.PaymentAddr = invpkg.BlankPayAddr

	invoice2, err := randInvoice(20000)
	require.NoError(t, err)
	invoice2.Terms.PaymentAddr = invpkg.BlankPayAddr

	ctxb := t.Context()

	// Inserting both should succeed without a duplicate payment address
	// failure.
	inv1Hash := invoice1.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice1, inv1Hash)
	require.NoError(t, err)

	inv2Hash := invoice2.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice2, inv2Hash)
	require.NoError(t, err)

	// Querying for each should succeed. Here we use hash+addr refs since
	// the lookup will fail if the hash and addr point to different
	// invoices, so if both succeed we can be assured they aren't included
	// in the payment address index.
	ref1 := invpkg.InvoiceRefByHashAndAddr(inv1Hash, invpkg.BlankPayAddr)
	dbInv1, err := db.LookupInvoice(ctxb, ref1)
	require.NoError(t, err)
	require.Equal(t, invoice1, &dbInv1)

	ref2 := invpkg.InvoiceRefByHashAndAddr(inv2Hash, invpkg.BlankPayAddr)
	dbInv2, err := db.LookupInvoice(ctxb, ref2)
	require.NoError(t, err)
	require.Equal(t, invoice2, &dbInv2)
}

// testFailInvoiceLookupMPPPayAddrOnly asserts that looking up a MPP invoice
// that matches _only_ by payment address fails with ErrInvoiceNotFound. This
// ensures that the HTLC's payment hash always matches the payment hash in the
// returned invoice.
func testFailInvoiceLookupMPPPayAddrOnly(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// Create and insert a random invoice.
	invoice, err := randInvoice(1000)
	require.NoError(t, err)

	payHash := invoice.Terms.PaymentPreimage.Hash()
	payAddr := invoice.Terms.PaymentAddr

	ctxb := t.Context()
	_, err = db.AddInvoice(ctxb, invoice, payHash)
	require.NoError(t, err)

	// Modify the queried payment hash to be invalid.
	payHash[0] ^= 0x01

	// Lookup the invoice by (invalid) payment hash and payment address. The
	// lookup should fail since we require the payment hash to match for
	// legacy/MPP invoices, as this guarantees that the preimage is valid
	// for the given HTLC.
	ref := invpkg.InvoiceRefByHashAndAddr(payHash, payAddr)
	_, err = db.LookupInvoice(ctxb, ref)
	require.ErrorIs(t, err, invpkg.ErrInvoiceNotFound)
}

// testInvRefEquivocation asserts that retrieving or updating an invoice using
// an equivocating InvoiceRef results in ErrInvRefEquivocation.
func testInvRefEquivocation(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// Add two random invoices.
	invoice1, err := randInvoice(1000)
	require.NoError(t, err)

	ctxb := t.Context()
	inv1Hash := invoice1.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice1, inv1Hash)
	require.NoError(t, err)

	invoice2, err := randInvoice(2000)
	require.NoError(t, err)

	inv2Hash := invoice2.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice2, inv2Hash)
	require.NoError(t, err)

	// Now, query using invoice 1's payment address, but invoice 2's payment
	// hash. We expect an error since the invref points to multiple
	// invoices.
	ref := invpkg.InvoiceRefByHashAndAddr(
		inv2Hash, invoice1.Terms.PaymentAddr,
	)
	_, err = db.LookupInvoice(ctxb, ref)
	require.Error(t, err, invpkg.ErrInvRefEquivocation)

	// The same error should be returned when updating an equivocating
	// reference.
	nop := func(_ *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc, error) {
		return nil, nil
	}
	_, err = db.UpdateInvoice(ctxb, ref, nil, nop)
	require.Error(t, err, invpkg.ErrInvRefEquivocation)
}

// testInvoiceCancelSingleHtlc tests that a single htlc can be canceled on the
// invoice.
func testInvoiceCancelSingleHtlc(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	preimage := lntypes.Preimage{1}
	paymentHash := preimage.Hash()

	testInvoice := &invpkg.Invoice{
		Htlcs: map[models.CircuitKey]*invpkg.InvoiceHTLC{},
		Terms: invpkg.ContractTerm{
			Value:           lnwire.NewMSatFromSatoshis(10000),
			Features:        emptyFeatures,
			PaymentPreimage: &preimage,
		},
	}

	ctxb := t.Context()
	if _, err := db.AddInvoice(ctxb, testInvoice, paymentHash); err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}

	// Accept an htlc on this invoice.
	key := models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 4,
	}
	htlc := invpkg.HtlcAcceptDesc{
		Amt:           500,
		CustomRecords: make(record.CustomSet),
	}

	callback := func(
		invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc, error) {

		htlcs := map[models.CircuitKey]*invpkg.HtlcAcceptDesc{
			key: &htlc,
		}

		return &invpkg.InvoiceUpdateDesc{
			UpdateType: invpkg.AddHTLCsUpdate,
			AddHtlcs:   htlcs,
		}, nil
	}

	ref := invpkg.InvoiceRefByHash(paymentHash)
	invoice, err := db.UpdateInvoice(ctxb, ref, nil, callback)
	require.NoError(t, err, "unable to add invoice htlc")
	if len(invoice.Htlcs) != 1 {
		t.Fatalf("expected the htlc to be added")
	}
	if invoice.Htlcs[key].State != invpkg.HtlcStateAccepted {
		t.Fatalf("expected htlc in state accepted")
	}

	callback = func(
		invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc, error) {

		return &invpkg.InvoiceUpdateDesc{
			UpdateType: invpkg.CancelHTLCsUpdate,
			CancelHtlcs: map[models.CircuitKey]struct{}{
				key: {},
			},
		}, nil
	}

	// Cancel the htlc again.
	invoice, err = db.UpdateInvoice(ctxb, ref, nil, callback)
	require.NoError(t, err, "unable to cancel htlc")
	if len(invoice.Htlcs) != 1 {
		t.Fatalf("expected the htlc to be present")
	}
	if invoice.Htlcs[key].State != invpkg.HtlcStateCanceled {
		t.Fatalf("expected htlc in state canceled")
	}
}

// testInvoiceCancelSingleHtlcAMP tests that it's possible to cancel a single
// invoice of an AMP HTLC across multiple set IDs, and also have that update
// the amount paid and other related fields as well.
func testInvoiceCancelSingleHtlcAMP(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// We'll start out by creating an invoice and writing it to the DB.
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoice, err := randInvoice(amt)
	require.Nil(t, err)

	// Set AMP-specific features so that we can settle with HTLC-level
	// preimages.
	invoice.Terms.Features = ampFeatures

	ctxb := t.Context()
	preimage := *invoice.Terms.PaymentPreimage
	payHash := preimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice, payHash)
	require.Nil(t, err)

	// Add two HTLC sets, one with one HTLC and the other with two.
	setID1 := &[32]byte{1}
	setID2 := &[32]byte{2}

	ref := invpkg.InvoiceRefByHashAndAddr(
		payHash, invoice.Terms.PaymentAddr,
	)

	// The first set ID with a single HTLC added.
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID1),
		updateAcceptAMPHtlc(0, amt, setID1, true),
	)
	require.Nil(t, err)

	// The second set ID with two HTLCs added.
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID2),
		updateAcceptAMPHtlc(1, amt, setID2, true),
	)
	require.Nil(t, err)
	dbInvoice, err := db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID2),
		updateAcceptAMPHtlc(2, amt, setID2, true),
	)
	require.Nil(t, err)

	// At this point, we should detect that 3k satoshis total has been
	// paid.
	require.EqualValues(t, amt*3, dbInvoice.AmtPaid)

	callback := func(
		invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc, error) {

		return &invpkg.InvoiceUpdateDesc{
			UpdateType: invpkg.CancelHTLCsUpdate,
			CancelHtlcs: map[models.CircuitKey]struct{}{
				{HtlcID: 0}: {},
			},
			SetID: (*invpkg.SetID)(setID1),
		}, nil
	}

	// Now we'll cancel a single invoice, and assert that the amount paid
	// is decremented, and the state for that HTLC set reflects that is
	// been cancelled.
	_, err = db.UpdateInvoice(ctxb, ref, (*invpkg.SetID)(setID1), callback)
	require.NoError(t, err, "unable to cancel htlc")

	freshInvoice, err := db.LookupInvoice(ctxb, ref)
	require.Nil(t, err)
	dbInvoice = &freshInvoice

	// The amount paid should reflect that an invoice was cancelled.
	require.Equal(t, dbInvoice.AmtPaid, amt*2)

	// The HTLC and AMP state should also show that only one HTLC set is
	// left.
	invoice.State = invpkg.ContractOpen
	invoice.AmtPaid = 2 * amt
	invoice.SettleDate = dbInvoice.SettleDate

	htlc0 := models.CircuitKey{HtlcID: 0}
	htlc1 := models.CircuitKey{HtlcID: 1}
	htlc2 := models.CircuitKey{HtlcID: 2}

	invoice.Htlcs = map[models.CircuitKey]*invpkg.InvoiceHTLC{
		htlc0: makeAMPInvoiceHTLC(amt, *setID1, payHash, &preimage),
		htlc1: makeAMPInvoiceHTLC(amt, *setID2, payHash, &preimage),
		htlc2: makeAMPInvoiceHTLC(amt, *setID2, payHash, &preimage),
	}
	invoice.AMPState[*setID1] = invpkg.InvoiceStateAMP{
		State: invpkg.HtlcStateCanceled,
		InvoiceKeys: map[models.CircuitKey]struct{}{
			{HtlcID: 0}: {},
		},
	}
	invoice.AMPState[*setID2] = invpkg.InvoiceStateAMP{
		State:   invpkg.HtlcStateAccepted,
		AmtPaid: amt * 2,
		InvoiceKeys: map[models.CircuitKey]struct{}{
			{HtlcID: 1}: {},
			{HtlcID: 2}: {},
		},
	}

	invoice.Htlcs[htlc0].State = invpkg.HtlcStateCanceled
	invoice.Htlcs[htlc0].ResolveTime = time.Unix(1, 0)

	require.Equal(t, invoice, dbInvoice)

	// Next, we'll cancel the _other_ HTLCs active, but we'll do them one
	// by one.
	_, err = db.UpdateInvoice(ctxb, ref, (*invpkg.SetID)(setID2),
		func(invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc,
			error) {

			return &invpkg.InvoiceUpdateDesc{
				UpdateType: invpkg.CancelHTLCsUpdate,
				CancelHtlcs: map[models.CircuitKey]struct{}{
					{HtlcID: 1}: {},
				},
				SetID: (*invpkg.SetID)(setID2),
			}, nil
		})
	require.NoError(t, err, "unable to cancel htlc")

	freshInvoice, err = db.LookupInvoice(ctxb, ref)
	require.Nil(t, err)
	dbInvoice = &freshInvoice

	invoice.Htlcs[htlc1].State = invpkg.HtlcStateCanceled
	invoice.Htlcs[htlc1].ResolveTime = time.Unix(1, 0)
	invoice.AmtPaid = amt

	ampState := invoice.AMPState[*setID2]
	ampState.State = invpkg.HtlcStateCanceled
	ampState.AmtPaid = amt
	invoice.AMPState[*setID2] = ampState

	require.Equal(t, invoice, dbInvoice)

	callback = func(
		invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc, error) {

		return &invpkg.InvoiceUpdateDesc{
			UpdateType: invpkg.CancelHTLCsUpdate,
			CancelHtlcs: map[models.CircuitKey]struct{}{
				{HtlcID: 2}: {},
			},
			SetID: (*invpkg.SetID)(setID2),
		}, nil
	}

	// Now we'll cancel the final HTLC, which should cause all the active
	// HTLCs to transition to the cancelled state.
	_, err = db.UpdateInvoice(ctxb, ref, (*invpkg.SetID)(setID2), callback)
	require.NoError(t, err, "unable to cancel htlc")

	freshInvoice, err = db.LookupInvoice(ctxb, ref)
	require.Nil(t, err)
	dbInvoice = &freshInvoice

	ampState = invoice.AMPState[*setID2]
	ampState.AmtPaid = 0
	invoice.AMPState[*setID2] = ampState

	invoice.Htlcs[htlc2].State = invpkg.HtlcStateCanceled
	invoice.Htlcs[htlc2].ResolveTime = time.Unix(1, 0)
	invoice.AmtPaid = 0

	require.Equal(t, invoice, dbInvoice)
}

// testInvoiceTimeSeries tests that newly added invoices invoices, as well as
// settled invoices are added to the database are properly placed in the add
// or settle index which serves as an event time series.
func testInvoiceAddTimeSeries(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	ctxb := t.Context()
	_, err := db.InvoicesAddedSince(ctxb, 0)
	require.NoError(t, err)

	// We'll start off by creating 20 random invoices, and inserting them
	// into the database.
	const numInvoices = 20
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]invpkg.Invoice, numInvoices)
	for i := 0; i < len(invoices); i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		paymentHash := invoice.Terms.PaymentPreimage.Hash()
		_, err = db.AddInvoice(ctxb, invoice, paymentHash)
		if err != nil {
			t.Fatalf("unable to add invoice %v", err)
		}

		invoices[i] = *invoice
	}

	// With the invoices constructed, we'll now create a series of queries
	// that we'll use to assert expected return values of
	// InvoicesAddedSince.
	addQueries := []struct {
		sinceAddIndex uint64

		resp []invpkg.Invoice
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
		resp, err := db.InvoicesAddedSince(ctxb, query.sinceAddIndex)
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

	_, err = db.InvoicesSettledSince(ctxb, 0)
	require.NoError(t, err)

	var (
		settledInvoices []invpkg.Invoice
		settleIndex     uint64 = 1
		htlcID          uint64 = 0
	)
	// We'll now only settle the latter half of each of those invoices.
	for i := 10; i < len(invoices); i++ {
		invoice := &invoices[i]

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		ref := invpkg.InvoiceRefByHash(paymentHash)
		_, err := db.UpdateInvoice(
			ctxb, ref, nil, getUpdateInvoice(
				htlcID, invoice.Terms.Value,
			),
		)
		if err != nil {
			t.Fatalf("unable to settle invoice: %v", err)
		}

		// Create the settled invoice for the expectation set.
		settleTestInvoice(invoice, htlcID, settleIndex)
		settleIndex++
		htlcID++

		settledInvoices = append(settledInvoices, *invoice)
	}

	// We'll now prepare an additional set of queries to ensure the settle
	// time series has properly been maintained in the database.
	settleQueries := []struct {
		sinceSettleIndex uint64

		resp []invpkg.Invoice
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
		resp, err := db.InvoicesSettledSince(
			ctxb, query.sinceSettleIndex,
		)
		if err != nil {
			t.Fatalf("unable to query: %v", err)
		}

		require.Equal(
			t, len(query.resp), len(resp),
			fmt.Sprintf("test: #%v", i),
		)

		for j := 0; j < len(query.resp); j++ {
			require.Equal(t,
				query.resp[j], resp[j],
				fmt.Sprintf("test: #%v, item: #%v", i, j),
			)
		}
	}
}

// testSettleIndexAmpPayments tests that repeated settles of the same invoice
// end up properly adding entries to the settle index, and the
// InvoicesSettledSince will emit a "projected" version of the invoice w/
// _just_ that HTLC information.
func testSettleIndexAmpPayments(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// First, we'll make a sample invoice that'll be paid to several times
	// below.
	amt := lnwire.NewMSatFromSatoshis(1000)
	testInvoice, err := randInvoice(amt)
	require.Nil(t, err)
	testInvoice.Terms.Features = ampFeatures

	// Add the invoice to the DB, we use a dummy payment hash here but the
	// invoice will have a valid payment address set.
	ctxb := t.Context()
	preimage := *testInvoice.Terms.PaymentPreimage
	payHash := preimage.Hash()
	_, err = db.AddInvoice(ctxb, testInvoice, payHash)
	require.Nil(t, err)

	// Now that we have the invoice, we'll simulate 3 different HTLC sets
	// being attached to the invoice. These represent 3 different
	// concurrent payments.
	setID1 := &[32]byte{1}
	setID2 := &[32]byte{2}
	setID3 := &[32]byte{3}

	ref := invpkg.InvoiceRefByHashAndAddr(
		payHash, testInvoice.Terms.PaymentAddr,
	)
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID1),
		updateAcceptAMPHtlc(1, amt, setID1, true),
	)
	require.Nil(t, err)
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID2),
		updateAcceptAMPHtlc(2, amt, setID2, true),
	)
	require.Nil(t, err)
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID3),
		updateAcceptAMPHtlc(3, amt, setID3, true),
	)
	require.Nil(t, err)

	// Now that the invoices have been accepted, we'll exercise the
	// behavior of the LookupInvoice call that allows us to modify exactly
	// how we query for invoices.
	//
	// First, we'll query for the invoice with just the payment addr, but
	// specify no HTLcs are to be included.
	refNoHtlcs := invpkg.InvoiceRefByAddrBlankHtlc(
		testInvoice.Terms.PaymentAddr,
	)
	invoiceNoHTLCs, err := db.LookupInvoice(ctxb, refNoHtlcs)
	require.Nil(t, err)

	require.Equal(t, 0, len(invoiceNoHTLCs.Htlcs))

	// We'll now look up the HTLCs based on the individual setIDs added
	// above.
	for i, setID := range []*[32]byte{setID1, setID2, setID3} {
		refFiltered := invpkg.InvoiceRefBySetIDFiltered(*setID)
		invoiceFiltered, err := db.LookupInvoice(ctxb, refFiltered)
		require.Nil(t, err)

		// Only a single HTLC should be present.
		require.Equal(t, 1, len(invoiceFiltered.Htlcs))

		// The set ID for the HTLC should match the queried set ID.
		key := models.CircuitKey{HtlcID: uint64(i + 1)}
		htlc := invoiceFiltered.Htlcs[key]
		require.Equal(t, *setID, htlc.AMP.Record.SetID())

		// The HTLC should show that it's in the accepted state.
		require.Equal(t, htlc.State, invpkg.HtlcStateAccepted)
	}

	// Now that we know the invoices are in the proper state, we'll settle
	// them on by one in distinct updates.
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID1),
		getUpdateInvoiceAMPSettle(
			setID1, preimage, models.CircuitKey{HtlcID: 1},
		),
	)
	require.Nil(t, err)
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID2),
		getUpdateInvoiceAMPSettle(
			setID2, preimage, models.CircuitKey{HtlcID: 2},
		),
	)
	require.Nil(t, err)
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID3),
		getUpdateInvoiceAMPSettle(
			setID3, preimage, models.CircuitKey{HtlcID: 3},
		),
	)
	require.Nil(t, err)

	// Now that all the invoices have been settled, we'll ensure that the
	// settle index was updated properly by obtaining all the currently
	// settled invoices in the time series. We use a value of 1 here to
	// ensure we get _all_ the invoices back.
	settledInvoices, err := db.InvoicesSettledSince(ctxb, 1)
	require.Nil(t, err)

	// To get around the settle index quirk, we'll fetch the very first
	// invoice in the HTLC filtered mode and append it to the set of
	// invoices.
	firstInvoice, err := db.LookupInvoice(
		ctxb, invpkg.InvoiceRefBySetIDFiltered(*setID1),
	)
	require.Nil(t, err)
	settledInvoices = append(
		[]invpkg.Invoice{firstInvoice}, settledInvoices...,
	)

	// There should be 3 invoices settled, as we created 3 "sub-invoices"
	// above.
	numInvoices := 3
	require.Equal(t, numInvoices, len(settledInvoices))

	// Each invoice should match the set of invoices we settled above, and
	// the AMPState should be set accordingly.
	for i, settledInvoice := range settledInvoices {
		// Only one HTLC should be projected for this settled index.
		require.Equal(t, 1, len(settledInvoice.Htlcs))

		// The invoice should show up as settled, and match the settle
		// index increment.
		invSetID := &[32]byte{byte(i + 1)}
		subInvoiceState, ok := settledInvoice.AMPState[*invSetID]
		require.True(t, ok)

		require.Equal(t, subInvoiceState.State, invpkg.HtlcStateSettled)
		require.Equal(t, int(subInvoiceState.SettleIndex), i+1)

		invoiceKey := models.CircuitKey{HtlcID: uint64(i + 1)}
		_, keyFound := subInvoiceState.InvoiceKeys[invoiceKey]
		require.True(t, keyFound)
	}

	// If we attempt to look up the invoice by the payment addr, with all
	// the HTLCs, the main invoice should have 3 HTLCs present.
	refWithHtlcs := invpkg.InvoiceRefByAddr(testInvoice.Terms.PaymentAddr)
	invoiceWithHTLCs, err := db.LookupInvoice(ctxb, refWithHtlcs)
	require.Nil(t, err)
	require.Equal(t, numInvoices, len(invoiceWithHTLCs.Htlcs))

	// Finally, delete the invoice. If we query again, then nothing should
	// be found.
	err = db.DeleteInvoice(ctxb, []invpkg.InvoiceDeleteRef{
		{
			PayHash:  payHash,
			PayAddr:  &testInvoice.Terms.PaymentAddr,
			AddIndex: testInvoice.AddIndex,
		},
	})
	require.NoError(t, err)

	settledInvoices, err = db.InvoicesSettledSince(ctxb, 0)
	require.NoError(t, err)
	require.Len(t, settledInvoices, 0)
}

// testFetchPendingInvoices tests that we can fetch all pending invoices from
// the database using the FetchPendingInvoices method.
func testFetchPendingInvoices(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	ctxb := t.Context()

	// Make sure that fetching pending invoices from an empty database
	// returns an empty result and no errors.
	pending, err := db.FetchPendingInvoices(ctxb)
	require.NoError(t, err)
	require.Empty(t, pending)

	const numInvoices = 20
	var (
		settleIndex uint64 = 1
		htlcID      uint64 = 0
	)

	pendingInvoices := make(map[lntypes.Hash]invpkg.Invoice)

	for i := 1; i <= numInvoices; i++ {
		amt := lnwire.MilliSatoshi(i * 1000)
		invoice, err := randInvoice(amt)
		require.NoError(t, err)

		invoice.CreationDate = invoice.CreationDate.Add(
			time.Duration(i-1) * time.Second,
		)

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		_, err = db.AddInvoice(ctxb, invoice, paymentHash)
		require.NoError(t, err)

		// Settle every second invoice.
		if i%2 == 0 {
			pendingInvoices[paymentHash] = *invoice
			continue
		}

		ref := invpkg.InvoiceRefByHash(paymentHash)
		_, err = db.UpdateInvoice(
			ctxb, ref, nil, getUpdateInvoice(htlcID, amt),
		)
		require.NoError(t, err)

		settleTestInvoice(invoice, htlcID, settleIndex)
		settleIndex++
		htlcID++
	}

	// Fetch all pending invoices.
	pending, err = db.FetchPendingInvoices(ctxb)
	require.NoError(t, err)
	require.Equal(t, pendingInvoices, pending)
}

// testDuplicateSettleInvoice tests that if we add a new invoice and settle it
// twice, then the second time we also receive the invoice that we settled as a
// return argument.
func testDuplicateSettleInvoice(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// We'll start out by creating an invoice and writing it to the DB.
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoice, err := randInvoice(amt)
	require.NoError(t, err, "unable to create invoice")

	payHash := invoice.Terms.PaymentPreimage.Hash()

	ctxb := t.Context()
	if _, err := db.AddInvoice(ctxb, invoice, payHash); err != nil {
		t.Fatalf("unable to add invoice %v", err)
	}

	// With the invoice in the DB, we'll now attempt to settle the invoice.
	ref := invpkg.InvoiceRefByHash(payHash)
	dbInvoice, err := db.UpdateInvoice(
		ctxb, ref, nil, getUpdateInvoice(0, amt),
	)
	require.NoError(t, err, "unable to settle invoice")

	// We'll update what we expect the settle invoice to be so that our
	// comparison below has the correct assumption.
	invoice.SettleIndex = 1
	invoice.State = invpkg.ContractSettled
	invoice.AmtPaid = amt
	invoice.SettleDate = dbInvoice.SettleDate
	invoice.Htlcs = map[models.CircuitKey]*invpkg.InvoiceHTLC{
		{}: {
			Amt:           amt,
			AcceptTime:    time.Unix(1, 0),
			ResolveTime:   time.Unix(1, 0),
			State:         invpkg.HtlcStateSettled,
			CustomRecords: make(record.CustomSet),
		},
	}

	// We should get back the exact same invoice that we just inserted.
	require.Equal(t, invoice, dbInvoice, "wrong invoice after settle")

	// If we try to settle the invoice again, then we should get the very
	// same invoice back, but with an error this time.
	dbInvoice, err = db.UpdateInvoice(
		ctxb, ref, nil, getUpdateInvoice(1, amt),
	)
	require.ErrorIs(t, err, invpkg.ErrInvoiceAlreadySettled)

	if dbInvoice == nil {
		t.Fatalf("invoice from db is nil after settle!")
	}

	invoice.SettleDate = dbInvoice.SettleDate
	require.Equal(
		t, invoice, dbInvoice, "wrong invoice after second settle",
	)
}

// testQueryInvoices ensures that we can properly query the invoice database for
// invoices using different types of queries.
func testQueryInvoices(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// To begin the test, we'll add 50 invoices to the database. We'll
	// assume that the index of the invoice within the database is the same
	// as the amount of the invoice itself.
	const numInvoices = 50
	var (
		settleIndex     uint64 = 1
		htlcID          uint64 = 0
		invoices        []invpkg.Invoice
		pendingInvoices []invpkg.Invoice
	)

	ctxb := t.Context()
	for i := 1; i <= numInvoices; i++ {
		amt := lnwire.MilliSatoshi(i)
		invoice, err := randInvoice(amt)
		invoice.CreationDate = invoice.CreationDate.Add(
			time.Duration(i-1) * time.Second,
		)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		if _, err := db.AddInvoice(
			ctxb, invoice, paymentHash,
		); err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		// We'll only settle half of all invoices created.
		if i%2 == 0 {
			ref := invpkg.InvoiceRefByHash(paymentHash)
			_, err := db.UpdateInvoice(
				ctxb, ref, nil, getUpdateInvoice(htlcID, amt),
			)
			if err != nil {
				t.Fatalf("unable to settle invoice: %v", err)
			}

			// Create the settled invoice for the expectation set.
			settleTestInvoice(invoice, htlcID, settleIndex)
			settleIndex++
			htlcID++
		} else {
			pendingInvoices = append(pendingInvoices, *invoice)
		}

		invoices = append(invoices, *invoice)
	}

	// The test will consist of several queries along with their respective
	// expected response. Each query response should match its expected one.
	testCases := []struct {
		query    invpkg.InvoiceQuery
		expected []invpkg.Invoice
	}{
		// Fetch all invoices with a single query.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices: numInvoices,
			},
			expected: invoices,
		},
		// Fetch all invoices with a single query, reversed.
		{
			query: invpkg.InvoiceQuery{
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices,
		},
		// Fetch the first 25 invoices.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices: numInvoices / 2,
			},
			expected: invoices[:numInvoices/2],
		},
		// Fetch the first 10 invoices, but this time iterating
		// backwards.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    11,
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices[:10],
		},
		// Fetch the last 40 invoices.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    10,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices[10:],
		},
		// Fetch all but the first invoice.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    1,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices[1:],
		},
		// Fetch one invoice, reversed, with index offset 3. This
		// should give us the second invoice in the array.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    3,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[1:2],
		},
		// Same as above, at index 2.
		{
			query: invpkg.InvoiceQuery{
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
			query: invpkg.InvoiceQuery{
				IndexOffset:    1,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: nil,
		},
		// Same as above, but don't restrict the number of invoices to
		// 1.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    1,
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			expected: nil,
		},
		// Fetch one invoice, reversed, with no offset set. We expect
		// the last invoice in the response.
		{
			query: invpkg.InvoiceQuery{
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[numInvoices-1:],
		},
		// Fetch one invoice, reversed, the offset set at numInvoices+1.
		// We expect this to return the last invoice.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    numInvoices + 1,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[numInvoices-1:],
		},
		// Same as above, at offset numInvoices.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    numInvoices,
				Reversed:       true,
				NumMaxInvoices: 1,
			},
			expected: invoices[numInvoices-2 : numInvoices-1],
		},
		// Fetch one invoice, at no offset (same as offset 0). We
		// expect the first invoice only in the response.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices: 1,
			},
			expected: invoices[:1],
		},
		// Same as above, at offset 1.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    1,
				NumMaxInvoices: 1,
			},
			expected: invoices[1:2],
		},
		// Same as above, at offset 2.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    2,
				NumMaxInvoices: 1,
			},
			expected: invoices[2:3],
		},
		// Same as above, at offset numInvoices-1. Expect the last
		// invoice to be returned.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    numInvoices - 1,
				NumMaxInvoices: 1,
			},
			expected: invoices[numInvoices-1:],
		},
		// Same as above, at offset numInvoices. No invoices should be
		// returned, as there are no invoices after this offset.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:    numInvoices,
				NumMaxInvoices: 1,
			},
			expected: nil,
		},
		// Fetch all pending invoices with a single query.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:    true,
				NumMaxInvoices: numInvoices,
			},
			expected: pendingInvoices,
		},
		// Fetch the first 12 pending invoices.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:    true,
				NumMaxInvoices: numInvoices / 4,
			},
			expected: pendingInvoices[:len(pendingInvoices)/2],
		},
		// Fetch the first 5 pending invoices, but this time iterating
		// backwards.
		{
			query: invpkg.InvoiceQuery{
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
			query: invpkg.InvoiceQuery{
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
			query: invpkg.InvoiceQuery{
				IndexOffset:    numInvoices * 2,
				PendingOnly:    false,
				Reversed:       true,
				NumMaxInvoices: numInvoices,
			},
			expected: invoices,
		},
		// Fetch invoices <= 25 by creation date.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices:  numInvoices,
				CreationDateEnd: 25,
			},
			expected: invoices[:25],
		},
		// Fetch invoices >= 26 creation date.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices:    numInvoices,
				CreationDateStart: 26,
			},
			expected: invoices[25:],
		},
		// Fetch pending invoices <= 25 by creation date.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:     true,
				NumMaxInvoices:  numInvoices,
				CreationDateEnd: 25,
			},
			expected: pendingInvoices[:13],
		},
		// Fetch pending invoices >= 26 creation date.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:       true,
				NumMaxInvoices:    numInvoices,
				CreationDateStart: 26,
			},
			expected: pendingInvoices[13:],
		},
		// Fetch pending invoices with offset and end creation date.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:     20,
				NumMaxInvoices:  numInvoices,
				CreationDateEnd: 30,
			},
			// Since we're skipping to invoice 20 and iterating
			// to invoice 30, we'll expect those invoices.
			expected: invoices[20:30],
		},
		// Fetch pending invoices with offset and start creation date
		// in reversed order.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:       21,
				Reversed:          true,
				NumMaxInvoices:    numInvoices,
				CreationDateStart: 11,
			},
			// Since we're skipping to invoice 20 and iterating
			// backward to invoice 10, we'll expect those invoices.
			expected: invoices[10:20],
		},
		// Fetch invoices with start and end creation date.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices:    numInvoices,
				CreationDateStart: 11,
				CreationDateEnd:   20,
			},
			expected: invoices[10:20],
		},
		// Fetch pending invoices with start and end creation date.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:       true,
				NumMaxInvoices:    numInvoices,
				CreationDateStart: 11,
				CreationDateEnd:   20,
			},
			expected: pendingInvoices[5:10],
		},
		// Fetch invoices with start and end creation date in reverse
		// order.
		{
			query: invpkg.InvoiceQuery{
				Reversed:          true,
				NumMaxInvoices:    numInvoices,
				CreationDateStart: 11,
				CreationDateEnd:   20,
			},
			expected: invoices[10:20],
		},
		// Fetch pending invoices with start and end creation date in
		// reverse order.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:       true,
				Reversed:          true,
				NumMaxInvoices:    numInvoices,
				CreationDateStart: 11,
				CreationDateEnd:   20,
			},
			expected: pendingInvoices[5:10],
		},
		// Fetch invoices with a start date greater than end date
		// should result in an empty slice.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices:    numInvoices,
				CreationDateStart: 20,
				CreationDateEnd:   11,
			},
			expected: nil,
		},
	}

	for i, testCase := range testCases {
		response, err := db.QueryInvoices(ctxb, testCase.query)
		if err != nil {
			t.Fatalf("unable to query invoice database: %v", err)
		}

		require.Equal(t, len(testCase.expected), len(response.Invoices),
			fmt.Sprintf("test: #%v", i))

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
func getUpdateInvoice(htlcID uint64,
	amt lnwire.MilliSatoshi) invpkg.InvoiceUpdateCallback {

	return func(invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc,
		error) {

		if invoice.State == invpkg.ContractSettled {
			return nil, invpkg.ErrInvoiceAlreadySettled
		}

		noRecords := make(record.CustomSet)
		htlcs := map[models.CircuitKey]*invpkg.HtlcAcceptDesc{
			{HtlcID: htlcID}: {
				Amt:           amt,
				CustomRecords: noRecords,
			},
		}
		update := &invpkg.InvoiceUpdateDesc{
			UpdateType: invpkg.AddHTLCsUpdate,
			State: &invpkg.InvoiceStateUpdateDesc{
				Preimage: invoice.Terms.PaymentPreimage,
				NewState: invpkg.ContractSettled,
			},
			AddHtlcs: htlcs,
		}

		return update, nil
	}
}

// testCustomRecords tests that custom records are properly recorded in the
// invoice database.
func testCustomRecords(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	preimage := lntypes.Preimage{1}
	paymentHash := preimage.Hash()

	testInvoice := &invpkg.Invoice{
		Htlcs: map[models.CircuitKey]*invpkg.InvoiceHTLC{},
		Terms: invpkg.ContractTerm{
			Value:           lnwire.NewMSatFromSatoshis(10000),
			Features:        emptyFeatures,
			PaymentPreimage: &preimage,
		},
	}

	ctxb := t.Context()
	if _, err := db.AddInvoice(ctxb, testInvoice, paymentHash); err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Accept an htlc with custom records on this invoice.
	key := models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 4,
	}

	records := record.CustomSet{
		100000: []byte{},
		100001: []byte{1, 2},
	}

	ref := invpkg.InvoiceRefByHash(paymentHash)
	callback := func(invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc,
		error) {

		htlcs := map[models.CircuitKey]*invpkg.HtlcAcceptDesc{
			key: {
				Amt:           500,
				CustomRecords: records,
			},
		}

		return &invpkg.InvoiceUpdateDesc{
			AddHtlcs:   htlcs,
			UpdateType: invpkg.AddHTLCsUpdate,
		}, nil
	}

	_, err := db.UpdateInvoice(ctxb, ref, nil, callback)
	require.NoError(t, err, "unable to add invoice htlc")

	// Retrieve the invoice from that database and verify that the custom
	// records are present.
	dbInvoice, err := db.LookupInvoice(ctxb, ref)
	require.NoError(t, err, "unable to lookup invoice")

	if len(dbInvoice.Htlcs) != 1 {
		t.Fatalf("expected the htlc to be added")
	}

	require.Equal(t,
		records, dbInvoice.Htlcs[key].CustomRecords,
		"invalid custom records",
	)
}

// testInvoiceHtlcAMPFields asserts that the set id and preimage fields are
// properly recorded when updating an invoice.
func testInvoiceHtlcAMPFields(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()

	t.Run("amp", func(t *testing.T) {
		t.Parallel()
		testInvoiceHtlcAMPFieldsImpl(t, true, makeDB)
	})
	t.Run("no amp", func(t *testing.T) {
		t.Parallel()
		testInvoiceHtlcAMPFieldsImpl(t, false, makeDB)
	})
}

func testInvoiceHtlcAMPFieldsImpl(t *testing.T, isAMP bool,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	db := makeDB(t)

	testInvoice, err := randInvoice(1000)
	require.Nil(t, err)

	if isAMP {
		testInvoice.Terms.Features = ampFeatures
	}

	ctxb := t.Context()
	payHash := testInvoice.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, testInvoice, payHash)
	require.Nil(t, err)

	// Accept an htlc with custom records on this invoice.
	key := models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 4,
	}
	records := make(map[uint64][]byte)

	var ampData *invpkg.InvoiceHtlcAMPData
	if isAMP {
		amp := record.NewAMP([32]byte{1}, [32]byte{2}, 3)
		preimage := &lntypes.Preimage{4}

		ampData = &invpkg.InvoiceHtlcAMPData{
			Record:   *amp,
			Hash:     preimage.Hash(),
			Preimage: preimage,
		}
	}

	callback := func(invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc,
		error) {

		htlcs := map[models.CircuitKey]*invpkg.HtlcAcceptDesc{
			key: {
				Amt:           500,
				AMP:           ampData,
				CustomRecords: records,
			},
		}

		return &invpkg.InvoiceUpdateDesc{
			AddHtlcs:   htlcs,
			UpdateType: invpkg.AddHTLCsUpdate,
		}, nil
	}

	ref := invpkg.InvoiceRefByHash(payHash)
	_, err = db.UpdateInvoice(ctxb, ref, nil, callback)
	require.Nil(t, err)

	// Retrieve the invoice from that database and verify that the AMP
	// fields are as expected.
	dbInvoice, err := db.LookupInvoice(ctxb, ref)
	require.Nil(t, err)

	require.Equal(t, 1, len(dbInvoice.Htlcs))
	require.Equal(t, ampData, dbInvoice.Htlcs[key].AMP)
}

// TestInvoiceRef asserts that the proper identifiers are returned from an
// InvoiceRef depending on the constructor used.
func TestInvoiceRef(t *testing.T) {
	t.Parallel()

	payHash := lntypes.Hash{0x01}
	payAddr := [32]byte{0x02}
	setID := [32]byte{0x03}

	// An InvoiceRef by hash should return the provided hash and a nil
	// payment addr.
	refByHash := invpkg.InvoiceRefByHash(payHash)
	require.Equal(t, &payHash, refByHash.PayHash())
	require.Equal(t, (*[32]byte)(nil), refByHash.PayAddr())
	require.Equal(t, (*[32]byte)(nil), refByHash.SetID())

	// An InvoiceRef by hash and addr should return the payment hash and
	// payment addr passed to the constructor.
	refByHashAndAddr := invpkg.InvoiceRefByHashAndAddr(payHash, payAddr)
	require.Equal(t, &payHash, refByHashAndAddr.PayHash())
	require.Equal(t, &payAddr, refByHashAndAddr.PayAddr())
	require.Equal(t, (*[32]byte)(nil), refByHashAndAddr.SetID())

	// An InvoiceRef by set id should return an empty pay hash, a nil pay
	// addr, and a reference to the given set id.
	refBySetID := invpkg.InvoiceRefBySetID(setID)
	require.Equal(t, (*lntypes.Hash)(nil), refBySetID.PayHash())
	require.Equal(t, (*[32]byte)(nil), refBySetID.PayAddr())
	require.Equal(t, &setID, refBySetID.SetID())

	// An InvoiceRef by pay addr should only return a pay addr, but nil for
	// pay hash and set id.
	refByAddr := invpkg.InvoiceRefByAddr(payAddr)
	require.Equal(t, (*lntypes.Hash)(nil), refByAddr.PayHash())
	require.Equal(t, &payAddr, refByAddr.PayAddr())
	require.Equal(t, (*[32]byte)(nil), refByAddr.SetID())
}

// TestHTLCSet asserts that HTLCSet returns the proper set of accepted HTLCs
// that can be considered for settlement. It asserts that MPP and AMP HTLCs do
// not comingle, and also that HTLCs with disjoint set ids appear in different
// sets.
func TestHTLCSet(t *testing.T) {
	t.Parallel()

	inv := &invpkg.Invoice{
		Htlcs: make(map[models.CircuitKey]*invpkg.InvoiceHTLC),
	}

	// Construct two distinct set id's, in this test we'll also track the
	// nil set id as a third group.
	setID1 := &[32]byte{1}
	setID2 := &[32]byte{2}

	// Create the expected htlc sets for each group, these will be updated
	// as the invoice is modified.

	expSetNil := make(map[models.CircuitKey]*invpkg.InvoiceHTLC)
	expSet1 := make(map[models.CircuitKey]*invpkg.InvoiceHTLC)
	expSet2 := make(map[models.CircuitKey]*invpkg.InvoiceHTLC)

	checkHTLCSets := func() {
		require.Equal(
			t, expSetNil,
			inv.HTLCSet(nil, invpkg.HtlcStateAccepted),
		)
		require.Equal(
			t, expSet1,
			inv.HTLCSet(setID1, invpkg.HtlcStateAccepted),
		)
		require.Equal(
			t, expSet2,
			inv.HTLCSet(setID2, invpkg.HtlcStateAccepted),
		)
	}

	// All HTLC sets should be empty initially.
	checkHTLCSets()

	// Add the following sequence of HTLCs to the invoice, sanity checking
	// all three HTLC sets after each transition. This sequence asserts:
	//   - both nil and non-nil set ids can have multiple htlcs.
	//   - there may be distinct htlc sets with non-nil set ids.
	//   - only accepted htlcs are returned as part of the set.
	htlcs := []struct {
		setID *[32]byte
		state invpkg.HtlcState
	}{
		{nil, invpkg.HtlcStateAccepted},
		{nil, invpkg.HtlcStateAccepted},
		{setID1, invpkg.HtlcStateAccepted},
		{setID1, invpkg.HtlcStateAccepted},
		{setID2, invpkg.HtlcStateAccepted},
		{setID2, invpkg.HtlcStateAccepted},
		{nil, invpkg.HtlcStateCanceled},
		{setID1, invpkg.HtlcStateCanceled},
		{setID2, invpkg.HtlcStateCanceled},
		{nil, invpkg.HtlcStateSettled},
		{setID1, invpkg.HtlcStateSettled},
		{setID2, invpkg.HtlcStateSettled},
	}

	for i, h := range htlcs {
		var ampData *invpkg.InvoiceHtlcAMPData
		if h.setID != nil {
			ampData = &invpkg.InvoiceHtlcAMPData{
				Record: *record.NewAMP(
					[32]byte{0}, *h.setID, 0,
				),
			}
		}

		// Add the HTLC to the invoice's set of HTLCs.
		key := models.CircuitKey{HtlcID: uint64(i)}
		htlc := &invpkg.InvoiceHTLC{
			AMP:   ampData,
			State: h.state,
		}
		inv.Htlcs[key] = htlc

		// Update our expected htlc set if the htlc is accepted,
		// otherwise it shouldn't be reflected.
		if h.state == invpkg.HtlcStateAccepted {
			switch h.setID {
			case nil:
				expSetNil[key] = htlc
			case setID1:
				expSet1[key] = htlc
			case setID2:
				expSet2[key] = htlc
			default:
				t.Fatalf("unexpected set id")
			}
		}

		checkHTLCSets()
	}
}

// testAddInvoiceWithHTLCs asserts that you can't insert an invoice that already
// has HTLCs.
func testAddInvoiceWithHTLCs(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	testInvoice, err := randInvoice(1000)
	require.Nil(t, err)

	key := models.CircuitKey{HtlcID: 1}
	testInvoice.Htlcs[key] = &invpkg.InvoiceHTLC{}

	payHash := testInvoice.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(t.Context(), testInvoice, payHash)
	require.Equal(t, invpkg.ErrInvoiceHasHtlcs, err)
}

// testSetIDIndex asserts that the set id index properly adds new invoices as we
// accept HTLCs, that they can be queried by their set id after accepting, and
// that invoices with duplicate set ids are disallowed.
func testSetIDIndex(t *testing.T, makeDB func(t *testing.T) invpkg.InvoiceDB) {
	t.Parallel()
	db := makeDB(t)

	// We'll start out by creating an invoice and writing it to the DB.
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoice, err := randInvoice(amt)
	require.Nil(t, err)

	// Set AMP-specific features so that we can settle with HTLC-level
	// preimages.
	invoice.Terms.Features = ampFeatures

	ctxb := t.Context()
	preimage := *invoice.Terms.PaymentPreimage
	payHash := preimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice, payHash)
	require.Nil(t, err)

	setID := &[32]byte{1}

	// Update the invoice with an accepted HTLC that also accepts the
	// invoice.
	ref := invpkg.InvoiceRefByHashAndAddr(
		payHash, invoice.Terms.PaymentAddr,
	)
	dbInvoice, err := db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID),
		updateAcceptAMPHtlc(0, amt, setID, true),
	)
	require.Nil(t, err)

	// We'll update what we expect the accepted invoice to be so that our
	// comparison below has the correct assumption.
	invoice.State = invpkg.ContractOpen
	invoice.AmtPaid = amt
	invoice.SettleDate = dbInvoice.SettleDate
	htlc0 := models.CircuitKey{HtlcID: 0}
	invoice.Htlcs = map[models.CircuitKey]*invpkg.InvoiceHTLC{
		htlc0: makeAMPInvoiceHTLC(amt, *setID, payHash, &preimage),
	}
	invoice.AMPState = map[invpkg.SetID]invpkg.InvoiceStateAMP{}
	invoice.AMPState[*setID] = invpkg.InvoiceStateAMP{
		State:   invpkg.HtlcStateAccepted,
		AmtPaid: amt,
		InvoiceKeys: map[models.CircuitKey]struct{}{
			htlc0: {},
		},
	}

	// We should get back the exact same invoice that we just inserted.
	require.Equal(t, invoice, dbInvoice)

	// Now lookup the invoice by set id and see that we get the same one.
	refBySetID := invpkg.InvoiceRefBySetID(*setID)
	dbInvoiceBySetID, err := db.LookupInvoice(ctxb, refBySetID)
	require.Nil(t, err)
	require.Equal(t, invoice, &dbInvoiceBySetID)

	// Trying to accept an HTLC to a different invoice, but using the same
	// set id should fail.
	invoice2, err := randInvoice(amt)
	require.Nil(t, err)

	// Set AMP-specific features so that we can settle with HTLC-level
	// preimages.
	invoice2.Terms.Features = ampFeatures

	payHash2 := invoice2.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice2, payHash2)
	require.Nil(t, err)

	ref2 := invpkg.InvoiceRefByHashAndAddr(
		payHash2, invoice2.Terms.PaymentAddr,
	)
	_, err = db.UpdateInvoice(
		ctxb, ref2, (*invpkg.SetID)(setID),
		// Note: we need a unique HTLC ID here otherwise the update will
		// be rejected as a duplicate (due to SQL unique constraint
		// violation).
		updateAcceptAMPHtlc(55, amt, setID, true),
	)
	require.Equal(t, invpkg.ErrDuplicateSetID{SetID: *setID}, err)

	// Now, begin constructing a second htlc set under a different set id.
	// This set will contain two distinct HTLCs.
	setID2 := &[32]byte{2}

	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID2),
		updateAcceptAMPHtlc(1, amt, setID2, false),
	)
	require.Nil(t, err)
	dbInvoice, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID2),
		updateAcceptAMPHtlc(2, amt, setID2, false),
	)
	require.Nil(t, err)

	// We'll update what we expect the settle invoice to be so that our
	// comparison below has the correct assumption.
	invoice.State = invpkg.ContractOpen
	invoice.AmtPaid += 2 * amt
	invoice.SettleDate = dbInvoice.SettleDate
	htlc1 := models.CircuitKey{HtlcID: 1}
	htlc2 := models.CircuitKey{HtlcID: 2}
	invoice.Htlcs = map[models.CircuitKey]*invpkg.InvoiceHTLC{
		htlc0: makeAMPInvoiceHTLC(amt, *setID, payHash, &preimage),
		htlc1: makeAMPInvoiceHTLC(amt, *setID2, payHash, nil),
		htlc2: makeAMPInvoiceHTLC(amt, *setID2, payHash, nil),
	}
	invoice.AMPState[*setID] = invpkg.InvoiceStateAMP{
		State:   invpkg.HtlcStateAccepted,
		AmtPaid: amt,
		InvoiceKeys: map[models.CircuitKey]struct{}{
			htlc0: {},
		},
	}
	invoice.AMPState[*setID2] = invpkg.InvoiceStateAMP{
		State:   invpkg.HtlcStateAccepted,
		AmtPaid: amt * 2,
		InvoiceKeys: map[models.CircuitKey]struct{}{
			htlc1: {},
			htlc2: {},
		},
	}

	// Since UpdateInvoice will only return the sub-set of updated HTLcs,
	// we'll query again to ensure we get the full set of HTLCs returned.
	freshInvoice, err := db.LookupInvoice(ctxb, ref)
	require.Nil(t, err)
	dbInvoice = &freshInvoice

	// We should get back the exact same invoice that we just inserted.
	require.Equal(t, invoice, dbInvoice)

	// Now lookup the invoice by second set id and see that we get the same
	// index, including the htlcs under the first set id.
	refBySetID = invpkg.InvoiceRefBySetID(*setID2)
	dbInvoiceBySetID, err = db.LookupInvoice(ctxb, refBySetID)
	require.Nil(t, err)
	require.Equal(t, invoice, &dbInvoiceBySetID)

	// Now attempt to settle a non-existent HTLC set, this set ID is the
	// zero setID so it isn't used for anything internally.
	_, err = db.UpdateInvoice(
		ctxb, ref, nil,
		getUpdateInvoiceAMPSettle(
			&[32]byte{}, [32]byte{},
			models.CircuitKey{HtlcID: 99},
		),
	)
	require.Equal(t, invpkg.ErrEmptyHTLCSet, err)

	// Now settle the first htlc set. The existing HTLCs should remain in
	// the accepted state and shouldn't be canceled, since we permit an
	// invoice to be settled multiple times.
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID),
		getUpdateInvoiceAMPSettle(
			setID, preimage, models.CircuitKey{HtlcID: 0},
		),
	)
	require.Nil(t, err)

	freshInvoice, err = db.LookupInvoice(ctxb, ref)
	require.Nil(t, err)
	dbInvoice = &freshInvoice

	invoice.State = invpkg.ContractOpen

	// The amount paid should reflect that we have 3 present HTLCs, each
	// with an amount of the original invoice.
	invoice.AmtPaid = amt * 3

	ampState := invoice.AMPState[*setID]
	ampState.State = invpkg.HtlcStateSettled
	ampState.SettleDate = testNow
	ampState.SettleIndex = 1

	invoice.AMPState[*setID] = ampState

	invoice.Htlcs[htlc0].State = invpkg.HtlcStateSettled
	invoice.Htlcs[htlc0].ResolveTime = time.Unix(1, 0)

	require.Equal(t, invoice, dbInvoice)

	// If we try to settle the same set ID again, then we should get an
	// error, as it's already been settled.
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID),
		getUpdateInvoiceAMPSettle(
			setID, preimage, models.CircuitKey{HtlcID: 0},
		),
	)
	require.Equal(t, invpkg.ErrEmptyHTLCSet, err)

	// Next, let's attempt to settle the other active set ID for this
	// invoice. This will allow us to exercise the case where we go to
	// settle an invoice with a new setID after one has already been fully
	// settled.
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID2),
		getUpdateInvoiceAMPSettle(
			setID2, preimage, models.CircuitKey{HtlcID: 1},
			models.CircuitKey{HtlcID: 2},
		),
	)
	require.Nil(t, err)

	freshInvoice, err = db.LookupInvoice(ctxb, ref)
	require.Nil(t, err)
	dbInvoice = &freshInvoice

	// Now the rest of the HTLCs should show as fully settled.
	ampState = invoice.AMPState[*setID2]
	ampState.State = invpkg.HtlcStateSettled
	ampState.SettleDate = testNow
	ampState.SettleIndex = 2

	invoice.AMPState[*setID2] = ampState

	invoice.Htlcs[htlc1].State = invpkg.HtlcStateSettled
	invoice.Htlcs[htlc1].ResolveTime = time.Unix(1, 0)
	invoice.Htlcs[htlc1].AMP.Preimage = &preimage

	invoice.Htlcs[htlc2].State = invpkg.HtlcStateSettled
	invoice.Htlcs[htlc2].ResolveTime = time.Unix(1, 0)
	invoice.Htlcs[htlc2].AMP.Preimage = &preimage

	require.Equal(t, invoice, dbInvoice)

	// Lastly, querying for an unknown set id should fail.
	refUnknownSetID := invpkg.InvoiceRefBySetID([32]byte{})
	_, err = db.LookupInvoice(ctxb, refUnknownSetID)
	require.Equal(t, invpkg.ErrInvoiceNotFound, err)
}

func makeAMPInvoiceHTLC(amt lnwire.MilliSatoshi, setID [32]byte,
	hash lntypes.Hash, preimage *lntypes.Preimage) *invpkg.InvoiceHTLC {

	return &invpkg.InvoiceHTLC{
		Amt:           amt,
		AcceptTime:    testNow,
		ResolveTime:   time.Time{},
		State:         invpkg.HtlcStateAccepted,
		CustomRecords: make(record.CustomSet),
		AMP: &invpkg.InvoiceHtlcAMPData{
			Record:   *record.NewAMP([32]byte{}, setID, 0),
			Hash:     hash,
			Preimage: preimage,
		},
	}
}

// updateAcceptAMPHtlc returns an invoice update callback that, when called,
// settles the invoice with the given amount.
func updateAcceptAMPHtlc(id uint64, amt lnwire.MilliSatoshi,
	setID *[32]byte, accept bool) invpkg.InvoiceUpdateCallback {

	return func(invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc,
		error) {

		if invoice.State == invpkg.ContractSettled {
			return nil, invpkg.ErrInvoiceAlreadySettled
		}

		noRecords := make(record.CustomSet)

		var (
			state    *invpkg.InvoiceStateUpdateDesc
			preimage *lntypes.Preimage
		)
		if accept {
			state = &invpkg.InvoiceStateUpdateDesc{
				NewState: invpkg.ContractAccepted,
				SetID:    setID,
			}
			pre := *invoice.Terms.PaymentPreimage
			preimage = &pre
		}

		ampData := &invpkg.InvoiceHtlcAMPData{
			Record:   *record.NewAMP([32]byte{}, *setID, 0),
			Hash:     invoice.Terms.PaymentPreimage.Hash(),
			Preimage: preimage,
		}

		htlcs := map[models.CircuitKey]*invpkg.HtlcAcceptDesc{
			{HtlcID: id}: {
				Amt:           amt,
				CustomRecords: noRecords,
				AMP:           ampData,
			},
		}

		update := &invpkg.InvoiceUpdateDesc{
			State:      state,
			AddHtlcs:   htlcs,
			UpdateType: invpkg.AddHTLCsUpdate,
		}

		return update, nil
	}
}

func getUpdateInvoiceAMPSettle(setID *[32]byte, preimage [32]byte,
	circuitKeys ...models.CircuitKey) invpkg.InvoiceUpdateCallback {

	return func(invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc,
		error) {

		if invoice.State == invpkg.ContractSettled {
			return nil, invpkg.ErrInvoiceAlreadySettled
		}

		preImageSet := make(map[models.CircuitKey]lntypes.Preimage)
		for _, key := range circuitKeys {
			preImageSet[key] = preimage
		}

		update := &invpkg.InvoiceUpdateDesc{
			// TODO(positiveblue): this would be an invalid update
			// because tires to settle an AMP invoice without adding
			// any new htlc.
			UpdateType: invpkg.AddHTLCsUpdate,
			State: &invpkg.InvoiceStateUpdateDesc{
				Preimage:      nil,
				NewState:      invpkg.ContractSettled,
				SetID:         setID,
				HTLCPreimages: preImageSet,
			},
		}

		return update, nil
	}
}

// testUnexpectedInvoicePreimage asserts that legacy or MPP invoices cannot be
// settled when referenced by payment address only. Since regular or MPP
// payments do not store the payment hash explicitly (it is stored in the
// index), this enforces that they can only be updated using a InvoiceRefByHash
// or InvoiceRefByHashOrAddr.
func testUnexpectedInvoicePreimage(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	invoice, err := randInvoice(lnwire.MilliSatoshi(100))
	require.NoError(t, err)

	ctxb := t.Context()

	// Add a random invoice indexed by payment hash and payment addr.
	paymentHash := invoice.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(ctxb, invoice, paymentHash)
	require.NoError(t, err)

	// Attempt to update the invoice by pay addr only. This will fail since,
	// in order to settle an MPP invoice, the InvoiceRef must present a
	// payment hash against which to validate the preimage.
	_, err = db.UpdateInvoice(
		ctxb, invpkg.InvoiceRefByAddr(invoice.Terms.PaymentAddr), nil,
		getUpdateInvoice(0, invoice.Terms.Value),
	)

	// Assert that we get ErrUnexpectedInvoicePreimage.
	require.Error(t, invpkg.ErrUnexpectedInvoicePreimage, err)
}

type updateHTLCPreimageTestCase struct {
	name               string
	settleSamePreimage bool
	expError           error
}

// testUpdateHTLCPreimages asserts various properties of setting HTLC-level
// preimages on invoice state transitions.
func testUpdateHTLCPreimages(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()

	tests := []updateHTLCPreimageTestCase{
		{
			name:               "same preimage on settle",
			settleSamePreimage: true,
			expError:           nil,
		},
		{
			name:               "diff preimage on settle",
			settleSamePreimage: false,
			expError:           invpkg.ErrHTLCPreimageAlreadyExists,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testUpdateHTLCPreimagesImpl(t, test, makeDB)
		})
	}
}

func testUpdateHTLCPreimagesImpl(t *testing.T, test updateHTLCPreimageTestCase,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	db := makeDB(t)
	// We'll start out by creating an invoice and writing it to the DB.
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoice, err := randInvoice(amt)
	require.Nil(t, err)

	preimage := *invoice.Terms.PaymentPreimage
	payHash := preimage.Hash()

	// Set AMP-specific features so that we can settle with HTLC-level
	// preimages.
	invoice.Terms.Features = ampFeatures

	ctxb := t.Context()
	_, err = db.AddInvoice(ctxb, invoice, payHash)
	require.Nil(t, err)

	setID := &[32]byte{1}

	// Update the invoice with an accepted HTLC that also accepts the
	// invoice.
	ref := invpkg.InvoiceRefByAddr(invoice.Terms.PaymentAddr)
	dbInvoice, err := db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID),
		updateAcceptAMPHtlc(0, amt, setID, true),
	)
	require.Nil(t, err)

	htlcPreimages := make(map[models.CircuitKey]lntypes.Preimage)
	for key := range dbInvoice.Htlcs {
		// Set the either the same preimage used to accept above, or a
		// blank preimage depending on the test case.
		var pre lntypes.Preimage
		if test.settleSamePreimage {
			pre = preimage
		}
		htlcPreimages[key] = pre
	}

	updateInvoice := func(
		invoice *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc, error) {

		update := &invpkg.InvoiceUpdateDesc{
			// TODO(positiveblue): this would be an invalid update
			// because tires to settle an AMP invoice without adding
			// any new htlc.
			State: &invpkg.InvoiceStateUpdateDesc{
				Preimage:      nil,
				NewState:      invpkg.ContractSettled,
				HTLCPreimages: htlcPreimages,
				SetID:         setID,
			},
			UpdateType: invpkg.AddHTLCsUpdate,
		}

		return update, nil
	}

	// Now settle the HTLC set and assert the resulting error.
	_, err = db.UpdateInvoice(
		ctxb, ref, (*invpkg.SetID)(setID), updateInvoice,
	)
	require.Equal(t, test.expError, err)
}

// testDeleteInvoices tests that deleting a list of invoices will succeed
// if all delete references are valid, or will fail otherwise.
func testDeleteInvoices(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// Add some invoices to the test db.
	numInvoices := 3
	invoicesToDelete := make([]invpkg.InvoiceDeleteRef, numInvoices)

	ctxb := t.Context()
	for i := 0; i < numInvoices; i++ {
		invoice, err := randInvoice(lnwire.MilliSatoshi(i + 1))
		require.NoError(t, err)

		paymentHash := invoice.Terms.PaymentPreimage.Hash()
		addIndex, err := db.AddInvoice(ctxb, invoice, paymentHash)
		require.NoError(t, err)

		// Settle the second invoice.
		if i == 1 {
			invoice, err = db.UpdateInvoice(
				ctxb, invpkg.InvoiceRefByHash(paymentHash), nil,
				getUpdateInvoice(
					uint64(i), invoice.Terms.Value,
				),
			)
			require.NoError(t, err, "unable to settle invoice")
		}

		// store the delete ref for later.
		invoicesToDelete[i] = invpkg.InvoiceDeleteRef{
			PayHash:     paymentHash,
			PayAddr:     &invoice.Terms.PaymentAddr,
			AddIndex:    addIndex,
			SettleIndex: invoice.SettleIndex,
		}
	}

	// assertInvoiceCount asserts that the number of invoices equals
	// to the passed count.
	assertInvoiceCount := func(count int) {
		// Query to collect all invoices.
		query := invpkg.InvoiceQuery{
			IndexOffset:    0,
			NumMaxInvoices: math.MaxUint64,
		}

		// Check that we really have 3 invoices.
		response, err := db.QueryInvoices(ctxb, query)
		require.NoError(t, err)
		require.Equal(t, count, len(response.Invoices))
	}

	// XOR one byte of one of the references' hash and attempt to delete.
	invoicesToDelete[0].PayHash[2] ^= 3
	require.Error(t, db.DeleteInvoice(ctxb, invoicesToDelete))
	assertInvoiceCount(3)

	// Restore the hash.
	invoicesToDelete[0].PayHash[2] ^= 3

	// XOR the second invoice's payment settle index as it is settled, and
	// attempt to delete.
	invoicesToDelete[1].SettleIndex ^= 11
	require.Error(t, db.DeleteInvoice(ctxb, invoicesToDelete))
	assertInvoiceCount(3)

	// Restore the settle index.
	invoicesToDelete[1].SettleIndex ^= 11

	// XOR the add index for one of the references and attempt to delete.
	invoicesToDelete[2].AddIndex ^= 13
	require.Error(t, db.DeleteInvoice(ctxb, invoicesToDelete))
	assertInvoiceCount(3)

	// Restore the add index.
	invoicesToDelete[2].AddIndex ^= 13

	// Delete should succeed with all the valid references.
	require.NoError(t, db.DeleteInvoice(ctxb, invoicesToDelete))
	assertInvoiceCount(0)
}

// testDeleteCanceledInvoices tests that deleting canceled invoices with the
// specific DeleteCanceledInvoices method works correctly.
func testDeleteCanceledInvoices(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	// Updatefunc is used to cancel an invoice.
	updateFunc := func(invoice *invpkg.Invoice) (
		*invpkg.InvoiceUpdateDesc, error) {

		return &invpkg.InvoiceUpdateDesc{
			UpdateType: invpkg.CancelInvoiceUpdate,
			State: &invpkg.InvoiceStateUpdateDesc{
				NewState: invpkg.ContractCanceled,
			},
		}, nil
	}

	// Test deletion of canceled invoices when there are none.
	ctxb := t.Context()
	require.NoError(t, db.DeleteCanceledInvoices(ctxb))

	// Add some invoices to the test db.
	var invoices []invpkg.Invoice
	for i := 0; i < 10; i++ {
		invoice, err := randInvoice(lnwire.MilliSatoshi(i + 1))
		require.NoError(t, err)

		paymentHash := invoice.Terms.PaymentPreimage.Hash()
		_, err = db.AddInvoice(ctxb, invoice, paymentHash)
		require.NoError(t, err)

		// Cancel every second invoice.
		if i%2 == 0 {
			invoice, err = db.UpdateInvoice(
				ctxb, invpkg.InvoiceRefByHash(paymentHash), nil,
				updateFunc,
			)
			require.NoError(t, err)
		} else {
			invoices = append(invoices, *invoice)
		}
	}

	// Delete canceled invoices.
	require.NoError(t, db.DeleteCanceledInvoices(ctxb))

	// Query to collect all invoices.
	query := invpkg.InvoiceQuery{
		IndexOffset:    0,
		NumMaxInvoices: math.MaxUint64,
	}

	dbInvoices, err := db.QueryInvoices(ctxb, query)
	require.NoError(t, err)

	// Check that we really have the expected invoices.
	require.Equal(t, invoices, dbInvoices.Invoices)
}

// testAddInvoiceInvalidFeatureDeps asserts that inserting an invoice with
// invalid transitive feature dependencies fails with the appropriate error.
func testAddInvoiceInvalidFeatureDeps(t *testing.T,
	makeDB func(t *testing.T) invpkg.InvoiceDB) {

	t.Parallel()
	db := makeDB(t)

	invoice, err := randInvoice(500)
	require.NoError(t, err)

	invoice.Terms.Features = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.MPPOptional,
		),
		lnwire.Features,
	)

	hash := invoice.Terms.PaymentPreimage.Hash()
	_, err = db.AddInvoice(t.Context(), invoice, hash)
	require.Error(t, err, feature.NewErrMissingFeatureDep(
		lnwire.PaymentAddrOptional,
	))
}
