package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// BlockPadding is used to increment the finalCltvDelta value for the last hop
// to prevent an HTLC being failed if some blocks are mined while it's in-flight.
const BlockPadding uint16 = 3

// noRouteError encodes a non-critical error encountered during path finding.
type noRouteError uint8

const (
	// errNoTlvPayload is returned when the destination hop does not support
	// a tlv payload.
	errNoTlvPayload noRouteError = iota

	// errNoPaymentAddr is returned when the destination hop does not
	// support payment addresses.
	errNoPaymentAddr

	// errNoPathFound is returned when a path to the target destination does
	// not exist in the graph.
	errNoPathFound

	// errInsufficientLocalBalance is returned when none of the local
	// channels have enough balance for the payment.
	errInsufficientBalance

	// errEmptyPaySession is returned when the empty payment session is
	// queried for a route.
	errEmptyPaySession
)

// Error returns the string representation of the noRouteError
func (e noRouteError) Error() string {
	switch e {
	case errNoTlvPayload:
		return "destination hop doesn't understand new TLV payloads"

	case errNoPaymentAddr:
		return "destination hop doesn't understand payment addresses"

	case errNoPathFound:
		return "unable to find a path to destination"

	case errEmptyPaySession:
		return "empty payment session"

	case errInsufficientBalance:
		return "insufficient local balance"

	default:
		return "unknown no-route error"
	}
}

// FailureReason converts a path finding error into a payment-level failure.
func (e noRouteError) FailureReason() channeldb.FailureReason {
	switch e {
	case
		errNoTlvPayload,
		errNoPaymentAddr,
		errNoPathFound,
		errEmptyPaySession:

		return channeldb.FailureReasonNoRoute

	case errInsufficientBalance:
		return channeldb.FailureReasonInsufficientBalance

	default:
		return channeldb.FailureReasonError
	}
}

// PaymentSession is used during SendPayment attempts to provide routes to
// attempt. It also defines methods to give the PaymentSession additional
// information learned during the previous attempts.
type PaymentSession interface {
	// RequestRoute returns the next route to attempt for routing the
	// specified HTLC payment to the target node. The returned route should
	// carry at most maxAmt to the target node, and pay at most feeLimit in
	// fees. It can carry less if the payment is MPP. The activeShards
	// argument should be set to instruct the payment session about the
	// number of in flight HTLCS for the payment, such that it can choose
	// splitting strategy accordingly.
	//
	// A noRouteError is returned if a non-critical error is encountered
	// during path finding.
	RequestRoute(maxAmt, feeLimit lnwire.MilliSatoshi,
		activeShards, height uint32) (*route.Route, error)
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

	getBandwidthHints func() (map[uint64]lnwire.MilliSatoshi, error)

	sessionSource *SessionSource

	payment *LightningPayment

	empty bool

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
func (p *paymentSession) RequestRoute(maxAmt, feeLimit lnwire.MilliSatoshi,
	activeShards, height uint32) (*route.Route, error) {

	if p.empty {
		return nil, errEmptyPaySession
	}

	// Add BlockPadding to the finalCltvDelta so that the receiving node
	// does not reject the HTLC if some blocks are mined while it's in-flight.
	finalCltvDelta := p.payment.FinalCLTVDelta
	finalCltvDelta += BlockPadding

	// We need to subtract the final delta before passing it into path
	// finding. The optimal path is independent of the final cltv delta and
	// the path finding algorithm is unaware of this value.
	cltvLimit := p.payment.CltvLimit - uint32(finalCltvDelta)

	// TODO(roasbeef): sync logic amongst dist sys

	// Taking into account this prune view, we'll attempt to locate a path
	// to our destination, respecting the recommendations from
	// MissionControl.
	ss := p.sessionSource

	restrictions := &RestrictParams{
		ProbabilitySource: ss.MissionControl.GetProbability,
		FeeLimit:          feeLimit,
		OutgoingChannelID: p.payment.OutgoingChannelID,
		LastHop:           p.payment.LastHop,
		CltvLimit:         cltvLimit,
		DestCustomRecords: p.payment.DestCustomRecords,
		DestFeatures:      p.payment.DestFeatures,
		PaymentAddr:       p.payment.PaymentAddr,
	}

	// We'll also obtain a set of bandwidthHints from the lower layer for
	// each of our outbound channels. This will allow the path finding to
	// skip any links that aren't active or just don't have enough bandwidth
	// to carry the payment. New bandwidth hints are queried for every new
	// path finding attempt, because concurrent payments may change
	// balances.
	bandwidthHints, err := p.getBandwidthHints()
	if err != nil {
		return nil, err
	}

	finalHtlcExpiry := int32(height) + int32(finalCltvDelta)

	path, err := p.pathFinder(
		&graphParams{
			graph:           ss.Graph,
			additionalEdges: p.additionalEdges,
			bandwidthHints:  bandwidthHints,
		},
		restrictions, &ss.PathFindingConfig,
		ss.SelfNode.PubKeyBytes, p.payment.Target,
		maxAmt, finalHtlcExpiry,
	)
	if err != nil {
		return nil, err
	}

	// With the next candidate path found, we'll attempt to turn this into
	// a route by applying the time-lock and fee requirements.
	sourceVertex := route.Vertex(ss.SelfNode.PubKeyBytes)
	route, err := newRoute(
		sourceVertex, path, height,
		finalHopParams{
			amt:         maxAmt,
			cltvDelta:   finalCltvDelta,
			records:     p.payment.DestCustomRecords,
			paymentAddr: p.payment.PaymentAddr,
		},
	)
	if err != nil {
		// TODO(roasbeef): return which edge/vertex didn't work
		// out
		return nil, err
	}

	return route, err
}
