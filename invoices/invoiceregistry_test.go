package invoices

import (
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

var (
	testTimeout = 5 * time.Second

	preimage = lntypes.Preimage{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1,
	}

	hash = preimage.Hash()

	// testPayReq is a dummy payment request that does parse properly. It
	// has no relation with the real invoice parameters and isn't asserted
	// on in this test. LookupInvoice requires this to have a valid value.
	testPayReq = "lnbc500u1pwywxzwpp5nd2u9xzq02t0tuf2654as7vma42lwkcjptx4yzfq0umq4swpa7cqdqqcqzysmlpc9ewnydr8rr8dnltyxphdyf6mcqrsd6dml8zajtyhwe6a45d807kxtmzayuf0hh2d9tn478ecxkecdg7c5g85pntupug5kakm7xcpn63zqk"
)

func decodeExpiry(payReq string) (uint32, error) {
	invoice, err := zpay32.Decode(payReq, &chaincfg.MainNetParams)
	if err != nil {
		return 0, err
	}
	return uint32(invoice.MinFinalCLTVExpiry()), nil
}

var (
	testInvoice = &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
			PaymentPreimage: preimage,
			Value:           lnwire.MilliSatoshi(100000),
		},
		PaymentRequest: []byte(testPayReq),
	}
)

func newTestContext(t *testing.T) (*InvoiceRegistry, func()) {
	cdb, cleanup, err := newDB()
	if err != nil {
		t.Fatal(err)
	}

	// Instantiate and start the invoice registry.
	registry := NewRegistry(cdb, decodeExpiry)

	err = registry.Start()
	if err != nil {
		cleanup()
		t.Fatal(err)
	}

	return registry, func() {
		registry.Stop()
		cleanup()
	}
}

// TestSettleInvoice tests settling of an invoice and related notifications.
func TestSettleInvoice(t *testing.T) {
	registry, cleanup := newTestContext(t)
	defer cleanup()

	allSubscriptions := registry.SubscribeNotifications(0, 0)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription := registry.SubscribeSingleInvoice(hash)
	defer subscription.Cancel()

	if subscription.hash != hash {
		t.Fatalf("expected subscription for provided hash")
	}

	// Add the invoice.
	addIdx, err := registry.AddInvoice(testInvoice, hash)
	if err != nil {
		t.Fatal(err)
	}

	if addIdx != 1 {
		t.Fatalf("expected addIndex to start with 1, but got %v",
			addIdx)
	}

	// We expect the open state to be sent to the single invoice subscriber.
	select {
	case update := <-subscription.Updates:
		if update.Terms.State != channeldb.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				update.Terms.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a new invoice notification to be sent out.
	select {
	case newInvoice := <-allSubscriptions.NewInvoices:
		if newInvoice.Terms.State != channeldb.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				newInvoice.Terms.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// Settle invoice with a slightly higher amount.
	amtPaid := lnwire.MilliSatoshi(100500)
	_, err = registry.NotifyExitHopHtlc(hash, amtPaid)
	if err != nil {
		t.Fatal(err)
	}

	// We expect the settled state to be sent to the single invoice
	// subscriber.
	select {
	case update := <-subscription.Updates:
		if update.Terms.State != channeldb.ContractSettled {
			t.Fatalf("expected state ContractOpen, but got %v",
				update.Terms.State)
		}
		if update.AmtPaid != amtPaid {
			t.Fatal("invoice AmtPaid incorrect")
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a settled notification to be sent out.
	select {
	case settledInvoice := <-allSubscriptions.SettledInvoices:
		if settledInvoice.Terms.State != channeldb.ContractSettled {
			t.Fatalf("expected state ContractOpen, but got %v",
				settledInvoice.Terms.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// Try to settle again.
	_, err = registry.NotifyExitHopHtlc(hash, amtPaid)
	if err != nil {
		t.Fatal("expected duplicate settle to succeed")
	}

	// Try to settle again with a different amount.
	_, err = registry.NotifyExitHopHtlc(hash, amtPaid+600)
	if err != nil {
		t.Fatal("expected duplicate settle to succeed")
	}

	// Check that settled amount remains unchanged.
	inv, _, err := registry.LookupInvoice(hash)
	if err != nil {
		t.Fatal(err)
	}
	if inv.AmtPaid != amtPaid {
		t.Fatal("expected amount to be unchanged")
	}

	// Try to cancel.
	err = registry.CancelInvoice(hash)
	if err != channeldb.ErrInvoiceAlreadySettled {
		t.Fatal("expected cancelation of a settled invoice to fail")
	}
}

// TestCancelInvoice tests cancelation of an invoice and related notifications.
func TestCancelInvoice(t *testing.T) {
	registry, cleanup := newTestContext(t)
	defer cleanup()

	allSubscriptions := registry.SubscribeNotifications(0, 0)
	defer allSubscriptions.Cancel()

	// Try to cancel the not yet existing invoice. This should fail.
	err := registry.CancelInvoice(hash)
	if err != channeldb.ErrInvoiceNotFound {
		t.Fatalf("expected ErrInvoiceNotFound, but got %v", err)
	}

	// Subscribe to the not yet existing invoice.
	subscription := registry.SubscribeSingleInvoice(hash)
	defer subscription.Cancel()

	if subscription.hash != hash {
		t.Fatalf("expected subscription for provided hash")
	}

	// Add the invoice.
	amt := lnwire.MilliSatoshi(100000)
	_, err = registry.AddInvoice(testInvoice, hash)
	if err != nil {
		t.Fatal(err)
	}

	// We expect the open state to be sent to the single invoice subscriber.
	select {
	case update := <-subscription.Updates:
		if update.Terms.State != channeldb.ContractOpen {
			t.Fatalf(
				"expected state ContractOpen, but got %v",
				update.Terms.State,
			)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a new invoice notification to be sent out.
	select {
	case newInvoice := <-allSubscriptions.NewInvoices:
		if newInvoice.Terms.State != channeldb.ContractOpen {
			t.Fatalf(
				"expected state ContractOpen, but got %v",
				newInvoice.Terms.State,
			)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// Cancel invoice.
	err = registry.CancelInvoice(hash)
	if err != nil {
		t.Fatal(err)
	}

	// We expect the canceled state to be sent to the single invoice
	// subscriber.
	select {
	case update := <-subscription.Updates:
		if update.Terms.State != channeldb.ContractCanceled {
			t.Fatalf(
				"expected state ContractCanceled, but got %v",
				update.Terms.State,
			)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect no cancel notification to be sent to all invoice
	// subscribers (backwards compatibility).

	// Try to cancel again.
	err = registry.CancelInvoice(hash)
	if err != nil {
		t.Fatal("expected cancelation of a canceled invoice to succeed")
	}

	// Notify arrival of a new htlc paying to this invoice. This should
	// succeed.
	event, err := registry.NotifyExitHopHtlc(hash, amt)
	if err != nil {
		t.Fatal("expected settlement of a canceled invoice to succeed")
	}

	if event.Preimage != nil {
		t.Fatal("expected cancel hodl event")
	}
}

func newDB() (*channeldb.DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		os.RemoveAll(tempDirName)
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}
