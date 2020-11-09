package wtwire

import (
	"io"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/watchtower/blob"
)

// CreateSession is sent from a client to tower when to negotiate a session, which
// specifies the total number of updates that can be made, as well as fee rates.
// An update is consumed by uploading an encrypted blob that contains
// information required to sweep a revoked commitment transaction.
type CreateSession struct {
	// BlobType specifies the blob format that must be used by all updates sent
	// under the session key used to negotiate this session.
	BlobType blob.Type

	// MaxUpdates is the maximum number of updates the watchtower will honor
	// for this session.
	MaxUpdates uint16

	// RewardBase is the fixed amount allocated to the tower when the
	// policy's blob type specifies a reward for the tower. This is taken
	// before adding the proportional reward.
	RewardBase uint32

	// RewardRate is the fraction of the total balance of the revoked
	// commitment that the watchtower is entitled to. This value is
	// expressed in millionths of the total balance.
	RewardRate uint32

	// SweepFeeRate expresses the intended fee rate to be used when
	// constructing the justice transaction. All sweep transactions created
	// for this session must use this value during construction, and the
	// signatures must implicitly commit to the resulting output values.
	SweepFeeRate chainfee.SatPerKWeight
}

// A compile time check to ensure CreateSession implements the wtwire.Message
// interface.
var _ Message = (*CreateSession)(nil)

// Decode deserializes a serialized CreateSession message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *CreateSession) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&m.BlobType,
		&m.MaxUpdates,
		&m.RewardBase,
		&m.RewardRate,
		&m.SweepFeeRate,
	)
}

// Encode serializes the target CreateSession into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the wtwire.Message interface.
func (m *CreateSession) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		m.BlobType,
		m.MaxUpdates,
		m.RewardBase,
		m.RewardRate,
		m.SweepFeeRate,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (m *CreateSession) MsgType() MessageType {
	return MsgCreateSession
}

// MaxPayloadLength returns the maximum allowed payload size for a CreateSession
// complete message observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *CreateSession) MaxPayloadLength(uint32) uint32 {
	return 2 + 2 + 4 + 4 + 8 // 20
}
