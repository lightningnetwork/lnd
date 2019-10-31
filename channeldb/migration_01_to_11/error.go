package migration_01_to_11

import (
	"fmt"
)

var (
	// ErrNoInvoicesCreated is returned when we don't have invoices in
	// our database to return.
	ErrNoInvoicesCreated = fmt.Errorf("there are no existing invoices")

	// ErrNoPaymentsCreated is returned when bucket of payments hasn't been
	// created.
	ErrNoPaymentsCreated = fmt.Errorf("there are no existing payments")

	// ErrGraphNotFound is returned when at least one of the components of
	// graph doesn't exist.
	ErrGraphNotFound = fmt.Errorf("graph bucket not initialized")

	// ErrSourceNodeNotSet is returned if the source node of the graph
	// hasn't been added The source node is the center node within a
	// star-graph.
	ErrSourceNodeNotSet = fmt.Errorf("source node does not exist")

	// ErrGraphNodeNotFound is returned when we're unable to find the target
	// node.
	ErrGraphNodeNotFound = fmt.Errorf("unable to find node")

	// ErrEdgeNotFound is returned when an edge for the target chanID
	// can't be found.
	ErrEdgeNotFound = fmt.Errorf("edge not found")

	// ErrUnknownAddressType is returned when a node's addressType is not
	// an expected value.
	ErrUnknownAddressType = fmt.Errorf("address type cannot be resolved")

	// ErrNoClosedChannels is returned when a node is queries for all the
	// channels it has closed, but it hasn't yet closed any channels.
	ErrNoClosedChannels = fmt.Errorf("no channel have been closed yet")

	// ErrEdgePolicyOptionalFieldNotFound is an error returned if a channel
	// policy field is not found in the db even though its message flags
	// indicate it should be.
	ErrEdgePolicyOptionalFieldNotFound = fmt.Errorf("optional field not " +
		"present")
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
