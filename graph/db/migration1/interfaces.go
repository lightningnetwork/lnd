package migration1

import (
	"context"

	"github.com/lightningnetwork/lnd/graph/db/migration1/models"
)

// V1Store represents the main interface for the channel graph database for all
// channels and nodes gossiped via the V1 gossip protocol as defined in BOLT 7.
type V1Store interface {
	// ForEachNode iterates through all the stored vertices/nodes in the
	// graph, executing the passed callback with each node encountered. If
	// the callback returns an error, then the transaction is aborted and
	// the iteration stops early.
	ForEachNode(ctx context.Context, cb func(*models.Node) error,
		reset func()) error

	// ForEachChannel iterates through all the channel edges stored within
	// the graph and invokes the passed callback for each edge. The callback
	// takes two edges as since this is a directed graph, both the in/out
	// edges are visited. If the callback returns an error, then the
	// transaction is aborted and the iteration stops early.
	//
	// NOTE: If an edge can't be found, or wasn't advertised, then a nil
	// pointer for that particular channel edge routing policy will be
	// passed into the callback.
	ForEachChannel(ctx context.Context, cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy, *models.ChannelEdgePolicy) error,
		reset func()) error

	// SourceNode returns the source node of the graph. The source node is
	// treated as the center node within a star-graph. This method may be
	// used to kick off a path finding algorithm in order to explore the
	// reachability of another node based off the source node.
	SourceNode(ctx context.Context) (*models.Node, error)
}
