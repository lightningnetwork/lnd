package lnwire

import "io"

// ReplyChannelRange is the response to the QueryChannelRange message. It
// includes the original query, and the next streaming chunk of encoded short
// channel ID's as the response. We'll also include a byte that indicates if
// this is the last query in the message.
type ReplyChannelRange struct {
	// QueryChannelRange is the corresponding query to this response.
	QueryChannelRange

	// Complete denotes if this is the conclusion of the set of streaming
	// responses to the original query.
	Complete uint8

	// EncodingType is a signal to the receiver of the message that
	// indicates exactly how the set of short channel ID's that follow have
	// been encoded.
	EncodingType ShortChanIDEncoding

	// ShortChanIDs is a slice of decoded short channel ID's.
	ShortChanIDs []ShortChannelID

	// noSort indicates whether or not to sort the short channel ids before
	// writing them out.
	//
	// NOTE: This should only be used for testing.
	noSort bool
}

// NewReplyChannelRange creates a new empty ReplyChannelRange message.
func NewReplyChannelRange() *ReplyChannelRange {
	return &ReplyChannelRange{}
}

// A compile time check to ensure ReplyChannelRange implements the
// lnwire.Message interface.
var _ Message = (*ReplyChannelRange)(nil)

// Decode deserializes a serialized ReplyChannelRange message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ReplyChannelRange) Decode(r io.Reader, pver uint32) error {
	err := c.QueryChannelRange.Decode(r, pver)
	if err != nil {
		return err
	}

	if err := ReadElements(r, &c.Complete); err != nil {
		return err
	}

	c.EncodingType, c.ShortChanIDs, err = decodeShortChanIDs(r)

	return err
}

// Encode serializes the target ReplyChannelRange into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ReplyChannelRange) Encode(w io.Writer, pver uint32) error {
	if err := c.QueryChannelRange.Encode(w, pver); err != nil {
		return err
	}

	if err := WriteElements(w, c.Complete); err != nil {
		return err
	}

	return encodeShortChanIDs(w, c.EncodingType, c.ShortChanIDs, c.noSort)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ReplyChannelRange) MsgType() MessageType {
	return MsgReplyChannelRange
}

// MaxPayloadLength returns the maximum allowed payload size for a
// ReplyChannelRange complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ReplyChannelRange) MaxPayloadLength(uint32) uint32 {
	return MaxMessagePayload
}
