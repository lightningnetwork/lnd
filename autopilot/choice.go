package autopilot

import (
	"errors"
	"fmt"
	"math/rand"
)

// ErrNoPositive is returned from weightedChoice when there are no positive
// weights left to choose from.
var ErrNoPositive = errors.New("no positive weights left")

// weightedChoice draws a random index from the slice of weights, with a
// probability proportional to the weight at the given index.
func weightedChoice(w []float64) (int, error) {
	// Calculate the sum of weights.
	var sum float64
	for _, v := range w {
		sum += v
	}

	if sum <= 0 {
		return 0, ErrNoPositive
	}

	// Pick a random number in the range [0.0, 1.0) and multiply it with
	// the sum of weights. Then we'll iterate the weights until the number
	// goes below 0. This means that each index is picked with a probability
	// equal to their normalized score.
	//
	// Example:
	// Items with scores [1, 5, 2, 2]
	// Normalized scores [0.1, 0.5, 0.2, 0.2]
	// Imagine they each occupy a "range" equal to their normalized score
	// in [0, 1.0]:
	// [|-0.1-||-----0.5-----||--0.2--||--0.2--|]
	// The following loop is now equivalent to "hitting" the intervals.
	r := rand.Float64() * sum
	for i := range w {
		r -= w[i]
		if r <= 0 {
			return i, nil
		}
	}

	return 0, fmt.Errorf("unable to make choice")
}

// chooseN picks at random min[n, len(s)] nodes if from the NodeScore map, with
// a probability weighted by their score.
func chooseN(n uint32, s map[NodeID]*NodeScore) (
	map[NodeID]*NodeScore, error) {

	// Keep track of the number of nodes not yet chosen, in addition to
	// their scores and NodeIDs.
	rem := len(s)
	scores := make([]float64, len(s))
	nodeIDs := make([]NodeID, len(s))
	i := 0
	for k, v := range s {
		scores[i] = v.Score
		nodeIDs[i] = k
		i++
	}

	// Pick a weighted choice from the remaining nodes as long as there are
	// nodes left, and we haven't already picked n.
	chosen := make(map[NodeID]*NodeScore)
	for len(chosen) < int(n) && rem > 0 {
		choice, err := weightedChoice(scores)
		if err == ErrNoPositive {
			return chosen, nil
		} else if err != nil {
			return nil, err
		}

		nID := nodeIDs[choice]

		chosen[nID] = s[nID]

		// We set the score of the chosen node to 0, so it won't be
		// picked the next iteration.
		scores[choice] = 0
	}

	return chosen, nil
}
