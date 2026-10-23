//go:build test_db_sqlite || test_db_postgres

package paymentsdb

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestOmitHopsRouteBuilding tests the omit_hops behavior in dbDataToRoute.
// When allowEmpty is true (omit_hops=true) and no hops are provided, a minimal
// route with only route-level fields should be returned. When allowEmpty is
// false, the same scenario should return an error.
func TestOmitHopsRouteBuilding(t *testing.T) {
	t.Parallel()

	sourceKey := vertex[:]

	// With allowEmpty=true (omit_hops), empty hops should return a
	// minimal route preserving route-level fields.
	t.Run("omit hops returns minimal route", func(t *testing.T) {
		t.Parallel()

		r, err := dbDataToRoute(
			nil, nil, 0, 123, 1000, sourceKey, nil, true,
		)
		require.NoError(t, err)
		require.Nil(t, r.Hops)
		require.Equal(t, uint32(123), r.TotalTimeLock)
		require.Equal(t, lnwire.MilliSatoshi(1000), r.TotalAmount)
		require.Equal(t, vertex, r.SourcePubKey)
	})

	// With allowEmpty=false (include hops), empty hops should error.
	t.Run("include hops errors on empty hops", func(t *testing.T) {
		t.Parallel()

		_, err := dbDataToRoute(
			nil, nil, 0, 123, 1000, sourceKey, nil, false,
		)
		require.Error(t, err)
	})
}

// TestOmitHopsAttemptConversion tests that dbAttemptToHTLCAttempt correctly
// handles the includeHops flag. When false, route custom records should be
// skipped and the route should have no hops. When true, hops and custom
// records should be fully populated.
func TestOmitHopsAttemptConversion(t *testing.T) {
	t.Parallel()

	var paymentHash lntypes.Hash
	copy(paymentHash[:], testHash[:])

	sessionKey := genSessionKey(t)
	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	baseAttempt := sqlc.FetchHtlcAttemptsForPaymentsRow{
		ID:                 1,
		AttemptIndex:       1,
		PaymentID:          1,
		SessionKey:         sessionKeyBytes[:],
		AttemptTime:        time.Now(),
		PaymentHash:        paymentHash[:],
		RouteTotalTimeLock: 123,
		RouteTotalAmount:   1000,
		RouteSourceKey:     vertex[:],
	}

	hops := []sqlc.FetchHopsForAttemptsRow{
		{
			ID:               10,
			HtlcAttemptIndex: 1,
			HopIndex:         0,
			PubKey:           vertex[:],
			Scid:             "12345",
			OutgoingTimeLock: 111,
			AmtToForward:     555,
		},
	}

	pathKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x31}, 32))
	errorPath := []*btcec.PublicKey{pathKey.PubKey()}
	var encodedErrorPath bytes.Buffer
	errorPathRecord := trampoline.NewErrorPathRecord(&errorPath)
	require.NoError(t, errorPathRecord.Encode(&encodedErrorPath))
	var errorSessionKey [btcec.PrivKeyBytesLen]byte
	copy(errorSessionKey[:], pathKey.Serialize())
	var encodedErrorSessionKey bytes.Buffer
	errorSessionKeyRecord := trampoline.NewErrorSessionKeyRecord(
		&errorSessionKey,
	)
	require.NoError(t, errorSessionKeyRecord.Encode(
		&encodedErrorSessionKey,
	))

	hopCustomRecords := map[int64][]sqlc.PaymentHopCustomRecord{
		10: {
			{ID: 1, HopID: 10, Key: 65536, Value: []byte("val")},
			{
				ID: 2, HopID: 10,
				Key:   int64(trampoline.ErrorPathDBType),
				Value: encodedErrorPath.Bytes(),
			},
			{
				ID: 3, HopID: 10,
				Key: int64(
					trampoline.ErrorSessionKeyDBType,
				),
				Value: encodedErrorSessionKey.Bytes(),
			},
		},
	}

	routeCustomRecords := []sqlc.PaymentAttemptFirstHopCustomRecord{
		{ID: 1, HtlcAttemptIndex: 1, Key: 65537, Value: []byte("rcr")},
	}

	t.Run("include hops populates route fully", func(t *testing.T) {
		t.Parallel()

		attempt, err := dbAttemptToHTLCAttempt(
			baseAttempt, hops, hopCustomRecords,
			routeCustomRecords, true,
		)
		require.NoError(t, err)
		require.Len(t, attempt.Route.Hops, 1)
		require.Equal(t, uint64(12345),
			attempt.Route.Hops[0].ChannelID)
		require.Equal(t,
			record.CustomSet{65536: []byte("val")},
			attempt.Route.Hops[0].CustomRecords,
		)
		require.Equal(t, errorPath,
			attempt.Route.Hops[0].TrampolineErrorPath)
		require.True(t,
			attempt.Route.Hops[0].HasTrampolineErrorSessionKey)
		require.Equal(t, errorSessionKey,
			attempt.Route.Hops[0].TrampolineErrorSessionKey)
		require.Equal(t,
			lnwire.CustomRecords{65537: []byte("rcr")},
			attempt.Route.FirstHopWireCustomRecords,
		)
	})

	t.Run("omit hops skips route data", func(t *testing.T) {
		t.Parallel()

		attempt, err := dbAttemptToHTLCAttempt(
			baseAttempt, nil, nil,
			routeCustomRecords, false,
		)
		require.NoError(t, err)
		require.Nil(t, attempt.Route.Hops)
		require.Nil(t, attempt.Route.FirstHopWireCustomRecords)
		require.Equal(t, uint32(123), attempt.Route.TotalTimeLock)
		require.Equal(t, lnwire.MilliSatoshi(1000),
			attempt.Route.TotalAmount)
	})
}

// TestTrampolineFailureAttemptConversion verifies SQL attempt rows preserve
// an authenticated nested trampoline failure without interpreting it as a
// wire failure message.
func TestTrampolineFailureAttemptConversion(t *testing.T) {
	t.Parallel()

	sessionKey := genSessionKey(t)
	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	failureMsg := bytes.Repeat(
		[]byte{0x6d}, TrampolineFailurePacketSize,
	)
	row := sqlc.FetchHtlcAttemptsForPaymentsRow{
		ID:                 1,
		AttemptIndex:       1,
		PaymentID:          1,
		SessionKey:         sessionKeyBytes[:],
		AttemptTime:        time.Now(),
		PaymentHash:        testHash[:],
		RouteTotalTimeLock: 123,
		RouteTotalAmount:   1000,
		RouteSourceKey:     vertex[:],
		ResolutionType: sql.NullInt32{
			Int32: int32(HTLCAttemptResolutionFailed),
			Valid: true,
		},
		ResolutionTime: sql.NullTime{
			Time: time.Now(), Valid: true,
		},
		HtlcFailReason: sql.NullInt32{
			Int32: int32(HTLCFailTrampoline), Valid: true,
		},
		FailureMsg: failureMsg,
	}
	hops := []sqlc.FetchHopsForAttemptsRow{{
		ID: 10, HtlcAttemptIndex: 1, HopIndex: 0,
		PubKey: vertex[:], Scid: "12345",
		OutgoingTimeLock: 111, AmtToForward: 555,
	}}

	attempt, err := dbAttemptToHTLCAttempt(
		row, hops, nil, nil, true,
	)
	require.NoError(t, err)
	require.NotNil(t, attempt.Failure)
	require.Equal(t, HTLCFailTrampoline, attempt.Failure.Reason)
	require.Equal(t, failureMsg, attempt.Failure.TrampolineReason[:])

	row.FailureMsg = failureMsg[:len(failureMsg)-1]
	_, err = dbAttemptToHTLCAttempt(row, hops, nil, nil, true)
	require.Error(t, err)
}

// TestOmitHopsBuildPayment tests that buildPaymentFromBatchData passes the
// includeHops flag correctly, producing payments with or without hop data
// while preserving payment-level information in both cases.
func TestOmitHopsBuildPayment(t *testing.T) {
	t.Parallel()

	var paymentHash lntypes.Hash
	copy(paymentHash[:], testHash[:])

	sessionKey := genSessionKey(t)
	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	now := time.Now().Truncate(time.Second)

	dbPayment := sqlc.FilterPaymentsRow{
		Payment: sqlc.Payment{
			ID:                1,
			PaymentIdentifier: paymentHash[:],
			AmountMsat:        1000,
			CreatedAt:         now.UTC(),
		},
		IntentPayload: []byte("test_payload"),
	}

	attemptRow := sqlc.FetchHtlcAttemptsForPaymentsRow{
		ID:                 1,
		AttemptIndex:       10,
		PaymentID:          1,
		SessionKey:         sessionKeyBytes[:],
		AttemptTime:        now,
		PaymentHash:        paymentHash[:],
		RouteTotalTimeLock: 123,
		RouteTotalAmount:   1000,
		RouteSourceKey:     vertex[:],
		ResolutionType: sql.NullInt32{
			Int32: int32(HTLCAttemptResolutionSettled),
			Valid: true,
		},
		ResolutionTime: sql.NullTime{
			Time: now, Valid: true,
		},
		SettlePreimage: rev[:],
	}

	hopRow := sqlc.FetchHopsForAttemptsRow{
		ID: 100, HtlcAttemptIndex: 10, HopIndex: 0,
		PubKey: vertex[:], Scid: "12345",
		OutgoingTimeLock: 111, AmtToForward: 555,
	}

	makeBatchData := func(
		withHops bool) *paymentsDetailsData {

		//nolint:ll
		bd := &paymentsDetailsData{
			paymentCustomRecords: make(
				map[int64][]sqlc.PaymentFirstHopCustomRecord,
			),
			attempts: map[int64][]sqlc.FetchHtlcAttemptsForPaymentsRow{
				1: {attemptRow},
			},
			hopsByAttempt: make(
				map[int64][]sqlc.FetchHopsForAttemptsRow,
			),
			hopCustomRecords: make(
				map[int64][]sqlc.PaymentHopCustomRecord,
			),
			routeCustomRecords: make(
				map[int64][]sqlc.PaymentAttemptFirstHopCustomRecord,
			),
		}
		if withHops {
			bd.hopsByAttempt[10] = []sqlc.FetchHopsForAttemptsRow{
				hopRow,
			}
		}

		return bd
	}

	t.Run("include hops builds full payment", func(t *testing.T) {
		t.Parallel()

		mp, err := buildPaymentFromBatchData(
			dbPayment, makeBatchData(true), true,
		)
		require.NoError(t, err)
		require.Len(t, mp.HTLCs, 1)
		require.Len(t, mp.HTLCs[0].Route.Hops, 1)
		require.Equal(t, paymentHash, mp.Info.PaymentIdentifier)
		require.NotNil(t, mp.HTLCs[0].Settle)
	})

	t.Run("omit hops preserves payment info without route data",
		func(t *testing.T) {
			t.Parallel()

			mp, err := buildPaymentFromBatchData(
				dbPayment, makeBatchData(false), false,
			)
			require.NoError(t, err)
			require.Len(t, mp.HTLCs, 1)
			require.Nil(t, mp.HTLCs[0].Route.Hops)
			require.Equal(t, paymentHash,
				mp.Info.PaymentIdentifier)
			require.Equal(t, lnwire.MilliSatoshi(1000),
				mp.Info.Value)
			require.NotNil(t, mp.HTLCs[0].Settle)
		},
	)
}
