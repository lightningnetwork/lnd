package channeldb

import (
	"fmt"
)

var (
	// ErrNoChanDBExists is returned when a channel bucket hasn't been
	// created.
	ErrNoChanDBExists = fmt.Errorf("channel db has not yet been created")

	// ErrNoHistoricalBucket is returned when the historical channel bucket
	// not been created yet.
	ErrNoHistoricalBucket = fmt.Errorf("historical channel bucket has " +
		"not yet been created")

	// ErrDBReversion is returned when detecting an attempt to revert to a
	// prior database version.
	ErrDBReversion = fmt.Errorf("channel db cannot revert to prior version")

	// ErrLinkNodesNotFound is returned when node info bucket hasn't been
	// created.
	ErrLinkNodesNotFound = fmt.Errorf("no link nodes exist")

	// ErrNoActiveChannels  is returned when there is no active (open)
	// channels within the database.
	ErrNoActiveChannels = fmt.Errorf("no active channels exist")

	// ErrNoPastDeltas is returned when the channel delta bucket hasn't been
	// created.
	ErrNoPastDeltas = fmt.Errorf("channel has no recorded deltas")

	// ErrNodeNotFound is returned when node bucket exists, but node with
	// specific identity can't be found.
	ErrNodeNotFound = fmt.Errorf("link node with target identity not found")

	// ErrChannelNotFound is returned when we attempt to locate a channel
	// for a specific chain, but it is not found.
	ErrChannelNotFound = fmt.Errorf("channel not found")

	// ErrMetaNotFound is returned when meta bucket hasn't been
	// created.
	ErrMetaNotFound = fmt.Errorf("unable to locate meta information")

	// ErrNoClosedChannels is returned when a node is queries for all the
	// channels it has closed, but it hasn't yet closed any channels.
	ErrNoClosedChannels = fmt.Errorf("no channel have been closed yet")

	// ErrNoForwardingEvents is returned in the case that a query fails due
	// to the log not having any recorded events.
	ErrNoForwardingEvents = fmt.Errorf("no recorded forwarding events")

	// ErrChanAlreadyExists is return when the caller attempts to create a
	// channel with a channel point that is already present in the
	// database.
	ErrChanAlreadyExists = fmt.Errorf("channel already exists")
)
