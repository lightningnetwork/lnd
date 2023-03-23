package routing

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// nodeEdgeUnifier holds all edge unifiers for connections towards a node.
type nodeEdgeUnifier struct {
	// edgeUnifiers contains an edge unifier for every from node.
	edgeUnifiers map[route.Vertex]*edgeUnifier

	// sourceNode is the sender of a payment. The rules to pick the final
	// policy are different for local channels.
	sourceNode route.Vertex

	// toNode is the node for which the edge unifiers are instantiated.
	toNode route.Vertex

	// outChanRestr is an optional outgoing channel restriction for the
	// local channel to use.
	outChanRestr map[uint64]struct{}
}

// newNodeEdgeUnifier instantiates a new nodeEdgeUnifier object. Channel
// policies can be added to this object.
func newNodeEdgeUnifier(sourceNode, toNode route.Vertex,
	outChanRestr map[uint64]struct{}) *nodeEdgeUnifier {

	return &nodeEdgeUnifier{
		edgeUnifiers: make(map[route.Vertex]*edgeUnifier),
		toNode:       toNode,
		sourceNode:   sourceNode,
		outChanRestr: outChanRestr,
	}
}

// addPolicy adds a single channel policy. Capacity may be zero if unknown
// (light clients).
func (u *nodeEdgeUnifier) addPolicy(fromNode route.Vertex,
	edge *channeldb.CachedEdgePolicy, capacity btcutil.Amount) {

	localChan := fromNode == u.sourceNode

	// Skip channels if there is an outgoing channel restriction.
	if localChan && u.outChanRestr != nil {
		if _, ok := u.outChanRestr[edge.ChannelID]; !ok {
			return
		}
	}

	// Update the edgeUnifiers map.
	unifier, ok := u.edgeUnifiers[fromNode]
	if !ok {
		unifier = &edgeUnifier{
			localChan: localChan,
		}
		u.edgeUnifiers[fromNode] = unifier
	}

	unifier.edges = append(unifier.edges, &unifiedEdge{
		policy:   edge,
		capacity: capacity,
	})
}

// addGraphPolicies adds all policies that are known for the toNode in the
// graph.
func (u *nodeEdgeUnifier) addGraphPolicies(g routingGraph) error {
	cb := func(channel *channeldb.DirectedChannel) error {
		// If there is no edge policy for this candidate node, skip.
		// Note that we are searching backwards so this node would have
		// come prior to the pivot node in the route.
		if channel.InPolicy == nil {
			return nil
		}

		// Add this policy to the corresponding edgeUnifier.
		u.addPolicy(
			channel.OtherNode, channel.InPolicy, channel.Capacity,
		)

		return nil
	}

	// Iterate over all channels of the to node.
	return g.forEachNodeChannel(u.toNode, cb)
}

// unifiedEdge is the individual channel data that is kept inside an edgeUnifier
// object.
type unifiedEdge struct {
	policy   *channeldb.CachedEdgePolicy
	capacity btcutil.Amount
}

// amtInRange checks whether an amount falls within the valid range for a
// channel.
func (u *unifiedEdge) amtInRange(amt lnwire.MilliSatoshi) bool {
	// If the capacity is available (non-light clients), skip channels that
	// are too small.
	if u.capacity > 0 &&
		amt > lnwire.NewMSatFromSatoshis(u.capacity) {

		log.Tracef("Not enough capacity: amt=%v, capacity=%v",
			amt, u.capacity)
		return false
	}

	// Skip channels for which this htlc is too large.
	if u.policy.MessageFlags.HasMaxHtlc() &&
		amt > u.policy.MaxHTLC {

		log.Tracef("Exceeds policy's MaxHTLC: amt=%v, MaxHTLC=%v",
			amt, u.policy.MaxHTLC)
		return false
	}

	// Skip channels for which this htlc is too small.
	if amt < u.policy.MinHTLC {
		log.Tracef("below policy's MinHTLC: amt=%v, MinHTLC=%v",
			amt, u.policy.MinHTLC)
		return false
	}

	return true
}

// edgeUnifier is an object that covers all channels between a pair of nodes.
type edgeUnifier struct {
	edges     []*unifiedEdge
	localChan bool
}

// getEdge returns the optimal unified edge to use for this connection given a
// specific amount to send. It differentiates between local and network
// channels.
func (u *edgeUnifier) getEdge(amt lnwire.MilliSatoshi,
	bandwidthHints bandwidthHints) *unifiedEdge {

	if u.localChan {
		return u.getEdgeLocal(amt, bandwidthHints)
	}

	return u.getEdgeNetwork(amt)
}

// getEdgeLocal returns the optimal unified edge to use for this local
// connection given a specific amount to send.
func (u *edgeUnifier) getEdgeLocal(amt lnwire.MilliSatoshi,
	bandwidthHints bandwidthHints) *unifiedEdge {

	var (
		bestEdge     *unifiedEdge
		maxBandwidth lnwire.MilliSatoshi
	)

	for _, edge := range u.edges {
		// Check valid amount range for the channel.
		if !edge.amtInRange(amt) {
			log.Debugf("Amount %v not in range for edge %v",
				amt, edge.policy.ChannelID)
			continue
		}

		// For local channels, there is no fee to pay or an extra time
		// lock. We only consider the currently available bandwidth for
		// channel selection. The disabled flag is ignored for local
		// channels.

		// Retrieve bandwidth for this local channel. If not
		// available, assume this channel has enough bandwidth.
		//
		// TODO(joostjager): Possibly change to skipping this
		// channel. The bandwidth hint is expected to be
		// available.
		bandwidth, ok := bandwidthHints.availableChanBandwidth(
			edge.policy.ChannelID, amt,
		)
		if !ok {
			log.Debugf("Cannot get bandwidth for edge %v, use max "+
				"instead", edge.policy.ChannelID)
			bandwidth = lnwire.MaxMilliSatoshi
		}

		// TODO(yy): if the above `!ok` is chosen, we'd have
		// `bandwidth` to be the max value, which will end up having
		// the `maxBandwidth` to be have the largest value and this
		// edge will be the chosen one. This is wrong in two ways,
		// 1. we need to understand why `availableChanBandwidth` cannot
		// find bandwidth for this edge as something is wrong with this
		// channel, and,
		// 2. this edge is likely NOT the local channel with the
		// highest available bandwidth.
		//
		// Skip channels that can't carry the payment.
		if amt > bandwidth {
			log.Debugf("Skipped edge %v: not enough bandwidth, "+
				"bandwidth=%v, amt=%v", edge.policy.ChannelID,
				bandwidth, amt)
			continue
		}

		// We pick the local channel with the highest available
		// bandwidth, to maximize the success probability. It
		// can be that the channel state changes between
		// querying the bandwidth hints and sending out the
		// htlc.
		if bandwidth < maxBandwidth {
			log.Debugf("Skipped edge %v: not max bandwidth, "+
				"bandwidth=%v, maxBandwidth=%v",
				bandwidth, maxBandwidth)
			continue
		}
		maxBandwidth = bandwidth

		// Update best edge.
		bestEdge = &unifiedEdge{
			policy:   edge.policy,
			capacity: edge.capacity,
		}
	}

	return bestEdge
}

// getEdgeNetwork returns the optimal unified edge to use for this connection
// given a specific amount to send. The goal is to return a unified edge with a
// policy that maximizes the probability of a successful forward in a non-strict
// forwarding context.
func (u *edgeUnifier) getEdgeNetwork(amt lnwire.MilliSatoshi) *unifiedEdge {
	var (
		bestPolicy  *channeldb.CachedEdgePolicy
		maxFee      lnwire.MilliSatoshi
		maxTimelock uint16
		maxCapMsat  lnwire.MilliSatoshi
	)

	for _, edge := range u.edges {
		// Check valid amount range for the channel.
		if !edge.amtInRange(amt) {
			log.Debugf("Amount %v not in range for edge %v",
				amt, edge.policy.ChannelID)
			continue
		}

		// For network channels, skip the disabled ones.
		edgeFlags := edge.policy.ChannelFlags
		isDisabled := edgeFlags&lnwire.ChanUpdateDisabled != 0
		if isDisabled {
			log.Debugf("Skipped edge %v due to it being disabled",
				edge.policy.ChannelID)
			continue
		}

		// Track the maximal capacity for usable channels. If we don't
		// know the capacity, we fall back to MaxHTLC.
		capMsat := lnwire.NewMSatFromSatoshis(edge.capacity)
		if capMsat == 0 && edge.policy.MessageFlags.HasMaxHtlc() {
			log.Tracef("No capacity available for channel %v, "+
				"using MaxHtlcMsat (%v) as a fallback.",
				edge.policy.ChannelID, edge.policy.MaxHTLC)

			capMsat = edge.policy.MaxHTLC
		}
		maxCapMsat = lntypes.Max(capMsat, maxCapMsat)

		// Track the maximum time lock of all channels that are
		// candidate for non-strict forwarding at the routing node.
		maxTimelock = lntypes.Max(
			maxTimelock, edge.policy.TimeLockDelta,
		)

		// Use the policy that results in the highest fee for this
		// specific amount.
		fee := edge.policy.ComputeFee(amt)
		if fee < maxFee {
			log.Debugf("Skipped edge %v due to it produces less "+
				"fee: fee=%v, maxFee=%v",
				edge.policy.ChannelID, fee, maxFee)
			continue
		}
		maxFee = fee

		bestPolicy = edge.policy
	}

	// Return early if no channel matches.
	if bestPolicy == nil {
		return nil
	}

	// We have already picked the highest fee that could be required for
	// non-strict forwarding. To also cover the case where a lower fee
	// channel requires a longer time lock, we modify the policy by setting
	// the maximum encountered time lock. Note that this results in a
	// synthetic policy that is not actually present on the routing node.
	//
	// The reason we do this, is that we try to maximize the chance that we
	// get forwarded. Because we penalize pair-wise, there won't be a second
	// chance for this node pair. But this is all only needed for nodes that
	// have distinct policies for channels to the same peer.
	policyCopy := *bestPolicy
	modifiedEdge := unifiedEdge{policy: &policyCopy}
	modifiedEdge.policy.TimeLockDelta = maxTimelock
	modifiedEdge.capacity = maxCapMsat.ToSatoshis()

	return &modifiedEdge
}

// minAmt returns the minimum amount that can be forwarded on this connection.
func (u *edgeUnifier) minAmt() lnwire.MilliSatoshi {
	min := lnwire.MaxMilliSatoshi
	for _, edge := range u.edges {
		min = lntypes.Min(min, edge.policy.MinHTLC)
	}

	return min
}
