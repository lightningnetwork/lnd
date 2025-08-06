package autopilot

import (
	"context"
	prand "math/rand"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
)

// minMedianChanSizeFraction determines the minimum size a channel must have to
// count positively when calculating the scores using preferential attachment.
// The minimum channel size is calculated as median/minMedianChanSizeFraction,
// where median is the median channel size of the entire graph.
const minMedianChanSizeFraction = 4

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

// Name returns the name of this heuristic.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (p *PrefAttachment) Name() string {
	return "preferential"
}

// NodeScores is a method that given the current channel graph and current set
// of local channels, scores the given nodes according to the preference of
// opening a channel of the given size with them. The returned channel
// candidates maps the NodeID to a NodeScore for the node.
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
// To avoid assigning a high score to nodes with a large number of small
// channels, we only count channels at least as large as a given fraction of
// the graph's median channel size.
//
// The returned scores will be in the range [0.0, 1.0], where higher scores are
// given to nodes already having high connectivity in the graph.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (p *PrefAttachment) NodeScores(ctx context.Context, g ChannelGraph,
	chans []LocalChannel, chanSize btcutil.Amount,
	nodes map[NodeID]struct{}) (map[NodeID]*NodeScore, error) {

	// We first run though the graph once in order to find the median
	// channel size.
	var (
		allChans  []btcutil.Amount
		seenChans = make(map[uint64]struct{})
	)
	err := g.ForEachNodesChannels(
		ctx, func(_ context.Context, node Node,
			channels []*ChannelEdge) error {

			for _, e := range channels {
				if _, ok := seenChans[e.ChanID.ToUint64()]; ok {
					continue
				}
				seenChans[e.ChanID.ToUint64()] = struct{}{}
				allChans = append(allChans, e.Capacity)
			}

			return nil
		},
		func() {
			allChans = nil
			clear(seenChans)
		},
	)
	if err != nil {
		return nil, err
	}

	medianChanSize := Median(allChans)
	log.Tracef("Found channel median %v for preferential score heuristic",
		medianChanSize)

	// Count the number of large-ish channels for each particular node in
	// the graph.
	var maxChans int
	nodeChanNum := make(map[NodeID]int)
	err = g.ForEachNodesChannels(
		ctx, func(ctx context.Context, node Node,
			edges []*ChannelEdge) error {

			var nodeChans int
			for _, e := range edges {
				// Since connecting to nodes with a lot of small
				// channels actually worsens our connectivity in
				// the graph (we will potentially waste time
				// trying to use these useless channels in path
				// finding), we decrease the counter for such
				// channels.
				//
				//nolint:ll
				if e.Capacity <
					medianChanSize/minMedianChanSizeFraction {

					nodeChans--

					continue
				}

				// Larger channels we count.
				nodeChans++
			}

			// We keep track of the highest-degree node we've seen,
			// as this will be given the max score.
			if nodeChans > maxChans {
				maxChans = nodeChans
			}

			// If this node is not among our nodes to score, we can
			// return early.
			nID := NodeID(node.PubKey())
			if _, ok := nodes[nID]; !ok {
				log.Tracef("Node %x not among nodes to score, "+
					"ignoring", nID[:])
				return nil
			}

			// Otherwise we'll record the number of channels.
			nodeChanNum[nID] = nodeChans
			log.Tracef("Counted %v channels for node %x", nodeChans,
				nID[:])

			return nil
		}, func() {
			maxChans = 0
			clear(nodeChanNum)
		},
	)
	if err != nil {
		return nil, err
	}

	// If there are no channels in the graph we cannot determine any
	// preferences, so we return, indicating all candidates get a score of
	// zero.
	if maxChans == 0 {
		log.Tracef("No channels in the graph")
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

		// If the node is among or existing channel peers, we don't
		// need another channel.
		if _, ok := existingPeers[nID]; ok {
			log.Tracef("Node %x among existing peers for pref "+
				"attach heuristic, giving zero score", nID[:])
			continue
		}

		// If the node had no large channels, we skip it, since it
		// would have gotten a zero score anyway.
		if nodeChans <= 0 {
			log.Tracef("Skipping node %x with channel count %v",
				nID[:], nodeChans)
			continue
		}

		// Otherwise we score the node according to its fraction of
		// channels in the graph, scaled such that the highest-degree
		// node will be given a score of 1.0.
		score := float64(nodeChans) / float64(maxChans)
		log.Tracef("Giving node %x a pref attach score of %v",
			nID[:], score)

		candidates[nID] = &NodeScore{
			NodeID: nID,
			Score:  score,
		}
	}

	return candidates, nil
}
