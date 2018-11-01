package wtdb

import (
	"errors"

	"github.com/btcsuite/btcutil"
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

	// ErrFeeExceedsInputs signals that the total input value of breaching
	// commitment txn is insufficient to cover the fees required to sweep
	// it.
	ErrFeeExceedsInputs = errors.New("sweep fee exceeds input values")
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

// ComputeSweepOutputs splits the total funds in a breaching commitment
// transaction between the victim and the tower, according to the sweep fee rate
// and reward rate. The fees are first subtracted from the overall total, before
// splitting the remaining balance amongst the victim and tower.
func (s *SessionInfo) ComputeSweepOutputs(totalAmt btcutil.Amount,
	txVSize int64) (btcutil.Amount, btcutil.Amount, error) {

	txFee := s.SweepFeeRate.FeeForWeight(txVSize)
	if txFee > totalAmt {
		return 0, 0, ErrFeeExceedsInputs
	}

	totalAmt -= txFee

	// Apply the reward rate to the remaining total, specified in millionths
	// of the available balance.
	rewardAmt := (totalAmt*btcutil.Amount(s.RewardRate) + 999999) / 1000000
	sweepAmt := totalAmt - rewardAmt

	// TODO(conner): check dustiness

	return sweepAmt, rewardAmt, nil
}

// Match is returned in response to a database query for a breach hints
// contained in a particular block. The match encapsulates all data required to
// properly decrypt a client's encrypted blob, and pursue action on behalf of
// the victim by reconstructing the justice transaction and broadcasting it to
// the network.
//
// NOTE: It is possible for a match to cause a false positive, since they are
// matched on a prefix of the txid. In such an event, the likely behavior is
// that the payload will fail to decrypt.
type Match struct {
	// ID is the session id of the client who uploaded the state update.
	ID SessionID

	// SeqNum is the session sequence number occupied by the client's state
	// update. Together with ID, this allows the tower to derive the
	// appropriate nonce for decryption.
	SeqNum uint16

	// Hint is the breach hint that triggered the match.
	Hint BreachHint

	// EncryptedBlob is the encrypted payload containing the justice kit
	// uploaded by the client.
	EncryptedBlob []byte

	// SessionInfo is the contract negotiated between tower and client, that
	// provides input parameters such as fee rate, reward rate, and reward
	// address when attempting to reconstruct the justice transaction.
	SessionInfo *SessionInfo
}
