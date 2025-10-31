package paymentsdb

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKVStoreDeleteNonInFlight checks that calling DeletePayments only
// deletes payments from the database that are not in-flight.
//
// TODO(ziggie): Make this test db agnostic.
func TestKVStoreDeleteNonInFlight(t *testing.T) {
	t.Parallel()

	paymentDB := NewKVTestDB(t)

	// Create a sequence number for duplicate payments that will not collide
	// with the sequence numbers for the payments we create. These values
	// start at 1, so 9999 is a safe bet for this test.
	var duplicateSeqNr = 9999

	payments := []struct {
		failed       bool
		success      bool
		hasDuplicate bool
	}{
		{
			failed:       true,
			success:      false,
			hasDuplicate: false,
		},
		{
			failed:       false,
			success:      true,
			hasDuplicate: false,
		},
		{
			failed:       false,
			success:      false,
			hasDuplicate: false,
		},
		{
			failed:       false,
			success:      true,
			hasDuplicate: true,
		},
	}

	var numSuccess, numInflight int

	for _, p := range payments {
		info, attempt, preimg, err := genInfo(t)
		if err != nil {
			t.Fatalf("unable to generate htlc message: %v", err)
		}

		// Sends base htlc message which initiate StatusInFlight.
		err = paymentDB.InitPayment(info.PaymentIdentifier, info)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}
		_, err = paymentDB.RegisterAttempt(
			info.PaymentIdentifier, attempt,
		)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}

		htlc := &htlcStatus{
			HTLCAttemptInfo: attempt,
		}

		switch {
		case p.failed:
			// Fail the payment attempt.
			htlcFailure := HTLCFailUnreadable
			_, err := paymentDB.FailAttempt(
				info.PaymentIdentifier, attempt.AttemptID,
				&HTLCFailInfo{
					Reason: htlcFailure,
				},
			)
			if err != nil {
				t.Fatalf("unable to fail htlc: %v", err)
			}

			// Fail the payment, which should moved it to Failed.
			failReason := FailureReasonNoRoute
			_, err = paymentDB.Fail(
				info.PaymentIdentifier, failReason,
			)
			if err != nil {
				t.Fatalf("unable to fail payment hash: %v", err)
			}

			// Verify the status is indeed Failed.
			assertDBPaymentstatus(
				t, paymentDB, info.PaymentIdentifier,
				StatusFailed,
			)

			htlc.failure = &htlcFailure
			assertPaymentInfo(
				t, paymentDB, info.PaymentIdentifier, info,
				&failReason, htlc,
			)

		case p.success:
			// Verifies that status was changed to StatusSucceeded.
			_, err := paymentDB.SettleAttempt(
				info.PaymentIdentifier, attempt.AttemptID,
				&HTLCSettleInfo{
					Preimage: preimg,
				},
			)
			if err != nil {
				t.Fatalf("error shouldn't have been received,"+
					" got: %v", err)
			}

			assertDBPaymentstatus(
				t, paymentDB, info.PaymentIdentifier,
				StatusSucceeded,
			)

			htlc.settle = &preimg
			assertPaymentInfo(
				t, paymentDB, info.PaymentIdentifier, info, nil,
				htlc,
			)

			numSuccess++

		default:
			assertDBPaymentstatus(
				t, paymentDB, info.PaymentIdentifier,
				StatusInFlight,
			)
			assertPaymentInfo(
				t, paymentDB, info.PaymentIdentifier, info, nil,
				htlc,
			)

			numInflight++
		}

		// If the payment is intended to have a duplicate payment, we
		// add one.
		if p.hasDuplicate {
			appendDuplicatePayment(
				t, paymentDB.db, info.PaymentIdentifier,
				uint64(duplicateSeqNr), preimg,
			)
			duplicateSeqNr++
			numSuccess++
		}
	}

	// Delete all failed payments.
	numPayments, err := paymentDB.DeletePayments(true, false)
	require.NoError(t, err)
	require.EqualValues(t, 1, numPayments)

	// This should leave the succeeded and in-flight payments.
	dbPayments, err := paymentDB.FetchPayments()
	if err != nil {
		t.Fatal(err)
	}

	if len(dbPayments) != numSuccess+numInflight {
		t.Fatalf("expected %d payments, got %d",
			numSuccess+numInflight, len(dbPayments))
	}

	var s, i int
	for _, p := range dbPayments {
		t.Log("fetch payment has status", p.Status)
		switch p.Status {
		case StatusSucceeded:
			s++
		case StatusInFlight:
			i++
		}
	}

	if s != numSuccess {
		t.Fatalf("expected %d succeeded payments , got %d",
			numSuccess, s)
	}
	if i != numInflight {
		t.Fatalf("expected %d in-flight payments, got %d",
			numInflight, i)
	}

	// Now delete all payments except in-flight.
	numPayments, err = paymentDB.DeletePayments(false, false)
	require.NoError(t, err)
	require.EqualValues(t, 2, numPayments)

	// This should leave the in-flight payment.
	dbPayments, err = paymentDB.FetchPayments()
	if err != nil {
		t.Fatal(err)
	}

	if len(dbPayments) != numInflight {
		t.Fatalf("expected %d payments, got %d", numInflight,
			len(dbPayments))
	}

	for _, p := range dbPayments {
		if p.Status != StatusInFlight {
			t.Fatalf("expected in-fligth status, got %v", p.Status)
		}
	}

	// Finally, check that we only have a single index left in the payment
	// index bucket.
	var indexCount int
	err = kvdb.View(paymentDB.db, func(tx walletdb.ReadTx) error {
		index := tx.ReadBucket(paymentsIndexBucket)

		return index.ForEach(func(k, v []byte) error {
			indexCount++
			return nil
		})
	}, func() { indexCount = 0 })
	require.NoError(t, err)

	require.Equal(t, 1, indexCount)
}

type htlcStatus struct {
	*HTLCAttemptInfo
	settle  *lntypes.Preimage
	failure *HTLCFailReason
}

// fetchPaymentIndexEntry gets the payment hash for the sequence number provided
// from our payment indexes bucket.
func fetchPaymentIndexEntry(t *testing.T, p *KVStore,
	sequenceNumber uint64) (*lntypes.Hash, error) {

	t.Helper()

	var hash lntypes.Hash

	if err := kvdb.View(p.db, func(tx walletdb.ReadTx) error {
		indexBucket := tx.ReadBucket(paymentsIndexBucket)
		key := make([]byte, 8)
		byteOrder.PutUint64(key, sequenceNumber)

		indexValue := indexBucket.Get(key)
		if indexValue == nil {
			return ErrNoSequenceNrIndex
		}

		r := bytes.NewReader(indexValue)

		var err error
		hash, err = deserializePaymentIndex(r)

		return err
	}, func() {
		hash = lntypes.Hash{}
	}); err != nil {
		return nil, err
	}

	return &hash, nil
}

// assertPaymentIndex looks up the index for a payment in the db and checks
// that its payment hash matches the expected hash passed in.
func assertPaymentIndex(t *testing.T, p DB, expectedHash lntypes.Hash) {
	t.Helper()

	// Only the kv implementation uses the index so we exit early if the
	// payment db is not a kv implementation. This helps us to reuse the
	// same test for both implementations.
	kvPaymentDB, ok := p.(*KVStore)
	if !ok {
		return
	}

	// Lookup the payment so that we have its sequence number and check
	// that is has correctly been indexed in the payment indexes bucket.
	pmt, err := kvPaymentDB.FetchPayment(expectedHash)
	require.NoError(t, err)

	hash, err := fetchPaymentIndexEntry(t, kvPaymentDB, pmt.SequenceNum)
	require.NoError(t, err)
	assert.Equal(t, expectedHash, *hash)
}

// assertNoIndex checks that an index for the sequence number provided does not
// exist.
func assertNoIndex(t *testing.T, p DB, seqNr uint64) {
	t.Helper()

	kvPaymentDB, ok := p.(*KVStore)
	if !ok {
		return
	}

	_, err := fetchPaymentIndexEntry(t, kvPaymentDB, seqNr)
	require.Equal(t, ErrNoSequenceNrIndex, err)
}

func makeFakeInfo(t *testing.T) (*PaymentCreationInfo,
	*HTLCAttemptInfo) {

	t.Helper()

	var preimg lntypes.Preimage
	copy(preimg[:], rev[:])

	hash := preimg.Hash()

	c := &PaymentCreationInfo{
		PaymentIdentifier: hash,
		Value:             1000,
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationTime:   time.Unix(time.Now().Unix(), 0),
		PaymentRequest: []byte("test"),
	}

	a, err := NewHtlcAttempt(
		44, priv, testRoute, time.Unix(100, 0), &hash,
	)
	require.NoError(t, err)

	return c, &a.HTLCAttemptInfo
}

func TestSentPaymentSerialization(t *testing.T) {
	t.Parallel()

	c, s := makeFakeInfo(t)

	var b bytes.Buffer
	require.NoError(t, serializePaymentCreationInfo(&b, c), "serialize")

	// Assert the length of the serialized creation info is as expected,
	// without any custom records.
	baseLength := 32 + 8 + 8 + 4 + len(c.PaymentRequest)
	require.Len(t, b.Bytes(), baseLength)

	newCreationInfo, err := deserializePaymentCreationInfo(&b)
	require.NoError(t, err, "deserialize")
	require.Equal(t, c, newCreationInfo)

	b.Reset()

	// Now we add some custom records to the creation info and serialize it
	// again.
	c.FirstHopCustomRecords = lnwire.CustomRecords{
		lnwire.MinCustomRecordsTlvType: []byte{1, 2, 3},
	}
	require.NoError(t, serializePaymentCreationInfo(&b, c), "serialize")

	newCreationInfo, err = deserializePaymentCreationInfo(&b)
	require.NoError(t, err, "deserialize")
	require.Equal(t, c, newCreationInfo)

	b.Reset()
	require.NoError(t, serializeHTLCAttemptInfo(&b, s), "serialize")

	newWireInfo, err := deserializeHTLCAttemptInfo(&b)
	require.NoError(t, err, "deserialize")

	// First we verify all the records match up properly.
	require.Equal(t, s.Route, newWireInfo.Route)

	// We now add the new fields and custom records to the route and
	// serialize it again.
	b.Reset()
	s.Route.FirstHopAmount = tlv.NewRecordT[tlv.TlvType0](
		tlv.NewBigSizeT(lnwire.MilliSatoshi(1234)),
	)
	s.Route.FirstHopWireCustomRecords = lnwire.CustomRecords{
		lnwire.MinCustomRecordsTlvType + 3: []byte{4, 5, 6},
	}
	require.NoError(t, serializeHTLCAttemptInfo(&b, s), "serialize")

	newWireInfo, err = deserializeHTLCAttemptInfo(&b)
	require.NoError(t, err, "deserialize")
	require.Equal(t, s.Route, newWireInfo.Route)

	err = newWireInfo.attachOnionBlobAndCircuit()
	require.NoError(t, err)

	// Clear routes to allow DeepEqual to compare the remaining fields.
	newWireInfo.Route = route.Route{}
	s.Route = route.Route{}
	newWireInfo.AttemptID = s.AttemptID

	// Call session key method to set our cached session key so we can use
	// DeepEqual, and assert that our key equals the original key.
	require.Equal(t, s.cachedSessionKey, newWireInfo.SessionKey())

	require.Equal(t, s, newWireInfo)
}

// TestRouteSerialization tests serialization of a regular and blinded route.
func TestRouteSerialization(t *testing.T) {
	t.Parallel()

	testSerializeRoute(t, testRoute)
	testSerializeRoute(t, testBlindedRoute)
}

func testSerializeRoute(t *testing.T, route route.Route) {
	var b bytes.Buffer
	err := SerializeRoute(&b, route)
	require.NoError(t, err)

	r := bytes.NewReader(b.Bytes())
	route2, err := DeserializeRoute(r)
	require.NoError(t, err)

	reflect.DeepEqual(route, route2)
}

// deletePayment removes a payment with paymentHash from the payments database.
func deletePayment(t *testing.T, db kvdb.Backend, paymentHash lntypes.Hash,
	seqNr uint64) {

	t.Helper()

	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		payments := tx.ReadWriteBucket(paymentsRootBucket)

		// Delete the payment bucket.
		err := payments.DeleteNestedBucket(paymentHash[:])
		if err != nil {
			return err
		}

		key := make([]byte, 8)
		byteOrder.PutUint64(key, seqNr)

		// Delete the index that references this payment.
		indexes := tx.ReadWriteBucket(paymentsIndexBucket)

		return indexes.Delete(key)
	}, func() {})

	if err != nil {
		t.Fatalf("could not delete "+
			"payment: %v", err)
	}
}

// TestFetchPaymentWithSequenceNumber tests lookup of payments with their
// sequence number. It sets up one payment with no duplicates, and another with
// two duplicates in its duplicates bucket then uses these payments to test the
// case where a specific duplicate is not found and the duplicates bucket is not
// present when we expect it to be.
func TestFetchPaymentWithSequenceNumber(t *testing.T) {
	paymentDB := NewKVTestDB(t)

	// Generate a test payment which does not have duplicates.
	noDuplicates, _, _, err := genInfo(t)
	require.NoError(t, err)

	// Create a new payment entry in the database.
	err = paymentDB.InitPayment(
		noDuplicates.PaymentIdentifier, noDuplicates,
	)
	require.NoError(t, err)

	// Fetch the payment so we can get its sequence nr.
	noDuplicatesPayment, err := paymentDB.FetchPayment(
		noDuplicates.PaymentIdentifier,
	)
	require.NoError(t, err)

	// Generate a test payment which we will add duplicates to.
	hasDuplicates, _, preimg, err := genInfo(t)
	require.NoError(t, err)

	// Create a new payment entry in the database.
	err = paymentDB.InitPayment(
		hasDuplicates.PaymentIdentifier, hasDuplicates,
	)
	require.NoError(t, err)

	// Fetch the payment so we can get its sequence nr.
	hasDuplicatesPayment, err := paymentDB.FetchPayment(
		hasDuplicates.PaymentIdentifier,
	)
	require.NoError(t, err)

	// We declare the sequence numbers used here so that we can reference
	// them in tests.
	var (
		duplicateOneSeqNr = hasDuplicatesPayment.SequenceNum + 1
		duplicateTwoSeqNr = hasDuplicatesPayment.SequenceNum + 2
	)

	// Add two duplicates to our second payment.
	appendDuplicatePayment(
		t, paymentDB.db, hasDuplicates.PaymentIdentifier,
		duplicateOneSeqNr, preimg,
	)
	appendDuplicatePayment(
		t, paymentDB.db, hasDuplicates.PaymentIdentifier,
		duplicateTwoSeqNr, preimg,
	)

	tests := []struct {
		name           string
		paymentHash    lntypes.Hash
		sequenceNumber uint64
		expectedErr    error
	}{
		{
			name:           "lookup payment without duplicates",
			paymentHash:    noDuplicates.PaymentIdentifier,
			sequenceNumber: noDuplicatesPayment.SequenceNum,
			expectedErr:    nil,
		},
		{
			name:           "lookup payment with duplicates",
			paymentHash:    hasDuplicates.PaymentIdentifier,
			sequenceNumber: hasDuplicatesPayment.SequenceNum,
			expectedErr:    nil,
		},
		{
			name:           "lookup first duplicate",
			paymentHash:    hasDuplicates.PaymentIdentifier,
			sequenceNumber: duplicateOneSeqNr,
			expectedErr:    nil,
		},
		{
			name:           "lookup second duplicate",
			paymentHash:    hasDuplicates.PaymentIdentifier,
			sequenceNumber: duplicateTwoSeqNr,
			expectedErr:    nil,
		},
		{
			name:           "lookup non-existent duplicate",
			paymentHash:    hasDuplicates.PaymentIdentifier,
			sequenceNumber: 999999,
			expectedErr:    ErrDuplicateNotFound,
		},
		{
			name: "lookup duplicate, no duplicates " +
				"bucket",
			paymentHash:    noDuplicates.PaymentIdentifier,
			sequenceNumber: duplicateTwoSeqNr,
			expectedErr:    ErrNoDuplicateBucket,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			//nolint:ll
			err := kvdb.Update(
				paymentDB.db, func(tx walletdb.ReadWriteTx) error {
					var seqNrBytes [8]byte
					byteOrder.PutUint64(
						seqNrBytes[:],
						test.sequenceNumber,
					)

					//nolint:ll
					_, err := fetchPaymentWithSequenceNumber(
						tx, test.paymentHash, seqNrBytes[:],
					)

					return err
				}, func() {},
			)
			require.Equal(t, test.expectedErr, err)
		})
	}
}

// appendDuplicatePayment adds a duplicate payment to an existing payment. Note
// that this function requires a unique sequence number.
//
// This code is *only* intended to replicate legacy duplicate payments in lnd,
// our current schema does not allow duplicates.
func appendDuplicatePayment(t *testing.T, db kvdb.Backend,
	paymentHash lntypes.Hash, seqNr uint64, preImg lntypes.Preimage) {

	err := kvdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		bucket, err := fetchPaymentBucketUpdate(
			tx, paymentHash,
		)
		if err != nil {
			return err
		}

		// Create the duplicates bucket if it is not
		// present.
		dup, err := bucket.CreateBucketIfNotExists(
			duplicatePaymentsBucket,
		)
		if err != nil {
			return err
		}

		var sequenceKey [8]byte
		byteOrder.PutUint64(sequenceKey[:], seqNr)

		// Create duplicate payments for the two dup
		// sequence numbers we've setup.
		putDuplicatePayment(t, dup, sequenceKey[:], paymentHash, preImg)

		// Finally, once we have created our entry we add an index for
		// it.
		err = createPaymentIndexEntry(tx, sequenceKey[:], paymentHash)
		require.NoError(t, err)

		return nil
	}, func() {})
	require.NoError(t, err, "could not create payment")
}

// putDuplicatePayment creates a duplicate payment in the duplicates bucket
// provided with the minimal information required for successful reading.
func putDuplicatePayment(t *testing.T, duplicateBucket kvdb.RwBucket,
	sequenceKey []byte, paymentHash lntypes.Hash,
	preImg lntypes.Preimage) {

	paymentBucket, err := duplicateBucket.CreateBucketIfNotExists(
		sequenceKey,
	)
	require.NoError(t, err)

	err = paymentBucket.Put(duplicatePaymentSequenceKey, sequenceKey)
	require.NoError(t, err)

	// Generate fake information for the duplicate payment.
	info, _, _, err := genInfo(t)
	require.NoError(t, err)

	// Write the payment info to disk under the creation info key. This code
	// is copied rather than using serializePaymentCreationInfo to ensure
	// we always write in the legacy format used by duplicate payments.
	var b bytes.Buffer
	var scratch [8]byte
	_, err = b.Write(paymentHash[:])
	require.NoError(t, err)

	byteOrder.PutUint64(scratch[:], uint64(info.Value))
	_, err = b.Write(scratch[:])
	require.NoError(t, err)

	err = serializeTime(&b, info.CreationTime)
	require.NoError(t, err)

	byteOrder.PutUint32(scratch[:4], 0)
	_, err = b.Write(scratch[:4])
	require.NoError(t, err)

	// Get the PaymentCreationInfo.
	err = paymentBucket.Put(duplicatePaymentCreationInfoKey, b.Bytes())
	require.NoError(t, err)

	// Duolicate payments are only stored for successes, so add the
	// preimage.
	err = paymentBucket.Put(duplicatePaymentSettleInfoKey, preImg[:])
	require.NoError(t, err)
}

// TestQueryPayments tests retrieval of payments with forwards and reversed
// queries.
//
// TODO(ziggie): Make this test db agnostic.
func TestQueryPayments(t *testing.T) {
	// Define table driven test for QueryPayments.
	// Test payments have sequence indices [1, 3, 4, 5, 6, 7].
	// Note that the payment with index 7 has the same payment hash as 6,
	// and is stored in a nested bucket within payment 6 rather than being
	// its own entry in the payments bucket. We do this to test retrieval
	// of legacy payments.
	tests := []struct {
		name       string
		query      Query
		firstIndex uint64
		lastIndex  uint64

		// expectedSeqNrs contains the set of sequence numbers we expect
		// our query to return.
		expectedSeqNrs []uint64
	}{
		{
			name: "IndexOffset at the end of the payments range",
			query: Query{
				IndexOffset:       7,
				MaxPayments:       7,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     0,
			lastIndex:      0,
			expectedSeqNrs: nil,
		},
		{
			name: "query in forwards order, start at beginning",
			query: Query{
				IndexOffset:       0,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      3,
			expectedSeqNrs: []uint64{1, 3},
		},
		{
			name: "query in forwards order, start at end, overflow",
			query: Query{
				IndexOffset:       6,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     7,
			lastIndex:      7,
			expectedSeqNrs: []uint64{7},
		},
		{
			name: "start at offset index outside of payments",
			query: Query{
				IndexOffset:       20,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     0,
			lastIndex:      0,
			expectedSeqNrs: nil,
		},
		{
			name: "overflow in forwards order",
			query: Query{
				IndexOffset:       4,
				MaxPayments:       math.MaxUint64,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     5,
			lastIndex:      7,
			expectedSeqNrs: []uint64{5, 6, 7},
		},
		{
			name: "start at offset index outside of payments, " +
				"reversed order",
			query: Query{
				IndexOffset:       9,
				MaxPayments:       2,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     6,
			lastIndex:      7,
			expectedSeqNrs: []uint64{6, 7},
		},
		{
			name: "query in reverse order, start at end",
			query: Query{
				IndexOffset:       0,
				MaxPayments:       2,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     6,
			lastIndex:      7,
			expectedSeqNrs: []uint64{6, 7},
		},
		{
			name: "query in reverse order, starting in middle",
			query: Query{
				IndexOffset:       4,
				MaxPayments:       2,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      3,
			expectedSeqNrs: []uint64{1, 3},
		},
		{
			name: "query in reverse order, starting in middle, " +
				"with underflow",
			query: Query{
				IndexOffset:       4,
				MaxPayments:       5,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      3,
			expectedSeqNrs: []uint64{1, 3},
		},
		{
			name: "all payments in reverse, order maintained",
			query: Query{
				IndexOffset:       0,
				MaxPayments:       7,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      7,
			expectedSeqNrs: []uint64{1, 3, 4, 5, 6, 7},
		},
		{
			name: "exclude incomplete payments",
			query: Query{
				IndexOffset:       0,
				MaxPayments:       7,
				Reversed:          false,
				IncludeIncomplete: false,
			},
			firstIndex:     7,
			lastIndex:      7,
			expectedSeqNrs: []uint64{7},
		},
		{
			name: "query payments at index gap",
			query: Query{
				IndexOffset:       1,
				MaxPayments:       7,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     3,
			lastIndex:      7,
			expectedSeqNrs: []uint64{3, 4, 5, 6, 7},
		},
		{
			name: "query payments reverse before index gap",
			query: Query{
				IndexOffset:       3,
				MaxPayments:       7,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      1,
			expectedSeqNrs: []uint64{1},
		},
		{
			name: "query payments reverse on index gap",
			query: Query{
				IndexOffset:       2,
				MaxPayments:       7,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      1,
			expectedSeqNrs: []uint64{1},
		},
		{
			name: "query payments forward on index gap",
			query: Query{
				IndexOffset:       2,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     3,
			lastIndex:      4,
			expectedSeqNrs: []uint64{3, 4},
		},
		{
			name: "query in forwards order, with start creation " +
				"time",
			query: Query{
				IndexOffset:       0,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
				CreationDateStart: 5,
			},
			firstIndex:     5,
			lastIndex:      6,
			expectedSeqNrs: []uint64{5, 6},
		},
		{
			name: "query in forwards order, with start creation " +
				"time at end, overflow",
			query: Query{
				IndexOffset:       0,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
				CreationDateStart: 7,
			},
			firstIndex:     7,
			lastIndex:      7,
			expectedSeqNrs: []uint64{7},
		},
		{
			name: "query with start and end creation time",
			query: Query{
				IndexOffset:       9,
				MaxPayments:       math.MaxUint64,
				Reversed:          true,
				IncludeIncomplete: true,
				CreationDateStart: 3,
				CreationDateEnd:   5,
			},
			firstIndex:     3,
			lastIndex:      5,
			expectedSeqNrs: []uint64{3, 4, 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()

			paymentDB := NewKVTestDB(t)

			// Initialize the payment database.
			paymentDB, err := NewKVStore(paymentDB.db)
			require.NoError(t, err)

			// Make a preliminary query to make sure it's ok to
			// query when we have no payments.
			resp, err := paymentDB.QueryPayments(ctx, tt.query)
			require.NoError(t, err)
			require.Len(t, resp.Payments, 0)

			// Populate the database with a set of test payments.
			// We create 6 original payments, deleting the payment
			// at index 2 so that we cover the case where sequence
			// numbers are missing. We also add a duplicate payment
			// to the last payment added to test the legacy case
			// where we have duplicates in the nested duplicates
			// bucket.
			nonDuplicatePayments := 6

			for i := 0; i < nonDuplicatePayments; i++ {
				// Generate a test payment.
				info, _, preimg, err := genInfo(t)
				if err != nil {
					t.Fatalf("unable to create test "+
						"payment: %v", err)
				}
				// Override creation time to allow for testing
				// of CreationDateStart and CreationDateEnd.
				info.CreationTime = time.Unix(int64(i+1), 0)

				// Create a new payment entry in the database.
				err = paymentDB.InitPayment(
					info.PaymentIdentifier, info,
				)
				require.NoError(t, err)

				// Immediately delete the payment with index 2.
				if i == 1 {
					pmt, err := paymentDB.FetchPayment(
						info.PaymentIdentifier,
					)
					require.NoError(t, err)

					deletePayment(
						t, paymentDB.db,
						info.PaymentIdentifier,
						pmt.SequenceNum,
					)
				}

				// If we are on the last payment entry, add a
				// duplicate payment with sequence number equal
				// to the parent payment + 1. Note that
				// duplicate payments will always be succeeded.
				if i == (nonDuplicatePayments - 1) {
					pmt, err := paymentDB.FetchPayment(
						info.PaymentIdentifier,
					)
					require.NoError(t, err)

					appendDuplicatePayment(
						t, paymentDB.db,
						info.PaymentIdentifier,
						pmt.SequenceNum+1,
						preimg,
					)
				}
			}

			// Fetch all payments in the database.
			allPayments, err := paymentDB.FetchPayments()
			if err != nil {
				t.Fatalf("payments could not be fetched from "+
					"database: %v", err)
			}

			if len(allPayments) != 6 {
				t.Fatalf("Number of payments received does "+
					"not match expected one. Got %v, "+
					"want %v.", len(allPayments), 6)
			}

			querySlice, err := paymentDB.QueryPayments(
				ctx, tt.query,
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.firstIndex != querySlice.FirstIndexOffset ||
				tt.lastIndex != querySlice.LastIndexOffset {

				t.Errorf("First or last index does not match "+
					"expected index. Want (%d, %d), "+
					"got (%d, %d).",
					tt.firstIndex, tt.lastIndex,
					querySlice.FirstIndexOffset,
					querySlice.LastIndexOffset)
			}

			if len(querySlice.Payments) != len(tt.expectedSeqNrs) {
				t.Errorf("expected: %v payments, got: %v",
					len(tt.expectedSeqNrs),
					len(querySlice.Payments))
			}

			for i, seqNr := range tt.expectedSeqNrs {
				q := querySlice.Payments[i]
				if seqNr != q.SequenceNum {
					t.Errorf("sequence numbers do not "+
						"match, got %v, want %v",
						q.SequenceNum, seqNr)
				}
			}
		})
	}
}

// TestLazySessionKeyDeserialize tests that we can read htlc attempt session
// keys that were previously serialized as a private key as raw bytes.
func TestLazySessionKeyDeserialize(t *testing.T) {
	var b bytes.Buffer

	// Serialize as a private key.
	err := WriteElements(&b, priv)
	require.NoError(t, err)

	// Deserialize into [btcec.PrivKeyBytesLen]byte.
	attempt := HTLCAttemptInfo{}
	err = ReadElements(&b, &attempt.sessionKey)
	require.NoError(t, err)
	require.Zero(t, b.Len())

	sessionKey := attempt.SessionKey()
	require.Equal(t, priv, sessionKey)
}

// TestDeserializeHTLCFailInfoInvalidTLV tests that deserializeHTLCFailInfo
// handles invalid extra tlv data gracefully by not failing.
func TestDeserializeHTLCFailInfoInvalidTLV(t *testing.T) {
	// Create a channel update with valid data first, then encode it.
	testSig := &ecdsa.Signature{}
	sig, _ := lnwire.NewSigFromSignature(testSig)
	chanUpdate := &lnwire.ChannelUpdate1{
		Signature:       sig,
		ShortChannelID:  lnwire.NewShortChanIDFromInt(1),
		Timestamp:       1,
		MessageFlags:    0,
		ChannelFlags:    1,
		ExtraOpaqueData: make([]byte, 0),
	}

	var chanUpdateBuf bytes.Buffer
	err := chanUpdate.Encode(&chanUpdateBuf, 0)
	require.NoError(t, err)

	// Append invalid inbound fee TLV record to the encoded channel update.
	// The inbound fee TLV has type 55555 and should have 8 bytes of data
	// (2 uint32 values: BaseFee and FeeRate). We create an invalid one by
	// using the correct type but with incomplete data (only 6 bytes
	// instead of 8).
	var invalidInboundFeeTLV bytes.Buffer

	// Write type 55555 as varint: 0xfd + 2 bytes (canonical encoding)
	err = invalidInboundFeeTLV.WriteByte(0xfd)
	require.NoError(t, err)

	var typeBytes [2]byte
	binary.BigEndian.PutUint16(typeBytes[:], 55555)
	_, err = invalidInboundFeeTLV.Write(typeBytes[:])
	require.NoError(t, err)

	// Write length as 8 (single byte since 8 < 0xfd, no varint needed)
	err = invalidInboundFeeTLV.WriteByte(8)
	require.NoError(t, err)

	// Write only 6 bytes of value data (incomplete, should be 8 bytes)
	var valueBytes [6]byte
	binary.BigEndian.PutUint32(valueBytes[0:4], 1)
	binary.BigEndian.PutUint16(valueBytes[4:6], 2)
	_, err = invalidInboundFeeTLV.Write(valueBytes[:])
	require.NoError(t, err)

	_, err = chanUpdateBuf.Write(invalidInboundFeeTLV.Bytes())
	require.NoError(t, err)

	// Manually create a TemporaryChannelFailure failure message with the
	// corrupted channel update bytes.
	var failureMsgBuf bytes.Buffer

	// Write the failure code.
	err = lnwire.WriteUint16(
		&failureMsgBuf, uint16(lnwire.CodeTemporaryChannelFailure),
	)
	require.NoError(t, err)

	// Write the length of the channel update (including invalid TLV).
	err = lnwire.WriteUint16(&failureMsgBuf, uint16(chanUpdateBuf.Len()))
	require.NoError(t, err)

	// Write the channel update bytes with invalid TLV appended.
	_, err = failureMsgBuf.Write(chanUpdateBuf.Bytes())
	require.NoError(t, err)

	_, err = lnwire.DecodeFailureMessage(&failureMsgBuf, 0)
	require.ErrorIs(t, err, lnwire.ErrParsingExtraTLVBytes)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	// Create an HTLCFailInfo and serialize it with the corrupted failure
	// message.
	failInfo := &HTLCFailInfo{
		FailTime:           time.Now(),
		Reason:             HTLCFailMessage,
		FailureSourceIndex: 2,
	}

	var buf bytes.Buffer

	// Manually serialize the HTLCFailInfo with the corrupted failure bytes.
	err = serializeTime(&buf, failInfo.FailTime)
	require.NoError(t, err)

	// Write the failure message bytes.
	err = wire.WriteVarBytes(&buf, 0, failureMsgBuf.Bytes())
	require.NoError(t, err)

	// Write reason and failure source index.
	err = WriteElements(
		&buf, byte(failInfo.Reason), failInfo.FailureSourceIndex,
	)
	require.NoError(t, err)

	// Now deserialize the HTLCFailInfo - this should NOT fail despite the
	// invalid TLV data.
	deserializedFailInfo, err := deserializeHTLCFailInfo(&buf)
	require.NoError(t, err, "deserializeHTLCFailInfo should not fail "+
		"with invalid TLV data")
	require.NotNil(t, deserializedFailInfo)

	// Verify the basic fields are correctly deserialized.
	require.Equal(t, failInfo.Reason, deserializedFailInfo.Reason)
	require.Equal(t, failInfo.FailureSourceIndex,
		deserializedFailInfo.FailureSourceIndex)

	// Verify the failure message is nil because the decoding failed
	// due to invalid TLV data. The important part is that the
	// HTLCFailInfo deserialization still succeeded despite the invalid
	// TLV data in the failure message.
	require.Nil(t, deserializedFailInfo.Message,
		"Message should be nil when TLV parsing fails")
}
