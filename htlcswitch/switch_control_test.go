package htlcswitch

import (
	"fmt"
	"testing"

	"github.com/btcsuite/fastsha256"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

func genHtlc() (*lnwire.UpdateAddHTLC, error) {
	preimage, err := genPreimage()
	if err != nil {
		return nil, fmt.Errorf("unable to generate preimage: %v", err)
	}

	rhash := fastsha256.Sum256(preimage[:])
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	return htlc, nil
}

// TestPaymentControlSwitch checks the ability of payment control
// change states of payments
func TestPaymentControlSwitch(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate base status
	// and move it to StatusInFlight and verifies that it
	// was changed.
	if err := pControl.CheckSend(htlc); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	pStatus, err := db.FetchPaymentStatus(htlc.PaymentHash)
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if pStatus != channeldb.StatusInFlight {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			channeldb.StatusInFlight, pStatus)
	}

	// Verifies that status was changed to StatusCompleted.
	if err := pControl.Success(htlc.PaymentHash); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	pStatus, err = db.FetchPaymentStatus(htlc.PaymentHash)
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if pStatus != channeldb.StatusCompleted {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			channeldb.StatusCompleted, pStatus)
	}
}

// TestPaymentControlSwitchFail checks that payment status returns
// to initial status after fail
func TestPaymentControlSwitchFail(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	if err := pControl.CheckSend(htlc); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	pStatus, err := db.FetchPaymentStatus(htlc.PaymentHash)
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if pStatus != channeldb.StatusInFlight {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			channeldb.StatusInFlight, pStatus)
	}

	// Move payment to completed status, second payment should return error.
	pControl.Fail(htlc.PaymentHash)

	pStatus, err = db.FetchPaymentStatus(htlc.PaymentHash)
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if pStatus != channeldb.StatusGrounded {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			channeldb.StatusGrounded, pStatus)
	}
}

// TestPaymentControlSwitchDoubleSend checks the ability of payment control
// to prevent double sending of htlc message, when message is in StatusInFlight
func TestPaymentControlSwitchDoubleSend(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate base status
	// and move it to StatusInFlight and verifies that it
	// was changed.
	if err := pControl.CheckSend(htlc); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	pStatus, err := db.FetchPaymentStatus(htlc.PaymentHash)
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if pStatus != channeldb.StatusInFlight {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			channeldb.StatusInFlight, pStatus)
	}

	// Tries to initiate double sending of htlc message with the same
	// payment hash.
	if err := pControl.CheckSend(htlc); err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}
}

// TestPaymentControlSwitchDoublePay checks the ability of payment control
// to prevent double payment
func TestPaymentControlSwitchDoublePay(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	if err := pControl.CheckSend(htlc); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	pStatus, err := db.FetchPaymentStatus(htlc.PaymentHash)
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if pStatus != channeldb.StatusInFlight {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			channeldb.StatusInFlight, pStatus)
	}

	// Move payment to completed status, second payment should return error.
	if err := pControl.Success(htlc.PaymentHash); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	if err := pControl.CheckSend(htlc); err != ErrAlreadyPaid {
		t.Fatalf("payment control wrong behaviour:" +
			" double payment must trigger ErrAlreadyPaid")
	}
}
