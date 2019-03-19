package routing

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// paymentSession is used during an HTLC routings session to prune the local
// chain view in response to failures, and also report those failures back to
// missionControl. The snapshot copied for this session will only ever grow,
// and will now be pruned after a decay like the main view within mission
// control. We do this as we want to avoid the case where we continually try a
// bad edge or route multiple times in a session. This can lead to an infinite
// loop if payment attempts take long enough. An additional set of edges can
// also be provided to assist in reaching the payment's destination.
type paymentSession struct {
	additionalEdges map[Vertex][]*channeldb.ChannelEdgePolicy

	bandwidthHints map[uint64]lnwire.MilliSatoshi

	// errFailedFeeChans is a map of the short channel IDs that were the
	// source of policy related routing failures during this payment attempt.
	// We'll use this map to prune out channels when the first error may not
	// require pruning, but any subsequent ones do.
	errFailedPolicyChans map[EdgeLocator]struct{}

	mc *missionControl

	haveRoutes     bool
	preBuiltRoutes []*Route

	pathFinder pathFinder
}

// ReportVertexFailure adds a vertex to the graph prune view after a client
// reports a routing failure localized to the vertex. The time the vertex was
// added is noted, as it'll be pruned from the shared view after a period of
// vertexDecay. However, the vertex will remain pruned for the *local* session.
// This ensures we don't retry this vertex during the payment attempt.
func (p *paymentSession) ReportVertexFailure(v Vertex) {
	p.mc.reportVertexFailure(v)
}

// ReportChannelFailure adds a channel to the graph prune view. The time the
// channel was added is noted, as it'll be pruned from the global view after a
// period of edgeDecay. However, the edge will remain pruned for the duration
// of the *local* session. This ensures that we don't flap by continually
// retrying an edge after its pruning has expired.
//
// TODO(roasbeef): also add value attempted to send and capacity of channel
func (p *paymentSession) ReportEdgeFailure(e *EdgeLocator) {
	p.mc.reportEdgeFailure(e)
}

// ReportChannelPolicyFailure handles a failure message that relates to a
// channel policy. For these types of failures, the policy is updated and we
// want to keep it included during path finding. This function does mark the
// edge as 'policy failed once'. The next time it fails, the whole node will be
// pruned. This is to prevent nodes from keeping us busy by continuously sending
// new channel updates.
func (p *paymentSession) ReportEdgePolicyFailure(
	errSource Vertex, failedEdge *EdgeLocator) {

	// Check to see if we've already reported a policy related failure for
	// this channel. If so, then we'll prune out the vertex.
	_, ok := p.errFailedPolicyChans[*failedEdge]
	if ok {
		// TODO(joostjager): is this aggresive pruning still necessary?
		// Just pruning edges may also work unless there is a huge
		// number of failing channels from that node?
		p.ReportVertexFailure(errSource)

		return
	}

	// Finally, we'll record a policy failure from this node and move on.
	p.errFailedPolicyChans[*failedEdge] = struct{}{}
}

// RequestRoute returns a route which is likely to be capable for successfully
// routing the specified HTLC payment to the target node. Initially the first
// set of paths returned from this method may encounter routing failure along
// the way, however as more payments are sent, mission control will start to
// build an up to date view of the network itself. With each payment a new area
// will be explored, which feeds into the recommendations made for routing.
//
// NOTE: This function is safe for concurrent access.
func (p *paymentSession) RequestRoute(payment *LightningPayment,
	height uint32, finalCltvDelta uint16) (*Route, error) {

	switch {
	// If we have a set of pre-built routes, then we'll just pop off the
	// next route from the queue, and use it directly.
	case p.haveRoutes && len(p.preBuiltRoutes) > 0:
		nextRoute := p.preBuiltRoutes[0]
		p.preBuiltRoutes[0] = nil // Set to nil to avoid GC leak.
		p.preBuiltRoutes = p.preBuiltRoutes[1:]

		return nextRoute, nil

	// If we were instantiated with a set of pre-built routes, and we've
	// run out, then we'll return a terminal error.
	case p.haveRoutes && len(p.preBuiltRoutes) == 0:
		return nil, fmt.Errorf("pre-built routes exhausted")
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
	// missionControl.
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
		},
		p.mc.selfNode.PubKeyBytes, payment.Target,
		payment.Amount,
	)
	if err != nil {
		return nil, err
	}

	// With the next candidate path found, we'll attempt to turn this into
	// a route by applying the time-lock and fee requirements.
	sourceVertex := Vertex(p.mc.selfNode.PubKeyBytes)
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
