package channeldb

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
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

	fakeInvoice.PaymentRequest = []byte("")

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

func TestOutgoingPaymentSerialization(t *testing.T) {
	t.Parallel()

	fakePayment := makeFakePayment()

	var b bytes.Buffer
	if err := serializeOutgoingPayment(&b, fakePayment); err != nil {
		t.Fatalf("unable to serialize outgoing payment: %v", err)
	}

	newPayment, err := deserializeOutgoingPayment(&b)
	if err != nil {
		t.Fatalf("unable to deserialize outgoing payment: %v", err)
	}

	if !reflect.DeepEqual(fakePayment, newPayment) {
		t.Fatalf("Payments do not match after "+
			"serialization/deserialization %v vs %v",
			spew.Sdump(fakePayment),
			spew.Sdump(newPayment),
		)
	}
}

func TestPaymentAddedSince(t *testing.T) {
	var err error
	var expectedPayments []*OutgoingPayment
	var fetchedPayments []*OutgoingPayment

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	for i := 0; i < 5; i++ {
		randomPayment, err := makeRandomFakePayment()
		if err != nil {
			t.Fatalf("Internal error in tests: %v", err)
		}

		if _, err = db.AddPayment(randomPayment); err != nil {
			t.Fatalf("unable to put payment in DB: %v", err)
		}

		expectedPayments = append(expectedPayments, randomPayment)
	}

	//index = 0 supposed to fetch all payments
	fetchedPayments, err = db.PaymentsAddedSince(0)
	if err != nil {
		t.Fatalf("Can't get payments from DB: %v", err)
	}

	if !reflect.DeepEqual(fetchedPayments, expectedPayments) {
		t.Fatalf("Wrong payments after reading from DB."+
			"Got %v, want %v",
			spew.Sdump(fetchedPayments),
			spew.Sdump(expectedPayments),
		)
	}

	//index = 1 suppose to retrieve first 4 payments
	fetchedPayments, err = db.PaymentsAddedSince(1)
	if err != nil {
		t.Fatalf("Can't get payments from DB: %v", err)
	}

	if !reflect.DeepEqual(fetchedPayments, expectedPayments[1:]) {
		t.Fatalf("Wrong payments after reading from DB."+
			"Got %v, want %v",
			spew.Sdump(fetchedPayments),
			spew.Sdump(expectedPayments),
		)
	}

	//index = 5, suppose to return nil
	fetchedPayments, err = db.PaymentsAddedSince(5)
	if err != nil {
		t.Fatalf("Can't get payments from DB: %v", err)
	}

	if fetchedPayments != nil {
		t.Fatalf("Wrong payments after reading from DB."+
			"Got %v, want nil",
			spew.Sdump(fetchedPayments),
		)
	}

	// Delete all payments.
	if err = db.DeleteAllPayments(); err != nil {
		t.Fatalf("unable to delete payments from DB: %v", err)
	}

	newPayment, err := makeRandomFakePayment()
	if err != nil {
		t.Fatalf("Internal error in tests: %v", err)
	}

	if _, err = db.AddPayment(newPayment); err != nil {
		t.Fatalf("unable to put payment in DB: %v", err)
	}
	expectedPayments = []*OutgoingPayment{newPayment}

	//check that deletion doesn't affect the index and the result of
	// PaymentsAddedSince call
	fetchedPayments, err = db.PaymentsAddedSince(5)
	if err != nil {
		t.Fatalf("Can't get payments from DB: %v", err)
	}

	if !reflect.DeepEqual(fetchedPayments, expectedPayments) {
		t.Fatalf("Wrong payments after reading from DB."+
			"Got %v, want %v",
			spew.Sdump(fetchedPayments),
			spew.Sdump(expectedPayments),
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
	if _, err = db.AddPayment(fakePayment); err != nil {
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

		if _, err = db.AddPayment(randomPayment); err != nil {
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

func TestPaymentStatusWorkflow(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	testCases := []struct {
		paymentHash [32]byte
		status      PaymentStatus
	}{
		{
			paymentHash: makeFakePaymentHash(),
			status:      StatusGrounded,
		},
		{
			paymentHash: makeFakePaymentHash(),
			status:      StatusInFlight,
		},
		{
			paymentHash: makeFakePaymentHash(),
			status:      StatusCompleted,
		},
	}

	for _, testCase := range testCases {
		err := db.UpdatePaymentStatus(testCase.paymentHash, testCase.status)
		if err != nil {
			t.Fatalf("unable to put payment in DB: %v", err)
		}

		status, err := db.FetchPaymentStatus(testCase.paymentHash)
		if err != nil {
			t.Fatalf("unable to fetch payments from DB: %v", err)
		}

		if status != testCase.status {
			t.Fatalf("Wrong payments status after reading from DB."+
				"Got %v, want %v",
				spew.Sdump(status),
				spew.Sdump(testCase.status),
			)
		}
	}
}
