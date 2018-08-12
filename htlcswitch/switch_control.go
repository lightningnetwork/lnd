package htlcswitch

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrAlreadyPaid is used when we have already paid
	ErrAlreadyPaid = errors.New("invoice was already paid")

	// ErrPaymentInFlight returns in case if payment is already "in flight"
	ErrPaymentInFlight = errors.New("payment is in transition")

	// ErrPaymentNotInitiated returns in case if payment wasn't initiated
	// in switch
	ErrPaymentNotInitiated = errors.New("payment isn't initiated")

	// ErrPaymentAlreadyCompleted returns in case of attempt to complete
	// completed payment
	ErrPaymentAlreadyCompleted = errors.New("payment is already completed")
)

// ControlTower is a controller interface of sending HTLC messages to switch
type ControlTower interface {
	// CheckSend intercepts incoming message to provide checks
	// and fail if specific message is not allowed by implementation
	CheckSend(htlc *lnwire.UpdateAddHTLC) error

	// Success marks message transition as successful
	Success(paymentHash [32]byte) error

	// Fail marks message transition as failed
	Fail(paymentHash [32]byte) error
}

// paymentControl is implementation of ControlTower to restrict double payment
// sending.
type paymentControl struct {
	mx sync.Mutex

	db *channeldb.DB
}

// NewPaymentControl creates a new instance of the paymentControl.
func NewPaymentControl(db *channeldb.DB) ControlTower {
	return &paymentControl{
		db: db,
	}
}

// CheckSend checks that a sending htlc wasn't triggered before for specific
// payment hash, if so, should trigger error depends on current status
func (p *paymentControl) CheckSend(htlc *lnwire.UpdateAddHTLC) error {
	p.mx.Lock()
	defer p.mx.Unlock()

	// Retrieve current status of payment from local database.
	paymentStatus, err := p.db.FetchPaymentStatus(htlc.PaymentHash)
	if err != nil {
		return err
	}

	switch paymentStatus {
	case channeldb.StatusGrounded:
		// It is safe to reattempt a payment if we know that we haven't
		// left one in flight prior to restarting and switch.
		return p.db.UpdatePaymentStatus(htlc.PaymentHash,
			channeldb.StatusInFlight)

	case channeldb.StatusInFlight:
		// Not clear if it's safe to reinitiate a payment if there
		// is already a payment in flight, so we should withhold any
		// additional attempts to send to that payment hash.
		return ErrPaymentInFlight

	case channeldb.StatusCompleted:
		// It has been already paid and don't want to pay again.
		return ErrAlreadyPaid
	}

	return nil
}

// Success proceed status changing of payment to next successful status
func (p *paymentControl) Success(paymentHash [32]byte) error {
	p.mx.Lock()
	defer p.mx.Unlock()

	paymentStatus, err := p.db.FetchPaymentStatus(paymentHash)
	if err != nil {
		return err
	}

	switch paymentStatus {
	case channeldb.StatusGrounded:
		// Payment isn't initiated but received.
		return ErrPaymentNotInitiated

	case channeldb.StatusInFlight:
		// Successful transition from InFlight transition to Completed.
		return p.db.UpdatePaymentStatus(paymentHash, channeldb.StatusCompleted)

	case channeldb.StatusCompleted:
		// Payment is completed before in should be ignored.
		return ErrPaymentAlreadyCompleted
	}

	return nil
}

// Fail proceed status changing of payment to initial status in case of failure
func (p *paymentControl) Fail(paymentHash [32]byte) error {
	p.mx.Lock()
	defer p.mx.Unlock()

	paymentStatus, err := p.db.FetchPaymentStatus(paymentHash)
	if err != nil {
		return err
	}

	switch paymentStatus {
	case channeldb.StatusGrounded:
		// Unpredictable behavior when payment wasn't transited to
		// StatusInFlight status and was failed.
		return ErrPaymentNotInitiated

	case channeldb.StatusInFlight:
		// If payment wasn't processed by some reason should return to
		// default status to unlock retrying option for the same payment hash.
		return p.db.UpdatePaymentStatus(paymentHash, channeldb.StatusGrounded)

	case channeldb.StatusCompleted:
		// Payment is completed before and can't be moved to another status.
		return ErrPaymentAlreadyCompleted
	}

	return nil
}
