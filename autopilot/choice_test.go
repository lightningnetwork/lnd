package autopilot

import (
	"encoding/binary"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
)

// TestWeightedChoiceEmptyMap tests that passing in an empty slice of weights
// returns an error.
func TestWeightedChoiceEmptyMap(t *testing.T) {
	t.Parallel()

	var w []float64
	_, err := weightedChoice(w)
	if err != ErrNoPositive {
		t.Fatalf("expected ErrNoPositive when choosing in "+
			"empty map, instead got %v", err)
	}
}

// singleNonZero is a type used to generate float64 slices with one non-zero
// element.
type singleNonZero []float64

// Generate generates a value of type sinelNonZero to be used during
// QuickTests.
func (singleNonZero) Generate(rand *rand.Rand, size int) reflect.Value {
	w := make([]float64, size)

	// Pick a random index and set it to a random float.
	i := rand.Intn(size)
	w[i] = rand.Float64()

	return reflect.ValueOf(w)
}

// TestWeightedChoiceSingleIndex tests that choosing randomly in a slice with
// one positive element always returns that one index.
func TestWeightedChoiceSingleIndex(t *testing.T) {
	t.Parallel()

	// Helper that returns the index of the non-zero element.
	allButOneZero := func(weights []float64) (bool, int) {
		var (
			numZero   uint32
			nonZeroEl int
		)

		for i, w := range weights {
			if w != 0 {
				numZero++
				nonZeroEl = i
			}
		}

		return numZero == 1, nonZeroEl
	}

	property := func(weights singleNonZero) bool {
		// Make sure the generated slice has exactly one non-zero
		// element.
		conditionMet, nonZeroElem := allButOneZero(weights[:])
		if !conditionMet {
			return false
		}

		// Call weightedChoice and assert it picks the non-zero
		// element.
		choice, err := weightedChoice(weights[:])
		if err != nil {
			return false
		}
		return choice == nonZeroElem
	}

	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

// nonNegative is a type used to generate float64 slices with non-negative
// elements.
type nonNegative []float64

// Generate generates a value of type nonNegative to be used during
// QuickTests.
func (nonNegative) Generate(rand *rand.Rand, size int) reflect.Value {
	w := make([]float64, size)

	for i := range w {
		r := rand.Float64()

		// For very small weights it won't work to check deviation from
		// expected value, so we set them to zero.
		if r < 0.01*float64(size) {
			r = 0
		}
		w[i] = float64(r)
	}
	return reflect.ValueOf(w)
}

func assertChoice(w []float64, iterations int) bool {
	var sum float64
	for _, v := range w {
		sum += v
	}

	// Calculate the expected frequency of each choice.
	expFrequency := make([]float64, len(w))
	for i, ww := range w {
		expFrequency[i] = ww / sum
	}

	chosen := make(map[int]int)
	for i := 0; i < iterations; i++ {
		res, err := weightedChoice(w)
		if err != nil {
			return false
		}
		chosen[res]++
	}

	// Since this is random we check that the number of times chosen is
	// within 20% of the expected value.
	totalChoices := 0
	for i, f := range expFrequency {
		exp := float64(iterations) * f
		v := float64(chosen[i])
		totalChoices += chosen[i]
		expHigh := exp + exp/5
		expLow := exp - exp/5
		if v < expLow || v > expHigh {
			return false
		}
	}

	// The sum of choices must be exactly iterations of course.
	return totalChoices == iterations

}

// TestWeightedChoiceDistribution asserts that the weighted choice algorithm
// chooses among indexes according to their scores.
func TestWeightedChoiceDistribution(t *testing.T) {
	const iterations = 100000

	property := func(weights nonNegative) bool {
		return assertChoice(weights, iterations)
	}

	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

// TestChooseNEmptyMap checks that chooseN returns an empty result when no
// nodes are chosen among.
func TestChooseNEmptyMap(t *testing.T) {
	t.Parallel()

	nodes := map[NodeID]*NodeScore{}
	property := func(n uint32) bool {
		res, err := chooseN(n, nodes)
		if err != nil {
			return false
		}

		// Result should always be empty.
		return len(res) == 0
	}

	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

// candidateMapVarLen is a type we'll use to generate maps of various lengths
// up to 255 to be used during QuickTests.
type candidateMapVarLen map[NodeID]*NodeScore

// Generate generates a value of type candidateMapVarLen to be used during
// QuickTests.
func (candidateMapVarLen) Generate(rand *rand.Rand, size int) reflect.Value {
	nodes := make(map[NodeID]*NodeScore)

	// To avoid creating huge maps, we restrict them to max uint8 len.
	n := uint8(rand.Uint32())

	for i := uint8(0); i < n; i++ {
		s := rand.Float64()

		// We set small values to zero, to ensure we handle these
		// correctly.
		if s < 0.01 {
			s = 0
		}

		var nID [33]byte
		binary.BigEndian.PutUint32(nID[:], uint32(i))
		nodes[nID] = &NodeScore{
			Score: s,
		}
	}

	return reflect.ValueOf(nodes)
}

// TestChooseNMinimum test that chooseN returns the minimum of the number of
// nodes we request and the number of positively scored nodes in the given map.
func TestChooseNMinimum(t *testing.T) {
	t.Parallel()

	// Helper to count the number of positive scores in the given map.
	numPositive := func(nodes map[NodeID]*NodeScore) int {
		cnt := 0
		for _, v := range nodes {
			if v.Score > 0 {
				cnt++
			}
		}
		return cnt
	}

	// We use let the type of n be uint8 to avoid generating huge numbers.
	property := func(nodes candidateMapVarLen, n uint8) bool {
		res, err := chooseN(uint32(n), nodes)
		if err != nil {
			return false
		}

		positive := numPositive(nodes)

		// Result should always be the minimum of the number of nodes
		// we wanted to select and the number of positively scored
		// nodes in the map.
		min := positive
		if int(n) < min {
			min = int(n)
		}

		if len(res) != min {
			return false

		}
		return true
	}

	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

// TestChooseNSample sanity checks that nodes are picked by chooseN according
// to their scores.
func TestChooseNSample(t *testing.T) {
	t.Parallel()

	const numNodes = 500
	const maxIterations = 100000
	fifth := uint32(numNodes / 5)

	nodes := make(map[NodeID]*NodeScore)

	// we make 5 buckets of nodes: 0, 0.1, 0.2, 0.4 and 0.8 score. We want
	// to check that zero scores never gets chosen, while a doubling the
	// score makes a node getting chosen about double the amount (this is
	// true only when n <<< numNodes).
	j := 2 * fifth
	score := 0.1
	for i := uint32(0); i < numNodes; i++ {

		// Each time i surpasses j we double the score we give to the
		// next fifth of nodes.
		if i >= j {
			score *= 2
			j += fifth
		}
		s := score

		// The first 1/5 of nodes we give a score of 0.
		if i < fifth {
			s = 0
		}

		var nID [33]byte
		binary.BigEndian.PutUint32(nID[:], i)
		nodes[nID] = &NodeScore{
			Score: s,
		}
	}

	// For each value of N we'll check that the nodes are picked the
	// expected number of times over time.
	for _, n := range []uint32{1, 5, 10, 20, 50} {
		// Since choosing more nodes will result in chooseN getting
		// slower we decrease the number of iterations. This is okay
		// since the variance in the total picks for a node will be
		// lower when choosing more nodes each time.
		iterations := maxIterations / n
		count := make(map[NodeID]int)
		for i := 0; i < int(iterations); i++ {
			res, err := chooseN(n, nodes)
			if err != nil {
				t.Fatalf("failed choosing nodes: %v", err)
			}

			for nID := range res {
				count[nID]++
			}
		}

		// Sum the number of times a node in each score bucket was
		// picked.
		sums := make(map[float64]int)
		for nID, s := range nodes {
			sums[s.Score] += count[nID]
		}

		// The count of each bucket should be about double of the
		// previous bucket.  Since this is all random, we check that
		// the result is within 20% of the expected value.
		for _, score := range []float64{0.2, 0.4, 0.8} {
			cnt := sums[score]
			half := cnt / 2
			expLow := half - half/5
			expHigh := half + half/5
			if sums[score/2] < expLow || sums[score/2] > expHigh {
				t.Fatalf("expected the nodes with score %v "+
					"to be chosen about %v times, instead "+
					"was %v", score/2, half, sums[score/2])
			}
		}
	}
}
