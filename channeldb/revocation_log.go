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

var (
	// NewHTLCEntryFromHTLC creates a new HTLCEntry from an HTLC.
	NewHTLCEntryFromHTLC = cstate.NewHTLCEntryFromHTLC

	// NewRevocationLog creates a new RevocationLog from the given
	// parameters.
	NewRevocationLog = cstate.NewRevocationLog

	// ErrLogEntryNotFound is returned when we cannot find a log entry at
	// the height requested in the revocation log.
	ErrLogEntryNotFound = cstate.ErrLogEntryNotFound

	// ErrOutputIndexTooBig is returned when the output index is greater
	// than uint16.
	ErrOutputIndexTooBig = cstate.ErrOutputIndexTooBig
)

// fetchRevocationLog queries the revocation log bucket to find an log entry.
// Return an error if not found.
func fetchRevocationLog(log kvdb.RBucket,
	updateNum uint64) (cstate.RevocationLog, error) {

	return cstate.FetchRevocationLog(log, updateNum)
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
	updateNum uint64) (cstate.ChannelCommitment, error) {

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
	updateNum uint64) (*cstate.RevocationLog, *cstate.ChannelCommitment,
	error) {

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
