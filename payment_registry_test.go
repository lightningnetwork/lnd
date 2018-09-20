package main

import (
	"bytes"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	rev = [chainhash.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}
)

func init() {
	// Disable logging to prevent panics bc. of global state
	channeldb.UseLogger(btclog.Disabled)
	ltndLog = btclog.Disabled
}

func makeFakePayment() *channeldb.OutgoingPayment {
	fakeInvoice := &channeldb.Invoice{
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

	fakePayment := &channeldb.OutgoingPayment{
		Invoice:        *fakeInvoice,
		Fee:            101,
		Path:           fakePath,
		TimeLockLength: 1000,
	}
	copy(fakePayment.PaymentPreimage[:], rev[:])
	return fakePayment
}

func TestPaymentRegistryRealtimeNotifier(t *testing.T) {
	db, cleanUp, err := makeTestChannelDB()
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}
	defer db.Close()
	defer cleanUp()

	var expectedNotifications []*channeldb.OutgoingPayment
	for i := 0; i < 5; i++ {
		expectedNotifications = append(expectedNotifications, makeFakePayment())
	}

	paymentReg := newPaymentRegistry(db)
	paymentReg.Start()
	defer paymentReg.Stop()

	subscription := paymentReg.SubscribeNotifications(0)
	var paymentNotifications []*channeldb.OutgoingPayment
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < len(expectedNotifications); i++ {
			newPayment := <-subscription.NewPayments
			if newPayment.AddIndex != uint64(i+1) {
				t.Errorf("Expected AddIndex to be %v, got %v", i+1, newPayment.AddIndex)
			}
			paymentNotifications = append(paymentNotifications, newPayment)
		}
	}()

	for i := 0; i < len(expectedNotifications); i++ {
		paymentReg.AddPayment(expectedNotifications[i])
	}

	wg.Wait()

	if !reflect.DeepEqual(expectedNotifications, paymentNotifications) {
		t.Fatalf("Wrong notification."+
			"Got %v, want %v",
			spew.Sdump(paymentNotifications),
			spew.Sdump(expectedNotifications),
		)
	}
}

func TestPaymentRegistryBackwordNotifier(t *testing.T) {
	db, cleanUp, err := makeTestChannelDB()
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}
	defer db.Close()
	defer cleanUp()

	var expectedNotifications []*channeldb.OutgoingPayment
	for i := 0; i < 5; i++ {
		expectedNotifications = append(expectedNotifications, makeFakePayment())
	}

	paymentReg := newPaymentRegistry(db)
	defer paymentReg.Stop()
	paymentReg.Start()

	// First add the payments to the database so we can test we get
	// notifications for these later
	for i := 0; i < len(expectedNotifications); i++ {
		paymentReg.AddPayment(expectedNotifications[i])
	}

	startIndex := 1
	subscription := paymentReg.SubscribeNotifications(uint64(startIndex))
	var paymentNotifications []*channeldb.OutgoingPayment
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := startIndex; i < len(expectedNotifications); i++ {
			newPayment := <-subscription.NewPayments
			if newPayment.AddIndex != uint64(i+1) {
				t.Errorf("Expected AddIndex to be %v, got %v", i+1, newPayment.AddIndex)
			}
			paymentNotifications = append(paymentNotifications, newPayment)
		}
	}()

	wg.Wait()

	if !reflect.DeepEqual(expectedNotifications[startIndex:], paymentNotifications) {
		t.Fatalf("Wrong notification."+
			"Got %v, want %v",
			spew.Sdump(paymentNotifications),
			spew.Sdump(expectedNotifications),
		)
	}
}
