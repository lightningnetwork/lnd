package channeldb

import (
	"bytes"
	crand "crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/fastsha256"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
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
)

func initDB() (*DB, error) {
	tempPath, err := ioutil.TempDir("", "switchdb")
	if err != nil {
		return nil, err
	}

	db, err := Open(tempPath)
	if err != nil {
		return nil, err
	}

	return db, err
}

func genPreimage() ([32]byte, error) {
	var preimage [32]byte
	if _, err := io.ReadFull(crand.Reader, preimage[:]); err != nil {
		return preimage, err
	}
	return preimage, nil
}

func genInfo() (*PaymentCreationInfo, *PaymentAttemptInfo,
	lntypes.Preimage, error) {

	preimage, err := genPreimage()
	if err != nil {
		return nil, nil, preimage, fmt.Errorf("unable to "+
			"generate preimage: %v", err)
	}

	rhash := fastsha256.Sum256(preimage[:])
	return &PaymentCreationInfo{
			PaymentHash:    rhash,
			Value:          1,
			CreationDate:   time.Unix(time.Now().Unix(), 0),
			PaymentRequest: []byte("hola"),
		},
		&PaymentAttemptInfo{
			PaymentID:  1,
			SessionKey: priv,
			Route:      testRoute,
		}, preimage, nil
}

func makeFakePayment() *outgoingPayment {
	fakeInvoice := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate:   time.Unix(time.Now().Unix(), 0),
		Memo:           []byte("fake memo"),
		Receipt:        []byte("fake receipt"),
		PaymentRequest: []byte(""),
	}

	copy(fakeInvoice.Terms.PaymentPreimage[:], rev[:])
	fakeInvoice.Terms.Value = lnwire.NewMSatFromSatoshis(10000)

	fakePath := make([][33]byte, 3)
	for i := 0; i < 3; i++ {
		copy(fakePath[i][:], bytes.Repeat([]byte{byte(i)}, 33))
	}

	fakePayment := &outgoingPayment{
		Invoice:        *fakeInvoice,
		Fee:            101,
		Path:           fakePath,
		TimeLockLength: 1000,
	}
	copy(fakePayment.PaymentPreimage[:], rev[:])
	return fakePayment
}

func makeFakeInfo() (*PaymentCreationInfo, *PaymentAttemptInfo) {
	var preimg lntypes.Preimage
	copy(preimg[:], rev[:])

	c := &PaymentCreationInfo{
		PaymentHash: preimg.Hash(),
		Value:       1000,
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate:   time.Unix(time.Now().Unix(), 0),
		PaymentRequest: []byte(""),
	}

	a := &PaymentAttemptInfo{
		PaymentID:  44,
		SessionKey: priv,
		Route:      testRoute,
	}
	return c, a
}

func makeFakePaymentHash() [32]byte {
	var paymentHash [32]byte
	rBytes, _ := randomBytes(0, 32)
	copy(paymentHash[:], rBytes)

	return paymentHash
}

// randomBytes creates random []byte with length in range [minLen, maxLen)
func randomBytes(minLen, maxLen int) ([]byte, error) {
	randBuf := make([]byte, minLen+rand.Intn(maxLen-minLen))

	if _, err := rand.Read(randBuf); err != nil {
		return nil, fmt.Errorf("Internal error. "+
			"Cannot generate random string: %v", err)
	}

	return randBuf, nil
}

func makeRandomFakePayment() (*outgoingPayment, error) {
	var err error
	fakeInvoice := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate: time.Unix(time.Now().Unix(), 0),
	}

	fakeInvoice.Memo, err = randomBytes(1, 50)
	if err != nil {
		return nil, err
	}

	fakeInvoice.Receipt, err = randomBytes(1, 50)
	if err != nil {
		return nil, err
	}

	fakeInvoice.PaymentRequest, err = randomBytes(1, 50)
	if err != nil {
		return nil, err
	}

	preImg, err := randomBytes(32, 33)
	if err != nil {
		return nil, err
	}
	copy(fakeInvoice.Terms.PaymentPreimage[:], preImg)

	fakeInvoice.Terms.Value = lnwire.MilliSatoshi(rand.Intn(10000))

	fakePathLen := 1 + rand.Intn(5)
	fakePath := make([][33]byte, fakePathLen)
	for i := 0; i < fakePathLen; i++ {
		b, err := randomBytes(33, 34)
		if err != nil {
			return nil, err
		}
		copy(fakePath[i][:], b)
	}

	fakePayment := &outgoingPayment{
		Invoice:        *fakeInvoice,
		Fee:            lnwire.MilliSatoshi(rand.Intn(1001)),
		Path:           fakePath,
		TimeLockLength: uint32(rand.Intn(10000)),
	}
	copy(fakePayment.PaymentPreimage[:], fakeInvoice.Terms.PaymentPreimage[:])

	return fakePayment, nil
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
	if err := serializePaymentAttemptInfo(&b, s); err != nil {
		t.Fatalf("unable to serialize info: %v", err)
	}

	newAttemptInfo, err := deserializePaymentAttemptInfo(&b)
	if err != nil {
		t.Fatalf("unable to deserialize info: %v", err)
	}

	if !reflect.DeepEqual(s, newAttemptInfo) {
		t.Fatalf("Payments do not match after "+
			"serialization/deserialization %v vs %v",
			spew.Sdump(s), spew.Sdump(newAttemptInfo),
		)
	}

}

func TestRouteSerialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	if err := serializeRoute(&b, testRoute); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(b.Bytes())
	route2, err := deserializeRoute(r)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(testRoute, route2) {
		t.Fatalf("routes not equal: \n%v vs \n%v",
			spew.Sdump(testRoute), spew.Sdump(route2))
	}

}

// TestPaymentControlSwitchFail checks that payment status returns to Failed
// status after failing, and that InitPayment allows another HTLC for the
// same payment hash.
func TestPaymentControlSwitchFail(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, attempt, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, nil, lntypes.Preimage{},
		nil,
	)

	// Fail the payment, which should moved it to Failed.
	failReason := FailureReasonNoRoute
	err = pControl.Fail(info.PaymentHash, failReason)
	if err != nil {
		t.Fatalf("unable to fail payment hash: %v", err)
	}

	// Verify the status is indeed Failed.
	assertPaymentStatus(t, db, info.PaymentHash, StatusFailed)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, nil, lntypes.Preimage{},
		&failReason,
	)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, nil, lntypes.Preimage{},
		nil,
	)

	// Record a new attempt.
	attempt.PaymentID = 2
	err = pControl.RegisterAttempt(info.PaymentHash, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}
	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, attempt, lntypes.Preimage{},
		nil,
	)

	// Verifies that status was changed to StatusSucceeded.
	if err := pControl.Success(info.PaymentHash, preimg); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusSucceeded)
	assertPaymentInfo(t, db, info.PaymentHash, info, attempt, preimg, nil)

	// Attempt a final payment, which should now fail since the prior
	// payment succeed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// TestPaymentControlSwitchDoubleSend checks the ability of payment control to
// prevent double sending of htlc message, when message is in StatusInFlight.
func TestPaymentControlSwitchDoubleSend(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, attempt, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate base status and move it to
	// StatusInFlight and verifies that it was changed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, nil, lntypes.Preimage{},
		nil,
	)

	// Try to initiate double sending of htlc message with the same
	// payment hash, should result in error indicating that payment has
	// already been sent.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}

	// Record an attempt.
	err = pControl.RegisterAttempt(info.PaymentHash, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}
	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, attempt, lntypes.Preimage{},
		nil,
	)

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}

	// After settling, the error should be ErrAlreadyPaid.
	if err := pControl.Success(info.PaymentHash, preimg); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}
	assertPaymentStatus(t, db, info.PaymentHash, StatusSucceeded)
	assertPaymentInfo(t, db, info.PaymentHash, info, attempt, preimg, nil)

	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// TestPaymentControlSuccessesWithoutInFlight checks that the payment
// control will disallow calls to Success when no payment is in flight.
func TestPaymentControlSuccessesWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Attempt to complete the payment should fail.
	err = pControl.Success(info.PaymentHash, preimg)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusUnknown)
	assertPaymentInfo(
		t, db, info.PaymentHash, nil, nil, lntypes.Preimage{},
		nil,
	)
}

// TestPaymentControlFailsWithoutInFlight checks that a strict payment
// control will disallow calls to Fail when no payment is in flight.
func TestPaymentControlFailsWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, _, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Calling Fail should return an error.
	err = pControl.Fail(info.PaymentHash, FailureReasonNoRoute)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusUnknown)
	assertPaymentInfo(
		t, db, info.PaymentHash, nil, nil, lntypes.Preimage{}, nil,
	)
}

func assertPaymentStatus(t *testing.T, db *DB,
	hash [32]byte, expStatus PaymentStatus) {

	t.Helper()

	var paymentStatus = StatusUnknown
	err := db.View(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		bucket := payments.Bucket(hash[:])
		if bucket == nil {
			return nil
		}

		// Get the existing status of this payment, if any.
		paymentStatus = fetchPaymentStatus(bucket)
		return nil
	})
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if paymentStatus != expStatus {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			expStatus, paymentStatus)
	}
}

func checkPaymentCreationInfo(bucket *bbolt.Bucket, c *PaymentCreationInfo) error {
	b := bucket.Get(paymentCreationInfoKey)
	switch {
	case b == nil && c == nil:
		return nil
	case b == nil:
		return fmt.Errorf("expected creation info not found")
	case c == nil:
		return fmt.Errorf("unexpected creation info found")
	}

	r := bytes.NewReader(b)
	c2, err := deserializePaymentCreationInfo(r)
	if err != nil {
		fmt.Println("creation info err: ", err)
		return err
	}
	if !reflect.DeepEqual(c, c2) {
		return fmt.Errorf("PaymentCreationInfos don't match: %v vs %v",
			spew.Sdump(c), spew.Sdump(c2))
	}

	return nil
}

func checkPaymentAttemptInfo(bucket *bbolt.Bucket, a *PaymentAttemptInfo) error {
	b := bucket.Get(paymentAttemptInfoKey)
	switch {
	case b == nil && a == nil:
		return nil
	case b == nil:
		return fmt.Errorf("expected attempt info not found")
	case a == nil:
		return fmt.Errorf("unexpected attempt info found")
	}

	r := bytes.NewReader(b)
	a2, err := deserializePaymentAttemptInfo(r)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, a2) {
		return fmt.Errorf("PaymentAttemptInfos don't match: %v vs %v",
			spew.Sdump(a), spew.Sdump(a2))
	}

	return nil
}

func checkSettleInfo(bucket *bbolt.Bucket, preimg lntypes.Preimage) error {
	zero := lntypes.Preimage{}
	b := bucket.Get(paymentSettleInfoKey)
	switch {
	case b == nil && preimg == zero:
		return nil
	case b == nil:
		return fmt.Errorf("expected preimage not found")
	case preimg == zero:
		return fmt.Errorf("unexpected preimage found")
	}

	var pre2 lntypes.Preimage
	copy(pre2[:], b[:])
	if preimg != pre2 {
		return fmt.Errorf("Preimages don't match: %x vs %x",
			preimg, pre2)
	}

	return nil
}

func checkFailInfo(bucket *bbolt.Bucket, failReason *FailureReason) error {
	b := bucket.Get(paymentFailInfoKey)
	switch {
	case b == nil && failReason == nil:
		return nil
	case b == nil:
		return fmt.Errorf("expected fail info not found")
	case failReason == nil:
		return fmt.Errorf("unexpected fail info found")
	}

	failReason2 := FailureReason(b[0])
	if *failReason != failReason2 {
		return fmt.Errorf("Failure infos don't match: %v vs %v",
			*failReason, failReason2)
	}

	return nil
}

func assertPaymentInfo(t *testing.T, db *DB, hash lntypes.Hash,
	c *PaymentCreationInfo, a *PaymentAttemptInfo, s lntypes.Preimage,
	f *FailureReason) {

	t.Helper()

	err := db.View(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(paymentsRootBucket)
		if payments == nil && c == nil {
			return nil
		}
		if payments == nil {
			return fmt.Errorf("sent payments not found")
		}

		bucket := payments.Bucket(hash[:])
		if bucket == nil && c == nil {
			return nil
		}

		if bucket == nil {
			return fmt.Errorf("payment not found")
		}

		if err := checkPaymentCreationInfo(bucket, c); err != nil {
			return err
		}

		if err := checkPaymentAttemptInfo(bucket, a); err != nil {
			return err
		}

		if err := checkSettleInfo(bucket, s); err != nil {
			return err
		}

		if err := checkFailInfo(bucket, f); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("assert payment info failed: %v", err)
	}

}
