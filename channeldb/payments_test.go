package channeldb

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	priv, _ = btcec.NewPrivateKey(btcec.S256())
	pub     = priv.PubKey()

	testHop1 = &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		CustomRecords: record.CustomSet{
			65536: []byte{},
			80001: []byte{},
		},
		MPP: record.NewMPP(32, [32]byte{0x42}),
	}

	testHop2 = &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		LegacyPayload:    true,
	}

	testRoute = route.Route{
		TotalTimeLock: 123,
		TotalAmount:   1234567,
		SourcePubKey:  route.NewVertex(pub),
		Hops: []*route.Hop{
			testHop2,
			testHop1,
		},
	}
)

func makeFakeInfo() (*PaymentCreationInfo, *HTLCAttemptInfo) {
	var preimg lntypes.Preimage
	copy(preimg[:], rev[:])

	c := &PaymentCreationInfo{
		PaymentHash: preimg.Hash(),
		Value:       1000,
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationTime:   time.Unix(time.Now().Unix(), 0),
		PaymentRequest: []byte(""),
	}

	a := &HTLCAttemptInfo{
		AttemptID:   44,
		SessionKey:  priv,
		Route:       testRoute,
		AttemptTime: time.Unix(100, 0),
	}
	return c, a
}

func TestSentPaymentSerialization(t *testing.T) {
	t.Parallel()

	c, s := makeFakeInfo()

	var b bytes.Buffer
	if err := serializePaymentCreationInfo(&b, c); err != nil {
		t.Fatalf("unable to serialize creation info: %v", err)
	}

	newCreationInfo, err := deserializePaymentCreationInfo(&b)
	if err != nil {
		t.Fatalf("unable to deserialize creation info: %v", err)
	}

	if !reflect.DeepEqual(c, newCreationInfo) {
		t.Fatalf("Payments do not match after "+
			"serialization/deserialization %v vs %v",
			spew.Sdump(c), spew.Sdump(newCreationInfo),
		)
	}

	b.Reset()
	if err := serializeHTLCAttemptInfo(&b, s); err != nil {
		t.Fatalf("unable to serialize info: %v", err)
	}

	newWireInfo, err := deserializeHTLCAttemptInfo(&b)
	if err != nil {
		t.Fatalf("unable to deserialize info: %v", err)
	}
	newWireInfo.AttemptID = s.AttemptID

	// First we verify all the records match up porperly, as they aren't
	// able to be properly compared using reflect.DeepEqual.
	err = assertRouteEqual(&s.Route, &newWireInfo.Route)
	if err != nil {
		t.Fatalf("Routes do not match after "+
			"serialization/deserialization: %v", err)
	}

	// Clear routes to allow DeepEqual to compare the remaining fields.
	newWireInfo.Route = route.Route{}
	s.Route = route.Route{}

	if !reflect.DeepEqual(s, newWireInfo) {
		s.SessionKey.Curve = nil
		newWireInfo.SessionKey.Curve = nil
		t.Fatalf("Payments do not match after "+
			"serialization/deserialization %v vs %v",
			spew.Sdump(s), spew.Sdump(newWireInfo),
		)
	}
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

func TestRouteSerialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	if err := SerializeRoute(&b, testRoute); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(b.Bytes())
	route2, err := DeserializeRoute(r)
	if err != nil {
		t.Fatal(err)
	}

	// First we verify all the records match up porperly, as they aren't
	// able to be properly compared using reflect.DeepEqual.
	err = assertRouteEqual(&testRoute, &route2)
	if err != nil {
		t.Fatalf("routes not equal: \n%v vs \n%v",
			spew.Sdump(testRoute), spew.Sdump(route2))
	}
}

// deletePayment removes a payment with paymentHash from the payments database.
func deletePayment(t *testing.T, db *DB, paymentHash lntypes.Hash) {
	t.Helper()

	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		payments := tx.ReadWriteBucket(paymentsRootBucket)

		err := payments.DeleteNestedBucket(paymentHash[:])
		if err != nil {
			return err
		}

		return nil
	})

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
			firstIndex:     0,
			lastIndex:      0,
			expectedSeqNrs: nil,
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
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db, err := initDB()
			if err != nil {
				t.Fatalf("unable to init db: %v", err)
			}

			// Populate the database with a set of test payments.
			numberOfPayments := 7
			pControl := NewPaymentControl(db)

			for i := 0; i < numberOfPayments; i++ {
				// Generate a test payment.
				info, _, _, err := genInfo()
				if err != nil {
					t.Fatalf("unable to create test "+
						"payment: %v", err)
				}

				// Create a new payment entry in the database.
				err = pControl.InitPayment(info.PaymentHash, info)
				if err != nil {
					t.Fatalf("unable to initialize "+
						"payment in database: %v", err)
				}

				// Immediately delete the payment with index 2.
				if i == 1 {
					deletePayment(t, db, info.PaymentHash)
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
					len(allPayments), len(querySlice.Payments))
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
