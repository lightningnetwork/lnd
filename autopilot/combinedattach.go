package autopilot

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
)

// WeightedHeuristic is a tuple that associates a weight to an
// AttachmentHeuristic. This is used to determining a node's final score when
// querying several heuristics for scores.
type WeightedHeuristic struct {
	// Weight is this AttachmentHeuristic's relative weight factor. It
	// should be between 0.0 and 1.0.
	Weight float64

	AttachmentHeuristic
}

// WeightedCombAttachment is an implementation of the AttachmentHeuristic
// interface that combines the scores given by several sub-heuristics into one.
type WeightedCombAttachment struct {
	heuristics []*WeightedHeuristic
}

// NewWeightedCombAttachment creates a new instance of a WeightedCombAttachment.
func NewWeightedCombAttachment(h ...*WeightedHeuristic) (
	*WeightedCombAttachment, error) {

	// The sum of weights given to the sub-heuristics must sum to exactly
	// 1.0.
	var sum float64
	for _, w := range h {
		sum += w.Weight
	}

	if sum != 1.0 {
		return nil, fmt.Errorf("weights MUST sum to 1.0 (was %v)", sum)
	}

	return &WeightedCombAttachment{
		heuristics: h,
	}, nil
}

// A compile time assertion to ensure WeightedCombAttachment meets the
// AttachmentHeuristic and ScoreSettable interfaces.
var _ AttachmentHeuristic = (*WeightedCombAttachment)(nil)
var _ ScoreSettable = (*WeightedCombAttachment)(nil)

// Name returns the name of this heuristic.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (c *WeightedCombAttachment) Name() string {
	return "weightedcomb"
}

// NodeScores is a method that given the current channel graph, current set of
// local channels and funds available, scores the given nodes according to the
// preference of opening a channel with them. The returned channel candidates
// maps the NodeID to an attachment directive containing a score and a channel
// size.
//
// The scores is determined by querying the set of sub-heuristics, then
// combining these scores into a final score according to the active
// configuration.
//
// The returned scores will be in the range [0, 1.0], where 0 indicates no
// improvement in connectivity if a channel is opened to this node, while 1.0
// is the maximum possible improvement in connectivity.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (c *WeightedCombAttachment) NodeScores(ctx context.Context, g ChannelGraph,
	chans []LocalChannel, chanSize btcutil.Amount,
	nodes map[NodeID]struct{}) (map[NodeID]*NodeScore, error) {

	// We now query each heuristic to determine the score they give to the
	// nodes for the given channel size.
	var subScores []map[NodeID]*NodeScore
	for _, h := range c.heuristics {
		log.Tracef("Getting scores from sub heuristic %v", h.Name())

		s, err := h.NodeScores(
			ctx, g, chans, chanSize, nodes,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to get sub score: %w",
				err)
		}

		subScores = append(subScores, s)
	}

	// We combine the scores given by the sub-heuristics by using the
	// heuristics' given weight factor.
	scores := make(map[NodeID]*NodeScore)
	for nID := range nodes {
		score := &NodeScore{
			NodeID: nID,
		}

		// Each sub-heuristic should have scored the node, if not it is
		// implicitly given a zero score by that heuristic.
		for i, h := range c.heuristics {
			sub, ok := subScores[i][nID]
			if !ok {
				log.Tracef("No score given to node %x by sub "+
					"heuristic %v", nID[:], h.Name())
				continue
			}
			// Use the heuristic's weight factor to determine of
			// how much weight we should give to this particular
			// score.
			subScore := h.Weight * sub.Score
			log.Tracef("Giving node %x a sub score of %v "+
				"(%v * %v) from sub heuristic %v", nID[:],
				subScore, h.Weight, sub.Score, h.Name())

			score.Score += subScore
		}

		log.Tracef("Node %x got final combined score %v", nID[:],
			score.Score)

		switch {
		// Instead of adding a node with score 0 to the returned set,
		// we just skip it.
		case score.Score == 0:
			continue

		// Sanity check the new score.
		case score.Score < 0 || score.Score > 1.0:
			return nil, fmt.Errorf("invalid node score from "+
				"combination: %v", score.Score)
		}

		scores[nID] = score
	}

	return scores, nil
}

// SetNodeScores is used to set the internal map from NodeIDs to scores. The
// passed scores must be in the range [0, 1.0]. The fist parameter is the name
// of the targeted heuristic, to allow recursively target specific
// sub-heuristics. The returned boolean indicates whether the targeted
// heuristic was found.
//
// Since this heuristic doesn't keep any internal scores, it will recursively
// apply the scores to its sub-heuristics.
//
// NOTE: This is a part of the ScoreSettable interface.
func (c *WeightedCombAttachment) SetNodeScores(targetHeuristic string,
	newScores map[NodeID]float64) (bool, error) {

	found := false
	for _, h := range c.heuristics {
		// It must be ScoreSettable to be available for external
		// scores.
		s, ok := h.AttachmentHeuristic.(ScoreSettable)
		if !ok {
			continue
		}

		// Heuristic supports scoring, attempt to set them.
		applied, err := s.SetNodeScores(targetHeuristic, newScores)
		if err != nil {
			return false, err
		}
		found = found || applied
	}

	return found, nil
}
