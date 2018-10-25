package wtwire

import "io"

// StateUpdate transmits an encrypted state update from the client to the
// watchtower. Each state update is tied to particular session, identified by
// the client's brontine key used to make the request.
type StateUpdate struct {
	// SeqNum is a 1-indexed, monotonically incrementing sequence number.
	// This number represents to the client's expected sequence number when
	// sending updates sent to the watchtower. This value must always be
	// less or equal than the negotiated MaxUpdates for the session, and
	// greater than the LastApplied sent in the same message.
	SeqNum uint16

	// LastApplied echos the LastApplied value returned from watchtower,
	// allowing the tower to detect faulty clients. This allow provides a
	// feedback mechanism for the tower if updates are allowed to stream in
	// an async fashion.
	LastApplied uint16

	// IsComplete is 1 if the watchtower should close the connection after
	// responding, and 0 otherwise.
	IsComplete uint8

	// Hint is the 16-byte prefix of the revoked commitment transaction ID
	// for which the encrypted blob can exact justice.
	Hint [16]byte

	// EncryptedBlob is the serialized ciphertext containing all necessary
	// information to sweep the commitment transaction corresponding to the
	// Hint. The ciphertext is to be encrypted using the full transaction ID
	// of the revoked commitment transaction.
	//
	// The plaintext MUST be encoded using the negotiated Version for
	// this session. In addition, the signatures must be computed over a
	// sweep transaction honoring the decided SweepFeeRate, RewardRate, and
	// (possibly) reward address returned in the SessionInitReply.
	EncryptedBlob []byte
}

// A compile time check to ensure StateUpdate implements the wtwire.Message
// interface.
var _ Message = (*StateUpdate)(nil)

// Decode deserializes a serialized StateUpdate message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *StateUpdate) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&m.SeqNum,
		&m.LastApplied,
		&m.IsComplete,
		&m.Hint,
		&m.EncryptedBlob,
	)
}

// Encode serializes the target StateUpdate into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the wtwire.Message interface.
func (m *StateUpdate) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		m.SeqNum,
		m.LastApplied,
		m.IsComplete,
		m.Hint,
		m.EncryptedBlob,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (m *StateUpdate) MsgType() MessageType {
	return MsgStateUpdate
}

// MaxPayloadLength returns the maximum allowed payload size for a StateUpdate
// complete message observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *StateUpdate) MaxPayloadLength(uint32) uint32 {
	return MaxMessagePayload
}
