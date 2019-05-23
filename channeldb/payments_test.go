package channeldb

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
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

func makeFakePayment() *OutgoingPayment {
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

	fakePayment := &OutgoingPayment{
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

func makeRandomFakePayment() (*OutgoingPayment, error) {
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

	fakePayment := &OutgoingPayment{
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

func TestOutgoingPaymentWorkflow(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	fakePayment := makeFakePayment()
	if err = db.AddPayment(fakePayment); err != nil {
		t.Fatalf("unable to put payment in DB: %v", err)
	}

	payments, err := db.FetchAllPayments()
	if err != nil {
		t.Fatalf("unable to fetch payments from DB: %v", err)
	}

	expectedPayments := []*OutgoingPayment{fakePayment}
	if !reflect.DeepEqual(payments, expectedPayments) {
		t.Fatalf("Wrong payments after reading from DB."+
			"Got %v, want %v",
			spew.Sdump(payments),
			spew.Sdump(expectedPayments),
		)
	}

	// Make some random payments
	for i := 0; i < 5; i++ {
		randomPayment, err := makeRandomFakePayment()
		if err != nil {
			t.Fatalf("Internal error in tests: %v", err)
		}

		if err = db.AddPayment(randomPayment); err != nil {
			t.Fatalf("unable to put payment in DB: %v", err)
		}

		expectedPayments = append(expectedPayments, randomPayment)
	}

	payments, err = db.FetchAllPayments()
	if err != nil {
		t.Fatalf("Can't get payments from DB: %v", err)
	}

	if !reflect.DeepEqual(payments, expectedPayments) {
		t.Fatalf("Wrong payments after reading from DB."+
			"Got %v, want %v",
			spew.Sdump(payments),
			spew.Sdump(expectedPayments),
		)
	}

	// Delete all payments.
	if err = db.DeleteAllPayments(); err != nil {
		t.Fatalf("unable to delete payments from DB: %v", err)
	}

	// Check that there is no payments after deletion
	paymentsAfterDeletion, err := db.FetchAllPayments()
	if err != nil {
		t.Fatalf("Can't get payments after deletion: %v", err)
	}
	if len(paymentsAfterDeletion) != 0 {
		t.Fatalf("After deletion DB has %v payments, want %v",
			len(paymentsAfterDeletion), 0)
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
