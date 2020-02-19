package channeldb

import (
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
)

// HTLCAttempt contains information about a specific HTLC attempt for a given
// payment. This information is used by the router to handle any errors coming
// back after an attempt is made, and to query the switch about the status of a
// payment. For settled payment this will be the information for the succeeding
// payment attempt.
type HTLCAttempt struct {
	// PaymentID is the unique ID used for this attempt.
	PaymentID uint64

	// SessionKey is the ephemeral key used for this payment attempt.
	SessionKey *btcec.PrivateKey

	// Route is the route attempted to send the HTLC.
	Route route.Route

	// AttemptTime is the time at which this HTLC was attempted.
	AttemptTime time.Time

	// Settle is the preimage of a successful payment. This serves as a
	// proof of payment. It will only be non-nil for settled payments.
	//
	// NOTE: Can be nil if payment is not settled.
	Settle *HTLCSettleInfo

	// Fail is a failure reason code indicating the reason the payment
	// failed. It is only non-nil for failed payments.
	//
	// NOTE: Can be nil if payment is not failed.
	Failure *HTLCFailInfo
}

// HTLCSettleInfo encapsulates the information that augments an HTLCAttempt in
// the event that the HTLC is successful.
type HTLCSettleInfo struct {
	// Preimage is the preimage of a successful HTLC. This serves as a proof
	// of payment.
	Preimage lntypes.Preimage

	// SettleTime is the time at which this HTLC was settled.
	SettleTime time.Time
}

// HTLCFailInfo encapsulates the information that augments an HTLCAttempt in the
// event that the HTLC fails.
type HTLCFailInfo struct {
	// FailTime is the time at which this HTLC was failed.
	FailTime time.Time
}

// MPPayment is a wrapper around a payment's PaymentCreationInfo and
// HTLCAttempts. All payments will have the PaymentCreationInfo set, any
// HTLCs made in attempts to be completed will populated in the HTLCs slice.
// Each populated HTLCAttempt represents an attempted HTLC, each of which may
// have the associated Settle or Fail struct populated if the HTLC is no longer
// in-flight.
type MPPayment struct {
	// sequenceNum is a unique identifier used to sort the payments in
	// order of creation.
	sequenceNum uint64

	// Info holds all static information about this payment, and is
	// populated when the payment is initiated.
	Info *PaymentCreationInfo

	// HTLCs holds the information about individual HTLCs that we send in
	// order to make the payment.
	HTLCs []HTLCAttempt

	// FailureReason is the failure reason code indicating the reason the
	// payment failed.
	//
	// NOTE: Will only be set once the daemon has given up on the payment
	// altogether.
	FailureReason *FailureReason

	// Status is the current PaymentStatus of this payment.
	Status PaymentStatus
}
