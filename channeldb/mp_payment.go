package channeldb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
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

// NewHtlcAttempt creates a htlc attempt.
func NewHtlcAttempt(attemptID uint64, sessionKey *btcec.PrivateKey,
	route route.Route, attemptTime time.Time,
	hash *lntypes.Hash) *HTLCAttempt {

	var scratch [btcec.PrivKeyBytesLen]byte
	copy(scratch[:], sessionKey.Serialize())

	info := HTLCAttemptInfo{
		AttemptID:        attemptID,
		sessionKey:       scratch,
		cachedSessionKey: sessionKey,
		Route:            route,
		AttemptTime:      attemptTime,
		Hash:             hash,
	}

	return &HTLCAttempt{HTLCAttemptInfo: info}
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

// MPPaymentState wraps a series of info needed for a given payment, which is
// used by both MPP and AMP. This is a memory representation of the payment's
// current state and is updated whenever the payment is read from disk.
type MPPaymentState struct {
	// NumAttemptsInFlight specifies the number of HTLCs the payment is
	// waiting results for.
	NumAttemptsInFlight int

	// RemainingAmt specifies how much more money to be sent.
	RemainingAmt lnwire.MilliSatoshi

	// FeesPaid specifies the total fees paid so far that can be used to
	// calculate remaining fee budget.
	FeesPaid lnwire.MilliSatoshi

	// HasSettledHTLC is true if at least one of the payment's HTLCs is
	// settled.
	HasSettledHTLC bool

	// PaymentFailed is true if the payment has been marked as failed with
	// a reason.
	PaymentFailed bool
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

	// State is the current state of the payment that holds a number of key
	// insights and is used to determine what to do on each payment loop
	// iteration.
	State *MPPaymentState
}

// Terminated returns a bool to specify whether the payment is in a terminal
// state.
func (m *MPPayment) Terminated() bool {
	// If the payment is in terminal state, it cannot be updated.
	return m.Status.updatable() != nil
}

// TerminalInfo returns any HTLC settle info recorded. If no settle info is
// recorded, any payment level failure will be returned. If neither a settle
// nor a failure is recorded, both return values will be nil.
func (m *MPPayment) TerminalInfo() (*HTLCAttempt, *FailureReason) {
	for _, h := range m.HTLCs {
		if h.Settle != nil {
			return &h, nil
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
	// TODO(yy): iteration can be slow, make it into a tree or use BS.
	for _, htlc := range m.HTLCs {
		htlc := htlc
		if htlc.AttemptID == id {
			return &htlc, nil
		}
	}

	return nil, errors.New("htlc attempt not found on payment")
}

// Registrable returns an error to specify whether adding more HTLCs to the
// payment with its current status is allowed. A payment can accept new HTLC
// registrations when it's newly created, or none of its HTLCs is in a terminal
// state.
func (m *MPPayment) Registrable() error {
	// If updating the payment is not allowed, we can't register new HTLCs.
	// Otherwise, the status must be either `StatusInitiated` or
	// `StatusInFlight`.
	if err := m.Status.updatable(); err != nil {
		return err
	}

	// Exit early if this is not inflight.
	if m.Status != StatusInFlight {
		return nil
	}

	// There are still inflight HTLCs and we need to check whether there
	// are settled HTLCs or the payment is failed. If we already have
	// settled HTLCs, we won't allow adding more HTLCs.
	if m.State.HasSettledHTLC {
		return ErrPaymentPendingSettled
	}

	// If the payment is already failed, we won't allow adding more HTLCs.
	if m.State.PaymentFailed {
		return ErrPaymentPendingFailed
	}

	// Otherwise we can add more HTLCs.
	return nil
}

// setState creates and attaches a new MPPaymentState to the payment. It also
// updates the payment's status based on its current state.
func (m *MPPayment) setState() error {
	// Fetch the total amount and fees that has already been sent in
	// settled and still in-flight shards.
	sentAmt, fees := m.SentAmt()

	// Sanity check we haven't sent a value larger than the payment amount.
	totalAmt := m.Info.Value
	if sentAmt > totalAmt {
		return fmt.Errorf("%w: sent=%v, total=%v", ErrSentExceedsTotal,
			sentAmt, totalAmt)
	}

	// Get any terminal info for this payment.
	settle, failure := m.TerminalInfo()

	// Now determine the payment's status.
	status, err := decidePaymentStatus(m.HTLCs, m.FailureReason)
	if err != nil {
		return err
	}

	// Update the payment state and status.
	m.State = &MPPaymentState{
		NumAttemptsInFlight: len(m.InFlightHTLCs()),
		RemainingAmt:        totalAmt - sentAmt,
		FeesPaid:            fees,
		HasSettledHTLC:      settle != nil,
		PaymentFailed:       failure != nil,
	}
	m.Status = status

	return nil
}

// SetState calls the internal method setState. This is a temporary method
// to be used by the tests in routing. Once the tests are updated to use mocks,
// this method can be removed.
//
// TODO(yy): delete.
func (m *MPPayment) SetState() error {
	return m.setState()
}

// NeedWaitAttempts decides whether we need to hold creating more HTLC attempts
// and wait for the results of the payment's inflight HTLCs. Return an error if
// the payment is in an unexpected state.
func (m *MPPayment) NeedWaitAttempts() (bool, error) {
	// Check when the remainingAmt is not zero, which means we have more
	// money to be sent.
	if m.State.RemainingAmt != 0 {
		switch m.Status {
		// If the payment is newly created, no need to wait for HTLC
		// results.
		case StatusInitiated:
			return false, nil

		// If we have inflight HTLCs, we'll check if we have terminal
		// states to decide if we need to wait.
		case StatusInFlight:
			// We still have money to send, and one of the HTLCs is
			// settled. We'd stop sending money and wait for all
			// inflight HTLC attempts to finish.
			if m.State.HasSettledHTLC {
				log.Warnf("payment=%v has remaining amount "+
					"%v, yet at least one of its HTLCs is "+
					"settled", m.Info.PaymentIdentifier,
					m.State.RemainingAmt)

				return true, nil
			}

			// The payment has a failure reason though we still
			// have money to send, we'd stop sending money and wait
			// for all inflight HTLC attempts to finish.
			if m.State.PaymentFailed {
				return true, nil
			}

			// Otherwise we don't need to wait for inflight HTLCs
			// since we still have money to be sent.
			return false, nil

		// We need to send more money, yet the payment is already
		// succeeded. Return an error in this case as the receiver is
		// violating the protocol.
		case StatusSucceeded:
			return false, fmt.Errorf("%w: parts of the payment "+
				"already succeeded but still have remaining "+
				"amount %v", ErrPaymentInternal,
				m.State.RemainingAmt)

		// The payment is failed and we have no inflight HTLCs, no need
		// to wait.
		case StatusFailed:
			return false, nil

		// Unknown payment status.
		default:
			return false, fmt.Errorf("%w: %s",
				ErrUnknownPaymentStatus, m.Status)
		}
	}

	// Now we determine whether we need to wait when the remainingAmt is
	// already zero.
	switch m.Status {
	// When the payment is newly created, yet the payment has no remaining
	// amount, return an error.
	case StatusInitiated:
		return false, fmt.Errorf("%w: %v", ErrPaymentInternal, m.Status)

	// If the payment is inflight, we must wait.
	//
	// NOTE: an edge case is when all HTLCs are failed while the payment is
	// not failed we'd still be in this inflight state. However, since the
	// remainingAmt is zero here, it means we cannot be in that state as
	// otherwise the remainingAmt would not be zero.
	case StatusInFlight:
		return true, nil

	// If the payment is already succeeded, no need to wait.
	case StatusSucceeded:
		return false, nil

	// If the payment is already failed, yet the remaining amount is zero,
	// return an error as this indicates an error state. We will only each
	// this status when there are no inflight HTLCs and the payment is
	// marked as failed with a reason, which means the remainingAmt must
	// not be zero because our sentAmt is zero.
	case StatusFailed:
		return false, fmt.Errorf("%w: %v", ErrPaymentInternal, m.Status)

	// Unknown payment status.
	default:
		return false, fmt.Errorf("%w: %s", ErrUnknownPaymentStatus,
			m.Status)
	}
}

// GetState returns the internal state of the payment.
func (m *MPPayment) GetState() *MPPaymentState {
	return m.State
}

// Status returns the current status of the payment.
func (m *MPPayment) GetStatus() PaymentStatus {
	return m.Status
}

// GetPayment returns all the HTLCs for this payment.
func (m *MPPayment) GetHTLCs() []HTLCAttempt {
	return m.HTLCs
}

// AllowMoreAttempts is used to decide whether we can safely attempt more HTLCs
// for a given payment state. Return an error if the payment is in an
// unexpected state.
func (m *MPPayment) AllowMoreAttempts() (bool, error) {
	// Now check whether the remainingAmt is zero or not. If we don't have
	// any remainingAmt, no more HTLCs should be made.
	if m.State.RemainingAmt == 0 {
		// If the payment is newly created, yet we don't have any
		// remainingAmt, return an error.
		if m.Status == StatusInitiated {
			return false, fmt.Errorf("%w: initiated payment has "+
				"zero remainingAmt", ErrPaymentInternal)
		}

		// Otherwise, exit early since all other statuses with zero
		// remainingAmt indicate no more HTLCs can be made.
		return false, nil
	}

	// Otherwise, the remaining amount is not zero, we now decide whether
	// to make more attempts based on the payment's current status.
	//
	// If at least one of the payment's attempts is settled, yet we haven't
	// sent all the amount, it indicates something is wrong with the peer
	// as the preimage is received. In this case, return an error state.
	if m.Status == StatusSucceeded {
		return false, fmt.Errorf("%w: payment already succeeded but "+
			"still have remaining amount %v", ErrPaymentInternal,
			m.State.RemainingAmt)
	}

	// Now check if we can register a new HTLC.
	err := m.Registrable()
	if err != nil {
		log.Warnf("Payment(%v): cannot register HTLC attempt: %v, "+
			"current status: %s", m.Info.PaymentIdentifier,
			err, m.Status)

		return false, nil
	}

	// Now we know we can register new HTLCs.
	return true, nil
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
		r, 0, math.MaxUint16, "failure",
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
