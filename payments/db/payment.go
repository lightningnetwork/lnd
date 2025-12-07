package paymentsdb

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// FailureReason encodes the reason a payment ultimately failed.
type FailureReason byte

const (
	// FailureReasonTimeout indicates that the payment did timeout before a
	// successful payment attempt was made.
	FailureReasonTimeout FailureReason = 0

	// FailureReasonNoRoute indicates no successful route to the
	// destination was found during path finding.
	FailureReasonNoRoute FailureReason = 1

	// FailureReasonError indicates that an unexpected error happened during
	// payment.
	FailureReasonError FailureReason = 2

	// FailureReasonPaymentDetails indicates that either the hash is unknown
	// or the final cltv delta or amount is incorrect.
	FailureReasonPaymentDetails FailureReason = 3

	// FailureReasonInsufficientBalance indicates that we didn't have enough
	// balance to complete the payment.
	FailureReasonInsufficientBalance FailureReason = 4

	// FailureReasonCanceled indicates that the payment was canceled by the
	// user.
	FailureReasonCanceled FailureReason = 5

	// TODO(joostjager): Add failure reasons for:
	// LocalLiquidityInsufficient, RemoteCapacityInsufficient.
)

// Error returns a human-readable error string for the FailureReason.
func (r FailureReason) Error() string {
	return r.String()
}

// String returns a human-readable FailureReason.
func (r FailureReason) String() string {
	switch r {
	case FailureReasonTimeout:
		return "timeout"
	case FailureReasonNoRoute:
		return "no_route"
	case FailureReasonError:
		return "error"
	case FailureReasonPaymentDetails:
		return "incorrect_payment_details"
	case FailureReasonInsufficientBalance:
		return "insufficient_balance"
	case FailureReasonCanceled:
		return "canceled"
	}

	return "unknown"
}

// PaymentCreationInfo is the information necessary to have ready when
// initiating a payment, moving it into state InFlight.
type PaymentCreationInfo struct {
	// PaymentIdentifier is the hash this payment is paying to in case of
	// non-AMP payments, and the SetID for AMP payments.
	PaymentIdentifier lntypes.Hash

	// Value is the amount we are paying.
	Value lnwire.MilliSatoshi

	// CreationTime is the time when this payment was initiated.
	CreationTime time.Time

	// PaymentRequest is the full payment request, if any.
	PaymentRequest []byte

	// FirstHopCustomRecords are the TLV records that are to be sent to the
	// first hop of this payment. These records will be transmitted via the
	// wire message (UpdateAddHTLC) only and therefore do not affect the
	// onion payload size.
	FirstHopCustomRecords lnwire.CustomRecords
}

// String returns a human-readable description of the payment creation info.
func (p *PaymentCreationInfo) String() string {
	return fmt.Sprintf("payment_id=%v, amount=%v, created_at=%v",
		p.PaymentIdentifier, p.Value, p.CreationTime)
}

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

	// onionBlob is the cached value for onion blob created from the sphinx
	// construction.
	onionBlob [lnwire.OnionPacketSize]byte

	// circuit is the cached value for sphinx circuit.
	circuit *sphinx.Circuit
}

// NewHtlcAttempt creates a htlc attempt.
func NewHtlcAttempt(attemptID uint64, sessionKey *btcec.PrivateKey,
	route route.Route, attemptTime time.Time,
	hash *lntypes.Hash) (*HTLCAttempt, error) {

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

	if err := info.attachOnionBlobAndCircuit(); err != nil {
		return nil, err
	}

	return &HTLCAttempt{HTLCAttemptInfo: info}, nil
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

// setSessionKey sets the session key for the htlc attempt.
//
// NOTE: Only used for testing.
//
//nolint:unused
func (h *HTLCAttemptInfo) setSessionKey(sessionKey *btcec.PrivateKey) {
	h.cachedSessionKey = sessionKey

	// Also set the session key as a raw bytes.
	var scratch [btcec.PrivKeyBytesLen]byte
	copy(scratch[:], sessionKey.Serialize())
	h.sessionKey = scratch
}

// OnionBlob returns the onion blob created from the sphinx construction.
func (h *HTLCAttemptInfo) OnionBlob() ([lnwire.OnionPacketSize]byte, error) {
	var zeroBytes [lnwire.OnionPacketSize]byte
	if h.onionBlob == zeroBytes {
		if err := h.attachOnionBlobAndCircuit(); err != nil {
			return zeroBytes, err
		}
	}

	return h.onionBlob, nil
}

// Circuit returns the sphinx circuit for this attempt.
func (h *HTLCAttemptInfo) Circuit() (*sphinx.Circuit, error) {
	if h.circuit == nil {
		if err := h.attachOnionBlobAndCircuit(); err != nil {
			return nil, err
		}
	}

	return h.circuit, nil
}

// attachOnionBlobAndCircuit creates a sphinx packet and caches the onion blob
// and circuit for this attempt.
func (h *HTLCAttemptInfo) attachOnionBlobAndCircuit() error {
	onionBlob, circuit, err := generateSphinxPacket(
		&h.Route, h.Hash[:], h.SessionKey(),
	)
	if err != nil {
		return err
	}

	copy(h.onionBlob[:], onionBlob)
	h.circuit = circuit

	return nil
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

	// HTLCFailUnreadable is recorded for htlcs that had a failure message
	// that couldn't be decrypted.
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
		return fmt.Errorf("%w: sent=%v, total=%v",
			ErrSentExceedsTotal, sentAmt, totalAmt)
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
		return false, fmt.Errorf("%w: %v",
			ErrPaymentInternal, m.Status)

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
		return false, fmt.Errorf("%w: %v",
			ErrPaymentInternal, m.Status)

	// Unknown payment status.
	default:
		return false, fmt.Errorf("%w: %s",
			ErrUnknownPaymentStatus, m.Status)
	}
}

// GetState returns the internal state of the payment.
func (m *MPPayment) GetState() *MPPaymentState {
	return m.State
}

// GetStatus returns the current status of the payment.
func (m *MPPayment) GetStatus() PaymentStatus {
	return m.Status
}

// GetHTLCs returns all the HTLCs for this payment.
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
				"zero remainingAmt",
				ErrPaymentInternal)
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
			"still have remaining amount %v",
			ErrPaymentInternal, m.State.RemainingAmt)
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

// generateSphinxPacket generates then encodes a sphinx packet which encodes
// the onion route specified by the passed layer 3 route. The blob returned
// from this function can immediately be included within an HTLC add packet to
// be sent to the first hop within the route.
func generateSphinxPacket(rt *route.Route, paymentHash []byte,
	sessionKey *btcec.PrivateKey) ([]byte, *sphinx.Circuit, error) {

	// Now that we know we have an actual route, we'll map the route into a
	// sphinx payment path which includes per-hop payloads for each hop
	// that give each node within the route the necessary information
	// (fees, CLTV value, etc.) to properly forward the payment.
	sphinxPath, err := rt.ToSphinxPath()
	if err != nil {
		return nil, nil, err
	}

	log.Tracef("Constructed per-hop payloads for payment_hash=%x: %v",
		paymentHash, lnutils.NewLogClosure(func() string {
			path := make(
				[]sphinx.OnionHop, sphinxPath.TrueRouteLength(),
			)
			for i := range path {
				hopCopy := sphinxPath[i]
				path[i] = hopCopy
			}

			return spew.Sdump(path)
		}),
	)

	// Next generate the onion routing packet which allows us to perform
	// privacy preserving source routing across the network.
	sphinxPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, paymentHash,
		sphinx.DeterministicPacketFiller,
	)
	if err != nil {
		return nil, nil, err
	}

	// Finally, encode Sphinx packet using its wire representation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := sphinxPacket.Encode(&onionBlob); err != nil {
		return nil, nil, err
	}

	log.Tracef("Generated sphinx packet: %v",
		lnutils.NewLogClosure(func() string {
			// We make a copy of the ephemeral key and unset the
			// internal curve here in order to keep the logs from
			// getting noisy.
			key := *sphinxPacket.EphemeralKey
			packetCopy := *sphinxPacket
			packetCopy.EphemeralKey = &key

			return spew.Sdump(packetCopy)
		}),
	)

	return onionBlob.Bytes(), &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: sphinxPath.NodeKeys(),
	}, nil
}

// verifyAttempt validates that a new HTLC attempt is compatible with the
// existing payment and its in-flight HTLCs. This function checks:
//  1. MPP (Multi-Path Payment) compatibility between attempts
//  2. Blinded payment consistency
//  3. Amount validation
//  4. Total payment amount limits
func verifyAttempt(payment *MPPayment, attempt *HTLCAttemptInfo) error {
	// If the final hop has encrypted data, then we know this is a
	// blinded payment. In blinded payments, MPP records are not set
	// for split payments and the recipient is responsible for using
	// a consistent PathID across the various encrypted data
	// payloads that we received from them for this payment. All we
	// need to check is that the total amount field for each HTLC
	// in the split payment is correct.
	isBlinded := len(attempt.Route.FinalHop().EncryptedData) != 0

	// Make sure any existing shards match the new one with regards
	// to MPP options.
	mpp := attempt.Route.FinalHop().MPP

	// MPP records should not be set for attempts to blinded paths.
	if isBlinded && mpp != nil {
		return ErrMPPRecordInBlindedPayment
	}

	for _, h := range payment.InFlightHTLCs() {
		hMpp := h.Route.FinalHop().MPP
		hBlinded := len(h.Route.FinalHop().EncryptedData) != 0

		// If this is a blinded payment, then no existing HTLCs
		// should have MPP records.
		if isBlinded && hMpp != nil {
			return ErrMPPRecordInBlindedPayment
		}

		// If the payment is blinded (previous attempts used blinded
		// paths) and the attempt is not, or vice versa, return an
		// error.
		if isBlinded != hBlinded {
			return ErrMixedBlindedAndNonBlindedPayments
		}

		// If this is a blinded payment, then we just need to
		// check that the TotalAmtMsat field for this shard
		// is equal to that of any other shard in the same
		// payment.
		if isBlinded {
			if attempt.Route.FinalHop().TotalAmtMsat !=
				h.Route.FinalHop().TotalAmtMsat {

				return ErrBlindedPaymentTotalAmountMismatch
			}

			continue
		}

		switch {
		// We tried to register a non-MPP attempt for a MPP
		// payment.
		case mpp == nil && hMpp != nil:
			return ErrMPPayment

		// We tried to register a MPP shard for a non-MPP
		// payment.
		case mpp != nil && hMpp == nil:
			return ErrNonMPPayment

		// Non-MPP payment, nothing more to validate.
		case mpp == nil:
			continue
		}

		// Check that MPP options match.
		if mpp.PaymentAddr() != hMpp.PaymentAddr() {
			return ErrMPPPaymentAddrMismatch
		}

		if mpp.TotalMsat() != hMpp.TotalMsat() {
			return ErrMPPTotalAmountMismatch
		}
	}

	// If this is a non-MPP attempt, it must match the total amount
	// exactly. Note that a blinded payment is considered an MPP
	// attempt.
	amt := attempt.Route.ReceiverAmt()
	if !isBlinded && mpp == nil && amt != payment.Info.Value {
		return ErrValueMismatch
	}

	// Ensure we aren't sending more than the total payment amount.
	sentAmt, _ := payment.SentAmt()
	if sentAmt+amt > payment.Info.Value {
		return fmt.Errorf("%w: attempted=%v, payment amount=%v",
			ErrValueExceedsAmt, sentAmt+amt, payment.Info.Value)
	}

	return nil
}
