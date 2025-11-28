//go:build !test_db_sqlite && !test_db_postgres

package paymentsdb

import (
	"bytes"
	"crypto/sha256"
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
	"github.com/stretchr/testify/require"
)

// TestKVStoreDeleteDuplicatePayments tests that when a payment with duplicate
// payments is deleted, both the parent payment and its duplicates are properly
// removed from the payment index. This is specific to the KV store's legacy
// duplicate payment handling.
func TestKVStoreDeleteDuplicatePayments(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	paymentDB := NewKVTestDB(t)

	// Create a successful payment.
	preimg := genPreimage(t)

	rhash := sha256.Sum256(preimg[:])
	info := genPaymentCreationInfo(t, rhash)
	attempt := genAttemptWithHash(t, 0, genSessionKey(t), rhash)

	// Init and settle the payment.
	err := paymentDB.InitPayment(ctx, info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to init payment")

	_, err = paymentDB.RegisterAttempt(
		ctx, info.PaymentIdentifier, attempt,
	)
	require.NoError(t, err, "unable to register attempt")

	_, err = paymentDB.SettleAttempt(
		ctx, info.PaymentIdentifier, attempt.AttemptID,
		&HTLCSettleInfo{
			Preimage: preimg,
		},
	)
	require.NoError(t, err, "unable to settle attempt")

	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusSucceeded,
	)

	// Fetch the payment to get its sequence number.
	payment, err := paymentDB.FetchPayment(ctx, info.PaymentIdentifier)
	require.NoError(t, err)

	// Add two duplicate payments. Use high sequence numbers that won't
	// collide with the original payment.
	duplicateSeqNr1 := payment.SequenceNum + 1000
	duplicateSeqNr2 := payment.SequenceNum + 1001

	appendDuplicatePayment(
		t, paymentDB.db, info.PaymentIdentifier, duplicateSeqNr1,
		preimg,
	)
	appendDuplicatePayment(
		t, paymentDB.db, info.PaymentIdentifier, duplicateSeqNr2,
		preimg,
	)

	// Verify we now have 3 index entries: original + 2 duplicates.
	var indexCount int
	err = kvdb.View(paymentDB.db, func(tx walletdb.ReadTx) error {
		index := tx.ReadBucket(paymentsIndexBucket)

		return index.ForEach(func(k, v []byte) error {
			indexCount++
			return nil
		})
	}, func() { indexCount = 0 })
	require.NoError(t, err)
	require.Equal(t, 3, indexCount, "expected 3 index entries "+
		"(parent + 2 duplicates)")

	// Delete all successful payments.
	numPayments, err := paymentDB.DeletePayments(ctx, false, false)
	require.NoError(t, err)
	require.EqualValues(t, 1, numPayments, "should delete 1 payment")

	// Verify all payments are deleted.
	dbPayments, err := paymentDB.FetchPayments()
	require.NoError(t, err)
	require.Empty(t, dbPayments, "all payments should be deleted")

	// Verify the payment index is now empty - all 3 entries (parent +
	// duplicates) should be removed.
	indexCount = 0
	err = kvdb.View(paymentDB.db, func(tx walletdb.ReadTx) error {
		index := tx.ReadBucket(paymentsIndexBucket)

		return index.ForEach(func(k, v []byte) error {
			indexCount++
			return nil
		})
	}, func() { indexCount = 0 })
	require.NoError(t, err)
	require.Equal(t, 0, indexCount, "payment index should be empty "+
		"after deleting payment with duplicates")
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

	ctx := t.Context()

	// Generate a test payment which does not have duplicates.
	noDuplicates, _ := genInfo(t)

	// Create a new payment entry in the database.
	err := paymentDB.InitPayment(
		ctx, noDuplicates.PaymentIdentifier, noDuplicates,
	)
	require.NoError(t, err)

	// Fetch the payment so we can get its sequence nr.
	noDuplicatesPayment, err := paymentDB.FetchPayment(
		ctx, noDuplicates.PaymentIdentifier,
	)
	require.NoError(t, err)

	// Generate a test payment which we will add duplicates to.
	hasDuplicates, preimg := genInfo(t)

	// Create a new payment entry in the database.
	err = paymentDB.InitPayment(
		ctx, hasDuplicates.PaymentIdentifier, hasDuplicates,
	)
	require.NoError(t, err)

	// Fetch the payment so we can get its sequence nr.
	hasDuplicatesPayment, err := paymentDB.FetchPayment(
		ctx, hasDuplicates.PaymentIdentifier,
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
	info, _ := genInfo(t)

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

// TestKVStoreQueryPaymentsDuplicates tests the KV store's legacy duplicate
// payment handling. This tests the specific case where duplicate payments
// are stored in a nested bucket within the parent payment bucket.
func TestKVStoreQueryPaymentsDuplicates(t *testing.T) {
	t.Parallel()

	// Test payments have sequence indices [1, 3, 4, 5, 6, 7].
	// Note that the payment with index 7 has the same payment hash as 6,
	// and is stored in a nested bucket within payment 6 rather than being
	// its own entry in the payments bucket. This tests retrieval of legacy
	// duplicate payments which is KV-store specific.
	// These test cases focus on validating that duplicate payments (seq 7,
	// nested under payment 6) are correctly returned in queries.
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
			name: "query includes duplicate payment in forward " +
				"order",
			query: Query{
				IndexOffset:       5,
				MaxPayments:       3,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     6,
			lastIndex:      7,
			expectedSeqNrs: []uint64{6, 7},
		},
		{
			name: "query duplicate payment at end",
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
			name: "query includes duplicate in reverse order",
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
			name: "query all payments includes duplicate",
			query: Query{
				IndexOffset:       0,
				MaxPayments:       math.MaxUint64,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      7,
			expectedSeqNrs: []uint64{1, 3, 4, 5, 6, 7},
		},
		{
			name: "exclude incomplete includes duplicate",
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()

			paymentDB := NewKVTestDB(t)

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
				info, preimg := genInfo(t)

				// Override creation time to allow for testing
				// of CreationDateStart and CreationDateEnd.
				info.CreationTime = time.Unix(int64(i+1), 0)

				// Create a new payment entry in the database.
				err = paymentDB.InitPayment(
					ctx, info.PaymentIdentifier, info,
				)
				require.NoError(t, err)

				// Immediately delete the payment with index 2.
				if i == 1 {
					pmt, err := paymentDB.FetchPayment(
						ctx, info.PaymentIdentifier,
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
						ctx, info.PaymentIdentifier,
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
