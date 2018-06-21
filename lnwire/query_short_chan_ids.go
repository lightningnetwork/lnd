package lnwire

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// ShortChanIDEncoding is an enum-like type that represents exactly how a set
// of short channel ID's is encoded on the wire. The set of encodings allows us
// to take advantage of the structure of a list of short channel ID's to
// achieving a high degree of compression.
type ShortChanIDEncoding uint8

const (
	// EncodingSortedPlain signals that the set of short channel ID's is
	// encoded using the regular encoding, in a sorted order.
	EncodingSortedPlain ShortChanIDEncoding = 0

	// TODO(roasbeef): list max number of short chan id's that are able to
	// use
)

// ErrUnknownShortChanIDEncoding is a parametrized error that indicates that we
// came across an unknown short channel ID encoding, and therefore were unable
// to continue parsing.
func ErrUnknownShortChanIDEncoding(encoding ShortChanIDEncoding) error {
	return fmt.Errorf("unknown short chan id encoding: %v", encoding)
}

// QueryShortChanIDs is a message that allows the sender to query a set of
// channel announcement and channel update messages that correspond to the set
// of encoded short channel ID's. The encoding of the short channel ID's is
// detailed in the query message ensuring that the receiver knows how to
// properly decode each encode short channel ID which may be encoded using a
// compression format. The receiver should respond with a series of channel
// announcement and channel updates, finally sending a ReplyShortChanIDsEnd
// message.
type QueryShortChanIDs struct {
	// ChainHash denotes the target chain that we're querying for the
	// channel channel ID's of.
	ChainHash chainhash.Hash

	// EncodingType is a signal to the receiver of the message that
	// indicates exactly how the set of short channel ID's that follow have
	// been encoded.
	EncodingType ShortChanIDEncoding

	// ShortChanIDs is a slice of decoded short channel ID's.
	ShortChanIDs []ShortChannelID
}

// NewQueryShortChanIDs creates a new QueryShortChanIDs message.
func NewQueryShortChanIDs(h chainhash.Hash, e ShortChanIDEncoding,
	s []ShortChannelID) *QueryShortChanIDs {

	return &QueryShortChanIDs{
		ChainHash:    h,
		EncodingType: e,
		ShortChanIDs: s,
	}
}

// A compile time check to ensure QueryShortChanIDs implements the
// lnwire.Message interface.
var _ Message = (*QueryShortChanIDs)(nil)

// Decode deserializes a serialized QueryShortChanIDs message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (q *QueryShortChanIDs) Decode(r io.Reader, pver uint32) error {
	err := readElements(r, q.ChainHash[:])
	if err != nil {
		return err
	}

	q.EncodingType, q.ShortChanIDs, err = decodeShortChanIDs(r)

	return err
}

// decodeShortChanIDs decodes a set of short channel ID's that have been
// encoded. The first byte of the body details how the short chan ID's were
// encoded. We'll use this type to govern exactly how we go about encoding the
// set of short channel ID's.
func decodeShortChanIDs(r io.Reader) (ShortChanIDEncoding, []ShortChannelID, error) {
	// First, we'll attempt to read the number of bytes in the body of the
	// set of encoded short channel ID's.
	var numBytesResp uint16
	err := readElements(r, &numBytesResp)
	if err != nil {
		return 0, nil, err
	}

	queryBody := make([]byte, numBytesResp)
	if _, err := io.ReadFull(r, queryBody); err != nil {
		return 0, nil, err
	}

	// The first byte is the encoding type, so we'll extract that so we can
	// continue our parsing.
	encodingType := ShortChanIDEncoding(queryBody[0])

	// Before continuing, we'll snip off the first byte of the query body
	// as that was just the encoding type.
	queryBody = queryBody[1:]

	// Otherwise, depending on the encoding type, we'll decode the encode
	// short channel ID's in a different manner.
	switch encodingType {

	// In this encoding, we'll simply read a sort array of encoded short
	// channel ID's from the buffer.
	case EncodingSortedPlain:
		// If after extracting the encoding type, then number of
		// remaining bytes instead a whole multiple of the size of an
		// encoded short channel ID (8 bytes), then we'll return a
		// parsing error.
		if len(queryBody)%8 != 0 {
			return 0, nil, fmt.Errorf("whole number of short "+
				"chan ID's cannot be encoded in len=%v",
				len(queryBody))
		}

		// As each short channel ID is encoded as 8 bytes, we can
		// compute the number of bytes encoded based on the size of the
		// query body.
		numShortChanIDs := len(queryBody) / 8
		shortChanIDs := make([]ShortChannelID, numShortChanIDs)

		// Finally, we'll read out the exact number of short channel
		// ID's to conclude our parsing.
		bodyReader := bytes.NewReader(queryBody)
		for i := 0; i < numShortChanIDs; i++ {
			if err := readElements(bodyReader, &shortChanIDs[i]); err != nil {
				return 0, nil, fmt.Errorf("unable to parse "+
					"short chan ID: %v", err)
			}
		}

		return encodingType, shortChanIDs, nil

	default:
		// If we've been sent an encoding type that we don't know of,
		// then we'll return a parsing error as we can't continue if
		// we're unable to encode them.
		return 0, nil, ErrUnknownShortChanIDEncoding(encodingType)
	}
}

// Encode serializes the target QueryShortChanIDs into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (q *QueryShortChanIDs) Encode(w io.Writer, pver uint32) error {
	// First, we'll write out the chain hash.
	err := writeElements(w, q.ChainHash[:])
	if err != nil {
		return err
	}

	// Base on our encoding type, we'll write out the set of short channel
	// ID's.
	return encodeShortChanIDs(w, q.EncodingType, q.ShortChanIDs)
}

// encodeShortChanIDs encodes the passed short channel ID's into the passed
// io.Writer, respecting the specified encoding type.
func encodeShortChanIDs(w io.Writer, encodingType ShortChanIDEncoding,
	shortChanIDs []ShortChannelID) error {

	switch encodingType {

	// In this encoding, we'll simply write a sorted array of encoded short
	// channel ID's from the buffer.
	case EncodingSortedPlain:
		// First, we'll write out the number of bytes of the query
		// body. We add 1 as the response will have the encoding type
		// prepended to it.
		numBytesBody := uint16(len(shortChanIDs)*8) + 1
		if err := writeElements(w, numBytesBody); err != nil {
			return err
		}

		// We'll then write out the encoding that that follows the
		// actual encoded short channel ID's.
		if err := writeElements(w, encodingType); err != nil {
			return err
		}

		// Next, we'll ensure that the set of short channel ID's is
		// properly sorted in place.
		sort.Slice(shortChanIDs, func(i, j int) bool {
			return shortChanIDs[i].ToUint64() <
				shortChanIDs[j].ToUint64()
		})

		// Now that we know they're sorted, we can write out each short
		// channel ID to the buffer.
		for _, chanID := range shortChanIDs {
			if err := writeElements(w, chanID); err != nil {
				return fmt.Errorf("unable to write short chan "+
					"ID: %v", err)
			}
		}

		return nil

	default:
		// If we're trying to encode with an encoding type that we
		// don't know of, then we'll return a parsing error as we can't
		// continue if we're unable to encode them.
		return ErrUnknownShortChanIDEncoding(encodingType)
	}
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (q *QueryShortChanIDs) MsgType() MessageType {
	return MsgQueryShortChanIDs
}

// MaxPayloadLength returns the maximum allowed payload size for a
// QueryShortChanIDs complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (q *QueryShortChanIDs) MaxPayloadLength(uint32) uint32 {
	return MaxMessagePayload
}
