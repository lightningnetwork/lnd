package routing

import (
	"math"

	"github.com/btcsuite/btcd/btcutil"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
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

	// useInboundFees indicates whether to take inbound fees into account.
	useInboundFees bool

	// outChanRestr is an optional outgoing channel restriction for the
	// local channel to use.
	outChanRestr map[uint64]struct{}
}

// newNodeEdgeUnifier instantiates a new nodeEdgeUnifier object. Channel
// policies can be added to this object.
func newNodeEdgeUnifier(sourceNode, toNode route.Vertex, useInboundFees bool,
	outChanRestr map[uint64]struct{}) *nodeEdgeUnifier {

	return &nodeEdgeUnifier{
		edgeUnifiers:   make(map[route.Vertex]*edgeUnifier),
		toNode:         toNode,
		useInboundFees: useInboundFees,
		sourceNode:     sourceNode,
		outChanRestr:   outChanRestr,
	}
}

// addPolicy adds a single channel policy. Capacity may be zero if unknown
// (light clients). We expect a non-nil payload size function and will request a
// graceful shutdown if it is not provided as this indicates that edges are
// incorrectly specified.
func (u *nodeEdgeUnifier) addPolicy(fromNode route.Vertex,
	edge *models.CachedEdgePolicy, inboundFee models.InboundFee,
	capacity btcutil.Amount, hopPayloadSizeFn PayloadSizeFunc,
	blindedPayment *BlindedPayment) {

	localChan := fromNode == u.sourceNode

	// Skip channels if there is an outgoing channel restriction.
	if localChan && u.outChanRestr != nil {
		if _, ok := u.outChanRestr[edge.ChannelID]; !ok {
			log.Debugf("Skipped adding policy for restricted edge "+
				"%v", edge.ChannelID)

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

	// In case no payload size function was provided a graceful shutdown
	// is requested, because this function is not used as intended.
	if hopPayloadSizeFn == nil {
		log.Criticalf("No payloadsize function was provided for the "+
			"edge (chanid=%v) when adding it to the edge unifier "+
			"of node: %v", edge.ChannelID, fromNode)

		return
	}

	// Zero inbound fee for exit hops.
	if !u.useInboundFees {
		inboundFee = models.InboundFee{}
	}

	unifier.edges = append(unifier.edges, newUnifiedEdge(
		edge, capacity, inboundFee, hopPayloadSizeFn, blindedPayment,
	))
}

// addGraphPolicies adds all policies that are known for the toNode in the
// graph.
func (u *nodeEdgeUnifier) addGraphPolicies(g Graph) error {
	var channels []*graphdb.DirectedChannel
	cb := func(channel *graphdb.DirectedChannel) error {
		// If there is no edge policy for this candidate node, skip.
		// Note that we are searching backwards so this node would have
		// come prior to the pivot node in the route.
		if channel.InPolicy == nil {
			log.Debugf("Skipped adding edge %v due to nil policy",
				channel.ChannelID)

			return nil
		}

		channels = append(channels, channel)

		return nil
	}

	// Iterate over all channels of the to node.
	err := g.ForEachNodeDirectedChannel(
		u.toNode, cb, func() {
			channels = nil
		},
	)
	if err != nil {
		return err
	}

	for _, channel := range channels {
		// Add this policy to the corresponding edgeUnifier. We default
		// to the clear hop payload size function because
		// `addGraphPolicies` is only used for cleartext intermediate
		// hops in a route.
		inboundFee := models.NewInboundFeeFromWire(
			channel.InboundFee,
		)

		u.addPolicy(
			channel.OtherNode, channel.InPolicy, inboundFee,
			channel.Capacity, defaultHopPayloadSize, nil,
		)
	}

	return nil
}

// unifiedEdge is the individual channel data that is kept inside an edgeUnifier
// object.
type unifiedEdge struct {
	policy      *models.CachedEdgePolicy
	capacity    btcutil.Amount
	inboundFees models.InboundFee

	// hopPayloadSize supplies an edge with the ability to calculate the
	// exact payload size if this edge would be included in a route. This
	// is needed because hops of a blinded path differ in their payload
	// structure compared to cleartext hops.
	hopPayloadSizeFn PayloadSizeFunc

	// blindedPayment if set, is the BlindedPayment that this edge was
	// derived from originally.
	blindedPayment *BlindedPayment
}

// newUnifiedEdge constructs a new unifiedEdge.
func newUnifiedEdge(policy *models.CachedEdgePolicy, capacity btcutil.Amount,
	inboundFees models.InboundFee, hopPayloadSizeFn PayloadSizeFunc,
	blindedPayment *BlindedPayment) *unifiedEdge {

	return &unifiedEdge{
		policy:           policy,
		capacity:         capacity,
		inboundFees:      inboundFees,
		hopPayloadSizeFn: hopPayloadSizeFn,
		blindedPayment:   blindedPayment,
	}
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
func (u *edgeUnifier) getEdge(netAmtReceived lnwire.MilliSatoshi,
	bandwidthHints bandwidthHints,
	nextOutFee lnwire.MilliSatoshi) *unifiedEdge {

	if u.localChan {
		return u.getEdgeLocal(
			netAmtReceived, bandwidthHints, nextOutFee,
		)
	}

	return u.getEdgeNetwork(netAmtReceived, nextOutFee)
}

// calcCappedInboundFee calculates the inbound fee for a channel, taking into
// account the total node fee for the "to" node.
func calcCappedInboundFee(edge *unifiedEdge, amt lnwire.MilliSatoshi,
	nextOutFee lnwire.MilliSatoshi) int64 {

	// Calculate the inbound fee charged for the amount that passes over the
	// channel.
	inboundFee := edge.inboundFees.CalcFee(amt)

	// Take into account that the total node fee cannot be negative.
	if inboundFee < -int64(nextOutFee) {
		inboundFee = -int64(nextOutFee)
	}

	return inboundFee
}

// getEdgeLocal returns the optimal unified edge to use for this local
// connection given a specific amount to send.
func (u *edgeUnifier) getEdgeLocal(netAmtReceived lnwire.MilliSatoshi,
	bandwidthHints bandwidthHints,
	nextOutFee lnwire.MilliSatoshi) *unifiedEdge {

	var (
		bestEdge     *unifiedEdge
		maxBandwidth lnwire.MilliSatoshi
	)

	for _, edge := range u.edges {
		// Calculate the inbound fee charged at the receiving node.
		inboundFee := calcCappedInboundFee(
			edge, netAmtReceived, nextOutFee,
		)

		// Add inbound fee to get to the amount that is sent over the
		// local channel.
		amt := netAmtReceived + lnwire.MilliSatoshi(inboundFee)
		// Check valid amount range for the channel. We skip this test

		// for payments with custom htlc data we skip the amount range
		// check because the amt of the payment does not relate to the
		// actual amount carried by the HTLC but instead is encoded in
		// the blob data.
		if !bandwidthHints.isCustomHTLCPayment() &&
			!edge.amtInRange(amt) {

			log.Debugf("Amount %v not in range for edge %v",
				netAmtReceived, edge.policy.ChannelID)

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
			log.Warnf("Cannot get bandwidth for edge %v, use max "+
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
		// bandwidth, to maximize the success probability. It can be
		// that the channel state changes between querying the bandwidth
		// hints and sending out the htlc.
		if bandwidth < maxBandwidth {
			log.Debugf("Skipped edge %v: not max bandwidth, "+
				"bandwidth=%v, maxBandwidth=%v",
				edge.policy.ChannelID, bandwidth, maxBandwidth)

			continue
		}
		maxBandwidth = bandwidth

		// Update best edge.
		bestEdge = newUnifiedEdge(
			edge.policy, edge.capacity, edge.inboundFees,
			edge.hopPayloadSizeFn, edge.blindedPayment,
		)
	}

	return bestEdge
}

// getEdgeNetwork returns the optimal unified edge to use for this connection
// given a specific amount to send. The goal is to return a unified edge with a
// policy that maximizes the probability of a successful forward in a non-strict
// forwarding context.
func (u *edgeUnifier) getEdgeNetwork(netAmtReceived lnwire.MilliSatoshi,
	nextOutFee lnwire.MilliSatoshi) *unifiedEdge {

	var (
		bestPolicy       *unifiedEdge
		maxFee           int64 = math.MinInt64
		maxTimelock      uint16
		maxCapMsat       lnwire.MilliSatoshi
		hopPayloadSizeFn PayloadSizeFunc
	)

	for _, edge := range u.edges {
		// Calculate the inbound fee charged at the receiving node.
		inboundFee := calcCappedInboundFee(
			edge, netAmtReceived, nextOutFee,
		)

		// Add inbound fee to get to the amount that is sent over the
		// channel.
		amt := netAmtReceived + lnwire.MilliSatoshi(inboundFee)

		// Check valid amount range for the channel.
		if !edge.amtInRange(amt) {
			log.Debugf("Amount %v not in range for edge %v",
				amt, edge.policy.ChannelID)
			continue
		}

		// For network channels, skip the disabled ones.
		if edge.policy.IsDisabled() {
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
		maxCapMsat = max(capMsat, maxCapMsat)

		// Track the maximum time lock of all channels that are
		// candidate for non-strict forwarding at the routing node.
		maxTimelock = max(maxTimelock, edge.policy.TimeLockDelta)

		outboundFee := int64(edge.policy.ComputeFee(amt))
		fee := outboundFee + inboundFee

		// Use the policy that results in the highest fee for this
		// specific amount.
		if fee < maxFee {
			log.Debugf("Skipped edge %v due to it produces less "+
				"fee: fee=%v, maxFee=%v",
				edge.policy.ChannelID, fee, maxFee)

			continue
		}
		maxFee = fee

		bestPolicy = newUnifiedEdge(
			edge.policy, 0, edge.inboundFees, nil,
			edge.blindedPayment,
		)

		// The payload size function for edges to a connected peer is
		// always the same hence there is not need to find the maximum.
		// This also counts for blinded edges where we only have one
		// edge to a blinded peer.
		hopPayloadSizeFn = edge.hopPayloadSizeFn
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
	policyCopy := *bestPolicy.policy
	policyCopy.TimeLockDelta = maxTimelock
	modifiedEdge := newUnifiedEdge(
		&policyCopy, maxCapMsat.ToSatoshis(), bestPolicy.inboundFees,
		hopPayloadSizeFn, bestPolicy.blindedPayment,
	)

	return modifiedEdge
}

// minAmt returns the minimum amount that can be forwarded on this connection.
func (u *edgeUnifier) minAmt() lnwire.MilliSatoshi {
	minAmount := lnwire.MaxMilliSatoshi
	for _, edge := range u.edges {
		minAmount = min(minAmount, edge.policy.MinHTLC)
	}

	return minAmount
}
