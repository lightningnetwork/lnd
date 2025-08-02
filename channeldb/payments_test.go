package channeldb

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
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

// assertRouteEquals compares to routes for equality and returns an error if
// they are not equal.
func assertRouteEqual(a, b *route.Route) error {
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("HTLCAttemptInfos don't match: %v vs %v",
			spew.Sdump(a), spew.Sdump(b))
	}

	return nil
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

			paymentDB := NewKVPaymentsDB(db)

			// Make a preliminary query to make sure it's ok to
			// query when we have no payments.
			resp, err := paymentDB.QueryPayments(tt.query)
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
				if err != nil {
					t.Fatalf("unable to initialize "+
						"payment in database: %v", err)
				}

				// Immediately delete the payment with index 2.
				if i == 1 {
					pmt, err := paymentDB.FetchPayment(
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

			querySlice, err := paymentDB.QueryPayments(tt.query)
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
