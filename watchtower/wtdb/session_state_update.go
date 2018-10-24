package wtdb

// SessionStateUpdate holds a state update sent by a client along with its
// SessionID.
type SessionStateUpdate struct {
	// ID the session id of the client who sent the state update.
	ID SessionID

	// SeqNum the sequence number of the update within the session.
	SeqNum uint16

	// LastApplied the highest index that client has acknowledged is
	// committed
	LastApplied uint16

	// Hint is the 16-byte prefix of the revoked commitment transaction.
	Hint BreachHint

	// EncryptedBlob is a ciphertext containing the sweep information for
	// exacting justice if the commitment transaction matching the breach
	// hint is braodcast.
	EncryptedBlob []byte
}
