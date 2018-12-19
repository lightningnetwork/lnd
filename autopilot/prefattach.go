package autopilot

import (
	prand "math/rand"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
)

// PrefAttachment is an implementation of the AttachmentHeuristic interface
// that implement a non-linear preferential attachment heuristic. This means
// that given a threshold to allocate to automatic channel establishment, the
// heuristic will attempt to favor connecting to nodes which already have a set
// amount of links, selected by sampling from a power law distribution. The
// attachment is non-linear in that it favors nodes with a higher in-degree but
// less so than regular linear preferential attachment. As a result, this
// creates smaller and less clusters than regular linear preferential
// attachment.
//
// TODO(roasbeef): BA, with k=-3
type PrefAttachment struct {
}

// NewPrefAttachment creates a new instance of a PrefAttachment heuristic.
func NewPrefAttachment() *PrefAttachment {
	prand.Seed(time.Now().Unix())
	return &PrefAttachment{}
}

// A compile time assertion to ensure PrefAttachment meets the
// AttachmentHeuristic interface.
var _ AttachmentHeuristic = (*PrefAttachment)(nil)

// NodeID is a simple type that holds an EC public key serialized in compressed
// format.
type NodeID [33]byte

// NewNodeID creates a new nodeID from a passed public key.
func NewNodeID(pub *btcec.PublicKey) NodeID {
	var n NodeID
	copy(n[:], pub.SerializeCompressed())
	return n
}

// NodeScores is a method that given the current channel graph and
// current set of local channels, scores the given nodes according to
// the preference of opening a channel of the given size with them.
//
// The heuristic employed by this method is one that attempts to promote a
// scale-free network globally, via local attachment preferences for new nodes
// joining the network with an amount of available funds to be allocated to
// channels. Specifically, we consider the degree of each node (and the flow
// in/out of the node available via its open channels) and utilize the
// Barabási–Albert model to drive our recommended attachment heuristics. If
// implemented globally for each new participant, this results in a channel
// graph that is scale-free and follows a power law distribution with k=-3.
//
// The returned scores will be in the range [0.0, 1.0], where higher scores are
// given to nodes already having high connectivity in the graph.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (p *PrefAttachment) NodeScores(g ChannelGraph, chans []Channel,
	chanSize btcutil.Amount, nodes map[NodeID]struct{}) (
	map[NodeID]*NodeScore, error) {

	// Count the number of channels in the graph. We'll also count the
	// number of channels as we go for the nodes we are interested in.
	var graphChans int
	nodeChanNum := make(map[NodeID]int)
	if err := g.ForEachNode(func(n Node) error {
		var nodeChans int
		err := n.ForEachChannel(func(_ ChannelEdge) error {
			nodeChans++
			graphChans++
			return nil
		})
		if err != nil {
			return err
		}

		// If this node is not among our nodes to score, we can return
		// early.
		nID := NodeID(n.PubKey())
		if _, ok := nodes[nID]; !ok {
			return nil
		}

		// Otherwise we'll record the number of channels.
		nodeChanNum[nID] = nodeChans

		return nil
	}); err != nil {
		return nil, err
	}

	// If there are no channels in the graph we cannot determine any
	// preferences, so we return, indicating all candidates get a score of
	// zero.
	if graphChans == 0 {
		return nil, nil
	}

	existingPeers := make(map[NodeID]struct{})
	for _, c := range chans {
		existingPeers[c.Node] = struct{}{}
	}

	// For each node in the set of nodes, count their fraction of channels
	// in the graph, and use that as the score.
	candidates := make(map[NodeID]*NodeScore)
	for nID, nodeChans := range nodeChanNum {

		_, ok := existingPeers[nID]

		switch {

		// If the node is among or existing channel peers, we don't
		// need another channel.
		case ok:
			continue

		// If the node had no channels, we skip it, since it would have
		// gotten a zero score anyway.
		case nodeChans == 0:
			continue
		}

		// Otherwise we score the node according to its fraction of
		// channels in the graph.
		score := float64(nodeChans) / float64(graphChans)
		candidates[nID] = &NodeScore{
			NodeID: nID,
			Score:  score,
		}
	}

	return candidates, nil
}
