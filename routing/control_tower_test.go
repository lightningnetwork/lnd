package routing

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	priv, _ = btcec.NewPrivateKey()
	pub     = priv.PubKey()

	testHop = &route.Hop{
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
			testHop,
			testHop,
		},
	}

	testTimeout = 5 * time.Second
)

// TestControlTowerSubscribeUnknown tests that subscribing to an unknown
// payment fails.
func TestControlTowerSubscribeUnknown(t *testing.T) {
	t.Parallel()

	db := initDB(t)

	paymentDB, err := paymentsdb.NewKVStore(
		db,
		paymentsdb.WithKeepFailedPaymentAttempts(true),
	)
	require.NoError(t, err)

	pControl := NewControlTower(paymentDB)

	// Subscription should fail when the payment is not known.
	_, err = pControl.SubscribePayment(lntypes.Hash{1})
	require.ErrorIs(t, err, paymentsdb.ErrPaymentNotInitiated)
}

// TestControlTowerSubscribeSuccess tests that payment updates for a
// successful payment are properly sent to subscribers.
func TestControlTowerSubscribeSuccess(t *testing.T) {
	t.Parallel()

	db := initDB(t)

	paymentDB, err := paymentsdb.NewKVStore(db)
	require.NoError(t, err)

	pControl := NewControlTower(paymentDB)

	// Initiate a payment.
	info, attempt, preimg, err := genInfo()
	if err != nil {
		t.Fatal(err)
	}

	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != nil {
		t.Fatal(err)
	}

	// Subscription should succeed and immediately report the InFlight
	// status.
	subscriber1, err := pControl.SubscribePayment(info.PaymentIdentifier)
	require.NoError(t, err, "expected subscribe to succeed, but got")

	// Register an attempt.
	err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	if err != nil {
		t.Fatal(err)
	}

	// Register a second subscriber after the first attempt has started.
	subscriber2, err := pControl.SubscribePayment(info.PaymentIdentifier)
	require.NoError(t, err, "expected subscribe to succeed, but got")

	// Mark the payment as successful.
	settleInfo := paymentsdb.HTLCSettleInfo{
		Preimage: preimg,
	}
	htlcAttempt, err := pControl.SettleAttempt(
		info.PaymentIdentifier, attempt.AttemptID, &settleInfo,
	)
	if err != nil {
		t.Fatal(err)
	}
	if *htlcAttempt.Settle != settleInfo {
		t.Fatalf("unexpected settle info returned")
	}

	// Register a third subscriber after the payment succeeded.
	subscriber3, err := pControl.SubscribePayment(info.PaymentIdentifier)
	require.NoError(t, err, "expected subscribe to succeed, but got")

	// We expect all subscribers to now report the final outcome followed by
	// no other events.
	subscribers := []ControlTowerSubscriber{
		subscriber1, subscriber2, subscriber3,
	}

	for i, s := range subscribers {
		var result *paymentsdb.MPPayment
		for result == nil || !result.Terminated() {
			select {
			case item := <-s.Updates():
				payment, ok := item.(*paymentsdb.MPPayment)
				require.True(
					t, ok, "unexpected payment type: %T",
					item)

				result = payment

			case <-time.After(testTimeout):
				t.Fatal("timeout waiting for payment result")
			}
		}

		require.Equalf(t, paymentsdb.StatusSucceeded,
			result.GetStatus(), "subscriber %v failed, want %s, "+
				"got %s", i, paymentsdb.StatusSucceeded,
			result.GetStatus(),
		)

		attempt, _ := result.TerminalInfo()
		if attempt.Settle.Preimage != preimg {
			t.Fatal("unexpected preimage")
		}
		if len(result.HTLCs) != 1 {
			t.Fatalf("expected one htlc, got %d", len(result.HTLCs))
		}
		htlc := result.HTLCs[0]
		if !reflect.DeepEqual(htlc.Route, attempt.Route) {
			t.Fatalf("unexpected htlc route: %v vs %v",
				spew.Sdump(htlc.Route),
				spew.Sdump(attempt.Route))
		}

		// After the final event, we expect the channel to be closed.
		select {
		case _, ok := <-s.Updates():
			if ok {
				t.Fatal("expected channel to be closed")
			}
		case <-time.After(testTimeout):
			t.Fatal("timeout waiting for result channel close")
		}
	}
}

// TestKVStoreSubscribeFail tests that payment updates for a
// failed payment are properly sent to subscribers.
func TestKVStoreSubscribeFail(t *testing.T) {
	t.Parallel()

	t.Run("register attempt, keep failed payments", func(t *testing.T) {
		testKVStoreSubscribeFail(t, true, true)
	})
	t.Run("register attempt, delete failed payments", func(t *testing.T) {
		testKVStoreSubscribeFail(t, true, false)
	})
	t.Run("no register attempt, keep failed payments", func(t *testing.T) {
		testKVStoreSubscribeFail(t, false, true)
	})
	t.Run("no register attempt, delete failed payments", func(t *testing.T) {
		testKVStoreSubscribeFail(t, false, false)
	})
}

// TestKVStoreSubscribeAllSuccess tests that multiple payments are
// properly sent to subscribers of TrackPayments.
func TestKVStoreSubscribeAllSuccess(t *testing.T) {
	t.Parallel()

	db := initDB(t)

	paymentDB, err := paymentsdb.NewKVStore(
		db,
		paymentsdb.WithKeepFailedPaymentAttempts(true),
	)
	require.NoError(t, err)

	pControl := NewControlTower(paymentDB)

	// Initiate a payment.
	info1, attempt1, preimg1, err := genInfo()
	require.NoError(t, err)

	err = pControl.InitPayment(info1.PaymentIdentifier, info1)
	require.NoError(t, err)

	// Subscription should succeed and immediately report the Initiated
	// status.
	subscription, err := pControl.SubscribeAllPayments()
	require.NoError(t, err, "expected subscribe to succeed, but got: %v")

	// Register an attempt.
	err = pControl.RegisterAttempt(info1.PaymentIdentifier, attempt1)
	require.NoError(t, err)

	// Initiate a second payment after the subscription is already active.
	info2, attempt2, preimg2, err := genInfo()
	require.NoError(t, err)

	err = pControl.InitPayment(info2.PaymentIdentifier, info2)
	require.NoError(t, err)

	// Register an attempt on the second payment.
	err = pControl.RegisterAttempt(info2.PaymentIdentifier, attempt2)
	require.NoError(t, err)

	// Mark the first payment as successful.
	settleInfo1 := paymentsdb.HTLCSettleInfo{
		Preimage: preimg1,
	}
	htlcAttempt1, err := pControl.SettleAttempt(
		info1.PaymentIdentifier, attempt1.AttemptID, &settleInfo1,
	)
	require.NoError(t, err)
	require.Equal(
		t, settleInfo1, *htlcAttempt1.Settle,
		"unexpected settle info returned",
	)

	// Mark the second payment as successful.
	settleInfo2 := paymentsdb.HTLCSettleInfo{
		Preimage: preimg2,
	}
	htlcAttempt2, err := pControl.SettleAttempt(
		info2.PaymentIdentifier, attempt2.AttemptID, &settleInfo2,
	)
	require.NoError(t, err)
	require.Equal(
		t, settleInfo2, *htlcAttempt2.Settle,
		"unexpected fail info returned",
	)

	// The two payments will be asserted individually, store the last update
	// for each payment.
	results := make(map[lntypes.Hash]*paymentsdb.MPPayment)

	// After exactly 6 updates both payments will/should have completed.
	for i := 0; i < 6; i++ {
		select {
		case item := <-subscription.Updates():
			payment, ok := item.(*paymentsdb.MPPayment)
			require.True(
				t, ok, "unexpected payment type: %T",
				item)

			id := payment.Info.PaymentIdentifier
			results[id] = payment

		case <-time.After(testTimeout):
			require.Fail(t, "timeout waiting for payment result")
		}
	}

	result1 := results[info1.PaymentIdentifier]
	require.Equal(
		t, paymentsdb.StatusSucceeded, result1.GetStatus(),
		"unexpected payment state payment 1",
	)

	settle1, _ := result1.TerminalInfo()
	require.Equal(t, preimg1, settle1.Settle.Preimage,
		"unexpected preimage payment 1")

	require.Len(
		t, result1.HTLCs, 1, "expect 1 htlc for payment 1, got %d",
		len(result1.HTLCs),
	)

	htlc1 := result1.HTLCs[0]
	require.Equal(t, attempt1.Route, htlc1.Route, "unexpected htlc route.")

	result2 := results[info2.PaymentIdentifier]
	require.Equal(
		t, paymentsdb.StatusSucceeded, result2.GetStatus(),
		"unexpected payment state payment 2",
	)

	settle2, _ := result2.TerminalInfo()
	require.Equal(t, preimg2, settle2.Settle.Preimage,
		"unexpected preimage payment 2")
	require.Len(
		t, result2.HTLCs, 1, "expect 1 htlc for payment 2, got %d",
		len(result2.HTLCs),
	)

	htlc2 := result2.HTLCs[0]
	require.Equal(t, attempt2.Route, htlc2.Route, "unexpected htlc route.")
}

// TestKVStoreSubscribeAllImmediate tests whether already inflight
// payments are reported at the start of the SubscribeAllPayments subscription.
func TestKVStoreSubscribeAllImmediate(t *testing.T) {
	t.Parallel()

	db := initDB(t)

	paymentDB, err := paymentsdb.NewKVStore(
		db,
		paymentsdb.WithKeepFailedPaymentAttempts(true),
	)
	require.NoError(t, err)

	pControl := NewControlTower(paymentDB)

	// Initiate a payment.
	info, attempt, _, err := genInfo()
	require.NoError(t, err)

	err = pControl.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err)

	// Register a payment update.
	err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	require.NoError(t, err)

	subscription, err := pControl.SubscribeAllPayments()
	require.NoError(t, err, "expected subscribe to succeed, but got: %v")

	// Assert the new subscription receives the old update.
	select {
	case update := <-subscription.Updates():
		require.NotNil(t, update)
		payment, ok := update.(*paymentsdb.MPPayment)
		if !ok {
			t.Fatalf("unexpected payment type: %T", update)
		}

		require.Equal(
			t, info.PaymentIdentifier,
			payment.Info.PaymentIdentifier,
		)
		require.Len(t, subscription.Updates(), 0)

	case <-time.After(testTimeout):
		require.Fail(t, "timeout waiting for payment result")
	}
}

// TestKVStoreUnsubscribeSuccess tests that when unsubscribed, there are
// no more notifications to that specific subscription.
func TestKVStoreUnsubscribeSuccess(t *testing.T) {
	t.Parallel()

	db := initDB(t)

	paymentDB, err := paymentsdb.NewKVStore(
		db,
		paymentsdb.WithKeepFailedPaymentAttempts(true),
	)
	require.NoError(t, err)

	pControl := NewControlTower(paymentDB)

	subscription1, err := pControl.SubscribeAllPayments()
	require.NoError(t, err, "expected subscribe to succeed, but got: %v")

	subscription2, err := pControl.SubscribeAllPayments()
	require.NoError(t, err, "expected subscribe to succeed, but got: %v")

	// Initiate a payment.
	info, attempt, _, err := genInfo()
	require.NoError(t, err)

	err = pControl.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err)

	// Assert all subscriptions receive the update.
	select {
	case update1 := <-subscription1.Updates():
		require.NotNil(t, update1)
	case <-time.After(testTimeout):
		require.Fail(t, "timeout waiting for payment result")
	}

	select {
	case update2 := <-subscription2.Updates():
		require.NotNil(t, update2)
	case <-time.After(testTimeout):
		require.Fail(t, "timeout waiting for payment result")
	}

	// Close the first subscription.
	subscription1.Close()

	// Register a payment update.
	err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	require.NoError(t, err)

	// Assert only subscription 2 receives the update.
	select {
	case update2 := <-subscription2.Updates():
		require.NotNil(t, update2)
	case <-time.After(testTimeout):
		require.Fail(t, "timeout waiting for payment result")
	}

	require.Len(t, subscription1.Updates(), 0)

	// Close the second subscription.
	subscription2.Close()

	// Register another update.
	failInfo := paymentsdb.HTLCFailInfo{
		Reason: paymentsdb.HTLCFailInternal,
	}
	_, err = pControl.FailAttempt(
		info.PaymentIdentifier, attempt.AttemptID, &failInfo,
	)
	require.NoError(t, err, "unable to fail htlc")

	// Assert no subscriptions receive the update.
	require.Len(t, subscription1.Updates(), 0)
	require.Len(t, subscription2.Updates(), 0)
}

func testKVStoreSubscribeFail(t *testing.T, registerAttempt,
	keepFailedPaymentAttempts bool) {

	db := initDB(t)

	paymentDB, err := paymentsdb.NewKVStore(
		db,
		paymentsdb.WithKeepFailedPaymentAttempts(
			keepFailedPaymentAttempts,
		),
	)
	require.NoError(t, err)

	pControl := NewControlTower(paymentDB)

	// Initiate a payment.
	info, attempt, _, err := genInfo()
	if err != nil {
		t.Fatal(err)
	}

	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != nil {
		t.Fatal(err)
	}

	// Subscription should succeed.
	subscriber1, err := pControl.SubscribePayment(info.PaymentIdentifier)
	require.NoError(t, err, "expected subscribe to succeed, but got")

	// Conditionally register the attempt based on the test type. This
	// allows us to simulate failing after attempting with an htlc or before
	// making any attempts at all.
	if registerAttempt {
		// Register an attempt.
		err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
		if err != nil {
			t.Fatal(err)
		}

		// Fail the payment attempt.
		failInfo := paymentsdb.HTLCFailInfo{
			Reason: paymentsdb.HTLCFailInternal,
		}
		htlcAttempt, err := pControl.FailAttempt(
			info.PaymentIdentifier, attempt.AttemptID, &failInfo,
		)
		if err != nil {
			t.Fatalf("unable to fail htlc: %v", err)
		}
		if *htlcAttempt.Failure != failInfo {
			t.Fatalf("unexpected fail info returned")
		}
	}

	// Mark the payment as failed.
	err = pControl.FailPayment(
		info.PaymentIdentifier, paymentsdb.FailureReasonTimeout,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Register a second subscriber after the payment failed.
	subscriber2, err := pControl.SubscribePayment(info.PaymentIdentifier)
	require.NoError(t, err, "expected subscribe to succeed, but got")

	// We expect both subscribers to now report the final outcome followed
	// by no other events.
	subscribers := []ControlTowerSubscriber{
		subscriber1, subscriber2,
	}

	for i, s := range subscribers {
		var result *paymentsdb.MPPayment
		for result == nil || !result.Terminated() {
			select {
			case item := <-s.Updates():
				payment, ok := item.(*paymentsdb.MPPayment)
				require.True(
					t, ok, "unexpected payment type: %T",
					item)

				result = payment
			case <-time.After(testTimeout):
				t.Fatal("timeout waiting for payment result")
			}
		}

		if result.GetStatus() == paymentsdb.StatusSucceeded {
			t.Fatal("unexpected payment state")
		}

		// There will either be one or zero htlcs depending on whether
		// or not the attempt was registered. Assert the correct number
		// is present, and the route taken if the attempt was
		// registered.
		if registerAttempt {
			if len(result.HTLCs) != 1 {
				t.Fatalf("expected 1 htlc, got: %d",
					len(result.HTLCs))
			}

			htlc := result.HTLCs[0]
			if !reflect.DeepEqual(htlc.Route, testRoute) {
				t.Fatalf("unexpected htlc route: %v vs %v",
					spew.Sdump(htlc.Route),
					spew.Sdump(testRoute))
			}
		} else if len(result.HTLCs) != 0 {
			t.Fatalf("expected 0 htlcs, got: %d",
				len(result.HTLCs))
		}

		require.Equalf(t, paymentsdb.StatusFailed, result.GetStatus(),
			"subscriber %v failed, want %s, got %s", i,
			paymentsdb.StatusFailed, result.GetStatus())

		if *result.FailureReason != paymentsdb.FailureReasonTimeout {
			t.Fatal("unexpected failure reason")
		}

		// After the final event, we expect the channel to be closed.
		select {
		case _, ok := <-s.Updates():
			if ok {
				t.Fatal("expected channel to be closed")
			}
		case <-time.After(testTimeout):
			t.Fatal("timeout waiting for result channel close")
		}
	}
}

func initDB(t *testing.T) *channeldb.DB {
	return channeldb.OpenForTesting(
		t, t.TempDir(),
	)
}

func genInfo() (*paymentsdb.PaymentCreationInfo, *paymentsdb.HTLCAttemptInfo,
	lntypes.Preimage, error) {

	preimage, err := genPreimage()
	if err != nil {
		return nil, nil, preimage, fmt.Errorf("unable to "+
			"generate preimage: %v", err)
	}

	rhash := sha256.Sum256(preimage[:])
	var hash lntypes.Hash
	copy(hash[:], rhash[:])

	attempt, err := paymentsdb.NewHtlcAttempt(
		1, priv, testRoute, time.Time{}, &hash,
	)
	if err != nil {
		return nil, nil, lntypes.Preimage{}, err
	}

	return &paymentsdb.PaymentCreationInfo{
			PaymentIdentifier: rhash,
			Value:             testRoute.ReceiverAmt(),
			CreationTime:      time.Unix(time.Now().Unix(), 0),
			PaymentRequest:    []byte("hola"),
		},
		&attempt.HTLCAttemptInfo, preimage, nil
}

func genPreimage() ([32]byte, error) {
	var preimage [32]byte
	if _, err := io.ReadFull(rand.Reader, preimage[:]); err != nil {
		return preimage, err
	}
	return preimage, nil
}
