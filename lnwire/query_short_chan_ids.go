package lnwire

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
)

// ErrUnsortedSIDs is returned when decoding a QueryShortChannelID request whose
// items were not sorted.
type ErrUnsortedSIDs struct {
	prevSID ShortChannelID
	curSID  ShortChannelID
}

// Error returns a human-readable description of the error.
func (e ErrUnsortedSIDs) Error() string {
	return fmt.Sprintf("current sid: %v isn't greater than last sid: %v",
		e.curSID, e.prevSID)
}

// ErrUnknownShortChanIDEncoding is a parametrized error that indicates that we
// came across an unknown short channel ID encoding, and therefore were unable
// to continue parsing.
func ErrUnknownShortChanIDEncoding(encoding QueryEncoding) error {
	return fmt.Errorf("unknown short chan id encoding: %v", encoding)
}

// ErrZlibNotSupported indicates that the peer attempted to use the deprecated
// zlib encoding for a short channel ID query or response.
var ErrZlibNotSupported = fmt.Errorf("zlib encoding (type %d) is no "+
	"longer supported", EncodingSortedZlib)

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
	// channel ID's of.
	ChainHash chainhash.Hash

	// EncodingType is a signal to the receiver of the message that
	// indicates exactly how the set of short channel ID's that follow have
	// been encoded.
	EncodingType QueryEncoding

	// ShortChanIDs is a slice of decoded short channel ID's.
	ShortChanIDs []ShortChannelID

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData

	// noSort indicates whether or not to sort the short channel ids before
	// writing them out.
	//
	// NOTE: This should only be used during testing.
	noSort bool
}

// NewQueryShortChanIDs creates a new QueryShortChanIDs message.
func NewQueryShortChanIDs(h chainhash.Hash, e QueryEncoding,
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

// A compile time check to ensure QueryShortChanIDs implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*QueryShortChanIDs)(nil)

// Decode deserializes a serialized QueryShortChanIDs message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (q *QueryShortChanIDs) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(r, q.ChainHash[:])
	if err != nil {
		return err
	}

	q.EncodingType, q.ShortChanIDs, err = decodeShortChanIDs(r)
	if err != nil {
		return err
	}

	return q.ExtraData.Decode(r)
}

// decodeShortChanIDs decodes a set of short channel ID's that have been
// encoded. The first byte of the body details how the short chan ID's were
// encoded. We'll use this type to govern exactly how we go about encoding the
// set of short channel ID's.
func decodeShortChanIDs(r io.Reader) (QueryEncoding, []ShortChannelID, error) {
	// First, we'll attempt to read the number of bytes in the body of the
	// set of encoded short channel ID's.
	var numBytesResp uint16
	err := ReadElements(r, &numBytesResp)
	if err != nil {
		return 0, nil, err
	}

	if numBytesResp == 0 {
		return 0, nil, nil
	}

	queryBody := make([]byte, numBytesResp)
	if _, err := io.ReadFull(r, queryBody); err != nil {
		return 0, nil, err
	}

	// The first byte is the encoding type, so we'll extract that so we can
	// continue our parsing.
	encodingType := QueryEncoding(queryBody[0])

	// Before continuing, we'll snip off the first byte of the query body
	// as that was just the encoding type.
	queryBody = queryBody[1:]

	// Otherwise, depending on the encoding type, we'll decode the encode
	// short channel ID's in a different manner.
	switch encodingType {

	// In this encoding, we'll simply read a sort array of encoded short
	// channel ID's from the buffer.
	case EncodingSortedPlain:
		// If after extracting the encoding type, the number of
		// remaining bytes is not a whole multiple of the size of an
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
		if numShortChanIDs == 0 {
			return encodingType, nil, nil
		}

		// Finally, we'll read out the exact number of short channel
		// ID's to conclude our parsing.
		shortChanIDs := make([]ShortChannelID, numShortChanIDs)
		bodyReader := bytes.NewReader(queryBody)
		var lastChanID ShortChannelID
		for i := 0; i < numShortChanIDs; i++ {
			if err := ReadElements(bodyReader, &shortChanIDs[i]); err != nil {
				return 0, nil, fmt.Errorf("unable to parse "+
					"short chan ID: %v", err)
			}

			// We'll ensure that this short chan ID is greater than
			// the last one. This is a requirement within the
			// encoding, and if violated can aide us in detecting
			// malicious payloads. This can only be true starting
			// at the second chanID.
			cid := shortChanIDs[i]
			if i > 0 && cid.ToUint64() <= lastChanID.ToUint64() {
				return 0, nil, ErrUnsortedSIDs{lastChanID, cid}
			}
			lastChanID = cid
		}

		return encodingType, shortChanIDs, nil

	// Zlib encoding was removed from the BOLT 7 spec. If a peer sends
	// us zlib-encoded data, return a specific error so it's clear what
	// happened rather than a generic "unknown encoding" error.
	case EncodingSortedZlib:
		return 0, nil, ErrZlibNotSupported

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
func (q *QueryShortChanIDs) Encode(w *bytes.Buffer, pver uint32) error {
	// First, we'll write out the chain hash.
	if err := WriteBytes(w, q.ChainHash[:]); err != nil {
		return err
	}

	// For both of the current encoding types, the channel ID's are to be
	// sorted in place, so we'll do that now. The sorting is applied unless
	// we were specifically requested not to for testing purposes.
	if !q.noSort {
		sort.Slice(q.ShortChanIDs, func(i, j int) bool {
			return q.ShortChanIDs[i].ToUint64() <
				q.ShortChanIDs[j].ToUint64()
		})
	}

	// Base on our encoding type, we'll write out the set of short channel
	// ID's.
	err := encodeShortChanIDs(w, q.EncodingType, q.ShortChanIDs)
	if err != nil {
		return err
	}

	return WriteBytes(w, q.ExtraData)
}

// encodeShortChanIDs encodes the passed short channel ID's into the passed
// io.Writer, respecting the specified encoding type.
func encodeShortChanIDs(w *bytes.Buffer, encodingType QueryEncoding,
	shortChanIDs []ShortChannelID) error {

	switch encodingType {

	// In this encoding, we'll simply write a sorted array of encoded short
	// channel ID's from the buffer.
	case EncodingSortedPlain:
		// First, we'll write out the number of bytes of the query
		// body. We add 1 as the response will have the encoding type
		// prepended to it.
		numBytesBody := uint16(len(shortChanIDs)*8) + 1
		if err := WriteUint16(w, numBytesBody); err != nil {
			return err
		}

		// We'll then write out the encoding that that follows the
		// actual encoded short channel ID's.
		err := WriteQueryEncoding(w, encodingType)
		if err != nil {
			return err
		}

		// Now that we know they're sorted, we can write out each short
		// channel ID to the buffer.
		for _, chanID := range shortChanIDs {
			if err := WriteShortChannelID(w, chanID); err != nil {
				return fmt.Errorf("unable to write short chan "+
					"ID: %v", err)
			}
		}

		return nil
	case EncodingSortedZlib:
		// Zlib encoding was removed from the BOLT 7 spec. If a peer
		// sends us zlib-encoded data, return a specific error so it's
		// clear what happened rather than a generic "unknown encoding"
		// error.
		return ErrZlibNotSupported

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

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (q *QueryShortChanIDs) SerializedSize() (uint32, error) {
	return MessageSerializedSize(q)
}
