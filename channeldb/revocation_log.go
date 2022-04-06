package channeldb

import (
	"bytes"
	"errors"
	"io"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

// OutputIndexEmpty is used when the output index doesn't exist.
const OutputIndexEmpty = math.MaxUint16

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

// HTLCEntry specifies the minimal info needed to be stored on disk for ALL the
// historical HTLCs, which is useful for constructing RevocationLog when a
// breach is detected.
//
// NOTE: all the fields saved to disk use the primitive go types so they can be
// made into tlv records without further conversion.
type HTLCEntry struct {
	// RHash is the payment hash of the HTLC.
	RHash [32]byte

	// RefundTimeout is the absolute timeout on the HTLC that the sender
	// must wait before reclaiming the funds in limbo.
	RefundTimeout uint32

	// OutputIndex is the output index for this particular HTLC output
	// within the commitment transaction.
	//
	// NOTE: we use uint16 instead of int32 here to save us 2 bytes, which
	// gives us a max number of HTLCs of 65K.
	OutputIndex uint16

	// Incoming denotes whether we're the receiver or the sender of this
	// HTLC.
	//
	// NOTE: this field is the memory representation of the field
	// incomingUint.
	Incoming bool

	// Amt is the amount of satoshis this HTLC escrows.
	//
	// NOTE: this field is the memory representation of the field amtUint.
	Amt btcutil.Amount

	// amtTlv is the uint64 format of Amt. This field is created so we can
	// easily make it into a tlv record and save it to disk.
	//
	// NOTE: we keep this field for accounting purpose only. If the disk
	// space becomes an issue, we could delete this field to save us extra
	// 8 bytes.
	amtTlv uint64

	// incomingTlv is the uint8 format of Incoming. This field is created
	// so we can easily make it into a tlv record and save it to disk.
	incomingTlv uint8
}

// toTlvStream converts an HTLCEntry record into a tlv representation.
func (h *HTLCEntry) toTlvStream() (*tlv.Stream, error) {
	const (
		// A set of tlv type definitions used to serialize htlc entries
		// to the database. We define it here instead of the head of
		// the file to avoid naming conflicts.
		//
		// NOTE: A migration should be added whenever this list
		// changes.
		rHashType         tlv.Type = 0
		refundTimeoutType tlv.Type = 1
		outputIndexType   tlv.Type = 2
		incomingType      tlv.Type = 3
		amtType           tlv.Type = 4
	)

	return tlv.NewStream(
		tlv.MakePrimitiveRecord(rHashType, &h.RHash),
		tlv.MakePrimitiveRecord(
			refundTimeoutType, &h.RefundTimeout,
		),
		tlv.MakePrimitiveRecord(
			outputIndexType, &h.OutputIndex,
		),
		tlv.MakePrimitiveRecord(incomingType, &h.incomingTlv),
		tlv.MakePrimitiveRecord(amtType, &h.amtTlv),
	)
}

// RevocationLog stores the info needed to construct a breach retribution. Its
// fields can be viewed as a subset of a ChannelCommitment's. In the database,
// all historical versions of the RevocationLog are saved using the
// CommitHeight as the key.
//
// NOTE: all the fields use the primitive go types so they can be made into tlv
// records without further conversion.
type RevocationLog struct {
	// OurOutputIndex specifies our output index in this commitment. In a
	// remote commitment transaction, this is the to remote output index.
	OurOutputIndex uint16

	// TheirOutputIndex specifies their output index in this commitment. In
	// a remote commitment transaction, this is the to local output index.
	TheirOutputIndex uint16

	// CommitTxHash is the hash of the latest version of the commitment
	// state, broadcast able by us.
	CommitTxHash [32]byte

	// HTLCEntries is the set of HTLCEntry's that are pending at this
	// particular commitment height.
	HTLCEntries []*HTLCEntry
}

// toTlvStream converts an RevocationLog record into a tlv representation.
func (rl *RevocationLog) toTlvStream() (*tlv.Stream, error) {
	const (
		// A set of tlv type definitions used to serialize the body of
		// revocation logs to the database. We define it here instead
		// of the head of the file to avoid naming conflicts.
		//
		// NOTE: A migration should be added whenever this list
		// changes.
		ourOutputIndexType   tlv.Type = 0
		theirOutputIndexType tlv.Type = 1
		commitTxHashType     tlv.Type = 2
	)

	return tlv.NewStream(
		tlv.MakePrimitiveRecord(ourOutputIndexType, &rl.OurOutputIndex),
		tlv.MakePrimitiveRecord(
			theirOutputIndexType, &rl.TheirOutputIndex,
		),
		tlv.MakePrimitiveRecord(commitTxHashType, &rl.CommitTxHash),
	)
}

// putRevocationLog uses the fields `CommitTx` and `Htlcs` from a
// ChannelCommitment to construct a revocation log entry and saves them to
// disk. It also saves our output index and their output index, which are
// useful when creating breach retribution.
func putRevocationLog(bucket kvdb.RwBucket, commit *ChannelCommitment,
	ourOutputIndex, theirOutputIndex uint32) error {

	// Sanity check that the output indexes can be safely converted.
	if ourOutputIndex > math.MaxUint16 {
		return ErrOutputIndexTooBig
	}
	if theirOutputIndex > math.MaxUint16 {
		return ErrOutputIndexTooBig
	}

	rl := &RevocationLog{
		OurOutputIndex:   uint16(ourOutputIndex),
		TheirOutputIndex: uint16(theirOutputIndex),
		CommitTxHash:     commit.CommitTx.TxHash(),
		HTLCEntries:      make([]*HTLCEntry, 0, len(commit.Htlcs)),
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

		entry := &HTLCEntry{
			RHash:         htlc.RHash,
			RefundTimeout: htlc.RefundTimeout,
			Incoming:      htlc.Incoming,
			OutputIndex:   uint16(htlc.OutputIndex),
			Amt:           htlc.Amt.ToSatoshis(),
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
	// Create the tlv stream.
	tlvStream, err := rl.toTlvStream()
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
		// Patch the incomingTlv field.
		if htlc.Incoming {
			htlc.incomingTlv = 1
		}

		// Patch the amtTlv field.
		htlc.amtTlv = uint64(htlc.Amt)

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

	// Create the tlv stream.
	tlvStream, err := rl.toTlvStream()
	if err != nil {
		return rl, err
	}

	// Read the tlv stream.
	if err := readTlvStream(r, tlvStream); err != nil {
		return rl, err
	}

	// Read the HTLC entries.
	rl.HTLCEntries, err = deserializeHTLCEntries(r)

	return rl, err
}

// deserializeHTLCEntries deserializes a list of HTLC entries based on tlv
// format.
func deserializeHTLCEntries(r io.Reader) ([]*HTLCEntry, error) {
	var htlcs []*HTLCEntry

	for {
		var htlc HTLCEntry

		// Create the tlv stream.
		tlvStream, err := htlc.toTlvStream()
		if err != nil {
			return nil, err
		}

		// Read the HTLC entry.
		if err := readTlvStream(r, tlvStream); err != nil {
			// We've reached the end when hitting an EOF.
			if err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}

		// Patch the Incoming field.
		if htlc.incomingTlv == 1 {
			htlc.Incoming = true
		}

		// Patch the Amt field.
		htlc.Amt = btcutil.Amount(htlc.amtTlv)

		// Append the entry.
		htlcs = append(htlcs, &htlc)
	}

	return htlcs, nil
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
func readTlvStream(r io.Reader, s *tlv.Stream) error {
	var bodyLen uint64

	// Read the stream's length.
	bodyLen, err := tlv.ReadVarInt(r, &[8]byte{})
	switch {
	// We'll convert any EOFs to ErrUnexpectedEOF, since this results in an
	// invalid record.
	case err == io.EOF:
		return io.ErrUnexpectedEOF

	// Other unexpected errors.
	case err != nil:
		return err
	}

	// TODO(yy): add overflow check.
	lr := io.LimitReader(r, int64(bodyLen))
	return s.Decode(lr)
}
