package routing

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// BlockPadding is used to increment the finalCltvDelta value for the last hop
// to prevent an HTLC being failed if some blocks are mined while it's in-flight.
const BlockPadding uint16 = 3

// PaymentSession is used during SendPayment attempts to provide routes to
// attempt. It also defines methods to give the PaymentSession additional
// information learned during the previous attempts.
type PaymentSession interface {
	// RequestRoute returns the next route to attempt for routing the
	// specified HTLC payment to the target node.
	RequestRoute(payment *LightningPayment,
		height uint32, finalCltvDelta uint16) (*route.Route, error)
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

	sessionSource *SessionSource

	preBuiltRoute      *route.Route
	preBuiltRouteTried bool

	pathFinder pathFinder
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

	// Add BlockPadding to the finalCltvDelta so that the receiving node
	// does not reject the HTLC if some blocks are mined while it's in-flight.
	finalCltvDelta += BlockPadding

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
	ss := p.sessionSource

	restrictions := &RestrictParams{
		ProbabilitySource: ss.MissionControl.GetEdgeProbability,
		FeeLimit:          payment.FeeLimit,
		OutgoingChannelID: payment.OutgoingChannelID,
		CltvLimit:         cltvLimit,
	}

	path, err := p.pathFinder(
		&graphParams{
			graph:           ss.Graph,
			additionalEdges: p.additionalEdges,
			bandwidthHints:  p.bandwidthHints,
		},
		restrictions, &ss.PathFindingConfig,
		ss.SelfNode.PubKeyBytes, payment.Target,
		payment.Amount,
	)
	if err != nil {
		return nil, err
	}

	// With the next candidate path found, we'll attempt to turn this into
	// a route by applying the time-lock and fee requirements.
	sourceVertex := route.Vertex(ss.SelfNode.PubKeyBytes)
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
