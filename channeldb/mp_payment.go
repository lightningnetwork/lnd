package channeldb

import (
	"bytes"
	"errors"
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// HTLCAttemptInfo contains static information about a specific HTLC attempt
// for a payment. This information is used by the router to handle any errors
// coming back after an attempt is made, and to query the switch about the
// status of the attempt.
type HTLCAttemptInfo struct {
	// AttemptID is the unique ID used for this attempt.
	AttemptID uint64

	// sessionKey is the raw bytes ephemeral key used for this attempt.
	// These bytes are lazily read off disk to save ourselves the expensive
	// EC operations used by btcec.PrivKeyFromBytes.
	sessionKey [btcec.PrivKeyBytesLen]byte

	// cachedSessionKey is our fully deserialized sesionKey. This value
	// may be nil if the attempt has just been read from disk and its
	// session key has not been used yet.
	cachedSessionKey *btcec.PrivateKey

	// Route is the route attempted to send the HTLC.
	Route route.Route

	// AttemptTime is the time at which this HTLC was attempted.
	AttemptTime time.Time

	// Hash is the hash used for this single HTLC attempt. For AMP payments
	// this will differ across attempts, for non-AMP payments each attempt
	// will use the same hash. This can be nil for older payment attempts,
	// in which the payment's PaymentHash in the PaymentCreationInfo should
	// be used.
	Hash *lntypes.Hash
}

// NewHtlcAttemptInfo creates a htlc attempt.
func NewHtlcAttemptInfo(attemptID uint64, sessionKey *btcec.PrivateKey,
	route route.Route, attemptTime time.Time,
	hash *lntypes.Hash) *HTLCAttemptInfo {

	var scratch [btcec.PrivKeyBytesLen]byte
	copy(scratch[:], sessionKey.Serialize())

	return &HTLCAttemptInfo{
		AttemptID:        attemptID,
		sessionKey:       scratch,
		cachedSessionKey: sessionKey,
		Route:            route,
		AttemptTime:      attemptTime,
		Hash:             hash,
	}
}

// SessionKey returns the ephemeral key used for a htlc attempt. This function
// performs expensive ec-ops to obtain the session key if it is not cached.
func (h *HTLCAttemptInfo) SessionKey() *btcec.PrivateKey {
	if h.cachedSessionKey == nil {
		h.cachedSessionKey, _ = btcec.PrivKeyFromBytes(
			h.sessionKey[:],
		)
	}

	return h.cachedSessionKey
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

// HTLCFailReason is the reason an htlc failed.
type HTLCFailReason byte

const (
	// HTLCFailUnknown is recorded for htlcs that failed with an unknown
	// reason.
	HTLCFailUnknown HTLCFailReason = 0

	// HTLCFailUnknown is recorded for htlcs that had a failure message that
	// couldn't be decrypted.
	HTLCFailUnreadable HTLCFailReason = 1

	// HTLCFailInternal is recorded for htlcs that failed because of an
	// internal error.
	HTLCFailInternal HTLCFailReason = 2

	// HTLCFailMessage is recorded for htlcs that failed with a network
	// failure message.
	HTLCFailMessage HTLCFailReason = 3
)

// HTLCFailInfo encapsulates the information that augments an HTLCAttempt in the
// event that the HTLC fails.
type HTLCFailInfo struct {
	// FailTime is the time at which this HTLC was failed.
	FailTime time.Time

	// Message is the wire message that failed this HTLC. This field will be
	// populated when the failure reason is HTLCFailMessage.
	Message lnwire.FailureMessage

	// Reason is the failure reason for this HTLC.
	Reason HTLCFailReason

	// The position in the path of the intermediate or final node that
	// generated the failure message. Position zero is the sender node. This
	// field will be populated when the failure reason is either
	// HTLCFailMessage or HTLCFailUnknown.
	FailureSourceIndex uint32
}

// MPPayment is a wrapper around a payment's PaymentCreationInfo and
// HTLCAttempts. All payments will have the PaymentCreationInfo set, any
// HTLCs made in attempts to be completed will populated in the HTLCs slice.
// Each populated HTLCAttempt represents an attempted HTLC, each of which may
// have the associated Settle or Fail struct populated if the HTLC is no longer
// in-flight.
type MPPayment struct {
	// SequenceNum is a unique identifier used to sort the payments in
	// order of creation.
	SequenceNum uint64

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

// TerminalInfo returns any HTLC settle info recorded. If no settle info is
// recorded, any payment level failure will be returned. If neither a settle
// nor a failure is recorded, both return values will be nil.
func (m *MPPayment) TerminalInfo() (*HTLCSettleInfo, *FailureReason) {
	for _, h := range m.HTLCs {
		if h.Settle != nil {
			return h.Settle, nil
		}
	}

	return nil, m.FailureReason
}

// SentAmt returns the sum of sent amount and fees for HTLCs that are either
// settled or still in flight.
func (m *MPPayment) SentAmt() (lnwire.MilliSatoshi, lnwire.MilliSatoshi) {
	var sent, fees lnwire.MilliSatoshi
	for _, h := range m.HTLCs {
		if h.Failure != nil {
			continue
		}

		// The attempt was not failed, meaning the amount was
		// potentially sent to the receiver.
		sent += h.Route.ReceiverAmt()
		fees += h.Route.TotalFees()
	}

	return sent, fees
}

// InFlightHTLCs returns the HTLCs that are still in-flight, meaning they have
// not been settled or failed.
func (m *MPPayment) InFlightHTLCs() []HTLCAttempt {
	var inflights []HTLCAttempt
	for _, h := range m.HTLCs {
		if h.Settle != nil || h.Failure != nil {
			continue
		}

		inflights = append(inflights, h)
	}

	return inflights
}

// GetAttempt returns the specified htlc attempt on the payment.
func (m *MPPayment) GetAttempt(id uint64) (*HTLCAttempt, error) {
	for _, htlc := range m.HTLCs {
		htlc := htlc
		if htlc.AttemptID == id {
			return &htlc, nil
		}
	}

	return nil, errors.New("htlc attempt not found on payment")
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

	// Write failure. If there is no failure message, write an empty
	// byte slice.
	var messageBytes bytes.Buffer
	if f.Message != nil {
		err := lnwire.EncodeFailureMessage(&messageBytes, f.Message, 0)
		if err != nil {
			return err
		}
	}
	if err := wire.WriteVarBytes(w, 0, messageBytes.Bytes()); err != nil {
		return err
	}

	return WriteElements(w, byte(f.Reason), f.FailureSourceIndex)
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

	// Read failure.
	failureBytes, err := wire.ReadVarBytes(
		r, 0, lnwire.FailureMessageLength, "failure",
	)
	if err != nil {
		return nil, err
	}
	if len(failureBytes) > 0 {
		f.Message, err = lnwire.DecodeFailureMessage(
			bytes.NewReader(failureBytes), 0,
		)
		if err != nil {
			return nil, err
		}
	}

	var reason byte
	err = ReadElements(r, &reason, &f.FailureSourceIndex)
	if err != nil {
		return nil, err
	}
	f.Reason = HTLCFailReason(reason)

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
