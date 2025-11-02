package migration1

import (
	"errors"
	"fmt"
)

var (
	// ErrEdgePolicyOptionalFieldNotFound is an error returned if a channel
	// policy field is not found in the db even though its message flags
	// indicate it should be.
	ErrEdgePolicyOptionalFieldNotFound = fmt.Errorf("optional field not " +
		"present")

	// ErrParsingExtraTLVBytes is returned when we attempt to parse
	// extra opaque bytes as a TLV stream, but the parsing fails.
	ErrParsingExtraTLVBytes = fmt.Errorf("error parsing extra TLV bytes")

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

	// ErrZombieEdge is an error returned when we attempt to look up an edge
	// but it is marked as a zombie within the zombie index.
	ErrZombieEdge = errors.New("edge marked as zombie")

	// ErrEdgeNotFound is returned when an edge for the target chanID
	// can't be found.
	ErrEdgeNotFound = fmt.Errorf("edge not found")

	// ErrEdgeAlreadyExist is returned when edge with specific
	// channel id can't be added because it already exist.
	ErrEdgeAlreadyExist = fmt.Errorf("edge already exist")

	// ErrNodeAliasNotFound is returned when alias for node can't be found.
	ErrNodeAliasNotFound = fmt.Errorf("alias for node not found")

	// ErrClosedScidsNotFound is returned when the closed scid bucket
	// hasn't been created.
	ErrClosedScidsNotFound = fmt.Errorf("closed scid bucket doesn't exist")

	// ErrZombieEdgeNotFound is an error returned when we attempt to find an
	// edge in the zombie index which is not there.
	ErrZombieEdgeNotFound = errors.New("edge not found in zombie index")

	// ErrUnknownAddressType is returned when a node's addressType is not
	// an expected value.
	ErrUnknownAddressType = fmt.Errorf("address type cannot be resolved")

	// ErrCantCheckIfZombieEdgeStr is an error returned when we
	// attempt to check if an edge is a zombie but encounter an error.
	ErrCantCheckIfZombieEdgeStr = fmt.Errorf("unable to check if edge " +
		"is a zombie")
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
