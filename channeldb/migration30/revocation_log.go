package migration30

import (
	"bytes"
	"errors"
	"io"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	mig24 "github.com/lightningnetwork/lnd/channeldb/migration24"
	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// OutputIndexEmpty is used when the output index doesn't exist.
	OutputIndexEmpty = math.MaxUint16

	// A set of tlv type definitions used to serialize the body of
	// revocation logs to the database.
	//
	// NOTE: A migration should be added whenever this list changes.
	revLogOurOutputIndexType   tlv.Type = 0
	revLogTheirOutputIndexType tlv.Type = 1
	revLogCommitTxHashType     tlv.Type = 2
	revLogOurBalanceType       tlv.Type = 3
	revLogTheirBalanceType     tlv.Type = 4
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

	// revocationStateKey stores their current revocation hash, our
	// preimage producer and their preimage store.
	revocationStateKey = []byte("revocation-state-key")

	// ErrNoRevocationsFound is returned when revocation state for a
	// particular channel cannot be found.
	ErrNoRevocationsFound = errors.New("no revocations found")

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

// RHashLen is used by MakeDynamicRecord to return the size of the RHash.
//
// NOTE: for zero hash, we return a length 0.
func (h *HTLCEntry) RHashLen() uint64 {
	if h.RHash == lntypes.ZeroHash {
		return 0
	}
	return 32
}

// RHashEncoder is the customized encoder which skips encoding the empty hash.
func RHashEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	v, ok := val.(*[32]byte)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "RHash")
	}

	// If the value is an empty hash, we will skip encoding it.
	if *v == lntypes.ZeroHash {
		return nil
	}

	return tlv.EBytes32(w, v, buf)
}

// RHashDecoder is the customized decoder which skips decoding the empty hash.
func RHashDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	v, ok := val.(*[32]byte)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "RHash")
	}

	// If the length is zero, we will skip encoding the empty hash.
	if l == 0 {
		return nil
	}

	return tlv.DBytes32(r, v, buf, 32)
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
		tlv.MakeDynamicRecord(
			rHashType, &h.RHash, h.RHashLen,
			RHashEncoder, RHashDecoder,
		),
		tlv.MakePrimitiveRecord(
			refundTimeoutType, &h.RefundTimeout,
		),
		tlv.MakePrimitiveRecord(
			outputIndexType, &h.OutputIndex,
		),
		tlv.MakePrimitiveRecord(incomingType, &h.incomingTlv),
		// We will save 3 bytes if the amount is less or equal to
		// 4,294,967,295 msat, or roughly 0.043 bitcoin.
		tlv.MakeBigSizeRecord(amtType, &h.amtTlv),
	)
}

// RevocationLog stores the info needed to construct a breach retribution. Its
// fields can be viewed as a subset of a ChannelCommitment's. In the database,
// all historical versions of the RevocationLog are saved using the
// CommitHeight as the key.
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

	// OurBalance is the current available balance within the channel
	// directly spendable by us. In other words, it is the value of the
	// to_remote output on the remote parties' commitment transaction.
	//
	// NOTE: this is a pointer so that it is clear if the value is zero or
	// nil. Since migration 30 of the channeldb initially did not include
	// this field, it could be the case that the field is not present for
	// all revocation logs.
	OurBalance *lnwire.MilliSatoshi

	// TheirBalance is the current available balance within the channel
	// directly spendable by the remote node. In other words, it is the
	// value of the to_local output on the remote parties' commitment.
	//
	// NOTE: this is a pointer so that it is clear if the value is zero or
	// nil. Since migration 30 of the channeldb initially did not include
	// this field, it could be the case that the field is not present for
	// all revocation logs.
	TheirBalance *lnwire.MilliSatoshi
}

// putRevocationLog uses the fields `CommitTx` and `Htlcs` from a
// ChannelCommitment to construct a revocation log entry and saves them to
// disk. It also saves our output index and their output index, which are
// useful when creating breach retribution.
func putRevocationLog(bucket kvdb.RwBucket, commit *mig.ChannelCommitment,
	ourOutputIndex, theirOutputIndex uint32, noAmtData bool) error {

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

	if !noAmtData {
		rl.OurBalance = &commit.LocalBalance
		rl.TheirBalance = &commit.RemoteBalance
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

	logEntrykey := mig24.MakeLogKey(commit.CommitHeight)
	return bucket.Put(logEntrykey[:], b.Bytes())
}

// fetchRevocationLog queries the revocation log bucket to find an log entry.
// Return an error if not found.
func fetchRevocationLog(log kvdb.RBucket,
	updateNum uint64) (RevocationLog, error) {

	logEntrykey := mig24.MakeLogKey(updateNum)
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
		tlv.MakePrimitiveRecord(
			revLogOurOutputIndexType, &rl.OurOutputIndex,
		),
		tlv.MakePrimitiveRecord(
			revLogTheirOutputIndexType, &rl.TheirOutputIndex,
		),
		tlv.MakePrimitiveRecord(
			revLogCommitTxHashType, &rl.CommitTxHash,
		),
	}

	// Now we add any optional fields that are non-nil.
	if rl.OurBalance != nil {
		lb := uint64(*rl.OurBalance)
		records = append(records, tlv.MakeBigSizeRecord(
			revLogOurBalanceType, &lb,
		))
	}

	if rl.TheirBalance != nil {
		rb := uint64(*rl.TheirBalance)
		records = append(records, tlv.MakeBigSizeRecord(
			revLogTheirBalanceType, &rb,
		))
	}

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
	var (
		rl            RevocationLog
		localBalance  uint64
		remoteBalance uint64
	)

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			revLogOurOutputIndexType, &rl.OurOutputIndex,
		),
		tlv.MakePrimitiveRecord(
			revLogTheirOutputIndexType, &rl.TheirOutputIndex,
		),
		tlv.MakePrimitiveRecord(
			revLogCommitTxHashType, &rl.CommitTxHash,
		),
		tlv.MakeBigSizeRecord(revLogOurBalanceType, &localBalance),
		tlv.MakeBigSizeRecord(
			revLogTheirBalanceType, &remoteBalance,
		),
	)
	if err != nil {
		return rl, err
	}

	// Read the tlv stream.
	parsedTypes, err := readTlvStream(r, tlvStream)
	if err != nil {
		return rl, err
	}

	if t, ok := parsedTypes[revLogOurBalanceType]; ok && t == nil {
		lb := lnwire.MilliSatoshi(localBalance)
		rl.OurBalance = &lb
	}

	if t, ok := parsedTypes[revLogTheirBalanceType]; ok && t == nil {
		rb := lnwire.MilliSatoshi(remoteBalance)
		rl.TheirBalance = &rb
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
		if _, err := readTlvStream(r, tlvStream); err != nil {
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

// fetchLogBucket returns a read bucket by visiting both the new and the old
// bucket.
func fetchLogBucket(chanBucket kvdb.RBucket) (kvdb.RBucket, error) {
	logBucket := chanBucket.NestedReadBucket(revocationLogBucket)
	if logBucket == nil {
		logBucket = chanBucket.NestedReadBucket(
			revocationLogBucketDeprecated,
		)
		if logBucket == nil {
			return nil, mig25.ErrNoPastDeltas
		}
	}

	return logBucket, nil
}

// putOldRevocationLog saves a revocation log using the old format.
func putOldRevocationLog(log kvdb.RwBucket,
	commit *mig.ChannelCommitment) error {

	var b bytes.Buffer
	if err := mig.SerializeChanCommit(&b, commit); err != nil {
		return err
	}

	logEntrykey := mig24.MakeLogKey(commit.CommitHeight)
	return log.Put(logEntrykey[:], b.Bytes())
}

func putChanRevocationState(chanBucket kvdb.RwBucket,
	channel *mig26.OpenChannel) error {

	var b bytes.Buffer
	err := mig.WriteElements(
		&b, channel.RemoteCurrentRevocation, channel.RevocationProducer,
		channel.RevocationStore,
	)
	if err != nil {
		return err
	}

	// TODO(roasbeef): don't keep producer on disk

	// If the next revocation is present, which is only the case after the
	// FundingLocked message has been sent, then we'll write it to disk.
	if channel.RemoteNextRevocation != nil {
		err = mig.WriteElements(&b, channel.RemoteNextRevocation)
		if err != nil {
			return err
		}
	}

	return chanBucket.Put(revocationStateKey, b.Bytes())
}

func fetchChanRevocationState(chanBucket kvdb.RBucket,
	c *mig26.OpenChannel) error {

	revBytes := chanBucket.Get(revocationStateKey)
	if revBytes == nil {
		return ErrNoRevocationsFound
	}
	r := bytes.NewReader(revBytes)

	err := mig.ReadElements(
		r, &c.RemoteCurrentRevocation, &c.RevocationProducer,
		&c.RevocationStore,
	)
	if err != nil {
		return err
	}

	// If there aren't any bytes left in the buffer, then we don't yet have
	// the next remote revocation, so we can exit early here.
	if r.Len() == 0 {
		return nil
	}

	// Otherwise we'll read the next revocation for the remote party which
	// is always the last item within the buffer.
	return mig.ReadElements(r, &c.RemoteNextRevocation)
}

func findOutputIndexes(chanState *mig26.OpenChannel,
	oldLog *mig.ChannelCommitment) (uint32, uint32, error) {

	// With the state number broadcast known, we can now derive/restore the
	// proper revocation preimage necessary to sweep the remote party's
	// output.
	revocationPreimage, err := chanState.RevocationStore.LookUp(
		oldLog.CommitHeight,
	)
	if err != nil {
		return 0, 0, err
	}

	return findOutputIndexesFromRemote(
		revocationPreimage, chanState, oldLog,
	)
}
