package autopilot

import (
	"fmt"
	"math/rand"
)

// weightedChoice draws a random index from the map of channel candidates, with
// a probability propotional to their score.
func weightedChoice(s map[NodeID]*AttachmentDirective) (NodeID, error) {
	// Calculate the sum of scores found in the map.
	var sum float64
	for _, v := range s {
		sum += v.Score
	}

	if sum <= 0 {
		return NodeID{}, fmt.Errorf("non-positive sum")
	}

	// Create a map of normalized scores such, that they sum to 1.0.
	norm := make(map[NodeID]float64)
	for k, v := range s {
		norm[k] = v.Score / sum
	}

	// Pick a random number in the range [0.0, 1.0), and iterate the map
	// until the number goes below 0. This means that each index is picked
	// with a probablity equal to their normalized score.
	//
	// Example:
	// Items with scores [1, 5, 2, 2]
	// Normalized scores [0.1, 0.5, 0.2, 0.2]
	// Imagine they each occupy a "range" equal to their normalized score
	// in [0, 1.0]:
	// [|-0.1-||-----0.5-----||--0.2--||--0.2--|]
	// The following loop is now equivalent to "hitting" the intervals.
	r := rand.Float64()
	for k, v := range norm {
		r -= v
		if r <= 0 {
			return k, nil
		}
	}
	return NodeID{}, fmt.Errorf("no choice made")
}

// chooseN picks at random min[n, len(s)] nodes if from the
// AttachmentDirectives map, with a probability weighted by their score.
func chooseN(n int, s map[NodeID]*AttachmentDirective) (
	map[NodeID]*AttachmentDirective, error) {

	// Keep a map of nodes not yet choosen.
	rem := make(map[NodeID]*AttachmentDirective)
	for k, v := range s {
		rem[k] = v
	}

	// Pick a weighted choice from the remaining nodes as long as there are
	// nodes left, and we haven't already picked n.
	chosen := make(map[NodeID]*AttachmentDirective)
	for len(chosen) < n && len(rem) > 0 {
		choice, err := weightedChoice(rem)
		if err != nil {
			return nil, err
		}

		chosen[choice] = rem[choice]
		delete(rem, choice)
	}

	return chosen, nil
}
