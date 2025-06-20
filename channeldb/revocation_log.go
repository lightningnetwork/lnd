package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// OutputIndexEmpty is used when the output index doesn't exist.
	OutputIndexEmpty = math.MaxUint16
)

type (
	// BigSizeAmount is a type alias for a TLV record of a btcutil.Amount.
	BigSizeAmount = tlv.BigSizeT[btcutil.Amount]

	// BigSizeMilliSatoshi is a type alias for a TLV record of a
	// lnwire.MilliSatoshi.
	BigSizeMilliSatoshi = tlv.BigSizeT[lnwire.MilliSatoshi]
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

// SparsePayHash is a type alias for a 32 byte array, which when serialized is
// able to save some space by not including an empty payment hash on disk.
type SparsePayHash [32]byte

// NewSparsePayHash creates a new SparsePayHash from a 32 byte array.
func NewSparsePayHash(rHash [32]byte) SparsePayHash {
	return SparsePayHash(rHash)
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

// HTLCEntry specifies the minimal info needed to be stored on disk for ALL the
// historical HTLCs, which is useful for constructing RevocationLog when a
// breach is detected.
// The actual size of each HTLCEntry varies based on its RHash and Amt(sat),
// summarized as follows,
//
//	| RHash empty | Amt<=252 | Amt<=65,535 | Amt<=4,294,967,295 | otherwise |
//	|:-----------:|:--------:|:-----------:|:------------------:|:---------:|
//	|     true    |    19    |      21     |         23         |     26    |
//	|     false   |    51    |      53     |         55         |     58    |
//
// So the size varies from 19 bytes to 58 bytes, where most likely to be 23 or
// 55 bytes.
//
// NOTE: all the fields saved to disk use the primitive go types so they can be
// made into tlv records without further conversion.
type HTLCEntry struct {
	// RHash is the payment hash of the HTLC.
	RHash tlv.RecordT[tlv.TlvType0, SparsePayHash]

	// RefundTimeout is the absolute timeout on the HTLC that the sender
	// must wait before reclaiming the funds in limbo.
	RefundTimeout tlv.RecordT[tlv.TlvType1, uint32]

	// OutputIndex is the output index for this particular HTLC output
	// within the commitment transaction.
	//
	// NOTE: we use uint16 instead of int32 here to save us 2 bytes, which
	// gives us a max number of HTLCs of 65K.
	OutputIndex tlv.RecordT[tlv.TlvType2, uint16]

	// Incoming denotes whether we're the receiver or the sender of this
	// HTLC.
	Incoming tlv.RecordT[tlv.TlvType3, bool]

	// Amt is the amount of satoshis this HTLC escrows.
	Amt tlv.RecordT[tlv.TlvType4, tlv.BigSizeT[btcutil.Amount]]

	// CustomBlob is an optional blob that can be used to store information
	// specific to revocation handling for a custom channel type.
	CustomBlob tlv.OptionalRecordT[tlv.TlvType5, tlv.Blob]

	// HtlcIndex is the index of the HTLC in the channel.
	HtlcIndex tlv.OptionalRecordT[tlv.TlvType6, tlv.BigSizeT[uint64]]
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

// NewHTLCEntryFromHTLC creates a new HTLCEntry from an HTLC.
func NewHTLCEntryFromHTLC(htlc HTLC) (*HTLCEntry, error) {
	h := &HTLCEntry{
		RHash: tlv.NewRecordT[tlv.TlvType0](
			NewSparsePayHash(htlc.RHash),
		),
		RefundTimeout: tlv.NewPrimitiveRecord[tlv.TlvType1](
			htlc.RefundTimeout,
		),
		OutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType2](
			uint16(htlc.OutputIndex),
		),
		Incoming: tlv.NewPrimitiveRecord[tlv.TlvType3](htlc.Incoming),
		Amt: tlv.NewRecordT[tlv.TlvType4](
			tlv.NewBigSizeT(htlc.Amt.ToSatoshis()),
		),
		HtlcIndex: tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType6](
			tlv.NewBigSizeT(htlc.HtlcIndex),
		)),
	}

	if len(htlc.CustomRecords) != 0 {
		blob, err := htlc.CustomRecords.Serialize()
		if err != nil {
			return nil, err
		}

		h.CustomBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5, tlv.Blob](blob),
		)
	}

	return h, nil
}

// RevocationLog stores the info needed to construct a breach retribution. Its
// fields can be viewed as a subset of a ChannelCommitment's. In the database,
// all historical versions of the RevocationLog are saved using the
// CommitHeight as the key.
type RevocationLog struct {
	// OurOutputIndex specifies our output index in this commitment. In a
	// remote commitment transaction, this is the to remote output index.
	OurOutputIndex tlv.RecordT[tlv.TlvType0, uint16]

	// TheirOutputIndex specifies their output index in this commitment. In
	// a remote commitment transaction, this is the to local output index.
	TheirOutputIndex tlv.RecordT[tlv.TlvType1, uint16]

	// CommitTxHash is the hash of the latest version of the commitment
	// state, broadcast able by us.
	CommitTxHash tlv.RecordT[tlv.TlvType2, [32]byte]

	// HTLCEntries is the set of HTLCEntry's that are pending at this
	// particular commitment height.
	HTLCEntries []*HTLCEntry

	// OurBalance is the current available balance within the channel
	// directly spendable by us. In other words, it is the value of the
	// to_remote output on the remote parties' commitment transaction.
	//
	// NOTE: this is an option so that it is clear if the value is zero or
	// nil. Since migration 30 of the channeldb initially did not include
	// this field, it could be the case that the field is not present for
	// all revocation logs.
	OurBalance tlv.OptionalRecordT[tlv.TlvType3, BigSizeMilliSatoshi]

	// TheirBalance is the current available balance within the channel
	// directly spendable by the remote node. In other words, it is the
	// value of the to_local output on the remote parties' commitment.
	//
	// NOTE: this is an option so that it is clear if the value is zero or
	// nil. Since migration 30 of the channeldb initially did not include
	// this field, it could be the case that the field is not present for
	// all revocation logs.
	TheirBalance tlv.OptionalRecordT[tlv.TlvType4, BigSizeMilliSatoshi]

	// CustomBlob is an optional blob that can be used to store information
	// specific to a custom channel type. This information is only created
	// at channel funding time, and after wards is to be considered
	// immutable.
	CustomBlob tlv.OptionalRecordT[tlv.TlvType5, tlv.Blob]
}

// NewRevocationLog creates a new RevocationLog from the given parameters.
func NewRevocationLog(ourOutputIndex uint16, theirOutputIndex uint16,
	commitHash [32]byte, ourBalance,
	theirBalance fn.Option[lnwire.MilliSatoshi], htlcs []*HTLCEntry,
	customBlob fn.Option[tlv.Blob]) RevocationLog {

	rl := RevocationLog{
		OurOutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType0](
			ourOutputIndex,
		),
		TheirOutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType1](
			theirOutputIndex,
		),
		CommitTxHash: tlv.NewPrimitiveRecord[tlv.TlvType2](commitHash),
		HTLCEntries:  htlcs,
	}

	ourBalance.WhenSome(func(balance lnwire.MilliSatoshi) {
		rl.OurBalance = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType3](
			tlv.NewBigSizeT(balance),
		))
	})

	theirBalance.WhenSome(func(balance lnwire.MilliSatoshi) {
		rl.TheirBalance = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType4](
			tlv.NewBigSizeT(balance),
		))
	})

	customBlob.WhenSome(func(blob tlv.Blob) {
		rl.CustomBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5, tlv.Blob](blob),
		)
	})

	return rl
}

// putRevocationLog uses the fields `CommitTx` and `Htlcs` from a
// ChannelCommitment to construct a revocation log entry and saves them to
// disk. It also saves our output index and their output index, which are
// useful when creating breach retribution.
func putRevocationLog(bucket kvdb.RwBucket, commit *ChannelCommitment,
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
	err := serializeRevocationLog(&b, rl)
	if err != nil {
		return err
	}

	logEntrykey := makeLogKey(commit.CommitHeight)
	return bucket.Put(logEntrykey[:], b.Bytes())
}

// fetchRevocationLog queries the revocation log bucket to find an log entry.
// Return an error if not found.
func fetchRevocationLog(log kvdb.RBucket,
	updateNum uint64) (RevocationLog, error) {

	logEntrykey := makeLogKey(updateNum)
	commitBytes := log.Get(logEntrykey[:])
	if commitBytes == nil {
		return RevocationLog{}, ErrLogEntryNotFound
	}

	commitReader := bytes.NewReader(commitBytes)

	return deserializeRevocationLog(commitReader)
}

// serializeRevocationLog serializes a RevocationLog record based on tlv
// format.
func serializeRevocationLog(w io.Writer, rl *RevocationLog) error {
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
	if err := writeTlvStream(w, tlvStream); err != nil {
		return err
	}

	// Write the HTLCs.
	return serializeHTLCEntries(w, rl.HTLCEntries)
}

// serializeHTLCEntries serializes a list of HTLCEntry records based on tlv
// format.
func serializeHTLCEntries(w io.Writer, htlcs []*HTLCEntry) error {
	for _, htlc := range htlcs {
		// Create the tlv stream.
		tlvStream, err := htlc.toTlvStream()
		if err != nil {
			return err
		}

		// Write the tlv stream.
		if err := writeTlvStream(w, tlvStream); err != nil {
			return err
		}
	}

	return nil
}

// deserializeRevocationLog deserializes a RevocationLog based on tlv format.
func deserializeRevocationLog(r io.Reader) (RevocationLog, error) {
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
	parsedTypes, err := readTlvStream(r, tlvStream)
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
	rl.HTLCEntries, err = deserializeHTLCEntries(r)

	return rl, err
}

// deserializeHTLCEntries deserializes a list of HTLC entries based on tlv
// format.
func deserializeHTLCEntries(r io.Reader) ([]*HTLCEntry, error) {
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
		parsedTypes, err := readTlvStream(r, tlvStream)
		if err != nil {
			// We've reached the end when hitting an EOF.
			if err == io.ErrUnexpectedEOF {
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

// writeTlvStream is a helper function that encodes the tlv stream into the
// writer.
func writeTlvStream(w io.Writer, s *tlv.Stream) error {
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

// readTlvStream is a helper function that decodes the tlv stream from the
// reader.
func readTlvStream(r io.Reader, s *tlv.Stream) (tlv.TypeMap, error) {
	var bodyLen uint64

	// Read the stream's length.
	bodyLen, err := tlv.ReadVarInt(r, &[8]byte{})
	switch {
	// We'll convert any EOFs to ErrUnexpectedEOF, since this results in an
	// invalid record.
	case err == io.EOF:
		return nil, io.ErrUnexpectedEOF

	// Other unexpected errors.
	case err != nil:
		return nil, err
	}

	// TODO(yy): add overflow check.
	lr := io.LimitReader(r, int64(bodyLen))

	return s.DecodeWithParsedTypes(lr)
}

// fetchOldRevocationLog finds the revocation log from the deprecated
// sub-bucket.
func fetchOldRevocationLog(log kvdb.RBucket,
	updateNum uint64) (ChannelCommitment, error) {

	logEntrykey := makeLogKey(updateNum)
	commitBytes := log.Get(logEntrykey[:])
	if commitBytes == nil {
		return ChannelCommitment{}, ErrLogEntryNotFound
	}

	commitReader := bytes.NewReader(commitBytes)
	return deserializeChanCommit(commitReader)
}

// fetchRevocationLogCompatible finds the revocation log from both the
// revocationLogBucket and revocationLogBucketDeprecated for compatibility
// concern. It returns three values,
//   - RevocationLog, if this is non-nil, it means we've found the log in the
//     new bucket.
//   - ChannelCommitment, if this is non-nil, it means we've found the log in the
//     old bucket.
//   - error, this can happen if the log cannot be found in neither buckets.
func fetchRevocationLogCompatible(chanBucket kvdb.RBucket,
	updateNum uint64) (*RevocationLog, *ChannelCommitment, error) {

	// Look into the new bucket first.
	logBucket := chanBucket.NestedReadBucket(revocationLogBucket)
	if logBucket != nil {
		rl, err := fetchRevocationLog(logBucket, updateNum)
		// We've found the record, no need to visit the old bucket.
		if err == nil {
			return &rl, nil, nil
		}

		// Return the error if it doesn't say the log cannot be found.
		if err != ErrLogEntryNotFound {
			return nil, nil, err
		}
	}

	// Otherwise, look into the old bucket and try to find the log there.
	oldBucket := chanBucket.NestedReadBucket(revocationLogBucketDeprecated)
	if oldBucket != nil {
		c, err := fetchOldRevocationLog(oldBucket, updateNum)
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

// fetchLogBucket returns a read bucket by visiting both the new and the old
// bucket.
func fetchLogBucket(chanBucket kvdb.RBucket) (kvdb.RBucket, error) {
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

// deleteLogBucket deletes the both the new and old revocation log buckets.
func deleteLogBucket(chanBucket kvdb.RwBucket) error {
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
