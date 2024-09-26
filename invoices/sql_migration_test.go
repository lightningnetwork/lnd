package invoices

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

var (
	// testHtlcIDSequence is a global counter for generating unique HTLC
	// IDs.
	testHtlcIDSequence uint64
)

// generateTestInvoice generates a random invoice for testing purposes. The
// invoice will have a random state and random HTLCs. If mpp is true, the
// invoice will have the MPP feature bit set. If amp is true, the invoice will
// have the AMP feature bit set and contain AMP HTLCs. Note that this function
// is not supposed to generate correct invoices, but rather random invoices for
// testing the SQL migration.
func generateTestInvoice(t *testing.T, mpp bool, amp bool) *Invoice {
	var preimage lntypes.Preimage
	_, err := crand.Read(preimage[:])
	require.NoError(t, err)

	terms := ContractTerm{
		FinalCltvDelta:  rand.Int31(),
		Expiry:          time.Duration(rand.Intn(4444)+1) * time.Minute,
		PaymentPreimage: &preimage,
		Value: lnwire.MilliSatoshi(
			rand.Int63n(9999999) + 1,
		),
		PaymentAddr: [32]byte{},
		Features:    lnwire.EmptyFeatureVector(),
	}

	if amp {
		terms.Features.Set(lnwire.AMPRequired)
	} else if mpp {
		terms.Features.Set(lnwire.MPPRequired)
	}

	created := randTime()

	// Note: this depends on the number of defined contract states. It is
	// unlikely that we will ever have more than 3 states, but if we do,
	// this test function will need to be updated.
	const maxContractState = 3
	state := ContractState(rand.Intn(maxContractState + 1))
	var (
		settled     time.Time
		settleIndex uint64
	)
	if state == ContractSettled {
		settled = randTimeBetween(
			created, created.Add(terms.Expiry),
		)
		settleIndex = uint64(rand.Intn(999) + 1)
	}

	// Note that generally we don't care about data consistency as much as
	// we do in the real world, but rather we strive to test the migration
	// code paths. Therefore some data might be inconsistent or not make
	// much sense.
	invoice := &Invoice{
		Memo:           []byte(randString(10)),
		PaymentRequest: []byte(randString(MaxPaymentRequestSize)),
		CreationDate:   created,
		SettleDate:     settled,
		Terms:          terms,
		// For our tests we don't care about the add index since it
		// won't be migrated as all SQL invoices have a SQL generated
		// ID instead of the add index.
		AddIndex:    0,
		SettleIndex: settleIndex,
		State:       state,
		AMPState:    make(map[SetID]InvoiceStateAMP),
		HodlInvoice: rand.Intn(2) == 1,
	}

	// Now generate some HTLCs for the invoice.
	invoice.Htlcs = make(map[models.CircuitKey]*InvoiceHTLC)

	if invoice.IsAMP() {
		generateAMPHtlcs(t, invoice)
	} else {
		generateInvoiceHTLCs(invoice)
	}

	for _, htlc := range invoice.Htlcs {
		if htlc.State == HtlcStateSettled {
			invoice.AmtPaid += htlc.Amt
		}
	}

	return invoice
}

// randString generates a random string of length n.
func randString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}

	return string(b)
}

// randTimeBetween generates a random time between min and max.
func randTimeBetween(min, max time.Time) time.Time {
	var timeZones = []*time.Location{
		time.UTC,
		time.FixedZone("EST", -5*3600),
		time.FixedZone("MST", -7*3600),
		time.FixedZone("PST", -8*3600),
		time.FixedZone("CEST", 2*3600),
	}

	// Ensure max is after min
	if max.Before(min) {
		min, max = max, min
	}

	// Calculate the range in nanoseconds
	duration := max.Sub(min)
	randDuration := time.Duration(rand.Int63n(duration.Nanoseconds()))

	// Generate the random time
	randomTime := min.Add(randDuration)

	// Assign a random time zone
	randomTimeZone := timeZones[rand.Intn(len(timeZones))]

	// Return the time in the random time zone
	return randomTime.In(randomTimeZone)
}

// randTime generates a random time between 2009 and 2140.
func randTime() time.Time {
	min := time.Date(2009, 1, 3, 0, 0, 0, 0, time.UTC)
	max := time.Date(2140, 1, 1, 0, 0, 0, 1000, time.UTC)

	return randTimeBetween(min, max)
}

func randInvoiceTime(invoice *Invoice) time.Time {
	return randTimeBetween(
		invoice.CreationDate,
		invoice.CreationDate.Add(invoice.Terms.Expiry),
	)
}

func randHTLC(invoice *Invoice, amt lnwire.MilliSatoshi) (models.CircuitKey,
	*InvoiceHTLC) {

	htlc := &InvoiceHTLC{
		Amt:          amt,
		AcceptHeight: uint32(rand.Intn(999) + 1),
		AcceptTime:   randInvoiceTime(invoice),
		Expiry:       uint32(rand.Intn(999) + 1),
	}
	if invoice.Terms.Features.HasFeature(lnwire.MPPRequired) {
		htlc.MppTotalAmt = invoice.Terms.Value
	}

	// For simplicity we just reflect the invoice state in the HTLC.
	switch invoice.State {
	case ContractSettled:
		htlc.State = HtlcStateSettled
		htlc.ResolveTime = randInvoiceTime(invoice)

	case ContractCanceled:
		htlc.State = HtlcStateCanceled
		htlc.ResolveTime = randInvoiceTime(invoice)

	case ContractAccepted:
		htlc.State = HtlcStateAccepted
	}

	// Also add some custom records.
	htlc.CustomRecords = make(record.CustomSet)
	numRecords := rand.Intn(5)
	for i := 0; i < numRecords; i++ {
		key := uint64(rand.Intn(1000) + record.CustomTypeStart)
		value := []byte(randString(10))
		htlc.CustomRecords[key] = value
	}

	htlcID := atomic.AddUint64(&testHtlcIDSequence, 1)
	// Spread HTLCs over multiple channels with some chance that
	// they arrive on the same channel if there are many HTLCs.
	randChanID := lnwire.NewShortChanIDFromInt(htlcID % 5)

	circutKey := models.CircuitKey{
		ChanID: randChanID,
		HtlcID: htlcID,
	}

	return circutKey, htlc
}

// generateInvoiceHTLCs generates all HTLCs including AMP HTLCs for an invoice.
func generateInvoiceHTLCs(invoice *Invoice) {
	mpp := invoice.Terms.Features.HasFeature(lnwire.MPPRequired)
	numHTLCs := 1
	if invoice.State == ContractOpen {
		numHTLCs = 0
	} else if mpp {
		numHTLCs = rand.Intn(10)
	}

	total := invoice.Terms.Value

	// Divide the total amount over the HTLCs equally.
	if numHTLCs > 0 {
		amt := total / lnwire.MilliSatoshi(numHTLCs)
		remainder := total - amt*lnwire.MilliSatoshi(numHTLCs)
		for i := 0; i < numHTLCs; i++ {
			if i == numHTLCs-1 {
				amt += remainder
			}

			circuitKey, htlc := randHTLC(invoice, amt)
			invoice.Htlcs[circuitKey] = htlc
		}
	}
}

// generateAMPHtlcs generates AMP HTLCs for an invoice.
func generateAMPHtlcs(t *testing.T, invoice *Invoice) {
	// If this is an AMP invoice, we will add a few AMP HTLC sets as well.
	numSetIDs := rand.Intn(5) + 1
	settledIdx := uint64(1)

	for i := 0; i < numSetIDs; i++ {
		var setID SetID
		_, err := crand.Read(setID[:])
		require.NoError(t, err)

		numHTLCs := rand.Intn(5) + 1
		total := invoice.Terms.Value
		invoiceKeys := make(map[CircuitKey]struct{})

		var htlcState HtlcState
		amt := total / lnwire.MilliSatoshi(numHTLCs)
		remainder := total - amt*lnwire.MilliSatoshi(numHTLCs)

		for j := 0; j < numHTLCs; j++ {
			if j == numHTLCs-1 {
				amt += remainder
			}

			// Generate the HTLC and the AMP data.
			circuitKey, htlc := randHTLC(invoice, amt)
			htlcState = htlc.State

			var (
				rootShare, hash [32]byte
				preimage        lntypes.Preimage
			)

			_, err := crand.Read(rootShare[:])
			require.NoError(t, err)

			_, err = crand.Read(hash[:])
			require.NoError(t, err)

			_, err = crand.Read(preimage[:])
			require.NoError(t, err)

			record := record.NewAMP(rootShare, setID, uint32(j))

			htlc.AMP = &InvoiceHtlcAMPData{
				Record:   *record,
				Hash:     hash,
				Preimage: &preimage,
			}

			invoice.Htlcs[circuitKey] = htlc
			invoiceKeys[circuitKey] = struct{}{}
		}

		ampState := InvoiceStateAMP{
			State:       htlcState,
			InvoiceKeys: invoiceKeys,
		}
		if htlcState == HtlcStateSettled {
			// For simplicity we don't use a global unique settle
			// index, but just increment it for each settled set.
			ampState.SettleIndex = settledIdx
			ampState.SettleDate = randInvoiceTime(invoice)
			settledIdx++
		}

		// Given that we don't currently mix states we can assume that
		// the invoice is fully paid if the AMP set is not canceled.
		if htlcState != HtlcStateCanceled {
			ampState.AmtPaid = invoice.Terms.Value
		}

		invoice.AMPState[setID] = ampState
	}
}

// TestMigrateSingleInvoice tests the migration of a single invoice with random
// data. Note that this test may need to be extended if the Invoice or any of
// the related types are extended.
func TestMigrateSingleInvoice(t *testing.T) {
	// First create a shared Postgres instance so we don't spawn a new
	// docker container for each test.
	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	makeSQLDB := func(t *testing.T, sqlite bool) *SQLStore {
		var db *sqldb.BaseDB
		if sqlite {
			db = sqldb.NewTestSqliteDB(t).BaseDB
		} else {
			db = sqldb.NewTestPostgresDB(t, pgFixture).BaseDB
		}

		executor := sqldb.NewTransactionExecutor(
			db, func(tx *sql.Tx) SQLInvoiceQueries {
				return db.WithTx(tx)
			},
		)

		testClock := clock.NewTestClock(time.Unix(1, 0))

		return NewSQLStore(executor, testClock)
	}

	tests := []struct {
		name string
		mpp  bool
		amp  bool
	}{
		{
			name: "no MPP no AMP",
			mpp:  false,
			amp:  false,
		},
		{
			name: "MPP",
			mpp:  true,
			amp:  false,
		},
		{
			name: "AMP",
			mpp:  false,
			amp:  true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name+"_SQLite", func(t *testing.T) {
			t.Parallel()

			store := makeSQLDB(t, true)
			testMigrateSingleInvoice(t, store, test.mpp, test.amp)
		})

		t.Run(test.name+"_Postgres", func(t *testing.T) {
			t.Parallel()

			store := makeSQLDB(t, false)
			testMigrateSingleInvoice(t, store, test.mpp, test.amp)
		})
	}
}

// testMigrateSingleInvoice is the main test function for the migration of a
// single invoice with random data. We'll generate a number of random invoices
// and migrate them to the SQL store. We'll then fetch the invoices from the
// store and compare them to the original invoices.
func testMigrateSingleInvoice(t *testing.T, store *SQLStore, mpp bool,
	amp bool) {

	ctxb := context.Background()
	invoices := make(map[lntypes.Hash]*Invoice)

	for i := 0; i < 100; i++ {
		invoice := generateTestInvoice(t, mpp, amp)
		var hash lntypes.Hash
		_, err := crand.Read(hash[:])
		require.NoError(t, err)

		invoices[hash] = invoice
	}

	var ops SQLInvoiceQueriesTxOptions
	err := store.db.ExecTx(ctxb, &ops, func(tx SQLInvoiceQueries) error {
		for hash, invoice := range invoices {
			err := MigrateSingleInvoice(ctxb, tx, invoice, hash)
			require.NoError(t, err)
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	// Fetch the newly migrated invoices from the store and compare them to
	// the original invoices. We also need to override the add index as
	// that's SQL generated.
	for hash, invoice := range invoices {
		sqlInvoice, err := store.LookupInvoice(
			ctxb, InvoiceRefByHash(hash),
		)
		require.NoError(t, err)

		invoice.AddIndex = sqlInvoice.AddIndex

		OverrideInvoiceTimeZone(invoice)
		OverrideInvoiceTimeZone(&sqlInvoice)

		require.Equal(t, *invoice, sqlInvoice)
	}
}
