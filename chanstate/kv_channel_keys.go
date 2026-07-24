package chanstate

var (
	// closedChannelBucket stores summarization information concerning
	// previously open, but now closed channels.
	closedChannelBucket = []byte("closed-chan-bucket")

	// openChannelBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Within
	// that node ID bucket, all attributes required to track, update, and
	// close a channel are stored.
	//
	// openChan -> nodeID -> chanPoint
	//
	// TODO(roasbeef): flesh out comment.
	openChannelBucket = []byte("open-chan-bucket")

	// outpointBucket stores all of our channel outpoints and a tlv
	// stream containing channel data.
	//
	// outpoint -> tlv stream.
	//
	outpointBucket = []byte("outpoint-bucket")

	// chanIDBucket stores all of the 32-byte channel ID's we know about.
	// These could be derived from outpointBucket, but it is more
	// convenient to have these in their own bucket.
	//
	// chanID -> tlv stream.
	//
	chanIDBucket = []byte("chan-id-bucket")

	// historicalChannelBucket stores all channels that have seen their
	// commitment tx confirm. All information from their previous open state
	// is retained.
	historicalChannelBucket = []byte("historical-chan-bucket")

	// chanInfoKey can be accessed within the bucket for a channel
	// (identified by its chanPoint). This key stores all the static
	// information for a channel which is decided at the end of  the
	// funding flow.
	chanInfoKey = []byte("chan-info-key")

	// localUpfrontShutdownKey can be accessed within the bucket for a
	// channel (identified by its chanPoint). This key stores an optional
	// upfront shutdown script for the local peer.
	localUpfrontShutdownKey = []byte("local-upfront-shutdown-key")

	// remoteUpfrontShutdownKey can be accessed within the bucket for a
	// channel (identified by its chanPoint). This key stores an optional
	// upfront shutdown script for the remote peer.
	remoteUpfrontShutdownKey = []byte("remote-upfront-shutdown-key")

	// chanCommitmentKey can be accessed within the sub-bucket for a
	// particular channel. This key stores the up to date commitment state
	// for a particular channel party. Appending a 0 to the end of this key
	// indicates it's the commitment for the local party, and appending a 1
	// to the end of this key indicates it's the commitment for the remote
	// party.
	chanCommitmentKey = []byte("chan-commitment-key")

	// unsignedAckedUpdatesKey is an entry in the channel bucket that
	// contains the remote updates that we have acked, but not yet signed
	// for in one of our remote commits.
	unsignedAckedUpdatesKey = []byte("unsigned-acked-updates-key")

	// remoteUnsignedLocalUpdatesKey is an entry in the channel bucket that
	// contains the local updates that the remote party has acked, but
	// has not yet signed for in one of their local commits.
	remoteUnsignedLocalUpdatesKey = []byte(
		"remote-unsigned-local-updates-key",
	)

	// revocationStateKey stores their current revocation hash, our
	// preimage producer and their preimage store.
	revocationStateKey = []byte("revocation-state-key")

	// commitDiffKey stores the current pending commitment state we've
	// extended to the remote party (if any). Each time we propose a new
	// state, we store the information necessary to reconstruct this state
	// from the prior commitment. This allows us to resync the remote party
	// to their expected state in the case of message loss.
	//
	// TODO(roasbeef): rename to commit chain?
	commitDiffKey = []byte("commit-diff-key")

	// lastWasRevokeKey is a key that stores true when the last update we
	// sent was a revocation and false when it was a commitment signature.
	// This is nil in the case of new channels with no updates exchanged.
	lastWasRevokeKey = []byte("last-was-revoke")
)

// ClosedChannelBucketKey returns the top-level closed-channel summary bucket
// key.
func ClosedChannelBucketKey() []byte {
	return closedChannelBucket
}

// OpenChannelBucketKey returns the top-level open-channel bucket key.
func OpenChannelBucketKey() []byte {
	return openChannelBucket
}

// OutpointBucketKey returns the top-level outpoint index bucket key.
func OutpointBucketKey() []byte {
	return outpointBucket
}

// ChanIDBucketKey returns the top-level channel ID index bucket key.
func ChanIDBucketKey() []byte {
	return chanIDBucket
}

// HistoricalChannelBucketKey returns the top-level historical channel bucket
// key.
func HistoricalChannelBucketKey() []byte {
	return historicalChannelBucket
}

// ChanInfoKey returns the channel-bucket key for static channel information.
func ChanInfoKey() []byte {
	return chanInfoKey
}

// LocalUpfrontShutdownKey returns the channel-bucket key for the local upfront
// shutdown script.
func LocalUpfrontShutdownKey() []byte {
	return localUpfrontShutdownKey
}

// RemoteUpfrontShutdownKey returns the channel-bucket key for the remote
// upfront shutdown script.
func RemoteUpfrontShutdownKey() []byte {
	return remoteUpfrontShutdownKey
}

// ChanCommitmentKey returns the channel-bucket key prefix for channel
// commitments.
func ChanCommitmentKey() []byte {
	return chanCommitmentKey
}

// UnsignedAckedUpdatesKey returns the channel-bucket key for unsigned acked
// remote updates.
func UnsignedAckedUpdatesKey() []byte {
	return unsignedAckedUpdatesKey
}

// RemoteUnsignedLocalUpdatesKey returns the channel-bucket key for remote
// unsigned local updates.
func RemoteUnsignedLocalUpdatesKey() []byte {
	return remoteUnsignedLocalUpdatesKey
}

// RevocationStateKey returns the channel-bucket key for revocation state.
func RevocationStateKey() []byte {
	return revocationStateKey
}

// CommitDiffKey returns the channel-bucket key for the current pending
// commitment diff.
func CommitDiffKey() []byte {
	return commitDiffKey
}

// LastWasRevokeKey returns the channel-bucket key for the last update type.
func LastWasRevokeKey() []byte {
	return lastWasRevokeKey
}
