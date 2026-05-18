package channeldb

import (
	"fmt"

	cstate "github.com/lightningnetwork/lnd/chanstate"
)

var (
	// ErrNoChanDBExists is returned when a channel bucket hasn't been
	// created.
	ErrNoChanDBExists = cstate.ErrNoChanDBExists

	// ErrNoHistoricalBucket is returned when the historical channel bucket
	// not been created yet.
	ErrNoHistoricalBucket = cstate.ErrNoHistoricalBucket

	// ErrDBReversion is returned when detecting an attempt to revert to a
	// prior database version.
	ErrDBReversion = fmt.Errorf("channel db cannot revert to prior version")

	// ErrLinkNodesNotFound is returned when node info bucket hasn't been
	// created.
	ErrLinkNodesNotFound = fmt.Errorf("no link nodes exist")

	// ErrNoActiveChannels  is returned when there is no active (open)
	// channels within the database.
	ErrNoActiveChannels = cstate.ErrNoActiveChannels

	// ErrNoPastDeltas is returned when the channel delta bucket hasn't been
	// created.
	ErrNoPastDeltas = cstate.ErrNoPastDeltas

	// ErrNodeNotFound is returned when node bucket exists, but node with
	// specific identity can't be found.
	ErrNodeNotFound = fmt.Errorf("link node with target identity not found")

	// ErrChannelNotFound is returned when we attempt to locate a channel
	// for a specific chain, but it is not found.
	ErrChannelNotFound = cstate.ErrChannelNotFound

	// ErrMetaNotFound is returned when meta bucket hasn't been
	// created.
	ErrMetaNotFound = fmt.Errorf("unable to locate meta information")

	// ErrDBVersionNotFound is returned when the meta bucket exists, but
	// the DB version key hasn't been written.
	ErrDBVersionNotFound = fmt.Errorf("unable to locate db version")

	// ErrNoClosedChannels is returned when a node is queries for all the
	// channels it has closed, but it hasn't yet closed any channels.
	ErrNoClosedChannels = cstate.ErrNoClosedChannels

	// ErrClosedChannelNotFound signals that a closed channel could not be
	// found in the channeldb.
	ErrClosedChannelNotFound = cstate.ErrClosedChannelNotFound

	// ErrNoForwardingEvents is returned in the case that a query fails due
	// to the log not having any recorded events.
	ErrNoForwardingEvents = fmt.Errorf("no recorded forwarding events")

	// ErrChanAlreadyExists is return when the caller attempts to create a
	// channel with a channel point that is already present in the
	// database.
	ErrChanAlreadyExists = cstate.ErrChanAlreadyExists
)
