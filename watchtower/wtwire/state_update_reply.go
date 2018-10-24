package wtwire

import "io"

// StateUpdateCode is an error code returned by a watchtower in response to a
// StateUpdate message.
type StateUpdateCode = ErrorCode

const (
	// StateUpdateCodeClientBehind signals that the client's sequence number
	// is behind what the watchtower expects based on its LastApplied. This
	// error should cause the client to record the LastApplied field in the
	// response, and initiate another attempt with the proper sequence
	// number.
	//
	// NOTE: Repeated occurrences of this could be interpreted as an attempt
	// to siphon state updates from the client. If the client believes it
	// is not violating the protocol, this could be grounds to blacklist
	// this tower from future session negotiation.
	StateUpdateCodeClientBehind StateUpdateCode = 70

	// StateUpdateCodeMaxUpdatesExceeded signals that the client tried to
	// send a sequence number beyond the negotiated MaxUpdates of the
	// session.
	StateUpdateCodeMaxUpdatesExceeded StateUpdateCode = 71

	// StateUpdateCodeSeqNumOutOfOrder signals the client sent an update
	// that does not follow the required incremental monotonicity required
	// by the tower.
	StateUpdateCodeSeqNumOutOfOrder StateUpdateCode = 72
)

// StateUpdateReply is a message sent from watchtower to client in response to a
// StateUpdate message, and signals either an acceptance or rejection of the
// proposed state update.
type StateUpdateReply struct {
	// Code will be non-zero if the watchtower rejected the state update.
	Code StateUpdateCode

	// LastApplied returns the sequence number of the last accepted update
	// known to the watchtower. If the update was successful, this value
	// should be the sequence number of the last update sent.
	LastApplied uint16
}

// A compile time check to ensure StateUpdateReply implements the wtwire.Message
// interface.
var _ Message = (*StateUpdateReply)(nil)

// Decode deserializes a serialized StateUpdateReply message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (t *StateUpdateReply) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&t.Code,
		&t.LastApplied,
	)
}

// Encode serializes the target StateUpdateReply into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the wtwire.Message interface.
func (t *StateUpdateReply) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		t.Code,
		t.LastApplied,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (t *StateUpdateReply) MsgType() MessageType {
	return MsgStateUpdateReply
}

// MaxPayloadLength returns the maximum allowed payload size for a
// StateUpdateReply complete message observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (t *StateUpdateReply) MaxPayloadLength(uint32) uint32 {
	return 4
}
