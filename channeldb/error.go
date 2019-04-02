package channeldb

import (
	"errors"
	"fmt"
)

var (
	// ErrNoChanDBExists is returned when a channel bucket hasn't been
	// created.
	ErrNoChanDBExists = fmt.Errorf("channel db has not yet been created")

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

	// ErrInvoiceNotFound is returned when a targeted invoice can't be
	// found.
	ErrInvoiceNotFound = fmt.Errorf("unable to locate invoice")

	// ErrNoInvoicesCreated is returned when we don't have invoices in
	// our database to return.
	ErrNoInvoicesCreated = fmt.Errorf("there are no existing invoices")

	// ErrDuplicateInvoice is returned when an invoice with the target
	// payment hash already exists.
	ErrDuplicateInvoice = fmt.Errorf("invoice with payment hash already exists")

	// ErrNoPaymentsCreated is returned when bucket of payments hasn't been
	// created.
	ErrNoPaymentsCreated = fmt.Errorf("there are no existing payments")

	// ErrNodeNotFound is returned when node bucket exists, but node with
	// specific identity can't be found.
	ErrNodeNotFound = fmt.Errorf("link node with target identity not found")

	// ErrChannelNotFound is returned when we attempt to locate a channel
	// for a specific chain, but it is not found.
	ErrChannelNotFound = fmt.Errorf("channel not found")

	// ErrMetaNotFound is returned when meta bucket hasn't been
	// created.
	ErrMetaNotFound = fmt.Errorf("unable to locate meta information")

	// ErrGraphNotFound is returned when at least one of the components of
	// graph doesn't exist.
	ErrGraphNotFound = fmt.Errorf("graph bucket not initialized")

	// ErrGraphNeverPruned is returned when graph was never pruned.
	ErrGraphNeverPruned = fmt.Errorf("graph never pruned")

	// ErrSourceNodeNotSet is returned if the source node of the graph
	// hasn't been added The source node is the center node within a
	// star-graph.
	ErrSourceNodeNotSet = fmt.Errorf("source node does not exist")

	// ErrGraphNodesNotFound is returned in case none of the nodes has
	// been added in graph node bucket.
	ErrGraphNodesNotFound = fmt.Errorf("no graph nodes exist")

	// ErrGraphNoEdgesFound is returned in case of none of the channel/edges
	// has been added in graph edge bucket.
	ErrGraphNoEdgesFound = fmt.Errorf("no graph edges exist")

	// ErrGraphNodeNotFound is returned when we're unable to find the target
	// node.
	ErrGraphNodeNotFound = fmt.Errorf("unable to find node")

	// ErrEdgeNotFound is returned when an edge for the target chanID
	// can't be found.
	ErrEdgeNotFound = fmt.Errorf("edge not found")

	// ErrZombieEdge is an error returned when we attempt to look up an edge
	// but it is marked as a zombie within the zombie index.
	ErrZombieEdge = errors.New("edge marked as zombie")

	// ErrEdgeAlreadyExist is returned when edge with specific
	// channel id can't be added because it already exist.
	ErrEdgeAlreadyExist = fmt.Errorf("edge already exist")

	// ErrNodeAliasNotFound is returned when alias for node can't be found.
	ErrNodeAliasNotFound = fmt.Errorf("alias for node not found")

	// ErrUnknownAddressType is returned when a node's addressType is not
	// an expected value.
	ErrUnknownAddressType = fmt.Errorf("address type cannot be resolved")

	// ErrNoClosedChannels is returned when a node is queries for all the
	// channels it has closed, but it hasn't yet closed any channels.
	ErrNoClosedChannels = fmt.Errorf("no channel have been closed yet")

	// ErrNoForwardingEvents is returned in the case that a query fails due
	// to the log not having any recorded events.
	ErrNoForwardingEvents = fmt.Errorf("no recorded forwarding events")

	// ErrEdgePolicyOptionalFieldNotFound is an error returned if a channel
	// policy field is not found in the db even though its message flags
	// indicate it should be.
	ErrEdgePolicyOptionalFieldNotFound = fmt.Errorf("optional field not " +
		"present")

	// ErrChanAlreadyExists is return when the caller attempts to create a
	// channel with a channel point that is already present in the
	// database.
	ErrChanAlreadyExists = fmt.Errorf("channel already exists")
)

// ErrTooManyExtraOpaqueBytes creates an error which should be returned if the
// caller attempts to write an announcement message which bares too many extra
// opaque bytes. We limit this value in order to ensure that we don't waste
// disk space due to nodes unnecessarily padding out their announcements with
// garbage data.
func ErrTooManyExtraOpaqueBytes(numBytes int) error {
	return fmt.Errorf("max allowed number of opaque bytes is %v, received "+
		"%v bytes", MaxAllowedExtraOpaqueBytes, numBytes)
}
