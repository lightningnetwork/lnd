package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// ReplyShortChanIDsEnd is a message that marks the end of a streaming message
// response to an initial QueryShortChanIDs message. This marks that the
// receiver of the original QueryShortChanIDs for the target chain has either
// sent all adequate responses it knows of, or doesn't know of any short chan
// ID's for the target chain.
type ReplyShortChanIDsEnd struct {
	// ChainHash denotes the target chain that we're respond to a short
	// chan ID query for.
	ChainHash chainhash.Hash

	// Complete will be set to 0 if we don't know of the chain that the
	// remote peer sent their query for. Otherwise, we'll set this to 1 in
	// order to indicate that we've sent all known responses for the prior
	// set of short chan ID's in the corresponding QueryShortChanIDs
	// message.
	Complete uint8
}

// NewReplyShortChanIDsEnd creates a new empty ReplyShortChanIDsEnd message.
func NewReplyShortChanIDsEnd() *ReplyShortChanIDsEnd {
	return &ReplyShortChanIDsEnd{}
}

// A compile time check to ensure ReplyShortChanIDsEnd implements the
// lnwire.Message interface.
var _ Message = (*ReplyShortChanIDsEnd)(nil)

// Decode deserializes a serialized ReplyShortChanIDsEnd message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ReplyShortChanIDsEnd) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		c.ChainHash[:],
		&c.Complete,
	)
}

// Encode serializes the target ReplyShortChanIDsEnd into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ReplyShortChanIDsEnd) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		c.ChainHash[:],
		c.Complete,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ReplyShortChanIDsEnd) MsgType() MessageType {
	return MsgReplyShortChanIDsEnd
}

// MaxPayloadLength returns the maximum allowed payload size for a
// ReplyShortChanIDsEnd complete message observing the specified protocol
// version.
//
// This is part of the lnwire.Message interface.
func (c *ReplyShortChanIDsEnd) MaxPayloadLength(uint32) uint32 {
	// 32 (chain hash) + 1 (complete)
	return 33
}
