package tlv

import (
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
)

var (
	// ErrInvalidSentinel signals that a sentinel record contained a
	// non-zero length.
	ErrInvalidSentinel = errors.New("invalid tlv sentinel")

	// ErrNotCanonicalStream signals that a decoded stream does not contain
	// records sorting by monotonically-increasing type.
	ErrNotCanonicalStream = errors.New("tlv stream is not canonical")
)

// ParseMode defines various modes when decoding a TLV stream.
type ParseMode uint8

const (
	// ParseModeDiscard will discard unknown records during decoding.
	ParseModeDiscard ParseMode = iota

	// ParseModeRetain will retain unknown records during decoding, allow a
	// successfully decoded stream to be reserialized identically to how it
	// was received.
	ParseModeRetain
)

// RecordEncoder specifies a codec for serializing a given field.
type RecordEncoder func(io.Writer, interface{}) error

// RecordDecoder specifies a codec for deserializing given field. The final
// argument is the expected length, and the method should return an error if
// thee number of bytes read does not match this value.
type RecordDecoder func(io.Reader, interface{}, uint64) error

// Stream defines a TLV stream that can be used for encoding or decoding a set
// of TLV Records.
type Stream struct {
	records []Record
	encoder RecordEncoder
	decoder RecordDecoder

	// retained captures any records with unknown types during decoding to
	// permit identical reserialization of the decoded bytes.
	retained []Record
}

// NewStream crates a new TLV Stream given an encoding code, a decoding codec,
// and a set of known records.
func NewStream(encoder RecordEncoder, decoder RecordDecoder,
	records ...Record) *Stream {

	// Assert that the ordering of the Records is canonical and appear in
	// ascending order of type.
	var min int
	for _, record := range records {
		if int(record.typ) < min {
			panic(ErrNotCanonicalStream.Error())
		}
		min = int(record.typ) + 1
	}

	return &Stream{
		records: records,
		encoder: encoder,
		decoder: decoder,
	}
}

// Encode writes a Stream to the passed io.Writer. Each of the Records known to
// the Stream is written in ascending order of their type so as to be canonical.
// This method can be made to encode a sentinel record by providing a
// MakeSentinelRecord as the last Record during the Stream's creation.
//
// The stream is constructed by concatenating the individual, serialized Records
// where each record has the following format:
//    [1: type]
//    [2: length]
//    [length: value]
//
// An error is returned if the io.Writer fails to accept bytes from the
// encoding, and nothing else. The ordering of the Records is asserted upon the
// creation of a Stream, and thus the output will be by definition canonical.
func (m Stream) Encode(w io.Writer) error {
	var (
		buf         [2]uint8
		typ         Type
		recordIdx   int
		retainedIdx int
	)

	// Iterate through all known and retained records, if any, serializing
	// each record's type, length and value.
	for recordIdx < len(m.records) || retainedIdx < len(m.retained) {
		// Determine the next record to write by comparing the head of
		// the remaining known and retained records.
		var record *Record
		switch {
		case recordIdx == len(m.records):
			record = &m.retained[retainedIdx]
			retainedIdx++
		case retainedIdx == len(m.retained):
			record = &m.records[recordIdx]
			recordIdx++
		case m.records[recordIdx].typ < m.retained[retainedIdx].typ:
			record = &m.records[recordIdx]
			recordIdx++
		default:
			record = &m.retained[retainedIdx]
			retainedIdx++
		}

		// Write the record's type.
		typ = record.typ
		buf[0] = uint8(typ)
		_, err := w.Write(buf[:1])
		if err != nil {
			return err
		}

		// Write the record's 2-byte length.
		binary.BigEndian.PutUint16(buf[:], record.Size())
		_, err = w.Write(buf[:])
		if err != nil {
			return err
		}

		// There is nothing to encode for a sentinel value, and by
		// definition must be the last element in the stream. Exit early
		// since the codec won't know how to process a nil element and
		// it must have a zero-length value.
		if typ == SentinelType {
			return nil
		}

		// Encode the current record's value using the stream's codec.
		err = m.encoder(w, record.value)
		if err != nil {
			return err
		}
	}

	return nil
}

// Decode deserializes TLV Stream from the passed io.Reader. The Stream will
// inspect each record that is parsed and check to see if it has a corresponding
// Record to facilitate deserialization of that field. If the record is unknown,
// the Stream will discard the record's bytes and proceed to the subsequent
// record.
//
// Each record has the following format:
//    [1: type]
//    [2: length]
//    [length: value]
//
// A series of (possibly zero) records are concatenated into a stream,
// optionally including a sentinel record. The following are examples of valid
// streams:
//
//    Stream without Sentinel Record:
//    (t: 0x01, l: 0x0004, v: 0xff, 0xff, 0xff, 0xff)
//
//    Stream with Sentinel Record:
//    (t: 0x02, l: 0x0002, v: 0xac, 0xbd)
//    (t: 0xff, l: 0x0000, v: )
//
// This method asserts that the byte stream is canonical, namely that each
// record is unique and that all records are sorted in ascending order. An
// ErrNotCanonicalStream error is returned if the encoded TLV stream is not.
//
// The sentinel record is identified via a type of SentinelType (0xff). When
// this record is encountered, the Stream will verify that the encoded length is
// zero and stop reading. If the length is not zero, an ErrInvalidSentinel error
// is returned.
//
// In the event that a TLV stream does not have a sentinel record, we permit an
// io.EOF error only when reading the type byte which signals that the last
// record was read cleanly and we should stop parsing. All other io.EOF or
// io.ErrUnexpectedEOF errors are returned.
//
// If ParseModeRetain is used, any unknown fields will be blindly copied so that
// they may be reserialized via a subsequent call to Encode. Otherwise, unknown
// fields are discarded.
func (s *Stream) Decode(r io.Reader, mode ParseMode) error {
	var (
		buf       [2]byte
		typ       Type
		length    uint64
		recordIdx int
		retained  []Record
	)

	// Iterate through all possible type identifiers. As types are read from
	// the io.Reader, min will skip forward to the last read type.
	for min := 0; min < 256; min++ {
		// Read the next type byte.
		_, err := io.ReadFull(r, buf[:1])
		switch {

		// We'll silence an EOF in this case in case the stream doesn't
		// have a sentinel record.
		case err == io.EOF:
			s.retained = retained
			return nil

		// Other unexpected errors.
		case err != nil:
			return err
		}

		typ = Type(buf[0])

		// Assert that this type is greater than any previously read.
		// This check prevents us from accepts encodings that have
		// duplicate records or from accepting an unsorted series.
		if int(typ) < min {
			return ErrNotCanonicalStream
		}

		// Read the 2-byte length.
		_, err = io.ReadFull(r, buf[:])
		if err != nil {
			return err
		}
		length = uint64(binary.BigEndian.Uint16(buf[:]))

		// Stop reading if we receive a sentinel record.
		if typ == SentinelType {
			switch {

			// If we are in retain mode, we must also retain the
			// sentinel record.
			case mode == ParseModeRetain:
				retained = append(
					retained, MakeSentinelRecord(),
				)
				fallthrough

			// Assert that the sentinel record has length zero. If
			// so, store the retained values so they can be encoded.
			case length == 0:
				s.retained = retained
				return nil

			// Sentinel value had a non-zero length, fail.
			default:
				return ErrInvalidSentinel
			}
		}

		// Otherwise, we are processing some non-sentinel record. Search
		// the records known to the stream for this type. We'll begin
		// the search and recordIdx and walk forward until we find it
		// or the next record's type is larger.
		field, newIdx, ok := s.getRecord(typ, recordIdx)
		switch {

		// We know of this record type, proceed to decode the value.
		// This method asserts that length bytes are read in the
		// process, and returns an error if the number of bytes is not
		// exactly length.
		case ok:
			err := s.decoder(r, field.value, length)
			if err != nil {
				return err
			}

		// This record type is unknown to the stream and the parse mode
		// requires we retain the value, discard exactly the number of
		// bytes specified by length.
		case mode == ParseModeRetain:
			retainedValue := make([]byte, length)
			_, err := io.ReadFull(r, retainedValue)
			if err != nil {
				return err
			}

			retainedRecord := makeRetainedRecord(typ, &retainedValue)
			retained = append(retained, retainedRecord)

		// This record type is unknown to the stream and the parse mode
		// does not require retaining the value, discard exactly the
		// number of bytes specified by length.
		case mode == ParseModeDiscard:
			_, err := io.CopyN(ioutil.Discard, r, int64(length))
			if err != nil {
				return err
			}
		}
		// Update our record index so that we can begin our next search
		// from where we left off.
		recordIdx = newIdx

		// Finally, set our lower bound on the next accept type to type
		// just processed. On the next iteration, this value will be
		// incremented ensuring the next type is greater.
		min = int(typ)
	}

	panic("unreachable")
}

// getRecord searches for a record matching typ known to the stream. The boolean
// return value indicates whether the record is known to the stream. The integer
// return value carries the index from where getRecord should be invoked on the
// subsequent call. The first call to getRecord should always use an idx of 0.
func (s *Stream) getRecord(typ Type, idx int) (Record, int, bool) {
	for idx < len(s.records) {
		record := s.records[idx]
		switch {

		// Found target record, return it to the caller. The next index
		// returned points to the immediately following record.
		case record.typ == typ:
			return record, idx + 1, true

		// This record's type is lower than the target. Advance our
		// index and continue to the next record which will have a
		// strictly higher type.
		case record.typ < typ:
			idx++
			continue

		// This record's type is larger than the target, hence we have
		// no record matching the current type. Return the current index
		// so that we can start our search from here when processing the
		// next tlv record.
		default:
			return Record{}, idx, false
		}
	}

	// All known records are exhausted.
	return Record{}, idx, false
}
