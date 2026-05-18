package channeldb

import (
	"bytes"
	"io"

	cstate "github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// OutputIndexEmpty is used when the output index doesn't exist.
	OutputIndexEmpty = cstate.OutputIndexEmpty
)

type (
	// BigSizeAmount is a type alias for a TLV record of a btcutil.Amount.
	BigSizeAmount = cstate.BigSizeAmount

	// BigSizeMilliSatoshi is a type alias for a TLV record of a
	// lnwire.MilliSatoshi.
	BigSizeMilliSatoshi = cstate.BigSizeMilliSatoshi

	// SparsePayHash is a type alias for a 32 byte array, which when
	// serialized is able to save some space by not including an empty
	// payment hash on disk.
	SparsePayHash = cstate.SparsePayHash

	// HTLCEntry specifies the minimal info needed to be stored on disk for
	// ALL the historical HTLCs, which is useful for constructing
	// RevocationLog when a breach is detected.
	HTLCEntry = cstate.HTLCEntry

	// RevocationLog stores the info needed to construct a breach
	// retribution.
	RevocationLog = cstate.RevocationLog
)

var (
	// NewSparsePayHash creates a new SparsePayHash from a 32 byte array.
	NewSparsePayHash = cstate.NewSparsePayHash

	// NewHTLCEntryFromHTLC creates a new HTLCEntry from an HTLC.
	NewHTLCEntryFromHTLC = cstate.NewHTLCEntryFromHTLC

	// NewRevocationLog creates a new RevocationLog from the given
	// parameters.
	NewRevocationLog = cstate.NewRevocationLog

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
	revocationLogBucket = cstate.RevocationLogBucketKey()

	// ErrLogEntryNotFound is returned when we cannot find a log entry at
	// the height requested in the revocation log.
	ErrLogEntryNotFound = cstate.ErrLogEntryNotFound

	// ErrOutputIndexTooBig is returned when the output index is greater
	// than uint16.
	ErrOutputIndexTooBig = cstate.ErrOutputIndexTooBig
)

// putRevocationLog uses the fields `CommitTx` and `Htlcs` from a
// ChannelCommitment to construct a revocation log entry and saves them to
// disk. It also saves our output index and their output index, which are
// useful when creating breach retribution.
func putRevocationLog(bucket kvdb.RwBucket, commit *ChannelCommitment,
	ourOutputIndex, theirOutputIndex uint32, noAmtData bool) error {

	return cstate.PutRevocationLog(
		bucket, commit, ourOutputIndex, theirOutputIndex, noAmtData,
	)
}

// fetchRevocationLog queries the revocation log bucket to find an log entry.
// Return an error if not found.
func fetchRevocationLog(log kvdb.RBucket,
	updateNum uint64) (RevocationLog, error) {

	return cstate.FetchRevocationLog(log, updateNum)
}

// serializeRevocationLog serializes a RevocationLog record based on tlv
// format.
func serializeRevocationLog(w io.Writer, rl *RevocationLog) error {
	return cstate.SerializeRevocationLog(w, rl)
}

// serializeHTLCEntries serializes a list of HTLCEntry records based on tlv
// format.
func serializeHTLCEntries(w io.Writer, htlcs []*HTLCEntry) error {
	return cstate.SerializeHTLCEntries(w, htlcs)
}

// deserializeRevocationLog deserializes a RevocationLog based on tlv format.
func deserializeRevocationLog(r io.Reader) (RevocationLog, error) {
	return cstate.DeserializeRevocationLog(r)
}

// deserializeHTLCEntries deserializes a list of HTLC entries based on tlv
// format.
func deserializeHTLCEntries(r io.Reader) ([]*HTLCEntry, error) {
	return cstate.DeserializeHTLCEntries(r)
}

// writeTlvStream is a helper function that encodes the tlv stream into the
// writer.
func writeTlvStream(w io.Writer, s *tlv.Stream) error {
	return cstate.WriteTlvStream(w, s)
}

// readTlvStream is a helper function that decodes the tlv stream from the
// reader.
func readTlvStream(r io.Reader, s *tlv.Stream) (tlv.TypeMap, error) {
	return cstate.ReadTlvStream(r, s)
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
