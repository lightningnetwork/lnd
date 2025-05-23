package autopilot

import (
	"context"
	"fmt"
	"sync"
)

// stack is a simple int stack to help with readability of Brandes'
// betweenness centrality implementation below.
type stack struct {
	stack []int
}

func (s *stack) push(v int) {
	s.stack = append(s.stack, v)
}

func (s *stack) top() int {
	return s.stack[len(s.stack)-1]
}

func (s *stack) pop() {
	s.stack = s.stack[:len(s.stack)-1]
}

func (s *stack) empty() bool {
	return len(s.stack) == 0
}

// queue is a simple int queue to help with readability of Brandes'
// betweenness centrality implementation below.
type queue struct {
	queue []int
}

func (q *queue) push(v int) {
	q.queue = append(q.queue, v)
}

func (q *queue) front() int {
	return q.queue[0]
}

func (q *queue) pop() {
	q.queue = q.queue[1:]
}

func (q *queue) empty() bool {
	return len(q.queue) == 0
}

// BetweennessCentrality is a NodeMetric that calculates node betweenness
// centrality using Brandes' algorithm. Betweenness centrality for each node
// is the number of shortest paths passing through that node, not counting
// shortest paths starting or ending at that node. This is a useful metric
// to measure control of individual nodes over the whole network.
type BetweennessCentrality struct {
	// workers number of goroutines are used to parallelize
	// centrality calculation.
	workers int

	// centrality stores original (not normalized) centrality values for
	// each node in the graph.
	centrality map[NodeID]float64

	// min is the minimum centrality in the graph.
	min float64

	// max is the maximum centrality in the graph.
	max float64
}

// NewBetweennessCentralityMetric creates a new BetweennessCentrality instance.
// Users can specify the number of workers to use for calculating centrality.
func NewBetweennessCentralityMetric(workers int) (*BetweennessCentrality, error) {
	// There should be at least one worker.
	if workers < 1 {
		return nil, fmt.Errorf("workers must be positive")
	}
	return &BetweennessCentrality{
		workers: workers,
	}, nil
}

// Name returns the name of the metric.
func (bc *BetweennessCentrality) Name() string {
	return "betweenness_centrality"
}

// betweennessCentrality is the core of Brandes' algorithm.
// We first calculate the shortest paths from the start node s to all other
// nodes with BFS, then update the betweenness centrality values by using
// Brandes' dependency trick.
// For detailed explanation please read:
// https://www.cl.cam.ac.uk/teaching/1617/MLRD/handbook/brandes.html
func betweennessCentrality(g *SimpleGraph, s int, centrality []float64) {
	// pred[w] is the list of nodes that immediately precede w on a
	// shortest path from s to t for each node t.
	pred := make([][]int, len(g.Nodes))

	// sigma[t] is the number of shortest paths between nodes s and t
	// for each node t.
	sigma := make([]int, len(g.Nodes))
	sigma[s] = 1

	// dist[t] holds the distance between s and t for each node t.
	// We initialize this to -1 (meaning infinity) for each t != s.
	dist := make([]int, len(g.Nodes))
	for i := range dist {
		dist[i] = -1
	}

	dist[s] = 0

	var (
		st stack
		q  queue
	)
	q.push(s)

	// BFS to calculate the shortest paths (sigma and pred)
	// from s to t for each node t.
	for !q.empty() {
		v := q.front()
		q.pop()
		st.push(v)

		for _, w := range g.Adj[v] {
			// If distance from s to w is infinity (-1)
			// then set it and enqueue w.
			if dist[w] < 0 {
				dist[w] = dist[v] + 1
				q.push(w)
			}

			// If w is on a shortest path the update
			// sigma and add v to w's predecessor list.
			if dist[w] == dist[v]+1 {
				sigma[w] += sigma[v]
				pred[w] = append(pred[w], v)
			}
		}
	}

	// delta[v] is the ratio of the shortest paths between s and t that go
	// through v and the total number of shortest paths between s and t.
	// If we have delta then the betweenness centrality is simply the sum
	// of delta[w] for each w != s.
	delta := make([]float64, len(g.Nodes))

	for !st.empty() {
		w := st.top()
		st.pop()

		// pred[w] is the list of nodes that immediately precede w on a
		// shortest path from s.
		for _, v := range pred[w] {
			// Update delta using Brandes' equation.
			delta[v] += (float64(sigma[v]) / float64(sigma[w])) * (1.0 + delta[w])
		}

		if w != s {
			// As noted above centrality is simply the sum
			// of delta[w] for each w != s.
			centrality[w] += delta[w]
		}
	}
}

// Refresh recalculates and stores centrality values.
func (bc *BetweennessCentrality) Refresh(ctx context.Context,
	graph ChannelGraph) error {

	cache, err := NewSimpleGraph(ctx, graph)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	work := make(chan int)
	partials := make(chan []float64, bc.workers)

	// Each worker will compute a partial result.
	// This partial result is a sum of centrality updates
	// on roughly N / workers nodes.
	worker := func() {
		defer wg.Done()
		partial := make([]float64, len(cache.Nodes))

		// Consume the next node, update centrality
		// parital to avoid unnecessary synchronization.
		for node := range work {
			betweennessCentrality(cache, node, partial)
		}
		partials <- partial
	}

	// Now start the N workers.
	wg.Add(bc.workers)
	for i := 0; i < bc.workers; i++ {
		go worker()
	}

	// Distribute work amongst workers.
	// Should be fair when the graph is sufficiently large.
	for node := range cache.Nodes {
		work <- node
	}

	close(work)
	wg.Wait()
	close(partials)

	// Collect and sum partials for final result.
	centrality := make([]float64, len(cache.Nodes))
	for partial := range partials {
		for i := 0; i < len(partial); i++ {
			centrality[i] += partial[i]
		}
	}

	// Get min/max to be able to normalize
	// centrality values between 0 and 1.
	bc.min = 0
	bc.max = 0
	if len(centrality) > 0 {
		for _, v := range centrality {
			if v < bc.min {
				bc.min = v
			} else if v > bc.max {
				bc.max = v
			}
		}
	}

	// Divide by two as this is an undirected graph.
	bc.min /= 2.0
	bc.max /= 2.0

	bc.centrality = make(map[NodeID]float64)
	for u, value := range centrality {
		// Divide by two as this is an undirected graph.
		bc.centrality[cache.Nodes[u]] = value / 2.0
	}

	return nil
}

// GetMetric returns the current centrality values for each node indexed
// by node id.
func (bc *BetweennessCentrality) GetMetric(normalize bool) map[NodeID]float64 {
	// Normalization factor.
	var z float64
	if (bc.max - bc.min) > 0 {
		z = 1.0 / (bc.max - bc.min)
	}

	centrality := make(map[NodeID]float64)
	for k, v := range bc.centrality {
		if normalize {
			v = (v - bc.min) * z
		}
		centrality[k] = v
	}

	return centrality
}
