package routing

import (
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// unifiedPolicies holds all unified policies for connections towards a node.
type unifiedPolicies struct {
	// policies contains a unified policy for every from node.
	policies map[route.Vertex]*unifiedPolicy

	// sourceNode is the sender of a payment. The rules to pick the final
	// policy are different for local channels.
	sourceNode route.Vertex

	// toNode is the node for which the unified policies are instantiated.
	toNode route.Vertex

	// outChanRestr is an optional outgoing channel restriction for the
	// local channel to use.
	outChanRestr map[uint64]struct{}
}

// newUnifiedPolicies instantiates a new unifiedPolicies object. Channel
// policies can be added to this object.
func newUnifiedPolicies(sourceNode, toNode route.Vertex,
	outChanRestr map[uint64]struct{}) *unifiedPolicies {

	return &unifiedPolicies{
		policies:     make(map[route.Vertex]*unifiedPolicy),
		toNode:       toNode,
		sourceNode:   sourceNode,
		outChanRestr: outChanRestr,
	}
}

// addPolicy adds a single channel policy. Capacity may be zero if unknown
// (light clients). Inbound fee is the fee that is charged by the node on the
// receiving end of the channel.
func (u *unifiedPolicies) addPolicy(fromNode route.Vertex,
	edge *channeldb.CachedEdgePolicy, inboundFee htlcswitch.InboundFee,
	capacity btcutil.Amount) {

	localChan := fromNode == u.sourceNode

	// Skip channels if there is an outgoing channel restriction.
	if localChan && u.outChanRestr != nil {
		if _, ok := u.outChanRestr[edge.ChannelID]; !ok {
			return
		}
	}

	// Update the policies map.
	policy, ok := u.policies[fromNode]
	if !ok {
		policy = &unifiedPolicy{
			localChan: localChan,
		}
		u.policies[fromNode] = policy
	}

	policy.edges = append(policy.edges, &unifiedPolicyEdge{
		policy:      edge,
		capacity:    capacity,
		inboundFees: inboundFee,
	})
}

// addGraphPolicies adds all policies that are known for the toNode in the
// graph.
func (u *unifiedPolicies) addGraphPolicies(g routingGraph) error {
	cb := func(channel *channeldb.DirectedChannel) error {
		// If there is no edge policy for this candidate node, skip.
		// Note that we are searching backwards so this node would have
		// come prior to the pivot node in the route.
		if channel.InPolicy == nil {
			return nil
		}

		// Add this policy to the unified policies map.
		inboundFee := htlcswitch.NewInboundFeeFromWire(
			channel.InboundFee,
		)

		u.addPolicy(
			channel.OtherNode, channel.InPolicy, inboundFee,
			channel.Capacity,
		)

		return nil
	}

	// Iterate over all channels of the to node.
	return g.forEachNodeChannel(u.toNode, cb)
}

// unifiedPolicyEdge is the individual channel data that is kept inside an
// unifiedPolicy object.
type unifiedPolicyEdge struct {
	policy      *channeldb.CachedEdgePolicy
	capacity    btcutil.Amount
	inboundFees htlcswitch.InboundFee
}

// ComputeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func (u *unifiedPolicyEdge) ComputeFee(
	amtToForward lnwire.MilliSatoshi) int64 {

	inboundFee := u.inboundFees.CalcFee(amtToForward)
	outboundFee := int64(
		u.policy.ComputeFee(
			amtToForward + lnwire.MilliSatoshi(inboundFee),
		),
	)

	return inboundFee + outboundFee
}

func (u *unifiedPolicyEdge) ComputeInboundFeeFromIncoming(
	htlcAmt lnwire.MilliSatoshi) int64 {

	return int64(htlcAmt) - divideCeil(
		1e6*(int64(htlcAmt)-int64(u.inboundFees.Base)),
		1e6+int64(u.inboundFees.Rate),
	)
}

// divideCeil divides dividend by factor and rounds the result up.
func divideCeil(dividend, factor int64) int64 {
	return (dividend + factor - 1) / factor
}

func calcFee(amt int64, feeBase, feeRate int32) int64 {
	return int64(feeBase) + amt*int64(feeRate)/1e6
}

// amtInRange checks whether an amount falls within the valid range for a
// channel.
func (u *unifiedPolicyEdge) amtInRange(amt lnwire.MilliSatoshi) bool {
	// If the capacity is available (non-light clients), skip channels that
	// are too small.
	if u.capacity > 0 &&
		amt > lnwire.NewMSatFromSatoshis(u.capacity) {

		return false
	}

	// Skip channels for which this htlc is too large.
	if u.policy.MessageFlags.HasMaxHtlc() &&
		amt > u.policy.MaxHTLC {

		return false
	}

	// Skip channels for which this htlc is too small.
	if amt < u.policy.MinHTLC {
		return false
	}

	return true
}

// unifiedPolicy is the unified policy that covers all channels between a pair
// of nodes.
type unifiedPolicy struct {
	edges     []*unifiedPolicyEdge
	localChan bool
}

// getPolicy returns the optimal policy to use for this connection given a
// specific amount to send. It differentiates between local and network
// channels.
func (u *unifiedPolicy) getPolicy(amt lnwire.MilliSatoshi,
	bandwidthHints bandwidthHints) *unifiedPolicyEdge {

	if u.localChan {
		return u.getPolicyLocal(amt, bandwidthHints)
	}

	return u.getPolicyNetwork(amt)
}

// getPolicyLocal returns the optimal policy to use for this local connection
// given a specific amount to send.
func (u *unifiedPolicy) getPolicyLocal(amt lnwire.MilliSatoshi,
	bandwidthHints bandwidthHints) *unifiedPolicyEdge {

	var (
		bestPolicy   *unifiedPolicyEdge
		maxBandwidth lnwire.MilliSatoshi
	)

	for _, edge := range u.edges {
		// Add inbound fee to the amount that is sent over the local
		// channel.
		amtWithInboundFee := amt +
			lnwire.MilliSatoshi(edge.inboundFees.CalcFee(amt))

		// Check valid amount range for the channel.
		if !edge.amtInRange(amtWithInboundFee) {
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
			bandwidth = lnwire.MaxMilliSatoshi
		}

		// Skip channels that can't carry the payment.
		if amtWithInboundFee > bandwidth {
			continue
		}

		// We pick the local channel with the highest available
		// bandwidth, to maximize the success probability. It
		// can be that the channel state changes between
		// querying the bandwidth hints and sending out the
		// htlc.
		if bandwidth < maxBandwidth {
			continue
		}
		maxBandwidth = bandwidth

		// Update best policy.
		bestPolicy = &unifiedPolicyEdge{
			policy:      edge.policy,
			inboundFees: edge.inboundFees,
		}
	}

	return bestPolicy
}

// getPolicyNetwork returns the optimal policy to use for this connection given
// a specific amount to send. The goal is to return a policy that maximizes the
// probability of a successful forward in a non-strict forwarding context.
func (u *unifiedPolicy) getPolicyNetwork(
	amt lnwire.MilliSatoshi) *unifiedPolicyEdge {

	var (
		bestPolicy  *unifiedPolicyEdge
		maxFee      int64 = math.MinInt64
		maxTimelock uint16
	)

	for _, edge := range u.edges {
		// Add inbound fee to the amount that is sent over the channel.
		amtWithInboundFee := amt +
			lnwire.MilliSatoshi(edge.inboundFees.CalcFee(amt))

		// Check valid amount range for the channel.
		if !edge.amtInRange(amtWithInboundFee) {
			continue
		}

		// For network channels, skip the disabled ones.
		edgeFlags := edge.policy.ChannelFlags
		isDisabled := edgeFlags&lnwire.ChanUpdateDisabled != 0
		if isDisabled {
			continue
		}

		// Track the maximum time lock of all channels that are
		// candidate for non-strict forwarding at the routing node.
		if edge.policy.TimeLockDelta > maxTimelock {
			maxTimelock = edge.policy.TimeLockDelta
		}

		// Use the policy that results in the highest fee for this
		// specific amount.
		fee := int64(edge.policy.ComputeFee(amtWithInboundFee))
		if fee < maxFee {
			continue
		}
		maxFee = fee

		bestPolicy = &unifiedPolicyEdge{
			policy:      edge.policy,
			inboundFees: edge.inboundFees,
		}
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
	modifiedPolicy := *bestPolicy
	modifiedPolicy.policy.TimeLockDelta = maxTimelock

	return &modifiedPolicy
}

// minAmt returns the minimum amount that can be forwarded on this connection.
func (u *unifiedPolicy) minAmt() lnwire.MilliSatoshi {
	min := lnwire.MaxMilliSatoshi
	for _, edge := range u.edges {
		edgeMin := edge.policy.MinHTLC

		// Calculate inbound fee that is charged for sending this
		// minimum amount.
		inboundFee := edge.inboundFees.CalcFee(edgeMin) // TODO: Fix buildroute
		if inboundFee < 0 {
			// If fee is negative, add the discount to the minimum
			// that can be sent to obtain what the node can
			// minimally receive.
			edgeMin += lnwire.MilliSatoshi(-inboundFee)
		}

		if edgeMin < min {
			min = edgeMin
		}
	}

	return min
}
