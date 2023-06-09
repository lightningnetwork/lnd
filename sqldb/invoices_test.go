package sqldb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

var (
	testTimeout = 10 * time.Second

	defautlInvoiceFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.PaymentAddrRequired,
			lnwire.MPPOptional,
		),
		lnwire.Features,
	)

	ampFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.PaymentAddrOptional,
			lnwire.AMPRequired,
		),
		lnwire.Features,
	)

	testNow = time.Unix(1, 0)
)

func newInvoieStoreWithDB(t *testing.T, db *BaseDB) (*InvoiceStore,
	sqlc.Querier) {

	dbTxer := NewTransactionExecutor(db,
		func(tx *sql.Tx) InvoiceQueries {
			return db.WithTx(tx)
		},
	)

	return NewInvoiceStore(dbTxer), db
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
			Expiry:          4000,
			PaymentPreimage: &pre,
			PaymentAddr:     payAddr,
			Value:           value,
			Features:        defautlInvoiceFeatures,
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
		i.PaymentRequest = r[:]
	} else {
		i.PaymentRequest = []byte("")
	}

	return i, nil
}

func TestAddInvoice(t *testing.T) {
	t.Parallel()

	ctxt, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	db := NewTestDB(t)

	executor := NewTransactionExecutor(
		db, func(tx *sql.Tx) InvoiceQueries {
			return db.BaseDB.WithTx(tx)
		},
	)

	store := NewInvoiceStore(executor)

	invAmt := lnwire.MilliSatoshi(1000)

	// Check that we can add a bolt11 invoice.
	inv, err := randInvoice(invAmt)
	require.NoError(t, err)

	hash := inv.Terms.PaymentPreimage.Hash()

	err = store.AddInvoice(ctxt, inv, hash)
	require.NoError(t, err)

	require.EqualValues(t, inv.AddIndex, 1)

	ref := invpkg.InvoiceRefByHash(hash)
	dbInv, err := store.LookupInvoice(ctxt, ref)
	require.NoError(t, err)
	require.Equal(t, inv, dbInv)
}

// TestInvoiceTimeSeries tests that newly added invoices invoices, as well as
// settled invoices are added to the database are properly placed in the add
// add or settle index which serves as an event time series.
func TestInvoiceAddTimeSeries(t *testing.T) {
	t.Parallel()

	ctxt, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	db := NewTestDB(t)

	executor := NewTransactionExecutor(
		db, func(tx *sql.Tx) InvoiceQueries {
			return db.BaseDB.WithTx(tx)
		},
	)

	store := NewInvoiceStore(executor)

	_, err := store.InvoicesAddedSince(ctxt, 0)
	require.NoError(t, err)

	// We'll start off by creating 20 random invoices, and inserting them
	// into the database.
	const numInvoices = 20
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]*invpkg.Invoice, numInvoices)
	for i := 0; i < len(invoices); i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		err = store.AddInvoice(ctxt, invoice, paymentHash)
		if err != nil {
			t.Fatalf("unable to add invoice %v", err)
		}

		invoices[i] = invoice
	}

	// With the invoices constructed, we'll now create a series of queries
	// that we'll use to assert expected return values of
	// InvoicesAddedSince.
	addQueries := []struct {
		sinceAddIndex uint64

		resp []*invpkg.Invoice
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
		resp, err := store.InvoicesAddedSince(ctxt, query.sinceAddIndex)
		if err != nil {
			t.Fatalf("unable to query: %v", err)
		}

		require.Equal(
			t, len(query.resp), len(resp),
			fmt.Sprintf("test: #%v", i),
		)

		require.Equal(t, len(query.resp), len(resp))

		for j := 0; j < len(query.resp); j++ {
			require.Equal(t,
				query.resp[j], resp[j],
				fmt.Sprintf("test: #%v, item: #%v", i, j),
			)
		}
	}

	_, err = store.InvoicesSettledSince(ctxt, 0)
	require.NoError(t, err)

	var settledInvoices []*invpkg.Invoice
	// We'll now only settle the latter half of each of those invoices.
	for i := 10; i < len(invoices); i++ {
		invoice := invoices[i]
		// TODO(positiveblue): really settle the invoice instead
		// of just making the changes directly in the invoice
		// and the db.
		invoice.State = invpkg.ContractSettled
		invoice.AmtPaid = amt

		preimage := invoices[i].Terms.PaymentPreimage
		params := sqlc.UpdateInvoiceParams{
			ID:             int64(invoice.AddIndex),
			State:          int16(invpkg.ContractSettled),
			Preimage:       preimage[:],
			AmountPaidMsat: int64(amt),
		}
		err := db.UpdateInvoice(ctxt, params)
		require.NoError(t, err)

		paymentParams := sqlc.InsertInvoicePaymentParams{
			InvoiceID:      int64(invoice.AddIndex),
			AmountPaidMsat: int64(amt),
		}
		settleIndex, err := db.InsertInvoicePayment(ctxt, paymentParams)
		require.NoError(t, err)
		invoice.SettleIndex = uint64(settleIndex)

		settledInvoices = append(settledInvoices, invoice)
	}

	// We'll now prepare an additional set of queries to ensure the settle
	// time series has properly been maintained in the database.
	settleQueries := []struct {
		sinceSettleIndex uint64

		resp []*invpkg.Invoice
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
		resp, err := store.InvoicesSettledSince(
			ctxt, query.sinceSettleIndex,
		)
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

// TestQueryInvoices ensures that we can properly query the invoice database for
// invoices using different types of queries.
func TestQueryInvoices(t *testing.T) {
	t.Parallel()

	ctxt, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	db := NewTestDB(t)

	executor := NewTransactionExecutor(
		db, func(tx *sql.Tx) InvoiceQueries {
			return db.BaseDB.WithTx(tx)
		},
	)

	store := NewInvoiceStore(executor)

	// To begin the test, we'll add 50 invoices to the database. We'll
	// assume that the index of the invoice within the database is the same
	// as the amount of the invoice itself.
	const numInvoices = 50
	var invoices []invpkg.Invoice
	var pendingInvoices []invpkg.Invoice

	for i := int64(1); i <= numInvoices; i++ {
		amt := lnwire.MilliSatoshi(i)
		invoice, err := randInvoice(amt)
		invoice.CreationDate = invoice.CreationDate.Add(
			time.Duration(i-1) * time.Second,
		)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		err = store.AddInvoice(ctxt, invoice, paymentHash)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		// We'll settle half of all invoices created.
		if i%2 == 0 {
			// TODO(positiveblue): really settle the invoice instead
			// of just making the changes directly in the invoice
			// and the db.
			invoice.State = invpkg.ContractSettled
			invoice.AmtPaid = amt

			preimage := invoice.Terms.PaymentPreimage
			params := sqlc.UpdateInvoiceParams{
				ID:             i,
				State:          int16(invpkg.ContractSettled),
				Preimage:       preimage[:],
				AmountPaidMsat: int64(amt),
			}
			err := db.UpdateInvoice(ctxt, params)
			require.NoError(t, err)
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
				CreationDateEnd: time.Unix(25, 0),
			},
			expected: invoices[:25],
		},
		// Fetch invoices >= 26 creation date.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices:    numInvoices,
				CreationDateStart: time.Unix(26, 0),
			},
			expected: invoices[25:],
		},
		// Fetch pending invoices <= 25 by creation date.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:     true,
				NumMaxInvoices:  numInvoices,
				CreationDateEnd: time.Unix(25, 0),
			},
			expected: pendingInvoices[:13],
		},
		// Fetch pending invoices >= 26 creation date.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:       true,
				NumMaxInvoices:    numInvoices,
				CreationDateStart: time.Unix(26, 0),
			},
			expected: pendingInvoices[13:],
		},
		// Fetch pending invoices with offset and end creation date.
		{
			query: invpkg.InvoiceQuery{
				IndexOffset:     20,
				NumMaxInvoices:  numInvoices,
				CreationDateEnd: time.Unix(30, 0),
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
				CreationDateStart: time.Unix(11, 0),
			},
			// Since we're skipping to invoice 20 and iterating
			// backward to invoice 10, we'll expect those invoices.
			expected: invoices[10:20],
		},
		// Fetch invoices with start and end creation date.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices:    numInvoices,
				CreationDateStart: time.Unix(11, 0),
				CreationDateEnd:   time.Unix(20, 0),
			},
			expected: invoices[10:20],
		},
		// Fetch pending invoices with start and end creation date.
		{
			query: invpkg.InvoiceQuery{
				PendingOnly:       true,
				NumMaxInvoices:    numInvoices,
				CreationDateStart: time.Unix(11, 0),
				CreationDateEnd:   time.Unix(20, 0),
			},
			expected: pendingInvoices[5:10],
		},
		// Fetch invoices with start and end creation date in reverse
		// order.
		{
			query: invpkg.InvoiceQuery{
				Reversed:          true,
				NumMaxInvoices:    numInvoices,
				CreationDateStart: time.Unix(11, 0),
				CreationDateEnd:   time.Unix(20, 0),
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
				CreationDateStart: time.Unix(11, 0),
				CreationDateEnd:   time.Unix(20, 0),
			},
			expected: pendingInvoices[5:10],
		},
		// Fetch invoices with a start date greater than end date
		// should result in an empty slice.
		{
			query: invpkg.InvoiceQuery{
				NumMaxInvoices:    numInvoices,
				CreationDateStart: time.Unix(20, 0),
				CreationDateEnd:   time.Unix(11, 0),
			},
			expected: nil,
		},
	}

	for i, testCase := range testCases {
		response, err := store.QueryInvoices(ctxt, testCase.query)
		if err != nil {
			t.Fatalf("unable to query invoice database: %v", err)
		}

		require.Equal(
			t, len(testCase.expected), len(response.Invoices),
			fmt.Sprintf("test: #%v", i),
		)

		for j, expected := range testCase.expected {
			require.Equal(
				t, expected, response.Invoices[j],
				fmt.Sprintf("test: #%v, item: #%v", i, j),
			)
		}
	}
}

// TestDeleteInvoices tests that deleting a list of invoices will succeed
// if all delete references are valid, or will fail otherwise.
func TestDeleteInvoices(t *testing.T) {
	t.Parallel()

	ctxt, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	db := NewTestDB(t)

	executor := NewTransactionExecutor(
		db, func(tx *sql.Tx) InvoiceQueries {
			return db.BaseDB.WithTx(tx)
		},
	)

	store := NewInvoiceStore(executor)

	// Add some invoices to the test db.
	numInvoices := 3
	invoicesToDelete := make([]invpkg.InvoiceDeleteRef, numInvoices)
	canBeDeleted := make([]invpkg.InvoiceDeleteRef, 0, numInvoices)

	for i := 0; i < numInvoices; i++ {
		invoice, err := randInvoice(lnwire.MilliSatoshi(i + 1))
		require.NoError(t, err)

		paymentHash := invoice.Terms.PaymentPreimage.Hash()
		err = store.AddInvoice(ctxt, invoice, paymentHash)
		require.NoError(t, err)

		// Settle the second invoice.
		if i == 1 {
			invoice.State = invpkg.ContractSettled
			invoice.AmtPaid = invoice.Terms.Value

			preimage := invoice.Terms.PaymentPreimage
			params := sqlc.UpdateInvoiceParams{
				ID:             int64(invoice.AddIndex),
				State:          int16(invpkg.ContractSettled),
				Preimage:       preimage[:],
				AmountPaidMsat: int64(invoice.AmtPaid),
			}
			err := db.UpdateInvoice(ctxt, params)
			require.NoError(t, err)

			paymentParams := sqlc.InsertInvoicePaymentParams{
				InvoiceID:      int64(invoice.AddIndex),
				AmountPaidMsat: int64(invoice.AmtPaid),
			}
			settleIndex, err := db.InsertInvoicePayment(
				ctxt, paymentParams,
			)
			require.NoError(t, err)
			invoice.SettleIndex = uint64(settleIndex)
		}

		// store the delete ref for later.
		ref := invpkg.InvoiceDeleteRef{
			PayHash:     paymentHash,
			PayAddr:     &invoice.Terms.PaymentAddr,
			AddIndex:    invoice.AddIndex,
			SettleIndex: invoice.SettleIndex,
		}

		if invoice.State != invpkg.ContractSettled {
			canBeDeleted = append(canBeDeleted, ref)
		}

		invoicesToDelete[i] = ref
	}

	// assertInvoiceCount asserts that the number of invoices equals
	// to the passed count.
	assertInvoiceCount := func(count int) {
		// Query to collect all invoices.
		query := invpkg.InvoiceQuery{
			IndexOffset:    0,
			NumMaxInvoices: uint64(DefaultRowsLimit),
		}

		// Check that we really have 3 invoices.
		response, err := store.QueryInvoices(ctxt, query)
		require.NoError(t, err)
		require.Equal(t, count, len(response.Invoices))
	}

	// XOR one byte of one of the references' hash and attempt to delete.
	invoicesToDelete[0].PayHash[2] ^= 3
	require.Error(t, store.DeleteInvoice(ctxt, invoicesToDelete))
	assertInvoiceCount(3)

	// Restore the hash.
	invoicesToDelete[0].PayHash[2] ^= 3

	// XOR the second invoice's payment settle index as it is settled, and
	// attempt to delete.
	invoicesToDelete[1].SettleIndex ^= 11
	require.Error(t, store.DeleteInvoice(ctxt, invoicesToDelete))
	assertInvoiceCount(3)

	// Restore the settle index.
	invoicesToDelete[1].SettleIndex ^= 11

	// XOR the add index for one of the references and attempt to delete.
	invoicesToDelete[2].AddIndex ^= 13
	require.Error(t, store.DeleteInvoice(ctxt, invoicesToDelete))
	assertInvoiceCount(3)

	// Restore the add index.
	invoicesToDelete[2].AddIndex ^= 13

	// We should be able to delete the invoices that are not settled.
	require.NoError(t, store.DeleteInvoice(ctxt, canBeDeleted))
	assertInvoiceCount(1)

	// We should not be able to delete settled invoices.
	require.Error(t, store.DeleteInvoice(ctxt, invoicesToDelete))
	assertInvoiceCount(1)
}
