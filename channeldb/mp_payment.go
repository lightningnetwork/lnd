package channeldb

import (
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
)

// HTLCAttemptInfo contains static information about a specific HTLC attempt
// for a payment. This information is used by the router to handle any errors
// coming back after an attempt is made, and to query the switch about the
// status of the attempt.
type HTLCAttemptInfo struct {
	// AttemptID is the unique ID used for this attempt.
	AttemptID uint64

	// SessionKey is the ephemeral key used for this attempt.
	SessionKey *btcec.PrivateKey

	// Route is the route attempted to send the HTLC.
	Route route.Route

	// AttemptTime is the time at which this HTLC was attempted.
	AttemptTime time.Time
}

// HTLCAttempt contains information about a specific HTLC attempt for a given
// payment. It contains the HTLCAttemptInfo used to send the HTLC, as well
// as a timestamp and any known outcome of the attempt.
type HTLCAttempt struct {
	HTLCAttemptInfo

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

// serializeHTLCSettleInfo serializes the details of a settled htlc.
func serializeHTLCSettleInfo(w io.Writer, s *HTLCSettleInfo) error {
	if _, err := w.Write(s.Preimage[:]); err != nil {
		return err
	}

	if err := serializeTime(w, s.SettleTime); err != nil {
		return err
	}

	return nil
}

// deserializeHTLCSettleInfo deserializes the details of a settled htlc.
func deserializeHTLCSettleInfo(r io.Reader) (*HTLCSettleInfo, error) {
	s := &HTLCSettleInfo{}
	if _, err := io.ReadFull(r, s.Preimage[:]); err != nil {
		return nil, err
	}

	var err error
	s.SettleTime, err = deserializeTime(r)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// serializeHTLCFailInfo serializes the details of a failed htlc including the
// wire failure.
func serializeHTLCFailInfo(w io.Writer, f *HTLCFailInfo) error {
	if err := serializeTime(w, f.FailTime); err != nil {
		return err
	}

	return nil
}

// deserializeHTLCFailInfo deserializes the details of a failed htlc including
// the wire failure.
func deserializeHTLCFailInfo(r io.Reader) (*HTLCFailInfo, error) {
	f := &HTLCFailInfo{}
	var err error
	f.FailTime, err = deserializeTime(r)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// deserializeTime deserializes time as unix nanoseconds.
func deserializeTime(r io.Reader) (time.Time, error) {
	var scratch [8]byte
	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return time.Time{}, err
	}

	// Convert to time.Time. Interpret unix nano time zero as a zero
	// time.Time value.
	unixNano := byteOrder.Uint64(scratch[:])
	if unixNano == 0 {
		return time.Time{}, nil
	}

	return time.Unix(0, int64(unixNano)), nil
}

// serializeTime serializes time as unix nanoseconds.
func serializeTime(w io.Writer, t time.Time) error {
	var scratch [8]byte

	// Convert to unix nano seconds, but only if time is non-zero. Calling
	// UnixNano() on a zero time yields an undefined result.
	var unixNano int64
	if !t.IsZero() {
		unixNano = t.UnixNano()
	}

	byteOrder.PutUint64(scratch[:], uint64(unixNano))
	_, err := w.Write(scratch[:])
	return err
}
