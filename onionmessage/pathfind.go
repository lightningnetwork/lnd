package onionmessage

import (
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// OnionMessagePath represents a route found for an onion message.
type OnionMessagePath struct {
	// Hops is ordered from the first-hop peer to the destination.
	Hops []route.Vertex
}

// FindPath finds the shortest path (by hop count) from source to destination
// through nodes that support onion messaging (feature bit 38/39). It uses a
// standard BFS on the channel graph filtered by the OnionMessagesOptional
// feature bit.
func FindPath(graph graphdb.NodeTraverser, source, destination route.Vertex,
	maxHops int) (*OnionMessagePath, error) {

	// Check that the destination supports onion messaging.
	destFeatures, err := graph.FetchNodeFeatures(destination)
	if err != nil {
		return nil, err
	}

	if !destFeatures.HasFeature(lnwire.OnionMessagesOptional) {
		return nil, ErrDestinationNoOnionSupport
	}

	// If source == destination, return empty path.
	if source == destination {
		return &OnionMessagePath{}, nil
	}

	parent := make(map[route.Vertex]route.Vertex)
	visited := make(map[route.Vertex]bool)
	featureCache := make(map[route.Vertex]bool)

	// Cache the destination as supporting onion messages (checked above).
	featureCache[destination] = true

	// supportsOnionMessages is a closure that checks (with caching) whether
	// a node advertises the onion messages feature bit.
	supportsOnionMessages := func(node route.Vertex) bool {
		if cached, ok := featureCache[node]; ok {
			return cached
		}

		features, err := graph.FetchNodeFeatures(node)
		if err != nil {
			log.Tracef("Unable to fetch features for node %v: %v",
				node.String(), err)
			featureCache[node] = false
			return false
		}

		supports := features.HasFeature(
			lnwire.OnionMessagesOptional,
		)

		featureCache[node] = supports

		return supports
	}

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
			err := graph.ForEachNodeDirectedChannel(current,
				func(channel *graphdb.DirectedChannel) error {
					neighbor := channel.OtherNode

					if visited[neighbor] {
						return nil
					}

					// Skip nodes that don't support
					// onion messaging.
					if !supportsOnionMessages(neighbor) {
						return nil
					}

					visited[neighbor] = true
					parent[neighbor] = current

					if neighbor == destination {
						return errBFSDone
					}

					nextQueue = append(
						nextQueue, neighbor,
					)

					return nil
				},
				func() {
					// Reset callback - nothing to
					// reset for BFS.
				},
			)

			// Check if we found the destination.
			if err == errBFSDone {
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
var errBFSDone = &bfsDoneError{}

type bfsDoneError struct{}

func (e *bfsDoneError) Error() string { return "bfs done" }

// reconstructPath rebuilds the path from destination back to source using the
// parent map, returning the hops in forward order (excluding source).
func reconstructPath(parent map[route.Vertex]route.Vertex,
	source, destination route.Vertex) *OnionMessagePath {

	var path []route.Vertex

	current := destination
	for current != source {
		path = append(path, current)
		current = parent[current]
	}

	// Reverse the path to get source-to-destination order.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	return &OnionMessagePath{Hops: path}
}
