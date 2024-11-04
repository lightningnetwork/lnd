package channeldb

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

var (
	priv, _ = btcec.NewPrivateKey()
	pub     = priv.PubKey()
	vertex  = route.NewVertex(pub)

	testHop1 = &route.Hop{
		PubKeyBytes:      vertex,
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		CustomRecords: record.CustomSet{
			65536: []byte{},
			80001: []byte{},
		},
		MPP:      record.NewMPP(32, [32]byte{0x42}),
		Metadata: []byte{1, 2, 3},
	}

	testHop2 = &route.Hop{
		PubKeyBytes:      vertex,
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		LegacyPayload:    true,
	}

	testHop3 = &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		CustomRecords: record.CustomSet{
			65536: []byte{},
			80001: []byte{},
		},
		AMP:      record.NewAMP([32]byte{0x69}, [32]byte{0x42}, 1),
		Metadata: []byte{1, 2, 3},
	}

	testRoute = route.Route{
		TotalTimeLock: 123,
		TotalAmount:   1234567,
		SourcePubKey:  vertex,
		Hops: []*route.Hop{
			testHop2,
			testHop1,
		},
	}

	testBlindedRoute = route.Route{
		TotalTimeLock: 150,
		TotalAmount:   1000,
		SourcePubKey:  vertex,
		Hops: []*route.Hop{
			{
				PubKeyBytes:      vertex,
				ChannelID:        9876,
				OutgoingTimeLock: 120,
				AmtToForward:     900,
				EncryptedData:    []byte{1, 3, 3},
				BlindingPoint:    pub,
			},
			{
				PubKeyBytes:   vertex,
				EncryptedData: []byte{3, 2, 1},
			},
			{
				PubKeyBytes:      vertex,
				Metadata:         []byte{4, 5, 6},
				AmtToForward:     500,
				OutgoingTimeLock: 100,
				TotalAmtMsat:     500,
			},
		},
	}
)

func makeFakeInfo(t *testing.T) (*PaymentCreationInfo, *HTLCAttemptInfo) {
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

// assertRouteEquals compares to routes for equality and returns an error if
// they are not equal.
func assertRouteEqual(a, b *route.Route) error {
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("HTLCAttemptInfos don't match: %v vs %v",
			spew.Sdump(a), spew.Sdump(b))
	}

	return nil
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
func deletePayment(t *testing.T, db *DB, paymentHash lntypes.Hash, seqNr uint64) {
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

// TestQueryPayments tests retrieval of payments with forwards and reversed
// queries.
func TestQueryPayments(t *testing.T) {
	// Define table driven test for QueryPayments.
	// Test payments have sequence indices [1, 3, 4, 5, 6, 7].
	// Note that the payment with index 7 has the same payment hash as 6,
	// and is stored in a nested bucket within payment 6 rather than being
	// its own entry in the payments bucket. We do this to test retrieval
	// of legacy payments.
	tests := []struct {
		name       string
		query      PaymentsQuery
		firstIndex uint64
		lastIndex  uint64

		// expectedSeqNrs contains the set of sequence numbers we expect
		// our query to return.
		expectedSeqNrs []uint64
	}{
		{
			name: "IndexOffset at the end of the payments range",
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
			query: PaymentsQuery{
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
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db, err := MakeTestDB(t)
			if err != nil {
				t.Fatalf("unable to init db: %v", err)
			}

			// Make a preliminary query to make sure it's ok to
			// query when we have no payments.
			resp, err := db.QueryPayments(tt.query)
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
			pControl := NewPaymentControl(db)

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
				err = pControl.InitPayment(info.PaymentIdentifier, info)
				if err != nil {
					t.Fatalf("unable to initialize "+
						"payment in database: %v", err)
				}

				// Immediately delete the payment with index 2.
				if i == 1 {
					pmt, err := pControl.FetchPayment(
						info.PaymentIdentifier,
					)
					require.NoError(t, err)

					deletePayment(t, db, info.PaymentIdentifier,
						pmt.SequenceNum)
				}

				// If we are on the last payment entry, add a
				// duplicate payment with sequence number equal
				// to the parent payment + 1. Note that
				// duplicate payments will always be succeeded.
				if i == (nonDuplicatePayments - 1) {
					pmt, err := pControl.FetchPayment(
						info.PaymentIdentifier,
					)
					require.NoError(t, err)

					appendDuplicatePayment(
						t, pControl.db,
						info.PaymentIdentifier,
						pmt.SequenceNum+1,
						preimg,
					)
				}
			}

			// Fetch all payments in the database.
			allPayments, err := db.FetchPayments()
			if err != nil {
				t.Fatalf("payments could not be fetched from "+
					"database: %v", err)
			}

			if len(allPayments) != 6 {
				t.Fatalf("Number of payments received does not "+
					"match expected one. Got %v, want %v.",
					len(allPayments), 6)
			}

			querySlice, err := db.QueryPayments(tt.query)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.firstIndex != querySlice.FirstIndexOffset ||
				tt.lastIndex != querySlice.LastIndexOffset {

				t.Errorf("First or last index does not match "+
					"expected index. Want (%d, %d), got (%d, %d).",
					tt.firstIndex, tt.lastIndex,
					querySlice.FirstIndexOffset,
					querySlice.LastIndexOffset)
			}

			if len(querySlice.Payments) != len(tt.expectedSeqNrs) {
				t.Errorf("expected: %v payments, got: %v",
					len(tt.expectedSeqNrs), len(querySlice.Payments))
			}

			for i, seqNr := range tt.expectedSeqNrs {
				q := querySlice.Payments[i]
				if seqNr != q.SequenceNum {
					t.Errorf("sequence numbers do not match, "+
						"got %v, want %v", q.SequenceNum, seqNr)
				}
			}
		})
	}
}

// TestFetchPaymentWithSequenceNumber tests lookup of payments with their
// sequence number. It sets up one payment with no duplicates, and another with
// two duplicates in its duplicates bucket then uses these payments to test the
// case where a specific duplicate is not found and the duplicates bucket is not
// present when we expect it to be.
func TestFetchPaymentWithSequenceNumber(t *testing.T) {
	db, err := MakeTestDB(t)
	require.NoError(t, err)

	pControl := NewPaymentControl(db)

	// Generate a test payment which does not have duplicates.
	noDuplicates, _, _, err := genInfo(t)
	require.NoError(t, err)

	// Create a new payment entry in the database.
	err = pControl.InitPayment(noDuplicates.PaymentIdentifier, noDuplicates)
	require.NoError(t, err)

	// Fetch the payment so we can get its sequence nr.
	noDuplicatesPayment, err := pControl.FetchPayment(
		noDuplicates.PaymentIdentifier,
	)
	require.NoError(t, err)

	// Generate a test payment which we will add duplicates to.
	hasDuplicates, _, preimg, err := genInfo(t)
	require.NoError(t, err)

	// Create a new payment entry in the database.
	err = pControl.InitPayment(hasDuplicates.PaymentIdentifier, hasDuplicates)
	require.NoError(t, err)

	// Fetch the payment so we can get its sequence nr.
	hasDuplicatesPayment, err := pControl.FetchPayment(
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
		t, db, hasDuplicates.PaymentIdentifier, duplicateOneSeqNr, preimg,
	)
	appendDuplicatePayment(
		t, db, hasDuplicates.PaymentIdentifier, duplicateTwoSeqNr, preimg,
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
			name:           "lookup duplicate, no duplicates bucket",
			paymentHash:    noDuplicates.PaymentIdentifier,
			sequenceNumber: duplicateTwoSeqNr,
			expectedErr:    ErrNoDuplicateBucket,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			err := kvdb.Update(
				db, func(tx walletdb.ReadWriteTx) error {
					var seqNrBytes [8]byte
					byteOrder.PutUint64(
						seqNrBytes[:], test.sequenceNumber,
					)

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
func appendDuplicatePayment(t *testing.T, db *DB, paymentHash lntypes.Hash,
	seqNr uint64, preImg lntypes.Preimage) {

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
