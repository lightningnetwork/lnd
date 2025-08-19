package invoices

import (
	crand "crypto/rand"
	"database/sql"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

var (
	// testHtlcIDSequence is a global counter for generating unique HTLC
	// IDs.
	testHtlcIDSequence uint64
)

// randomString generates a random string of a given length using rapid.
func randomStringRapid(t *rapid.T, length int) string {
	// Define the character set for the string.
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" //nolint:ll

	// Generate a string by selecting random characters from the charset.
	runes := make([]rune, length)
	for i := range runes {
		// Draw a random index and use it to select a character from the
		// charset.
		index := rapid.IntRange(0, len(charset)-1).Draw(t, "charIndex")
		runes[i] = rune(charset[index])
	}

	return string(runes)
}

// randTimeBetween generates a random time between min and max.
func randTimeBetween(minTime, maxTime time.Time) time.Time {
	var timeZones = []*time.Location{
		time.UTC,
		time.FixedZone("EST", -5*3600),
		time.FixedZone("MST", -7*3600),
		time.FixedZone("PST", -8*3600),
		time.FixedZone("CEST", 2*3600),
	}

	// Ensure max is after min
	if maxTime.Before(minTime) {
		minTime, maxTime = maxTime, minTime
	}

	// Calculate the range in nanoseconds
	duration := maxTime.Sub(minTime)
	randDuration := time.Duration(rand.Int63n(duration.Nanoseconds()))

	// Generate the random time
	randomTime := minTime.Add(randDuration)

	// Assign a random time zone
	randomTimeZone := timeZones[rand.Intn(len(timeZones))]

	// Return the time in the random time zone
	return randomTime.In(randomTimeZone)
}

// randTime generates a random time between 2009 and 2140.
func randTime() time.Time {
	minTime := time.Date(2009, 1, 3, 0, 0, 0, 0, time.UTC)
	maxTime := time.Date(2140, 1, 1, 0, 0, 0, 1000, time.UTC)

	return randTimeBetween(minTime, maxTime)
}

func randInvoiceTime(invoice *Invoice) time.Time {
	return randTimeBetween(
		invoice.CreationDate,
		invoice.CreationDate.Add(invoice.Terms.Expiry),
	)
}

// randHTLCRapid generates a random HTLC for an invoice using rapid to randomize
// its parameters.
func randHTLCRapid(t *rapid.T, invoice *Invoice, amt lnwire.MilliSatoshi) (
	models.CircuitKey, *InvoiceHTLC) {

	htlc := &InvoiceHTLC{
		Amt:          amt,
		AcceptHeight: rapid.Uint32Range(1, 999).Draw(t, "AcceptHeight"),
		AcceptTime:   randInvoiceTime(invoice),
		Expiry:       rapid.Uint32Range(1, 999).Draw(t, "Expiry"),
	}

	// Set MPP total amount if MPP feature is enabled in the invoice.
	if invoice.Terms.Features.HasFeature(lnwire.MPPRequired) {
		htlc.MppTotalAmt = invoice.Terms.Value
	}

	// Set the HTLC state and resolve time based on the invoice state.
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

	// Add randomized custom records to the HTLC.
	htlc.CustomRecords = make(record.CustomSet)
	numRecords := rapid.IntRange(0, 5).Draw(t, "numRecords")
	for i := 0; i < numRecords; i++ {
		key := rapid.Uint64Range(
			record.CustomTypeStart, 1000+record.CustomTypeStart,
		).Draw(t, "customRecordKey")
		value := []byte(randomStringRapid(t, 10))
		htlc.CustomRecords[key] = value
	}

	// Generate a unique HTLC ID and assign it to a channel ID.
	htlcID := atomic.AddUint64(&testHtlcIDSequence, 1)
	randChanID := lnwire.NewShortChanIDFromInt(htlcID % 5)

	circuitKey := models.CircuitKey{
		ChanID: randChanID,
		HtlcID: htlcID,
	}

	return circuitKey, htlc
}

// generateInvoiceHTLCsRapid generates all HTLCs for an invoice, including AMP
// HTLCs if applicable, using rapid for randomization of HTLC count and
// distribution.
func generateInvoiceHTLCsRapid(t *rapid.T, invoice *Invoice) {
	mpp := invoice.Terms.Features.HasFeature(lnwire.MPPRequired)

	// Use rapid to determine the number of HTLCs based on invoice state and
	// MPP feature.
	numHTLCs := 1
	if invoice.State == ContractOpen {
		numHTLCs = 0
	} else if mpp {
		numHTLCs = rapid.IntRange(1, 10).Draw(t, "numHTLCs")
	}

	total := invoice.Terms.Value

	// Distribute the total amount across the HTLCs, adding any remainder to
	// the last HTLC.
	if numHTLCs > 0 {
		amt := total / lnwire.MilliSatoshi(numHTLCs)
		remainder := total - amt*lnwire.MilliSatoshi(numHTLCs)

		for i := 0; i < numHTLCs; i++ {
			if i == numHTLCs-1 {
				// Add remainder to the last HTLC.
				amt += remainder
			}

			// Generate an HTLC with a random circuit key and add it
			// to the invoice.
			circuitKey, htlc := randHTLCRapid(t, invoice, amt)
			invoice.Htlcs[circuitKey] = htlc
		}
	}
}

// generateAMPHtlcsRapid generates AMP HTLCs for an invoice using rapid to
// randomize various parameters of the HTLCs in the AMP set.
func generateAMPHtlcsRapid(t *rapid.T, invoice *Invoice) {
	// Randomly determine the number of AMP sets (1 to 5).
	numSetIDs := rapid.IntRange(1, 5).Draw(t, "numSetIDs")
	settledIdx := uint64(1)

	for i := 0; i < numSetIDs; i++ {
		var setID SetID
		_, err := crand.Read(setID[:])
		require.NoError(t, err)

		// Determine the number of HTLCs in this set (1 to 5).
		numHTLCs := rapid.IntRange(1, 5).Draw(t, "numHTLCs")
		total := invoice.Terms.Value
		invoiceKeys := make(map[CircuitKey]struct{})

		// Calculate the amount per HTLC and account for remainder in
		// the final HTLC.
		amt := total / lnwire.MilliSatoshi(numHTLCs)
		remainder := total - amt*lnwire.MilliSatoshi(numHTLCs)

		var htlcState HtlcState
		for j := 0; j < numHTLCs; j++ {
			if j == numHTLCs-1 {
				amt += remainder
			}

			// Generate HTLC with randomized parameters.
			circuitKey, htlc := randHTLCRapid(t, invoice, amt)
			htlcState = htlc.State

			var (
				rootShare, hash [32]byte
				preimage        lntypes.Preimage
			)

			// Randomize AMP data fields.
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
			ampState.SettleIndex = settledIdx
			ampState.SettleDate = randInvoiceTime(invoice)
			settledIdx++
		}

		// Set the total amount paid if the AMP set is not canceled.
		if htlcState != HtlcStateCanceled {
			ampState.AmtPaid = invoice.Terms.Value
		}

		invoice.AMPState[setID] = ampState
	}
}

// TestMigrateSingleInvoiceRapid tests the migration of single invoices with
// random data variations using rapid. This test generates a random invoice
// configuration and ensures successful migration.
//
// NOTE: This test may need to be changed if the Invoice or any of the related
// types are modified.
func TestMigrateSingleInvoiceRapid(t *testing.T) {
	// Create a shared Postgres instance for efficient testing.
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

	// Define property-based test using rapid.
	rapid.Check(t, func(rt *rapid.T) {
		// Randomized feature flags for MPP and AMP.
		mpp := rapid.Bool().Draw(rt, "mpp")
		amp := rapid.Bool().Draw(rt, "amp")

		for _, sqlite := range []bool{true, false} {
			store := makeSQLDB(t, sqlite)
			testMigrateSingleInvoiceRapid(rt, store, mpp, amp)
		}
	})
}

// testMigrateSingleInvoiceRapid is the primary function for the migration of a
// single invoice with random data in a rapid-based test setup.
func testMigrateSingleInvoiceRapid(t *rapid.T, store *SQLStore, mpp bool,
	amp bool) {

	ctxb := t.Context()
	invoices := make(map[lntypes.Hash]*Invoice)

	for i := 0; i < 100; i++ {
		invoice := generateTestInvoiceRapid(t, mpp, amp)
		var hash lntypes.Hash
		_, err := crand.Read(hash[:])
		require.NoError(t, err)

		invoices[hash] = invoice
	}

	ops := sqldb.WriteTxOpt()
	err := store.db.ExecTx(ctxb, ops, func(tx SQLInvoiceQueries) error {
		for hash, invoice := range invoices {
			err := MigrateSingleInvoice(ctxb, tx, invoice, hash)
			require.NoError(t, err)
		}

		return nil
	}, sqldb.NoOpReset)
	require.NoError(t, err)

	// Fetch and compare each migrated invoice from the store with the
	// original.
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

// generateTestInvoiceRapid generates a random invoice with variations based on
// mpp and amp flags.
func generateTestInvoiceRapid(t *rapid.T, mpp bool, amp bool) *Invoice {
	var preimage lntypes.Preimage
	_, err := crand.Read(preimage[:])
	require.NoError(t, err)

	terms := ContractTerm{
		FinalCltvDelta: rapid.Int32Range(1, 1000).Draw(
			t, "FinalCltvDelta",
		),
		Expiry: time.Duration(
			rapid.IntRange(1, 4444).Draw(t, "Expiry"),
		) * time.Minute,
		PaymentPreimage: &preimage,
		Value: lnwire.MilliSatoshi(
			rapid.Int64Range(1, 9999999).Draw(t, "Value"),
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

	const maxContractState = 3
	state := ContractState(
		rapid.IntRange(0, maxContractState).Draw(t, "ContractState"),
	)
	var (
		settled     time.Time
		settleIndex uint64
	)
	if state == ContractSettled {
		settled = randTimeBetween(created, created.Add(terms.Expiry))
		settleIndex = rapid.Uint64Range(1, 999).Draw(t, "SettleIndex")
	}

	invoice := &Invoice{
		Memo: []byte(randomStringRapid(t, 10)),
		PaymentRequest: []byte(
			randomStringRapid(t, MaxPaymentRequestSize),
		),
		CreationDate: created,
		SettleDate:   settled,
		Terms:        terms,
		AddIndex:     0,
		SettleIndex:  settleIndex,
		State:        state,
		AMPState:     make(map[SetID]InvoiceStateAMP),
		HodlInvoice:  rapid.Bool().Draw(t, "HodlInvoice"),
	}

	invoice.Htlcs = make(map[models.CircuitKey]*InvoiceHTLC)

	if invoice.IsAMP() {
		generateAMPHtlcsRapid(t, invoice)
	} else {
		generateInvoiceHTLCsRapid(t, invoice)
	}

	for _, htlc := range invoice.Htlcs {
		if htlc.State == HtlcStateSettled {
			invoice.AmtPaid += htlc.Amt
		}
	}

	return invoice
}
