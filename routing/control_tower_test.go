package routing

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/routing/route"

	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	priv, _ = btcec.NewPrivateKey(btcec.S256())
	pub     = priv.PubKey()

	testHop = &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
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

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewControlTower(channeldb.NewPaymentControl(db))

	// Subscription should fail when the payment is not known.
	_, _, err = pControl.SubscribePayment(lntypes.Hash{1})
	if err != channeldb.ErrPaymentNotInitiated {
		t.Fatal("expected subscribe to fail for unknown payment")
	}
}

// TestControlTowerSubscribeSuccess tests that payment updates for a
// successful payment are properly sent to subscribers.
func TestControlTowerSubscribeSuccess(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewControlTower(channeldb.NewPaymentControl(db))

	// Initiate a payment.
	info, attempt, preimg, err := genInfo()
	if err != nil {
		t.Fatal(err)
	}

	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatal(err)
	}

	// Subscription should succeed and immediately report the InFlight
	// status.
	inFlight, subscriber1, err := pControl.SubscribePayment(info.PaymentHash)
	if err != nil {
		t.Fatalf("expected subscribe to succeed, but got: %v", err)
	}
	if !inFlight {
		t.Fatalf("unexpected payment to be in flight")
	}

	// Register an attempt.
	err = pControl.RegisterAttempt(info.PaymentHash, attempt)
	if err != nil {
		t.Fatal(err)
	}

	// Register a second subscriber after the first attempt has started.
	inFlight, subscriber2, err := pControl.SubscribePayment(info.PaymentHash)
	if err != nil {
		t.Fatalf("expected subscribe to succeed, but got: %v", err)
	}
	if !inFlight {
		t.Fatalf("unexpected payment to be in flight")
	}

	// Mark the payment as successful.
	if err := pControl.Success(info.PaymentHash, preimg); err != nil {
		t.Fatal(err)
	}

	// Register a third subscriber after the payment succeeded.
	inFlight, subscriber3, err := pControl.SubscribePayment(info.PaymentHash)
	if err != nil {
		t.Fatalf("expected subscribe to succeed, but got: %v", err)
	}
	if inFlight {
		t.Fatalf("expected payment to be finished")
	}

	// We expect all subscribers to now report the final outcome followed by
	// no other events.
	subscribers := []chan PaymentResult{
		subscriber1, subscriber2, subscriber3,
	}

	for _, s := range subscribers {
		var result PaymentResult
		select {
		case result = <-s:
		case <-time.After(testTimeout):
			t.Fatal("timeout waiting for payment result")
		}

		if !result.Success {
			t.Fatal("unexpected payment state")
		}
		if result.Preimage != preimg {
			t.Fatal("unexpected preimage")
		}
		if !reflect.DeepEqual(result.Route, &attempt.Route) {
			t.Fatal("unexpected route")
		}

		// After the final event, we expect the channel to be closed.
		select {
		case _, ok := <-s:
			if ok {
				t.Fatal("expected channel to be closed")
			}
		case <-time.After(testTimeout):
			t.Fatal("timeout waiting for result channel close")
		}
	}
}

// TestPaymentControlSubscribeFail tests that payment updates for a
// failed payment are properly sent to subscribers.
func TestPaymentControlSubscribeFail(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewControlTower(channeldb.NewPaymentControl(db))

	// Initiate a payment.
	info, _, _, err := genInfo()
	if err != nil {
		t.Fatal(err)
	}

	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatal(err)
	}

	// Subscription should succeed.
	_, subscriber1, err := pControl.SubscribePayment(info.PaymentHash)
	if err != nil {
		t.Fatalf("expected subscribe to succeed, but got: %v", err)
	}

	// Mark the payment as failed.
	if err := pControl.Fail(info.PaymentHash, channeldb.FailureReasonTimeout); err != nil {
		t.Fatal(err)
	}

	// Register a second subscriber after the payment failed.
	inFlight, subscriber2, err := pControl.SubscribePayment(info.PaymentHash)
	if err != nil {
		t.Fatalf("expected subscribe to succeed, but got: %v", err)
	}
	if inFlight {
		t.Fatalf("expected payment to be finished")
	}

	// We expect all subscribers to now report the final outcome followed by
	// no other events.
	subscribers := []chan PaymentResult{
		subscriber1, subscriber2,
	}

	for _, s := range subscribers {
		var result PaymentResult
		select {
		case result = <-s:
		case <-time.After(testTimeout):
			t.Fatal("timeout waiting for payment result")
		}

		if result.Success {
			t.Fatal("unexpected payment state")
		}
		if result.Route != nil {
			t.Fatal("expected no route")
		}
		if result.FailureReason != channeldb.FailureReasonTimeout {
			t.Fatal("unexpected failure reason")
		}

		// After the final event, we expect the channel to be closed.
		select {
		case _, ok := <-s:
			if ok {
				t.Fatal("expected channel to be closed")
			}
		case <-time.After(testTimeout):
			t.Fatal("timeout waiting for result channel close")
		}
	}
}

func initDB() (*channeldb.DB, error) {
	tempPath, err := ioutil.TempDir("", "routingdb")
	if err != nil {
		return nil, err
	}

	db, err := channeldb.Open(tempPath)
	if err != nil {
		return nil, err
	}

	return db, err
}

func genInfo() (*channeldb.PaymentCreationInfo, *channeldb.PaymentAttemptInfo,
	lntypes.Preimage, error) {

	preimage, err := genPreimage()
	if err != nil {
		return nil, nil, preimage, fmt.Errorf("unable to "+
			"generate preimage: %v", err)
	}

	rhash := sha256.Sum256(preimage[:])
	return &channeldb.PaymentCreationInfo{
			PaymentHash:    rhash,
			Value:          1,
			CreationDate:   time.Unix(time.Now().Unix(), 0),
			PaymentRequest: []byte("hola"),
		},
		&channeldb.PaymentAttemptInfo{
			PaymentID:  1,
			SessionKey: priv,
			Route:      testRoute,
		}, preimage, nil
}

func genPreimage() ([32]byte, error) {
	var preimage [32]byte
	if _, err := io.ReadFull(rand.Reader, preimage[:]); err != nil {
		return preimage, err
	}
	return preimage, nil
}
