package channeldb

import (
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

	return cstate.FetchOldRevocationLog(log, updateNum)
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

	return cstate.FetchRevocationLogCompatible(chanBucket, updateNum)
}

// fetchLogBucket returns a read bucket by visiting both the new and the old
// bucket.
func fetchLogBucket(chanBucket kvdb.RBucket) (kvdb.RBucket, error) {
	return cstate.FetchLogBucket(chanBucket)
}

// deleteLogBucket deletes the both the new and old revocation log buckets.
func deleteLogBucket(chanBucket kvdb.RwBucket) error {
	return cstate.DeleteLogBucket(chanBucket)
}
