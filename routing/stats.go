package routing

import (
	"fmt"
	"sync"
	"time"
)

// routerStats is a struct that tracks various updates to the graph and
// facilitates aggregate logging of the statistics.
type routerStats struct {
	numChannels uint32
	numUpdates  uint32
	numNodes    uint32
	lastReset   time.Time

	mu sync.RWMutex
}

// incNumEdges increments the number of discovered edges.
func (g *routerStats) incNumEdgesDiscovered() {
	g.mu.Lock()
	g.numChannels++
	g.mu.Unlock()
}

// incNumUpdates increments the number of channel updates processed.
func (g *routerStats) incNumChannelUpdates() {
	g.mu.Lock()
	g.numUpdates++
	g.mu.Unlock()
}

// incNumNodeUpdates increments the number of node updates processed.
func (g *routerStats) incNumNodeUpdates() {
	g.mu.Lock()
	g.numNodes++
	g.mu.Unlock()
}

// Empty returns true if all stats are zero.
func (g *routerStats) Empty() bool {
	g.mu.RLock()
	isEmpty := g.numChannels == 0 &&
		g.numUpdates == 0 &&
		g.numNodes == 0
	g.mu.RUnlock()
	return isEmpty
}

// Reset clears any router stats and sets the lastReset field to now.
func (g *routerStats) Reset() {
	g.mu.Lock()
	g.numChannels = 0
	g.numUpdates = 0
	g.numNodes = 0
	g.lastReset = time.Now()
	g.mu.Unlock()
}

// String returns a human-readable description of the router stats.
func (g *routerStats) String() string {
	g.mu.RLock()
	str := fmt.Sprintf("Processed channels=%d updates=%d nodes=%d in "+
		"last %v", g.numChannels, g.numUpdates, g.numNodes,
		time.Since(g.lastReset))
	g.mu.RUnlock()
	return str
}
