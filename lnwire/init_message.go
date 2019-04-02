package lnwire

import "io"

// Init is the first message reveals the features supported or required by this
// node. Nodes wait for receipt of the other's features to simplify error
// diagnosis where features are incompatible. Each node MUST wait to receive
// init before sending any other messages.
type Init struct {
	// GlobalFeatures is feature vector which affects HTLCs and thus are
	// also advertised to other nodes.
	GlobalFeatures *RawFeatureVector

	// LocalFeatures is feature vector which only affect the protocol
	// between two nodes.
	LocalFeatures *RawFeatureVector
}

// NewInitMessage creates new instance of init message object.
func NewInitMessage(gf *RawFeatureVector, lf *RawFeatureVector) *Init {
	return &Init{
		GlobalFeatures: gf,
		LocalFeatures:  lf,
	}
}

// A compile time check to ensure Init implements the lnwire.Message
// interface.
var _ Message = (*Init)(nil)

// Decode deserializes a serialized Init message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&msg.GlobalFeatures,
		&msg.LocalFeatures,
	)
}

// Encode serializes the target Init into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		msg.GlobalFeatures,
		msg.LocalFeatures,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (msg *Init) MsgType() MessageType {
	return MsgInit
}

// MaxPayloadLength returns the maximum allowed payload size for an Init
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *Init) MaxPayloadLength(uint32) uint32 {
	return 2 + 2 + maxAllowedSize + 2 + maxAllowedSize
}
