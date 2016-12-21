package channeldb

import (
	"bytes"
	"fmt"
	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/roasbeef/btcutil"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func makeFakePayment() *OutgoingPayment {
	// Create a fake invoice which
	// we'll use several times in the tests below.
	fakeInvoice := &Invoice{
		CreationDate: time.Now(),
	}
	fakeInvoice.Memo = []byte("memo")
	fakeInvoice.Receipt = []byte("recipt")
	copy(fakeInvoice.Terms.PaymentPreimage[:], rev[:])
	fakeInvoice.Terms.Value = btcutil.Amount(10000)
	// Make fake path
	fakePath := make([][33]byte, 3)
	for i := 0; i < 3; i++ {
		for j := 0; j < 33; j++ {
			fakePath[i][j] = byte(i)
		}
	}
	var rHash [32]byte = fastsha256.Sum256(rev[:])
	fakePayment := &OutgoingPayment{
		Invoice:        *fakeInvoice,
		Fee:            101,
		Path:           fakePath,
		TimeLockLength: 1000,
		RHash:          rHash,
		Timestamp:      time.Unix(100000, 0),
	}
	return fakePayment
}

// randomBytes creates random []byte with length
// in range [minLen, maxLen)
func randomBytes(minLen, maxLen int) ([]byte, error) {
	l := minLen + rand.Intn(maxLen-minLen)
	b := make([]byte, l)
	_, err := rand.Read(b)
	if err != nil {
		return nil, fmt.Errorf("Internal error. "+
			"Cannot generate random string: %v", err)
	}
	return b, nil
}

func makeRandomFakePayment() (*OutgoingPayment, error) {
	var err error
	fakeInvoice := &Invoice{
		CreationDate: time.Now(),
	}

	fakeInvoice.Memo, err = randomBytes(1, 50)
	if err != nil {
		return nil, err
	}

	fakeInvoice.Receipt, err = randomBytes(1, 50)
	if err != nil {
		return nil, err
	}

	preImg, err := randomBytes(32, 33)
	if err != nil {
		return nil, err
	}
	copy(fakeInvoice.Terms.PaymentPreimage[:], preImg)

	fakeInvoice.Terms.Value = btcutil.Amount(rand.Intn(10000))

	// Make fake path
	fakePathLen := 1 + rand.Intn(5)
	fakePath := make([][33]byte, fakePathLen)
	for i := 0; i < fakePathLen; i++ {
		b, err := randomBytes(33, 34)
		if err != nil {
			return nil, err
		}
		copy(fakePath[i][:], b)
	}

	var rHash [32]byte = fastsha256.Sum256(
		fakeInvoice.Terms.PaymentPreimage[:],
	)
	fakePayment := &OutgoingPayment{
		Invoice:        *fakeInvoice,
		Fee:            btcutil.Amount(rand.Intn(1001)),
		Path:           fakePath,
		TimeLockLength: uint32(rand.Intn(10000)),
		RHash:          rHash,
		Timestamp:      time.Unix(rand.Int63n(10000), 0),
	}
	return fakePayment, nil
}

func TestOutgoingPaymentSerialization(t *testing.T) {
	fakePayment := makeFakePayment()
	b := new(bytes.Buffer)
	err := serializeOutgoingPayment(b, fakePayment)
	if err != nil {
		t.Fatalf("Can't serialize outgoing payment: %v", err)
	}

	newPayment, err := deserializeOutgoingPayment(b)
	if err != nil {
		t.Fatalf("Can't deserialize outgoing payment: %v", err)
	}

	if !reflect.DeepEqual(fakePayment, newPayment) {
		t.Fatalf("Payments do not match after "+
			"serialization/deserialization %v vs %v",
			spew.Sdump(fakePayment),
			spew.Sdump(newPayment),
		)
	}
}

func TestOutgoingPaymentWorkflow(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}
	defer cleanUp()

	fakePayment := makeFakePayment()
	err = db.AddPayment(fakePayment)
	if err != nil {
		t.Fatalf("Can't put payment in DB: %v", err)
	}

	payments, err := db.FetchAllPayments()
	if err != nil {
		t.Fatalf("Can't get payments from DB: %v", err)
	}
	correctPayments := []*OutgoingPayment{fakePayment}
	if !reflect.DeepEqual(payments, correctPayments) {
		t.Fatalf("Wrong payments after reading from DB."+
			"Got %v, want %v",
			spew.Sdump(payments),
			spew.Sdump(correctPayments),
		)
	}

	// Make some random payments
	for i := 0; i < 5; i++ {
		randomPayment, err := makeRandomFakePayment()
		if err != nil {
			t.Fatalf("Internal error in tests: %v", err)
		}
		err = db.AddPayment(randomPayment)
		if err != nil {
			t.Fatalf("Can't put payment in DB: %v", err)
		}
		correctPayments = append(correctPayments, randomPayment)
	}

	payments, err = db.FetchAllPayments()
	if err != nil {
		t.Fatalf("Can't get payments from DB: %v", err)
	}

	if !reflect.DeepEqual(payments, correctPayments) {
		t.Fatalf("Wrong payments after reading from DB."+
			"Got %v, want %v",
			spew.Sdump(payments),
			spew.Sdump(correctPayments),
		)
	}

	// Delete all payments.
	err = db.DeleteAllPayments()
	if err != nil {
		t.Fatalf("Can't delete payments from DB: %v", err)
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
