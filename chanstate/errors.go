package chanstate

import (
	"errors"
	"fmt"
)

var (
	// ErrNoChanDBExists is returned when a channel bucket hasn't been
	// created.
	ErrNoChanDBExists = fmt.Errorf("channel db has not yet been created")

	// ErrNoCommitmentsFound is returned when a channel has not set
	// commitment states.
	ErrNoCommitmentsFound = fmt.Errorf("no commitments found")

	// ErrNoChanInfoFound is returned when a particular channel does not
	// have any channels state.
	ErrNoChanInfoFound = fmt.Errorf("no chan info found")

	// ErrChannelNotFound is returned when we attempt to locate a channel
	// for a specific chain, but it is not found.
	ErrChannelNotFound = fmt.Errorf("channel not found")

	// ErrChanAlreadyExists is return when the caller attempts to create a
	// channel with a channel point that is already present in the
	// database.
	ErrChanAlreadyExists = fmt.Errorf("channel already exists")

	// ErrNoRevocationsFound is returned when revocation state for a
	// particular channel cannot be found.
	ErrNoRevocationsFound = fmt.Errorf("no revocations found")

	// ErrNoPendingCommit is returned when there is not a pending
	// commitment for a remote party. A new commitment is written to disk
	// each time we write a new state in order to be properly fault
	// tolerant.
	ErrNoPendingCommit = fmt.Errorf("no pending commits found")

	// ErrNoActiveChannels is returned when there is no active (open)
	// channels within the database.
	ErrNoActiveChannels = fmt.Errorf("no active channels exist")

	// ErrNoCommitPoint is returned when no data loss commit point is found
	// in the database.
	ErrNoCommitPoint = fmt.Errorf("no commit point found")

	// ErrNoCloseTx is returned when no closing tx is found for a channel
	// in the state CommitBroadcasted.
	ErrNoCloseTx = fmt.Errorf("no closing tx found")

	// ErrNoShutdownInfo is returned when no shutdown info has been
	// persisted for a channel.
	ErrNoShutdownInfo = errors.New("no shutdown info")

	// ErrNoRestoredChannelMutation is returned when a caller attempts to
	// mutate a channel that's been recovered.
	ErrNoRestoredChannelMutation = fmt.Errorf("cannot mutate restored " +
		"channel state")

	// ErrChanBorked is returned when a caller attempts to mutate a borked
	// channel.
	ErrChanBorked = fmt.Errorf("cannot mutate borked channel")

	// ErrMissingIndexEntry is returned when a caller attempts to close a
	// channel and the outpoint is missing from the index.
	ErrMissingIndexEntry = fmt.Errorf("missing outpoint from index")

	// ErrOnionBlobLength is returned is an onion blob with incorrect
	// length is read from disk.
	ErrOnionBlobLength = errors.New("onion blob < 1366 bytes")
)
