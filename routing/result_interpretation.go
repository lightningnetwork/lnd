package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Instantiate variables to allow taking a reference from the failure reason.
var (
	reasonError = channeldb.FailureReasonError
)

// interpretedResult contains the result of the interpretation of a payment
// outcome.
type interpretedResult struct {
	// nodeFailure points to a node pubkey if all channels of that node are
	// responsible for the result.
	nodeFailure *route.Vertex

	// pairResults contains a map of node pairs that could be responsible
	// for the failure. The map values are the minimum amounts for which a
	// future penalty should be applied.
	pairResults map[DirectedNodePair]lnwire.MilliSatoshi

	// finalFailureReason is set to a non-nil value if it makes no more
	// sense to start another payment attempt. It will contain the reason
	// why.
	finalFailureReason *channeldb.FailureReason

	// policyFailure is set to a node pair if there is a policy failure on
	// that connection. This is used to control the second chance logic for
	// policy failures.
	policyFailure *DirectedNodePair
}

// interpretResult interprets a payment outcome and returns an object that
// contains information required to update mission control.
func interpretResult(rt *route.Route, failureSrcIdx *int,
	failure lnwire.FailureMessage) *interpretedResult {

	i := &interpretedResult{
		pairResults: make(map[DirectedNodePair]lnwire.MilliSatoshi),
	}

	final, reason := i.processFail(rt, failureSrcIdx, failure)
	if final {
		i.finalFailureReason = &reason
	}

	return i
}

// processFail processes a failed payment attempt.
func (i *interpretedResult) processFail(
	rt *route.Route, errSourceIdx *int,
	failure lnwire.FailureMessage) (bool, channeldb.FailureReason) {

	if errSourceIdx == nil {
		i.processPaymentOutcomeUnknown(rt)
		return false, 0
	}

	var failureVertex route.Vertex

	failureSourceIdxInt := *errSourceIdx
	if failureSourceIdxInt > 0 {
		failureVertex = rt.Hops[failureSourceIdxInt-1].PubKeyBytes
	} else {
		failureVertex = rt.SourcePubKey
	}
	log.Tracef("Node %x (index %v) reported failure when sending htlc",
		failureVertex, errSourceIdx)

	// Always determine chan id ourselves, because a channel update with id
	// may not be available.
	failedPair, failedAmt := getFailedPair(
		rt, failureSourceIdxInt,
	)

	switch failure.(type) {

	// If the end destination didn't know the payment hash or we sent the
	// wrong payment amount to the destination, then we'll terminate
	// immediately.
	case *lnwire.FailIncorrectDetails:
		// TODO(joostjager): Check onionErr.Amount() whether it matches
		// what we expect. (Will it ever not match, because if not
		// final_incorrect_htlc_amount would be returned?)

		return true, channeldb.FailureReasonIncorrectPaymentDetails

	// If we sent the wrong amount to the destination, then we'll exit
	// early.
	case *lnwire.FailIncorrectPaymentAmount:
		return true, channeldb.FailureReasonIncorrectPaymentDetails

	// If the time-lock that was extended to the final node was incorrect,
	// then we can't proceed.
	case *lnwire.FailFinalIncorrectCltvExpiry:
		// TODO(joostjager): Take into account that second last hop may
		// have deliberately handed out an htlc that expires too soon.
		// In that case we should continue routing.
		return true, channeldb.FailureReasonError

	// If we crafted an invalid onion payload for the final node, then we'll
	// exit early.
	case *lnwire.FailFinalIncorrectHtlcAmount:
		// TODO(joostjager): Take into account that second last hop may
		// have deliberately handed out an htlc with a too low value. In
		// that case we should continue routing.
		return true, channeldb.FailureReasonError

	// Similarly, if the HTLC expiry that we extended to the final hop
	// expires too soon, then will fail the payment.
	//
	// TODO(roasbeef): can happen to to race condition, try again with
	// recent block height
	case *lnwire.FailFinalExpiryTooSoon:
		// TODO(joostjager): Take into account that any hop may have
		// delayed. Ideally we should continue routing. Knowing the
		// delaying node at this point would help.
		return true, channeldb.FailureReasonIncorrectPaymentDetails

	// If we erroneously attempted to cross a chain border, then we'll
	// cancel the payment.
	case *lnwire.FailInvalidRealm:
		return true, channeldb.FailureReasonError

	// If we get a notice that the expiry was too soon for an intermediate
	// node, then we'll prune out the node that sent us this error, as it
	// doesn't now what the correct block height is.
	case *lnwire.FailExpiryTooSoon:
		i.nodeFailure = &failureVertex
		return false, 0

	// If we hit an instance of onion payload corruption or an invalid
	// version, then we'll exit early as this shouldn't happen in the
	// typical case.
	//
	// TODO(joostjager): Take into account that the previous hop may have
	// tampered with the onion. Routing should continue using other paths.
	case *lnwire.FailInvalidOnionVersion:
		return true, channeldb.FailureReasonError
	case *lnwire.FailInvalidOnionHmac:
		return true, channeldb.FailureReasonError
	case *lnwire.FailInvalidOnionKey:
		return true, channeldb.FailureReasonError

	// If we get a failure due to violating the minimum amount, we'll apply
	// the new minimum amount and retry routing.
	case *lnwire.FailAmountBelowMinimum:
		i.policyFailure = &failedPair
		i.pairResults[failedPair] = 0
		return false, 0

	// If we get a failure due to a fee, we'll apply the new fee update, and
	// retry our attempt using the newly updated fees.
	case *lnwire.FailFeeInsufficient:
		i.policyFailure = &failedPair
		i.pairResults[failedPair] = 0
		return false, 0

	// If we get the failure for an intermediate node that disagrees with
	// our time lock values, then we'll apply the new delta value and try it
	// once more.
	case *lnwire.FailIncorrectCltvExpiry:
		i.policyFailure = &failedPair
		i.pairResults[failedPair] = 0
		return false, 0

	// The outgoing channel that this node was meant to forward one is
	// currently disabled, so we'll apply the update and continue.
	case *lnwire.FailChannelDisabled:
		i.pairResults[failedPair] = 0
		return false, 0

	// It's likely that the outgoing channel didn't have sufficient
	// capacity, so we'll prune this edge for now, and continue onwards with
	// our path finding.
	case *lnwire.FailTemporaryChannelFailure:
		i.pairResults[failedPair] = failedAmt
		return false, 0

	// If the send fail due to a node not having the required features, then
	// we'll note this error and continue.
	case *lnwire.FailRequiredNodeFeatureMissing:
		i.nodeFailure = &failureVertex
		return false, 0

	// If the send fail due to a node not having the required features, then
	// we'll note this error and continue.
	case *lnwire.FailRequiredChannelFeatureMissing:
		i.nodeFailure = &failureVertex
		return false, 0

	// If the next hop in the route wasn't known or offline, we'll only the
	// channel which we attempted to route over. This is conservative, and
	// it can handle faulty channels between nodes properly. Additionally,
	// this guards against routing nodes returning errors in order to
	// attempt to black list another node.
	case *lnwire.FailUnknownNextPeer:
		i.pairResults[failedPair] = 0
		return false, 0

	// If the node wasn't able to forward for which ever reason, then we'll
	// note this and continue with the routes.
	case *lnwire.FailTemporaryNodeFailure:
		i.nodeFailure = &failureVertex
		return false, 0

	case *lnwire.FailPermanentNodeFailure:
		i.nodeFailure = &failureVertex
		return false, 0

	// If we crafted a route that contains a too long time lock for an
	// intermediate node, we'll prune the node. As there currently is no way
	// of knowing that node's maximum acceptable cltv, we cannot take this
	// constraint into account during routing.
	//
	// TODO(joostjager): Record the rejected cltv and use that as a hint
	// during future path finding through that node.
	case *lnwire.FailExpiryTooFar:
		i.nodeFailure = &failureVertex
		return false, 0

	// If we get a permanent channel or node failure, then we'll prune the
	// channel in both directions and continue with the rest of the routes.
	case *lnwire.FailPermanentChannelFailure:
		i.pairResults[failedPair] = 0
		i.pairResults[failedPair.Reverse()] = 0
		return false, 0

	// Any other failure or an empty failure will get the node pruned.
	default:
		i.nodeFailure = &failureVertex
		return false, 0
	}
}

// processPaymentOutcomeUnknown processes a payment outcome for which no failure
// message or source is available.
func (i *interpretedResult) processPaymentOutcomeUnknown(route *route.Route) {
	n := len(route.Hops)

	// If this is a direct payment, the destination must be at fault.
	if n == 1 {
		i.failNode(route, n)
		i.finalFailureReason = &reasonError
		return
	}

	// Otherwise penalize all channels in the route to make sure the
	// responsible node is at least hit too. We even penalize the connection
	// to our own peer, because that peer could also be responsible.
	i.failPairRange(route, 0, n-1)
}

// failNode marks the node indicated by idx in the route as failed. This
// function intentionally panics when the self node is failed.
func (i *interpretedResult) failNode(rt *route.Route, idx int) {
	i.nodeFailure = &rt.Hops[idx-1].PubKeyBytes
}

// failPairRange marks the node pairs from node fromIdx to node toIdx as failed.
func (i *interpretedResult) failPairRange(
	rt *route.Route, fromIdx, toIdx int) {

	for idx := fromIdx; idx <= toIdx; idx++ {
		i.failPair(rt, idx)
	}
}

// failPair marks a pair as failed in both directions.
func (i *interpretedResult) failPair(
	rt *route.Route, idx int) {

	pair, _ := getPair(rt, idx)

	// Report pair in both directions without a minimum penalization amount.
	i.pairResults[pair] = 0
	i.pairResults[pair.Reverse()] = 0
}

// getPair returns a node pair from the route and the amount passed between that
// pair.
func getPair(rt *route.Route, channelIdx int) (DirectedNodePair,
	lnwire.MilliSatoshi) {

	nodeTo := rt.Hops[channelIdx].PubKeyBytes
	var (
		nodeFrom route.Vertex
		amt      lnwire.MilliSatoshi
	)

	if channelIdx == 0 {
		nodeFrom = rt.SourcePubKey
		amt = rt.TotalAmount
	} else {
		nodeFrom = rt.Hops[channelIdx-1].PubKeyBytes
		amt = rt.Hops[channelIdx-1].AmtToForward
	}

	pair := NewDirectedNodePair(nodeFrom, nodeTo)

	return pair, amt
}

// getFailedPair tries to locate the failing pair given a route and the pubkey
// of the node that sent the failure. It will assume that the failure is
// associated with the outgoing channel set of the failing node. As a second
// result, it returns the amount sent between the pair.
func getFailedPair(route *route.Route, failureSource int) (DirectedNodePair,
	lnwire.MilliSatoshi) {

	// Determine if we have a failure from the final hop. If it is, we
	// assume that the failing channel is the incoming channel.
	//
	// TODO(joostjager): In this case, certain types of failures are not
	// expected. For example FailUnknownNextPeer. This could be a reason to
	// prune the node?
	if failureSource == len(route.Hops) {
		failureSource--
	}

	// As this failure indicates that the target channel was unable to carry
	// this HTLC (for w/e reason), we'll return the _outgoing_ channel that
	// the source of the failure was meant to pass the HTLC along to.
	if failureSource == 0 {
		return NewDirectedNodePair(
			route.SourcePubKey,
			route.Hops[0].PubKeyBytes,
		), route.TotalAmount
	}

	return NewDirectedNodePair(
		route.Hops[failureSource-1].PubKeyBytes,
		route.Hops[failureSource].PubKeyBytes,
	), route.Hops[failureSource-1].AmtToForward
}
