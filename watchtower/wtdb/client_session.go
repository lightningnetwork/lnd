package wtdb

import (
	"errors"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
)

var (
	// ErrClientSessionNotFound signals that the requested client session
	// was not found in the database.
	ErrClientSessionNotFound = errors.New("client session not found")

	// ErrUpdateAlreadyCommitted signals that the chosen sequence number has
	// already been committed to an update with a different breach hint.
	ErrUpdateAlreadyCommitted = errors.New("update already committed")

	// ErrCommitUnorderedUpdate signals the client tried to commit a
	// sequence number other than the next unallocated sequence number.
	ErrCommitUnorderedUpdate = errors.New("update seqnum not monotonic")

	// ErrCommittedUpdateNotFound signals that the tower tried to ACK a
	// sequence number that has not yet been allocated by the client.
	ErrCommittedUpdateNotFound = errors.New("committed update not found")

	// ErrUnallocatedLastApplied signals that the tower tried to provide a
	// LastApplied value greater than any allocated sequence number.
	ErrUnallocatedLastApplied = errors.New("tower echoed last appiled " +
		"greater than allocated seqnum")
)

// ClientSession encapsulates a SessionInfo returned from a successful
// session negotiation, and also records the tower and ephemeral secret used for
// communicating with the tower.
type ClientSession struct {
	// ID is the client's public key used when authenticating with the
	// tower.
	ID SessionID

	// SeqNum is the next unallocated sequence number that can be sent to
	// the tower.
	SeqNum uint16

	// TowerLastApplied the last last-applied the tower has echoed back.
	TowerLastApplied uint16

	// TowerID is the unique, db-assigned identifier that references the
	// Tower with which the session is negotiated.
	TowerID uint64

	// Tower holds the pubkey and address of the watchtower.
	//
	// NOTE: This value is not serialized. It is recovered by looking up the
	// tower with TowerID.
	Tower *Tower

	// SessionKeyDesc is the key descriptor used to derive the client's
	// session key so that it can authenticate with the tower to update its
	// session.
	SessionKeyDesc keychain.KeyLocator

	// SessionPrivKey is the ephemeral secret key used to connect to the
	// watchtower.
	// TODO(conner): remove after HD keys
	SessionPrivKey *btcec.PrivateKey

	// Policy holds the negotiated session parameters.
	Policy wtpolicy.Policy

	// RewardPkScript is the pkscript that the tower's reward will be
	// deposited to if a sweep transaction confirms and the sessions
	// specifies a reward output.
	RewardPkScript []byte

	// CommittedUpdates is a map from allocated sequence numbers to unacked
	// updates. These updates can be resent after a restart if the update
	// failed to send or receive an acknowledgment.
	CommittedUpdates map[uint16]*CommittedUpdate

	// AckedUpdates is a map from sequence number to backup id to record
	// which revoked states were uploaded via this session.
	AckedUpdates map[uint16]BackupID
}

// BackupID identifies a particular revoked, remote commitment by channel id and
// commitment height.
type BackupID struct {
	// ChanID is the channel id of the revoked commitment.
	ChanID lnwire.ChannelID

	// CommitHeight is the commitment height of the revoked commitment.
	CommitHeight uint64
}

// CommittedUpdate holds a state update sent by a client along with its
// SessionID.
type CommittedUpdate struct {
	BackupID BackupID

	// Hint is the 16-byte prefix of the revoked commitment transaction ID.
	Hint BreachHint

	// EncryptedBlob is a ciphertext containing the sweep information for
	// exacting justice if the commitment transaction matching the breach
	// hint is braodcast.
	EncryptedBlob []byte
}
