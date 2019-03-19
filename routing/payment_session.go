package routing

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// PaymentSession is used during SendPayment attempts to provide routes to
// attempt. It also defines methods to give the PaymentSession additional
// information learned during the previous attempts.
type PaymentSession interface {
	// RequestRoute returns the next route to attempt for routing the
	// specified HTLC payment to the target node.
	RequestRoute(payment *LightningPayment,
		height uint32, finalCltvDelta uint16) (*route.Route, error)

	// ReportVertexFailure reports to the PaymentSession that the passsed
	// vertex failed to route the previous payment attempt. The
	// PaymentSession will use this information to produce a better next
	// route.
	ReportVertexFailure(v route.Vertex)

	// ReportEdgeFailure reports to the PaymentSession that the passed edge
	// failed to route the previous payment attempt. A minimum penalization
	// amount is included to attenuate the failure. This is set to a
	// non-zero value for channel balance failures. The PaymentSession will
	// use this information to produce a better next route.
	ReportEdgeFailure(failedEdge edge, minPenalizeAmt lnwire.MilliSatoshi)

	// ReportEdgePolicyFailure reports to the PaymentSession that we
	// received a failure message that relates to a channel policy. For
	// these types of failures, the PaymentSession can decide whether to to
	// keep the edge included in the next attempted route. The
	// PaymentSession will use this information to produce a better next
	// route.
	ReportEdgePolicyFailure(failedEdge edge)
}

// paymentSession is used during an HTLC routings session to prune the local
// chain view in response to failures, and also report those failures back to
// MissionControl. The snapshot copied for this session will only ever grow,
// and will now be pruned after a decay like the main view within mission
// control. We do this as we want to avoid the case where we continually try a
// bad edge or route multiple times in a session. This can lead to an infinite
// loop if payment attempts take long enough. An additional set of edges can
// also be provided to assist in reaching the payment's destination.
type paymentSession struct {
	additionalEdges map[route.Vertex][]*channeldb.ChannelEdgePolicy

	bandwidthHints map[uint64]lnwire.MilliSatoshi

	// errFailedFeeChans is a map of the short channel IDs that were the
	// source of policy related routing failures during this payment attempt.
	// We'll use this map to prune out channels when the first error may not
	// require pruning, but any subsequent ones do.
	errFailedPolicyChans map[nodeChannel]struct{}

	mc *MissionControl

	preBuiltRoute      *route.Route
	preBuiltRouteTried bool

	pathFinder pathFinder
}

// A compile time assertion to ensure paymentSession meets the PaymentSession
// interface.
var _ PaymentSession = (*paymentSession)(nil)

// ReportVertexFailure adds a vertex to the graph prune view after a client
// reports a routing failure localized to the vertex. The time the vertex was
// added is noted, as it'll be pruned from the shared view after a period of
// vertexDecay. However, the vertex will remain pruned for the *local* session.
// This ensures we don't retry this vertex during the payment attempt.
//
// NOTE: Part of the PaymentSession interface.
func (p *paymentSession) ReportVertexFailure(v route.Vertex) {
	p.mc.reportVertexFailure(v)
}

// ReportEdgeFailure adds a channel to the graph prune view. The time the
// channel was added is noted, as it'll be pruned from the global view after a
// period of edgeDecay. However, the edge will remain pruned for the duration
// of the *local* session. This ensures that we don't flap by continually
// retrying an edge after its pruning has expired.
//
// TODO(roasbeef): also add value attempted to send and capacity of channel
//
// NOTE: Part of the PaymentSession interface.
func (p *paymentSession) ReportEdgeFailure(failedEdge edge,
	minPenalizeAmt lnwire.MilliSatoshi) {

	p.mc.reportEdgeFailure(failedEdge, minPenalizeAmt)
}

// ReportEdgePolicyFailure handles a failure message that relates to a
// channel policy. For these types of failures, the policy is updated and we
// want to keep it included during path finding. This function does mark the
// edge as 'policy failed once'. The next time it fails, the whole node will be
// pruned. This is to prevent nodes from keeping us busy by continuously sending
// new channel updates.
//
// NOTE: Part of the PaymentSession interface.
//
// TODO(joostjager): Move this logic into global mission control.
func (p *paymentSession) ReportEdgePolicyFailure(failedEdge edge) {
	key := nodeChannel{
		node:    failedEdge.from,
		channel: failedEdge.channel,
	}

	// Check to see if we've already reported a policy related failure for
	// this channel. If so, then we'll prune out the vertex.
	_, ok := p.errFailedPolicyChans[key]
	if ok {
		// TODO(joostjager): is this aggressive pruning still necessary?
		// Just pruning edges may also work unless there is a huge
		// number of failing channels from that node?
		p.ReportVertexFailure(key.node)

		return
	}

	// Finally, we'll record a policy failure from this node and move on.
	p.errFailedPolicyChans[key] = struct{}{}
}

// RequestRoute returns a route which is likely to be capable for successfully
// routing the specified HTLC payment to the target node. Initially the first
// set of paths returned from this method may encounter routing failure along
// the way, however as more payments are sent, mission control will start to
// build an up to date view of the network itself. With each payment a new area
// will be explored, which feeds into the recommendations made for routing.
//
// NOTE: This function is safe for concurrent access.
// NOTE: Part of the PaymentSession interface.
func (p *paymentSession) RequestRoute(payment *LightningPayment,
	height uint32, finalCltvDelta uint16) (*route.Route, error) {

	switch {

	// If we have a pre-built route, use that directly.
	case p.preBuiltRoute != nil && !p.preBuiltRouteTried:
		p.preBuiltRouteTried = true

		return p.preBuiltRoute, nil

	// If the pre-built route has been tried already, the payment session is
	// over.
	case p.preBuiltRoute != nil:
		return nil, fmt.Errorf("pre-built route already tried")
	}

	// If a route cltv limit was specified, we need to subtract the final
	// delta before passing it into path finding. The optimal path is
	// independent of the final cltv delta and the path finding algorithm is
	// unaware of this value.
	var cltvLimit *uint32
	if payment.CltvLimit != nil {
		limit := *payment.CltvLimit - uint32(finalCltvDelta)
		cltvLimit = &limit
	}

	// TODO(roasbeef): sync logic amongst dist sys

	// Taking into account this prune view, we'll attempt to locate a path
	// to our destination, respecting the recommendations from
	// MissionControl.
	path, err := p.pathFinder(
		&graphParams{
			graph:           p.mc.graph,
			additionalEdges: p.additionalEdges,
			bandwidthHints:  p.bandwidthHints,
		},
		&RestrictParams{
			ProbabilitySource:     p.mc.getEdgeProbability,
			FeeLimit:              payment.FeeLimit,
			OutgoingChannelID:     payment.OutgoingChannelID,
			CltvLimit:             cltvLimit,
			PaymentAttemptPenalty: DefaultPaymentAttemptPenalty,
			MinProbability:        DefaultMinProbability,
		},
		p.mc.selfNode.PubKeyBytes, payment.Target,
		payment.Amount,
	)
	if err != nil {
		return nil, err
	}

	// With the next candidate path found, we'll attempt to turn this into
	// a route by applying the time-lock and fee requirements.
	sourceVertex := route.Vertex(p.mc.selfNode.PubKeyBytes)
	route, err := newRoute(
		payment.Amount, sourceVertex, path, height, finalCltvDelta,
	)
	if err != nil {
		// TODO(roasbeef): return which edge/vertex didn't work
		// out
		return nil, err
	}

	return route, err
}

// nodeChannel is a combination of the node pubkey and one of its channels.
type nodeChannel struct {
	node    route.Vertex
	channel uint64
}
