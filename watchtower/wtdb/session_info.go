package wtdb

import (
	"errors"

	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	// ErrSessionNotFound is returned when querying by session id for a
	// session that does not exist.
	ErrSessionNotFound = errors.New("session not found in db")

	// ErrSessionAlreadyExists signals that a session creation failed
	// because a session with the same session id already exists.
	ErrSessionAlreadyExists = errors.New("session already exists")

	// ErrUpdateOutOfOrder is returned when the sequence number is not equal
	// to the server's LastApplied+1.
	ErrUpdateOutOfOrder = errors.New("update sequence number is not " +
		"sequential")

	// ErrLastAppliedReversion is returned when the client echos a
	// last-applied value that is less than it claimed in a prior update.
	ErrLastAppliedReversion = errors.New("update last applied must be " +
		"non-decreasing")

	// ErrSeqNumAlreadyApplied is returned when the client sends a sequence
	// number for which they already claim to have an ACK.
	ErrSeqNumAlreadyApplied = errors.New("update sequence number has " +
		"already been applied")

	// ErrSessionConsumed is returned if the client tries to send a sequence
	// number larger than the session's max number of updates.
	ErrSessionConsumed = errors.New("all session updates have been " +
		"consumed")
)

// SessionInfo holds the negotiated session parameters for single session id,
// and handles the acceptance and validation of state updates sent by the
// client.
type SessionInfo struct {
	// ID is the remote public key of the watchtower client.
	ID SessionID

	// Version specifies the plaintext blob encoding of all state updates.
	Version uint16

	// MaxUpdates is the total number of updates the client can send for
	// this session.
	MaxUpdates uint16

	// LastApplied the sequence number of the last successful state update.
	LastApplied uint16

	// ClientLastApplied the last last-applied the client has echoed back.
	ClientLastApplied uint16

	// RewardRate the fraction of the swept amount that goes to the tower,
	// expressed in millionths of the swept balance.
	RewardRate uint32

	// SweepFeeRate is the agreed upon fee rate used to sign any sweep
	// transactions.
	SweepFeeRate lnwallet.SatPerKWeight

	// RewardAddress the address that the tower's reward will be deposited
	// to if a sweep transaction confirms.
	RewardAddress []byte

	// TODO(conner): store client metrics, DOS score, etc
}

// AcceptUpdateSequence validates that a state update's sequence number and last
// applied are valid given our past history with the client. These checks ensure
// that clients are properly in sync and following the update protocol properly.
// If validation is successful, the receiver's LastApplied and ClientLastApplied
// are updated with the latest values presented by the client. Any errors
// returned from this method are converted into an appropriate
// wtwire.StateUpdateCode.
func (s *SessionInfo) AcceptUpdateSequence(seqNum, lastApplied uint16) error {
	switch {

	// Client already claims to have an ACK for this seqnum.
	case seqNum <= lastApplied:
		return ErrSeqNumAlreadyApplied

	// Client echos a last applied that is lower than previously sent.
	case lastApplied < s.ClientLastApplied:
		return ErrLastAppliedReversion

	// Client update exceeds capacity of session.
	case seqNum > s.MaxUpdates:
		return ErrSessionConsumed

	// Client update does not match our expected next seqnum.
	case seqNum != s.LastApplied+1:
		return ErrUpdateOutOfOrder
	}

	s.LastApplied = seqNum
	s.ClientLastApplied = lastApplied

	return nil
}
