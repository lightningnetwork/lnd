package autopilot

import (
	"bytes"
	"fmt"
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

// shuffleCandidates shuffles the set of candidate nodes for preferential
// attachment in order to break any ordering already enforced by the sorted
// order of the public key for each node. To shuffle the set of candidates, we
// use a version of the Fisher–Yates shuffle algorithm.
func shuffleCandidates(candidates []Node) []Node {
	shuffledNodes := make([]Node, len(candidates))
	perm := prand.Perm(len(candidates))

	for i, v := range perm {
		shuffledNodes[v] = candidates[i]
	}

	return shuffledNodes
}

// Select returns a candidate set of attachment directives that should be
// executed based on the current internal state, the state of the channel
// graph, the set of nodes we should exclude, and the amount of funds
// available. The heuristic employed by this method is one that attempts to
// promote a scale-free network globally, via local attachment preferences for
// new nodes joining the network with an amount of available funds to be
// allocated to channels. Specifically, we consider the degree of each node
// (and the flow in/out of the node available via its open channels) and
// utilize the Barabási–Albert model to drive our recommended attachment
// heuristics. If implemented globally for each new participant, this results
// in a channel graph that is scale-free and follows a power law distribution
// with k=-3.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (p *ConstrainedPrefAttachment) Select(self *btcec.PublicKey, g ChannelGraph,
	fundsAvailable btcutil.Amount, numNewChans uint32,
	skipNodes map[NodeID]struct{}) ([]AttachmentDirective, error) {

	// TODO(roasbeef): rename?

	var directives []AttachmentDirective

	if fundsAvailable < p.constraints.MinChanSize {
		return directives, nil
	}

	selfPubBytes := self.SerializeCompressed()

	// We'll continue our attachment loop until we've exhausted the current
	// amount of available funds.
	visited := make(map[NodeID]struct{})
	for i := uint32(0); i < numNewChans; i++ {
		// selectionSlice will be used to randomly select a node
		// according to a power law distribution. For each connected
		// edge, we'll add an instance of the node to this slice. Thus,
		// for a given node, the probability that we'll attach to it
		// is: k_i / sum(k_j), where k_i is the degree of the target
		// node, and k_j is the degree of all other nodes i != j. This
		// implements the classic Barabási–Albert model for
		// preferential attachment.
		var selectionSlice []Node

		// For each node, and each channel that the node has, we'll add
		// an instance of that node to the selection slice above.
		// This'll slice where the frequency of each node is equivalent
		// to the number of channels that connect to it.
		//
		// TODO(roasbeef): add noise to make adversarially resistant?
		if err := g.ForEachNode(func(node Node) error {
			nID := NodeID(node.PubKey())

			// Once a node has already been attached to, we'll
			// ensure that it isn't factored into any further
			// decisions within this round.
			if _, ok := visited[nID]; ok {
				return nil
			}

			// If we come across ourselves, them we'll continue in
			// order to avoid attempting to make a channel with
			// ourselves.
			if bytes.Equal(nID[:], selfPubBytes) {
				return nil
			}

			// Additionally, if this node is in the blacklist, then
			// we'll skip it.
			if _, ok := skipNodes[nID]; ok {
				return nil
			}

			// For initial bootstrap purposes, if a node doesn't
			// have any channels, then we'll ensure that it has at
			// least one item in the selection slice.
			//
			// TODO(roasbeef): make conditional?
			selectionSlice = append(selectionSlice, node)

			// For each active channel the node has, we'll add an
			// additional channel to the selection slice to
			// increase their weight.
			if err := node.ForEachChannel(func(channel ChannelEdge) error {
				selectionSlice = append(selectionSlice, node)
				return nil
			}); err != nil {
				return err
			}

			return nil
		}); err != nil {
			return nil, err
		}

		// If no nodes at all were accumulated, then we'll exit early
		// as there are no eligible candidates.
		if len(selectionSlice) == 0 {
			break
		}

		// Given our selection slice, we'll now generate a random index
		// into this slice. The node we select will be recommended by
		// us to create a channel to.
		candidates := shuffleCandidates(selectionSlice)
		selectedIndex := prand.Int31n(int32(len(candidates)))
		selectedNode := candidates[selectedIndex]

		// TODO(roasbeef): cap on num channels to same participant?

		// With the node selected, we'll add this (node, amount) tuple
		// to out set of recommended directives.
		pubBytes := selectedNode.PubKey()
		nID := NodeID(pubBytes)
		directives = append(directives, AttachmentDirective{
			NodeID: nID,
			Addrs:  selectedNode.Addrs(),
		})

		// With the node selected, we'll add it to the set of visited
		// nodes to avoid attaching to it again.
		visited[nID] = struct{}{}
	}

	numSelectedNodes := int64(len(directives))
	switch {
	// If we have enough available funds to distribute the maximum channel
	// size for each of the selected peers to attach to, then we'll
	// allocate the maximum amount to each peer.
	case int64(fundsAvailable) >= numSelectedNodes*int64(p.constraints.MaxChanSize):
		for i := 0; i < int(numSelectedNodes); i++ {
			directives[i].ChanAmt = p.constraints.MaxChanSize
		}

		return directives, nil

	// Otherwise, we'll greedily allocate our funds to the channels
	// successively until we run out of available funds, or can't create a
	// channel above the min channel size.
	case int64(fundsAvailable) < numSelectedNodes*int64(p.constraints.MaxChanSize):
		i := 0
		for fundsAvailable > p.constraints.MinChanSize {
			// We'll attempt to allocate the max channel size
			// initially. If we don't have enough funds to do this,
			// then we'll allocate the remainder of the funds
			// available to the channel.
			delta := p.constraints.MaxChanSize
			if fundsAvailable-delta < 0 {
				delta = fundsAvailable
			}

			directives[i].ChanAmt = delta

			fundsAvailable -= delta
			i++
		}

		// We'll slice the initial set of directives to properly
		// reflect the amount of funds we were able to allocate.
		return directives[:i:i], nil

	default:
		return nil, fmt.Errorf("err")
	}
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

		switch {

		// If the node is among or existing channel peers, we don't
		// need another channel.
		case ok:
			continue

		// If the amount is too small, we don't want to attempt opening
		// another channel.
		case chanSize == 0 || chanSize < p.constraints.MinChanSize:
			continue
		}

		// Otherwise we score the node according to its fraction of
		// channels in the graph.
		score := float64(nodeChans) / float64(graphChans)
		candidates[nID] = &AttachmentDirective{
			NodeID:  nID,
			ChanAmt: chanSize,
			Score:   score,
		}
	}

	return candidates, nil
}
