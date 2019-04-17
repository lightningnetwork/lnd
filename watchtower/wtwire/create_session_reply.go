package wtwire

import "io"

// CreateSessionCode is an error code returned by a watchtower in response to a
// CreateSession message. The code directs the client in interpreting the payload
// in the reply.
type CreateSessionCode = ErrorCode

const (
	// CreateSessionCodeAlreadyExists is returned when a session is already
	// active for the public key used to connect to the watchtower. The
	// response includes the serialized reward address in case the original
	// reply was never received and/or processed by the client.
	CreateSessionCodeAlreadyExists CreateSessionCode = 60

	// CreateSessionCodeRejectMaxUpdates the tower rejected the maximum
	// number of state updates proposed by the client.
	CreateSessionCodeRejectMaxUpdates CreateSessionCode = 61

	// CreateSessionCodeRejectRewardRate the tower rejected the reward rate
	// proposed by the client.
	CreateSessionCodeRejectRewardRate CreateSessionCode = 62

	// CreateSessionCodeRejectSweepFeeRate the tower rejected the sweep fee
	// rate proposed by the client.
	CreateSessionCodeRejectSweepFeeRate CreateSessionCode = 63

	// CreateSessionCodeRejectBlobType is returned when the tower does not
	// support the proposed blob type.
	CreateSessionCodeRejectBlobType CreateSessionCode = 64
)

// MaxCreateSessionReplyDataLength is the maximum size of the Data payload
// returned in a CreateSessionReply message. This does not include the length of
// the Data field, which is a varint up to 3 bytes in size.
const MaxCreateSessionReplyDataLength = 1024

// CreateSessionReply is a message sent from watchtower to client in response to a
// CreateSession message, and signals either an acceptance or rejection of the
// proposed session parameters.
type CreateSessionReply struct {
	// Code will be non-zero if the watchtower rejected the session init.
	Code CreateSessionCode

	// Data is a byte slice returned the caller of the message, and is to be
	// interpreted according to the error Code. When the response is
	// CreateSessionCodeOK, data encodes the reward address to be included in
	// any sweep transactions if the reward is not dusty. Otherwise, it may
	// encode the watchtowers configured parameters for any policy
	// rejections.
	Data []byte
}

// A compile time check to ensure CreateSessionReply implements the wtwire.Message
// interface.
var _ Message = (*CreateSessionReply)(nil)

// Decode deserializes a serialized CreateSessionReply message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *CreateSessionReply) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&m.Code,
		&m.Data,
	)
}

// Encode serializes the target CreateSessionReply into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the wtwire.Message interface.
func (m *CreateSessionReply) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		m.Code,
		m.Data,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (m *CreateSessionReply) MsgType() MessageType {
	return MsgCreateSessionReply
}

// MaxPayloadLength returns the maximum allowed payload size for a CreateSessionReply
// complete message observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *CreateSessionReply) MaxPayloadLength(uint32) uint32 {
	return 2 + 3 + MaxCreateSessionReplyDataLength
}
