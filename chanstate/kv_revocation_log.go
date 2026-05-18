package chanstate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// revocationLogBucket is a sub-bucket under openChannelBucket. This
	// sub-bucket is dedicated for storing the minimal info required to
	// re-construct a past state in order to punish a counterparty
	// attempting a non-cooperative channel closure.
	revocationLogBucket = []byte("revocation-log")

	// ErrLogEntryNotFound is returned when we cannot find a log entry at
	// the height requested in the revocation log.
	ErrLogEntryNotFound = errors.New("log entry not found")

	// ErrOutputIndexTooBig is returned when the output index is greater
	// than uint16.
	ErrOutputIndexTooBig = errors.New("output index is over uint16")
)

// RevocationLogBucketKey returns the sub-bucket key that stores the current
// revocation log format.
func RevocationLogBucketKey() []byte {
	return revocationLogBucket
}

// PutRevocationLog uses the fields `CommitTx` and `Htlcs` from a
// ChannelCommitment to construct a revocation log entry and saves them to
// disk. It also saves our output index and their output index, which are
// useful when creating breach retribution.
func PutRevocationLog(bucket kvdb.RwBucket, commit *ChannelCommitment,
	ourOutputIndex, theirOutputIndex uint32, noAmtData bool) error {

	// Sanity check that the output indexes can be safely converted.
	if ourOutputIndex > math.MaxUint16 {
		return ErrOutputIndexTooBig
	}
	if theirOutputIndex > math.MaxUint16 {
		return ErrOutputIndexTooBig
	}

	rl := &RevocationLog{
		OurOutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType0](
			uint16(ourOutputIndex),
		),
		TheirOutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType1](
			uint16(theirOutputIndex),
		),
		CommitTxHash: tlv.NewPrimitiveRecord[tlv.TlvType2, [32]byte](
			commit.CommitTx.TxHash(),
		),
		HTLCEntries: make([]*HTLCEntry, 0, len(commit.Htlcs)),
	}

	commit.CustomBlob.WhenSome(func(blob tlv.Blob) {
		rl.CustomBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5, tlv.Blob](blob),
		)
	})

	if !noAmtData {
		rl.OurBalance = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType3](
			tlv.NewBigSizeT(commit.LocalBalance),
		))

		rl.TheirBalance = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType4](
			tlv.NewBigSizeT(commit.RemoteBalance),
		))
	}

	for _, htlc := range commit.Htlcs {
		// Skip dust HTLCs.
		if htlc.OutputIndex < 0 {
			continue
		}

		// Sanity check that the output indexes can be safely
		// converted.
		if htlc.OutputIndex > math.MaxUint16 {
			return ErrOutputIndexTooBig
		}

		entry, err := NewHTLCEntryFromHTLC(htlc)
		if err != nil {
			return err
		}
		rl.HTLCEntries = append(rl.HTLCEntries, entry)
	}

	var b bytes.Buffer
	err := SerializeRevocationLog(&b, rl)
	if err != nil {
		return err
	}

	logEntrykey := revocationLogKey(commit.CommitHeight)

	return bucket.Put(logEntrykey[:], b.Bytes())
}

// FetchRevocationLog queries the revocation log bucket to find an log entry.
// Return an error if not found.
func FetchRevocationLog(log kvdb.RBucket,
	updateNum uint64) (RevocationLog, error) {

	logEntrykey := revocationLogKey(updateNum)
	commitBytes := log.Get(logEntrykey[:])
	if commitBytes == nil {
		return RevocationLog{}, ErrLogEntryNotFound
	}

	commitReader := bytes.NewReader(commitBytes)

	return DeserializeRevocationLog(commitReader)
}

// SerializeRevocationLog serializes a RevocationLog record based on tlv
// format.
func SerializeRevocationLog(w io.Writer, rl *RevocationLog) error {
	// Add the tlv records for all non-optional fields.
	records := []tlv.Record{
		rl.OurOutputIndex.Record(),
		rl.TheirOutputIndex.Record(),
		rl.CommitTxHash.Record(),
	}

	// Now we add any optional fields that are non-nil.
	rl.OurBalance.WhenSome(
		func(r tlv.RecordT[tlv.TlvType3, BigSizeMilliSatoshi]) {
			records = append(records, r.Record())
		},
	)

	rl.TheirBalance.WhenSome(
		func(r tlv.RecordT[tlv.TlvType4, BigSizeMilliSatoshi]) {
			records = append(records, r.Record())
		},
	)

	rl.CustomBlob.WhenSome(func(r tlv.RecordT[tlv.TlvType5, tlv.Blob]) {
		records = append(records, r.Record())
	})

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	// Write the tlv stream.
	if err := WriteTlvStream(w, tlvStream); err != nil {
		return err
	}

	// Write the HTLCs.
	return SerializeHTLCEntries(w, rl.HTLCEntries)
}

// SerializeHTLCEntries serializes a list of HTLCEntry records based on tlv
// format.
func SerializeHTLCEntries(w io.Writer, htlcs []*HTLCEntry) error {
	for _, htlc := range htlcs {
		// Create the tlv stream.
		tlvStream, err := htlc.toTlvStream()
		if err != nil {
			return err
		}

		// Write the tlv stream.
		if err := WriteTlvStream(w, tlvStream); err != nil {
			return err
		}
	}

	return nil
}

// DeserializeRevocationLog deserializes a RevocationLog based on tlv format.
func DeserializeRevocationLog(r io.Reader) (RevocationLog, error) {
	var rl RevocationLog

	ourBalance := rl.OurBalance.Zero()
	theirBalance := rl.TheirBalance.Zero()
	customBlob := rl.CustomBlob.Zero()

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(
		rl.OurOutputIndex.Record(),
		rl.TheirOutputIndex.Record(),
		rl.CommitTxHash.Record(),
		ourBalance.Record(),
		theirBalance.Record(),
		customBlob.Record(),
	)
	if err != nil {
		return rl, err
	}

	// Read the tlv stream.
	parsedTypes, err := ReadTlvStream(r, tlvStream)
	if err != nil {
		return rl, err
	}

	if t, ok := parsedTypes[ourBalance.TlvType()]; ok && t == nil {
		rl.OurBalance = tlv.SomeRecordT(ourBalance)
	}

	if t, ok := parsedTypes[theirBalance.TlvType()]; ok && t == nil {
		rl.TheirBalance = tlv.SomeRecordT(theirBalance)
	}

	if t, ok := parsedTypes[customBlob.TlvType()]; ok && t == nil {
		rl.CustomBlob = tlv.SomeRecordT(customBlob)
	}

	// Read the HTLC entries.
	rl.HTLCEntries, err = DeserializeHTLCEntries(r)

	return rl, err
}

// DeserializeHTLCEntries deserializes a list of HTLC entries based on tlv
// format.
func DeserializeHTLCEntries(r io.Reader) ([]*HTLCEntry, error) {
	var (
		htlcs []*HTLCEntry

		// htlcIndexBlob defines the tlv record type to be used when
		// decoding from the disk. We use it instead of the one defined
		// in `HTLCEntry.HtlcIndex` as previously this field was encoded
		// using `uint16`, thus we will read it as raw bytes and
		// deserialize it further below.
		htlcIndexBlob tlv.OptionalRecordT[tlv.TlvType6, tlv.Blob]
	)

	for {
		var htlc HTLCEntry

		customBlob := htlc.CustomBlob.Zero()
		htlcIndex := htlcIndexBlob.Zero()

		// Create the tlv stream.
		records := []tlv.Record{
			htlc.RHash.Record(),
			htlc.RefundTimeout.Record(),
			htlc.OutputIndex.Record(),
			htlc.Incoming.Record(),
			htlc.Amt.Record(),
			customBlob.Record(),
			htlcIndex.Record(),
		}

		tlvStream, err := tlv.NewStream(records...)
		if err != nil {
			return nil, err
		}

		// Read the HTLC entry.
		parsedTypes, err := ReadTlvStream(r, tlvStream)
		if err != nil {
			// We've reached the end when hitting an EOF.
			if errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}

			return nil, err
		}

		if t, ok := parsedTypes[customBlob.TlvType()]; ok && t == nil {
			htlc.CustomBlob = tlv.SomeRecordT(customBlob)
		}

		if t, ok := parsedTypes[htlcIndex.TlvType()]; ok && t == nil {
			record, err := deserializeHtlcIndexCompatible(
				htlcIndex.Val,
			)
			if err != nil {
				return nil, err
			}

			htlc.HtlcIndex = record
		}

		// Append the entry.
		htlcs = append(htlcs, &htlc)
	}

	return htlcs, nil
}

// deserializeHtlcIndexCompatible takes raw bytes and decodes it into an
// optional record that's assigned to the entry's HtlcIndex.
//
// NOTE: previously this `HtlcIndex` was a tlv record that used `uint16` to
// encode its value. Given now its value is encoded using BigSizeT, and for any
// BigSizeT, its possible length values are 1, 3, 5, and 8. This means if the
// tlv record has a length of 2, we know for sure it must be an old record
// whose value was encoded using uint16.
func deserializeHtlcIndexCompatible(rawBytes []byte) (
	tlv.OptionalRecordT[tlv.TlvType6, tlv.BigSizeT[uint64]], error) {

	var (
		// record defines the record that's used by the HtlcIndex in the
		// entry.
		record tlv.OptionalRecordT[
			tlv.TlvType6, tlv.BigSizeT[uint64],
		]

		// htlcIndexVal is the decoded uint64 value.
		htlcIndexVal uint64
	)

	// If the length of the tlv record is 2, it must be encoded using uint16
	// as the BigSizeT encoding cannot have this length.
	if len(rawBytes) == 2 {
		// Decode the raw bytes into uint16 and convert it into uint64.
		htlcIndexVal = uint64(binary.BigEndian.Uint16(rawBytes))
	} else {
		// This value is encoded using BigSizeT, we now use the decoder
		// to deserialize the raw bytes.
		r := bytes.NewBuffer(rawBytes)

		// Create a buffer to be used in the decoding process.
		buf := [8]byte{}

		// Use the BigSizeT's decoder.
		err := tlv.DBigSize(r, &htlcIndexVal, &buf, 8)
		if err != nil {
			return record, err
		}
	}

	record = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType6](
		tlv.NewBigSizeT(htlcIndexVal),
	))

	return record, nil
}

// WriteTlvStream is a helper function that encodes the tlv stream into the
// writer.
func WriteTlvStream(w io.Writer, s *tlv.Stream) error {
	var b bytes.Buffer
	if err := s.Encode(&b); err != nil {
		return err
	}

	// Write the stream's length as a varint.
	err := tlv.WriteVarInt(w, uint64(b.Len()), &[8]byte{})
	if err != nil {
		return err
	}

	if _, err = w.Write(b.Bytes()); err != nil {
		return err
	}

	return nil
}

// ReadTlvStream is a helper function that decodes the tlv stream from the
// reader.
func ReadTlvStream(r io.Reader, s *tlv.Stream) (tlv.TypeMap, error) {
	var bodyLen uint64

	// Read the stream's length.
	bodyLen, err := tlv.ReadVarInt(r, &[8]byte{})
	switch {
	// We'll convert any EOFs to ErrUnexpectedEOF, since this results in an
	// invalid record.
	case errors.Is(err, io.EOF):
		return nil, io.ErrUnexpectedEOF

	// Other unexpected errors.
	case err != nil:
		return nil, err
	}

	// TODO(yy): add overflow check.
	lr := io.LimitReader(r, int64(bodyLen))

	return s.DecodeWithParsedTypes(lr)
}

// revocationLogKey converts a uint64 into an 8 byte revocation log key.
func revocationLogKey(updateNum uint64) [8]byte {
	var key [8]byte
	byteOrder.PutUint64(key[:], updateNum)
	return key
}
