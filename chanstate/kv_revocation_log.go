package chanstate

import (
	"bytes"
	"errors"
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

	logEntrykey := makeLogKey(commit.CommitHeight)

	return bucket.Put(logEntrykey[:], b.Bytes())
}

// FetchRevocationLog queries the revocation log bucket to find an log entry.
// Return an error if not found.
func FetchRevocationLog(log kvdb.RBucket,
	updateNum uint64) (RevocationLog, error) {

	logEntrykey := makeLogKey(updateNum)
	commitBytes := log.Get(logEntrykey[:])
	if commitBytes == nil {
		return RevocationLog{}, ErrLogEntryNotFound
	}

	commitReader := bytes.NewReader(commitBytes)

	return DeserializeRevocationLog(commitReader)
}
