package autopilot

import (
	prand "math/rand"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
)

// ConstrainedPrefAttachment is an implementation of the AttachmentHeuristic
// interface that implement a constrained non-linear preferential attachment
// heuristic. This means that given a threshold to allocate to automatic
// channel establishment, the heuristic will attempt to favor connecting to
// nodes which already have a set amount of links, selected by sampling from a
// power law distribution. The attachment is non-linear in that it favors
// nodes with a higher in-degree but less so that regular linear preferential
// attachment. As a result, this creates smaller and less clusters than regular
// linear preferential attachment.
//
// TODO(roasbeef): BA, with k=-3
type ConstrainedPrefAttachment struct {
	constraints *HeuristicConstraints
}

// NewConstrainedPrefAttachment creates a new instance of a
// ConstrainedPrefAttachment heuristics given bounds on allowed channel sizes,
// and an allocation amount which is interpreted as a percentage of funds that
// is to be committed to channels at all times.
func NewConstrainedPrefAttachment(
	cfg *HeuristicConstraints) *ConstrainedPrefAttachment {

	prand.Seed(time.Now().Unix())

	return &ConstrainedPrefAttachment{
		constraints: cfg,
	}
}

// A compile time assertion to ensure ConstrainedPrefAttachment meets the
// AttachmentHeuristic interface.
var _ AttachmentHeuristic = (*ConstrainedPrefAttachment)(nil)

// NeedMoreChans is a predicate that should return true if, given the passed
// parameters, and its internal state, more channels should be opened within
// the channel graph. If the heuristic decides that we do indeed need more
// channels, then the second argument returned will represent the amount of
// additional funds to be used towards creating channels.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (p *ConstrainedPrefAttachment) NeedMoreChans(channels []Channel,
	funds btcutil.Amount) (btcutil.Amount, uint32, bool) {

	// We'll try to open more channels as long as the constraints allow it.
	availableFunds, availableChans := p.constraints.availableChans(
		channels, funds,
	)
	return availableFunds, availableChans, availableChans > 0
}

// NodeID is a simple type that holds an EC public key serialized in compressed
// format.
type NodeID [33]byte

// NewNodeID creates a new nodeID from a passed public key.
func NewNodeID(pub *btcec.PublicKey) NodeID {
	var n NodeID
	copy(n[:], pub.SerializeCompressed())
	return n
}

// NodeScores is a method that given the current channel graph, current set of
// local channels and funds available, scores the given nodes according the the
// preference of opening a channel with them.
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
func (p *ConstrainedPrefAttachment) NodeScores(g ChannelGraph, chans []Channel,
	fundsAvailable btcutil.Amount, nodes map[NodeID]struct{}) (
	map[NodeID]*AttachmentDirective, error) {

	// Count the number of channels in the graph. We'll also count the
	// number of channels as we go for the nodes we are interested in, and
	// record their addresses found in the db.
	var graphChans int
	nodeChanNum := make(map[NodeID]int)
	addresses := make(map[NodeID][]net.Addr)
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

		// Otherwise we'll record the number of channels, and also
		// populate the address in our channel candidates map.
		nodeChanNum[nID] = nodeChans
		addresses[nID] = n.Addrs()

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
	candidates := make(map[NodeID]*AttachmentDirective)
	for nID, nodeChans := range nodeChanNum {
		// As channel size we'll use the maximum channel size available.
		chanSize := p.constraints.MaxChanSize
		if fundsAvailable-chanSize < 0 {
			chanSize = fundsAvailable
		}

		_, ok := existingPeers[nID]
		addrs := addresses[nID]

		switch {

		// If the node is among or existing channel peers, we don't
		// need another channel.
		case ok:
			continue

		// If the amount is too small, we don't want to attempt opening
		// another channel.
		case chanSize == 0 || chanSize < p.constraints.MinChanSize:
			continue

		// If the node has no addresses, we cannot connect to it, so we
		// skip it for now, which implicitly gives it a score of 0.
		case len(addrs) == 0:
			continue

		// If the node had no channels, we skip it, since it would have
		// gotten a zero score anyway.
		case nodeChans == 0:
			continue
		}

		// Otherwise we score the node according to its fraction of
		// channels in the graph.
		score := float64(nodeChans) / float64(graphChans)
		candidates[nID] = &AttachmentDirective{
			NodeID:  nID,
			ChanAmt: chanSize,
			Addrs:   addrs,
			Score:   score,
		}
	}

	return candidates, nil
}
