package autopilot

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
)

// ExternalScoreAttachment is an implementation of the AttachmentHeuristic
// interface that allows an external source to provide it with node scores.
type ExternalScoreAttachment struct {
	// TODO(halseth): persist across restarts.
	nodeScores map[NodeID]float64

	sync.Mutex
}

// NewExternalScoreAttachment creates a new instance of an
// ExternalScoreAttachment.
func NewExternalScoreAttachment() *ExternalScoreAttachment {
	return &ExternalScoreAttachment{}
}

// A compile time assertion to ensure ExternalScoreAttachment meets the
// AttachmentHeuristic and ScoreSettable interfaces.
var _ AttachmentHeuristic = (*ExternalScoreAttachment)(nil)
var _ ScoreSettable = (*ExternalScoreAttachment)(nil)

// Name returns the name of this heuristic.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (s *ExternalScoreAttachment) Name() string {
	return "externalscore"
}

// SetNodeScores is used to set the internal map from NodeIDs to scores. The
// passed scores must be in the range [0, 1.0]. The fist parameter is the name
// of the targeted heuristic, to allow recursively target specific
// sub-heuristics. The returned boolean indicates whether the targeted
// heuristic was found.
//
// NOTE: This is a part of the ScoreSettable interface.
func (s *ExternalScoreAttachment) SetNodeScores(targetHeuristic string,
	newScores map[NodeID]float64) (bool, error) {

	// Return if this heuristic wasn't targeted.
	if targetHeuristic != s.Name() {
		return false, nil
	}

	// Since there's a requirement that all score are in the range [0,
	// 1.0], we validate them before setting the internal list.
	for nID, s := range newScores {
		if s < 0 || s > 1.0 {
			return false, fmt.Errorf("invalid score %v for "+
				"nodeID %v", s, nID)
		}
	}

	s.Lock()
	defer s.Unlock()

	s.nodeScores = newScores
	log.Tracef("Setting %v external scores", len(s.nodeScores))

	return true, nil
}

// NodeScores is a method that given the current channel graph and current set
// of local channels, scores the given nodes according to the preference of
// opening a channel of the given size with them. The returned channel
// candidates maps the NodeID to a NodeScore for the node.
//
// The returned scores will be in the range [0, 1.0], where 0 indicates no
// improvement in connectivity if a channel is opened to this node, while 1.0
// is the maximum possible improvement in connectivity.
//
// The scores are determined by checking the internal node scores list. Nodes
// not known will get a score of 0.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (s *ExternalScoreAttachment) NodeScores(_ context.Context, g ChannelGraph,
	chans []LocalChannel, chanSize btcutil.Amount,
	nodes map[NodeID]struct{}) (map[NodeID]*NodeScore, error) {

	existingPeers := make(map[NodeID]struct{})
	for _, c := range chans {
		existingPeers[c.Node] = struct{}{}
	}

	s.Lock()
	defer s.Unlock()

	log.Tracef("External scoring %v nodes, from %v set scores",
		len(nodes), len(s.nodeScores))

	// Fill the map of candidates to return.
	candidates := make(map[NodeID]*NodeScore)
	for nID := range nodes {
		var score float64
		if nodeScore, ok := s.nodeScores[nID]; ok {
			score = nodeScore
		}

		// If the node is among or existing channel peers, we don't
		// need another channel.
		if _, ok := existingPeers[nID]; ok {
			log.Tracef("Skipping existing peer %x from external "+
				"score results", nID[:])
			continue
		}

		log.Tracef("External score %v given to node %x", score, nID[:])

		// Instead of adding a node with score 0 to the returned set,
		// we just skip it.
		if score == 0 {
			continue
		}

		candidates[nID] = &NodeScore{
			NodeID: nID,
			Score:  score,
		}
	}

	return candidates, nil
}
