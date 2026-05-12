package onionmessage

import (
	"context"
	"errors"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// OnionMessagePath represents a route found for an onion message. It is a slice
// of vertices ordered from the first-hop peer to the destination.
type OnionMessagePath []route.Vertex

// FindPath finds the shortest path (by hop count) from source to destination
// through nodes that support onion messaging (feature bit 38/39). It uses a
// standard BFS on the channel graph filtered by the OnionMessagesOptional
// feature bit.
func FindPath(ctx context.Context, graph graphdb.NodeTraverser, source,
	destination route.Vertex, maxHops int) (OnionMessagePath, error) {

	// Check that the destination supports onion messaging.
	destFeatures, err := graph.FetchNodeFeatures(ctx, destination)
	if err != nil {
		return nil, err
	}

	// An empty feature vector means the node is absent from our graph.
	// In that case, we send back a NotFound error.
	if len(destFeatures.Features()) == 0 {
		return nil, ErrNodeNotFound
	}

	if !destFeatures.HasFeature(lnwire.OnionMessagesOptional) {
		return nil, ErrDestinationNoOnionSupport
	}

	// If source == destination, return empty path.
	if source == destination {
		return OnionMessagePath{}, nil
	}

	parent := make(map[route.Vertex]route.Vertex)
	visited := make(map[route.Vertex]bool)

	visited[source] = true

	queue := []route.Vertex{source}
	depth := 0

	for len(queue) > 0 {
		depth++
		if depth > maxHops {
			break
		}

		nextQueue := make([]route.Vertex, 0)

		for _, current := range queue {
			err := graph.ForEachNodeDirectedChannel(ctx, current,
				func(channel *graphdb.DirectedChannel) error {
					neighbor := channel.OtherNode

					if visited[neighbor] {
						return nil
					}

					// Mark visited before the feature check
					// so we never fetch features for the
					// same node twice.
					visited[neighbor] = true

					// Skip nodes that don't support onion
					// messaging.
					feats, err := graph.FetchNodeFeatures(
						ctx, neighbor,
					)
					if err != nil {
						// If the context is canceled or
						// deadline exceeded, propagate
						// the error.

						if ctx.Err() != nil {
							return err
						}

						log.Tracef("Unable to fetch "+
							"features for node "+
							"%v: %v",
							neighbor.String(), err)

						return nil
					}

					if !feats.HasFeature(
						lnwire.OnionMessagesOptional,
					) {

						return nil
					}

					parent[neighbor] = current

					if neighbor == destination {
						return errBFSDone
					}

					nextQueue = append(
						nextQueue, neighbor,
					)

					return nil
				},
				func() {},
			)

			// Check if we found the destination.
			if errors.Is(err, errBFSDone) {
				return reconstructPath(
					parent, source, destination,
				), nil
			}

			if err != nil {
				return nil, err
			}
		}

		queue = nextQueue
	}

	return nil, ErrNoPathFound
}

// errBFSDone is a sentinel error used internally to break out of the
// ForEachNodeDirectedChannel callback when the destination is found.
var errBFSDone = errors.New("bfs done")

// reconstructPath rebuilds the path from destination back to source using the
// parent map, returning the hops in forward order (excluding source).
func reconstructPath(parent map[route.Vertex]route.Vertex,
	source, destination route.Vertex) OnionMessagePath {

	// Calculate path length to pre-allocate the slice.
	pathLen := 0
	for curr := destination; curr != source; curr = parent[curr] {
		pathLen++
	}

	// Populate the path in correct order, avoiding a separate reversal
	// step.
	path := make(OnionMessagePath, pathLen)
	curr := destination
	for i := pathLen - 1; i >= 0; i-- {
		path[i] = curr
		curr = parent[curr]
	}

	return path
}
