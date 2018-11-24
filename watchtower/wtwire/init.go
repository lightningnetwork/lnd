package wtwire

import "github.com/lightningnetwork/lnd/lnwire"

// Init is the first message sent over the watchtower wire protocol, and
// specifies features and level of requiredness maintained by the sending node.
// The watchtower Init message is identical to the LN Init message, except it
// uses a different message type to ensure the two are not conflated.
type Init struct {
	*lnwire.Init
}

// NewInitMessage generates a new Init message from raw global and local feature
// vectors.
func NewInitMessage(gf, lf *lnwire.RawFeatureVector) *Init {
	return &Init{
		Init: lnwire.NewInitMessage(gf, lf),
	}
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (msg *Init) MsgType() MessageType {
	return MsgInit
}

// A compile-time constraint to ensure Init implements the Message interface.
var _ Message = (*Init)(nil)
