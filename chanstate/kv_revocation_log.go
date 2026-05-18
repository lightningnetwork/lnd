package chanstate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// revocationLogBucketDeprecated is dedicated for storing the necessary
	// delta state between channel updates required to re-construct a past
	// state in order to punish a counterparty attempting a non-cooperative
	// channel closure. This key should be accessed from within the
	// sub-bucket of a target channel, identified by its channel point.
	//
	// Deprecated: This bucket is kept for read-only in case the user
	// choose not to migrate the old data.
	revocationLogBucketDeprecated = []byte("revocation-log-key")

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

// RevocationLogBucketDeprecatedKey returns the deprecated revocation-log bucket
// key.
func RevocationLogBucketDeprecatedKey() []byte {
	return revocationLogBucketDeprecated
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

// FetchOldRevocationLog finds the revocation log from the deprecated
// sub-bucket.
func FetchOldRevocationLog(log kvdb.RBucket,
	updateNum uint64) (ChannelCommitment, error) {

	logEntrykey := revocationLogKey(updateNum)
	commitBytes := log.Get(logEntrykey[:])
	if commitBytes == nil {
		return ChannelCommitment{}, ErrLogEntryNotFound
	}

	commitReader := bytes.NewReader(commitBytes)

	return DeserializeChanCommit(commitReader)
}

// FetchRevocationLogCompatible finds the revocation log from both the
// revocationLogBucket and revocationLogBucketDeprecated for compatibility
// concern. It returns three values,
//   - RevocationLog, if this is non-nil, it means we've found the log in the
//     new bucket.
//   - ChannelCommitment, if this is non-nil, it means we've found the log
//     in the old bucket.
//   - error, this can happen if the log cannot be found in neither buckets.
func FetchRevocationLogCompatible(chanBucket kvdb.RBucket,
	updateNum uint64) (*RevocationLog, *ChannelCommitment, error) {

	// Look into the new bucket first.
	logBucket := chanBucket.NestedReadBucket(revocationLogBucket)
	if logBucket != nil {
		rl, err := FetchRevocationLog(logBucket, updateNum)
		// We've found the record, no need to visit the old bucket.
		if err == nil {
			return &rl, nil, nil
		}

		// Return the error if it doesn't say the log cannot be found.
		if !errors.Is(err, ErrLogEntryNotFound) {
			return nil, nil, err
		}
	}

	// Otherwise, look into the old bucket and try to find the log there.
	oldBucket := chanBucket.NestedReadBucket(revocationLogBucketDeprecated)
	if oldBucket != nil {
		c, err := FetchOldRevocationLog(oldBucket, updateNum)
		if err != nil {
			return nil, nil, err
		}

		// Found an old record and return it.
		return nil, &c, nil
	}

	// If both the buckets are nil, then the sub-buckets haven't been
	// created yet.
	if logBucket == nil && oldBucket == nil {
		return nil, nil, ErrNoPastDeltas
	}

	// Otherwise, we've tried to query the new bucket but the log cannot be
	// found.
	return nil, nil, ErrLogEntryNotFound
}

// FetchLogBucket returns a read bucket by visiting both the new and the old
// bucket.
func FetchLogBucket(chanBucket kvdb.RBucket) (kvdb.RBucket, error) {
	logBucket := chanBucket.NestedReadBucket(revocationLogBucket)
	if logBucket == nil {
		logBucket = chanBucket.NestedReadBucket(
			revocationLogBucketDeprecated,
		)
		if logBucket == nil {
			return nil, ErrNoPastDeltas
		}
	}

	return logBucket, nil
}

// DeleteLogBucket deletes the both the new and old revocation log buckets.
func DeleteLogBucket(chanBucket kvdb.RwBucket) error {
	// Check if the bucket exists and delete it.
	logBucket := chanBucket.NestedReadWriteBucket(
		revocationLogBucket,
	)
	if logBucket != nil {
		err := chanBucket.DeleteNestedBucket(revocationLogBucket)
		if err != nil {
			return err
		}
	}

	// We also check whether the old revocation log bucket exists
	// and delete it if so.
	oldLogBucket := chanBucket.NestedReadWriteBucket(
		revocationLogBucketDeprecated,
	)
	if oldLogBucket != nil {
		err := chanBucket.DeleteNestedBucket(
			revocationLogBucketDeprecated,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// RevocationLogTailCommitHeight returns the commit height at the end of the
// revocation log.
func RevocationLogTailCommitHeight(backend kvdb.Backend,
	channel *OpenChannel) (uint64, error) {

	var height uint64

	// If we haven't created any state updates yet, then we'll exit early as
	// there's nothing to be found on disk in the revocation bucket.
	if channel.RemoteCommitment.CommitHeight == 0 {
		return height, nil
	}

	if err := kvdb.View(backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		logBucket, err := FetchLogBucket(chanBucket)
		if err != nil {
			return err
		}

		// Once we have the bucket that stores the revocation log from
		// this channel, we'll jump to the _last_ key in bucket. Since
		// the key is the commit height, we'll decode the bytes and
		// return it.
		cursor := logBucket.ReadCursor()
		rawHeight, _ := cursor.Last()
		height = byteOrder.Uint64(rawHeight)

		return nil
	}, func() {}); err != nil {
		return height, err
	}

	return height, nil
}

// FindPreviousState scans through the append-only log in an attempt to recover
// the previous channel state indicated by the update number. This method is
// intended to be used for obtaining the relevant data needed to claim all
// funds rightfully spendable in the case of an on-chain broadcast of the
// commitment transaction.
func (s *KVStore) FindPreviousState(channel *OpenChannel,
	updateNum uint64) (*RevocationLog, *ChannelCommitment, error) {

	commit := &ChannelCommitment{}
	rl := &RevocationLog{}

	err := kvdb.View(s.backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// Find the revocation log from both the new and the old
		// bucket.
		r, c, err := FetchRevocationLogCompatible(
			chanBucket, updateNum,
		)
		if err != nil {
			return err
		}

		rl = r
		commit = c

		return nil
	}, func() {})
	if err != nil {
		return nil, nil, err
	}

	// Either the `rl` or the `commit` is nil here. We return them as-is
	// and leave it to the caller to decide its following action.
	return rl, commit, nil
}

// Record returns a tlv record for the SparsePayHash.
func (s *SparsePayHash) Record() tlv.Record {
	// We use a zero for the type here, as this'll be used along with the
	// RecordT type.
	return tlv.MakeDynamicRecord(
		0, s, s.hashLen,
		sparseHashEncoder, sparseHashDecoder,
	)
}

// hashLen is used by MakeDynamicRecord to return the size of the RHash.
//
// NOTE: for zero hash, we return a length 0.
func (s *SparsePayHash) hashLen() uint64 {
	if bytes.Equal(s[:], lntypes.ZeroHash[:]) {
		return 0
	}

	return 32
}

// sparseHashEncoder is the customized encoder which skips encoding the empty
// hash.
func sparseHashEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	v, ok := val.(*SparsePayHash)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "SparsePayHash")
	}

	// If the value is an empty hash, we will skip encoding it.
	if bytes.Equal(v[:], lntypes.ZeroHash[:]) {
		return nil
	}

	vArray := (*[32]byte)(v)

	return tlv.EBytes32(w, vArray, buf)
}

// sparseHashDecoder is the customized decoder which skips decoding the empty
// hash.
func sparseHashDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	v, ok := val.(*SparsePayHash)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "SparsePayHash")
	}

	// If the length is zero, we will skip encoding the empty hash.
	if l == 0 {
		return nil
	}

	vArray := (*[32]byte)(v)

	return tlv.DBytes32(r, vArray, buf, 32)
}

// toTlvStream converts an HTLCEntry record into a tlv representation.
func (h *HTLCEntry) toTlvStream() (*tlv.Stream, error) {
	records := []tlv.Record{
		h.RHash.Record(),
		h.RefundTimeout.Record(),
		h.OutputIndex.Record(),
		h.Incoming.Record(),
		h.Amt.Record(),
	}

	h.CustomBlob.WhenSome(func(r tlv.RecordT[tlv.TlvType5, tlv.Blob]) {
		records = append(records, r.Record())
	})

	h.HtlcIndex.WhenSome(func(r tlv.RecordT[tlv.TlvType6,
		tlv.BigSizeT[uint64]]) {

		records = append(records, r.Record())
	})

	tlv.SortRecords(records)

	return tlv.NewStream(records...)
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
