package routing

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Instantiate variables to allow taking a reference from the failure reason.
var (
	reasonError            = channeldb.FailureReasonError
	reasonIncorrectDetails = channeldb.FailureReasonPaymentDetails
)

// pairResult contains the result of the interpretation of a payment attempt for
// a specific node pair.
type pairResult struct {
	// amt is the amount that was forwarded for this pair. Can be set to
	// zero for failures that are amount independent.
	amt lnwire.MilliSatoshi

	// success indicates whether the payment attempt was successful through
	// this pair.
	success bool
}

// failPairResult creates a new result struct for a failure.
func failPairResult(minPenalizeAmt lnwire.MilliSatoshi) pairResult {
	return pairResult{
		amt: minPenalizeAmt,
	}
}

// newSuccessPairResult creates a new result struct for a success.
func successPairResult(successAmt lnwire.MilliSatoshi) pairResult {
	return pairResult{
		success: true,
		amt:     successAmt,
	}
}

// String returns the human-readable representation of a pair result.
func (p pairResult) String() string {
	var resultType string
	if p.success {
		resultType = "success"
	} else {
		resultType = "failed"
	}

	return fmt.Sprintf("%v (amt=%v)", resultType, p.amt)
}

// interpretedResult contains the result of the interpretation of a payment
// attempt.
type interpretedResult struct {
	// nodeFailure points to a node pubkey if all channels of that node are
	// responsible for the result.
	nodeFailure *route.Vertex

	// pairResults contains a map of node pairs for which we have a result.
	pairResults map[DirectedNodePair]pairResult

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
func interpretResult(rt *route.Route, success bool, failureSrcIdx *int,
	failure lnwire.FailureMessage) *interpretedResult {

	i := &interpretedResult{
		pairResults: make(map[DirectedNodePair]pairResult),
	}

	if success {
		i.processSuccess(rt)
	} else {
		i.processFail(rt, failureSrcIdx, failure)
	}
	return i
}

// processSuccess processes a successful payment attempt.
func (i *interpretedResult) processSuccess(route *route.Route) {
	// For successes, all nodes must have acted in the right way. Therefore
	// we mark all of them with a success result.
	i.successPairRange(route, 0, len(route.Hops)-1)
}

// processFail processes a failed payment attempt.
func (i *interpretedResult) processFail(
	rt *route.Route, errSourceIdx *int,
	failure lnwire.FailureMessage) {

	if errSourceIdx == nil {
		i.processPaymentOutcomeUnknown(rt)
		return
	}

	// If the payment was to a blinded route and we received an error from
	// after the introduction point, handle this error separately - there
	// has been a protocol violation from the introduction node. This
	// penalty applies regardless of the error code that is returned.
	introIdx, isBlinded := introductionPointIndex(rt)
	if isBlinded && introIdx < *errSourceIdx {
		i.processPaymentOutcomeBadIntro(rt, introIdx, *errSourceIdx)
		return
	}

	switch *errSourceIdx {

	// We are the source of the failure.
	case 0:
		i.processPaymentOutcomeSelf(rt, failure)

	// A failure from the final hop was received.
	case len(rt.Hops):
		i.processPaymentOutcomeFinal(
			rt, failure,
		)

	// An intermediate hop failed. Interpret the outcome, update reputation
	// and try again.
	default:
		i.processPaymentOutcomeIntermediate(
			rt, *errSourceIdx, failure,
		)
	}
}

// processPaymentOutcomeBadIntro handles the case where we have made payment
// to a blinded route, but received an error from a node after the introduction
// node. This indicates that the introduction node is not obeying the route
// blinding specification, as we expect all errors from the introduction node
// to be source from it.
func (i *interpretedResult) processPaymentOutcomeBadIntro(route *route.Route,
	introIdx, errSourceIdx int) {

	// We fail the introduction node for not obeying the specification.
	i.failNode(route, introIdx)

	// Other preceding channels in the route forwarded correctly. Note
	// that we do not assign success to the incoming link to the
	// introduction node because it has not handled the error correctly.
	if introIdx > 1 {
		i.successPairRange(route, 0, introIdx-2)
	}

	// If the source of the failure was from the final node, we also set
	// a final failure reason because the recipient can't process the
	// payment (independent of the introduction failing to convert the
	// error, we can't complete the payment if the last hop fails).
	if errSourceIdx == len(route.Hops) {
		i.finalFailureReason = &reasonError
	}
}

// processPaymentOutcomeSelf handles failures sent by ourselves.
func (i *interpretedResult) processPaymentOutcomeSelf(
	rt *route.Route, failure lnwire.FailureMessage) {

	switch failure.(type) {

	// We receive a malformed htlc failure from our peer. We trust ourselves
	// to send the correct htlc, so our peer must be at fault.
	case *lnwire.FailInvalidOnionVersion,
		*lnwire.FailInvalidOnionHmac,
		*lnwire.FailInvalidOnionKey:

		i.failNode(rt, 1)

		// If this was a payment to a direct peer, we can stop trying.
		if len(rt.Hops) == 1 {
			i.finalFailureReason = &reasonError
		}

	// Any other failure originating from ourselves should be temporary and
	// caused by changing conditions between path finding and execution of
	// the payment. We just retry and trust that the information locally
	// available in the link has been updated.
	default:
		log.Warnf("Routing failure for local channel %v occurred",
			rt.Hops[0].ChannelID)
	}
}

// processPaymentOutcomeFinal handles failures sent by the final hop.
func (i *interpretedResult) processPaymentOutcomeFinal(
	route *route.Route, failure lnwire.FailureMessage) {

	n := len(route.Hops)

	// If a failure from the final node is received, we will fail the
	// payment in almost all cases. Only when the penultimate node sends an
	// incorrect htlc, we want to retry via another route. Invalid onion
	// failures are not expected, because the final node wouldn't be able to
	// encrypt that failure.
	switch failure.(type) {

	// Expiry or amount of the HTLC doesn't match the onion, try another
	// route.
	case *lnwire.FailFinalIncorrectCltvExpiry,
		*lnwire.FailFinalIncorrectHtlcAmount:

		// We trust ourselves. If this is a direct payment, we penalize
		// the final node and fail the payment.
		if n == 1 {
			i.failNode(route, n)
			i.finalFailureReason = &reasonError

			return
		}

		// Otherwise penalize the last pair of the route and retry.
		// Either the final node is at fault, or it gets sent a bad htlc
		// from its predecessor.
		i.failPair(route, n-1)

		// The other hops relayed correctly, so assign those pairs a
		// success result. At this point, n >= 2.
		i.successPairRange(route, 0, n-2)

	// We are using wrong payment hash or amount, fail the payment.
	case *lnwire.FailIncorrectPaymentAmount,
		*lnwire.FailIncorrectDetails:

		// Assign all pairs a success result, as the payment reached the
		// destination correctly.
		i.successPairRange(route, 0, n-1)

		i.finalFailureReason = &reasonIncorrectDetails

	// The HTLC that was extended to the final hop expires too soon. Fail
	// the payment, because we may be using the wrong final cltv delta.
	case *lnwire.FailFinalExpiryTooSoon:
		// TODO(roasbeef): can happen to to race condition, try again
		// with recent block height

		// TODO(joostjager): can also happen because a node delayed
		// deliberately. What to penalize?
		i.finalFailureReason = &reasonIncorrectDetails

	case *lnwire.FailMPPTimeout:
		// Assign all pairs a success result, as the payment reached the
		// destination correctly. Continue the payment process.
		i.successPairRange(route, 0, n-1)

	default:
		// All other errors are considered terminal if coming from the
		// final hop. They indicate that something is wrong at the
		// recipient, so we do apply a penalty.
		i.failNode(route, n)

		// Other channels in the route forwarded correctly.
		if n >= 2 {
			i.successPairRange(route, 0, n-2)
		}

		i.finalFailureReason = &reasonError
	}
}

// processPaymentOutcomeIntermediate handles failures sent by an intermediate
// hop.
func (i *interpretedResult) processPaymentOutcomeIntermediate(
	route *route.Route, errorSourceIdx int,
	failure lnwire.FailureMessage) {

	reportOutgoing := func() {
		i.failPair(
			route, errorSourceIdx,
		)
	}

	reportOutgoingBalance := func() {
		i.failPairBalance(
			route, errorSourceIdx,
		)

		// All nodes up to the failing pair must have forwarded
		// successfully.
		i.successPairRange(route, 0, errorSourceIdx-1)
	}

	reportIncoming := func() {
		// We trust ourselves. If the error comes from the first hop, we
		// can penalize the whole node. In that case there is no
		// uncertainty as to which node to blame.
		if errorSourceIdx == 1 {
			i.failNode(route, errorSourceIdx)
			return
		}

		// Otherwise report the incoming pair.
		i.failPair(
			route, errorSourceIdx-1,
		)

		// All nodes up to the failing pair must have forwarded
		// successfully.
		if errorSourceIdx > 1 {
			i.successPairRange(route, 0, errorSourceIdx-2)
		}
	}

	reportNode := func() {
		// Fail only the node that reported the failure.
		i.failNode(route, errorSourceIdx)

		// Other preceding channels in the route forwarded correctly.
		if errorSourceIdx > 1 {
			i.successPairRange(route, 0, errorSourceIdx-2)
		}
	}

	reportAll := func() {
		// We trust ourselves. If the error comes from the first hop, we
		// can penalize the whole node. In that case there is no
		// uncertainty as to which node to blame.
		if errorSourceIdx == 1 {
			i.failNode(route, errorSourceIdx)
			return
		}

		// Otherwise penalize all pairs up to the error source. This
		// includes our own outgoing connection.
		i.failPairRange(
			route, 0, errorSourceIdx-1,
		)
	}

	switch failure.(type) {

	// If a node reports onion payload corruption or an invalid version,
	// that node may be responsible, but it could also be that it is just
	// relaying a malformed htlc failure from it successor. By reporting the
	// outgoing channel set, we will surely hit the responsible node. At
	// this point, it is not possible that the node's predecessor corrupted
	// the onion blob. If the predecessor would have corrupted the payload,
	// the error source wouldn't have been able to encrypt this failure
	// message for us.
	case *lnwire.FailInvalidOnionVersion,
		*lnwire.FailInvalidOnionHmac,
		*lnwire.FailInvalidOnionKey:

		reportOutgoing()

	// If InvalidOnionPayload is received, we penalize only the reporting
	// node. We know the preceding hop didn't corrupt the onion, since the
	// reporting node is able to send the failure. We assume that we
	// constructed a valid onion payload and that the failure is most likely
	// an unknown required type or a bug in their implementation.
	case *lnwire.InvalidOnionPayload:
		reportNode()

	// If the next hop in the route wasn't known or offline, we'll only
	// penalize the channel set which we attempted to route over. This is
	// conservative, and it can handle faulty channels between nodes
	// properly. Additionally, this guards against routing nodes returning
	// errors in order to attempt to black list another node.
	case *lnwire.FailUnknownNextPeer:
		reportOutgoing()

	// Some implementations use this error when the next hop is offline, so we
	// do the same as FailUnknownNextPeer and also process the channel update.
	case *lnwire.FailChannelDisabled:

		// Set the node pair for which a channel update may be out of
		// date. The second chance logic uses the policyFailure field.
		i.policyFailure = &DirectedNodePair{
			From: route.Hops[errorSourceIdx-1].PubKeyBytes,
			To:   route.Hops[errorSourceIdx].PubKeyBytes,
		}

		reportOutgoing()

		// All nodes up to the failing pair must have forwarded
		// successfully.
		i.successPairRange(route, 0, errorSourceIdx-1)

	// If we get a permanent channel, we'll prune the channel set in both
	// directions and continue with the rest of the routes.
	case *lnwire.FailPermanentChannelFailure:
		reportOutgoing()

	// When an HTLC parameter is incorrect, the node sending the error may
	// be doing something wrong. But it could also be that its predecessor
	// is intentionally modifying the htlc parameters that we instructed it
	// via the hop payload. Therefore we penalize the incoming node pair. A
	// third cause of this error may be that we have an out of date channel
	// update. This is handled by the second chance logic up in mission
	// control.
	case *lnwire.FailAmountBelowMinimum,
		*lnwire.FailFeeInsufficient,
		*lnwire.FailIncorrectCltvExpiry:

		// Set the node pair for which a channel update may be out of
		// date. The second chance logic uses the policyFailure field.
		i.policyFailure = &DirectedNodePair{
			From: route.Hops[errorSourceIdx-1].PubKeyBytes,
			To:   route.Hops[errorSourceIdx].PubKeyBytes,
		}

		// We report incoming channel. If a second pair is granted in
		// mission control, this report is ignored.
		reportIncoming()

	// If the outgoing channel doesn't have enough capacity, we penalize.
	// But we penalize only in a single direction and only for amounts
	// greater than the attempted amount.
	case *lnwire.FailTemporaryChannelFailure:
		reportOutgoingBalance()

	// If FailExpiryTooSoon is received, there must have been some delay
	// along the path. We can't know which node is causing the delay, so we
	// penalize all of them up to the error source.
	//
	// Alternatively it could also be that we ourselves have fallen behind
	// somehow. We ignore that case for now.
	case *lnwire.FailExpiryTooSoon:
		reportAll()

	// We only expect to get FailInvalidBlinding from an introduction node
	// in a blinded route. The introduction node in a blinded route is
	// always responsible for reporting errors for the blinded portion of
	// the route (to protect the privacy of the members of the route), so
	// we need to be careful not to unfairly "shoot the messenger".
	//
	// The introduction node has no incentive to falsely report errors to
	// sabotage the blinded route because:
	//   1. Its ability to route this payment is strictly tied to the
	//      blinded route.
	//   2. The pubkeys in the blinded route are ephemeral, so doing so
	//      will have no impact on the nodes beyond the individual payment.
	//
	// Here we handle a few cases where we could unexpectedly receive this
	// error:
	// 1. Outside of a blinded route: erring node is not spec compliant.
	// 2. Before the introduction point: erring node is not spec compliant.
	//
	// Note that we expect the case where this error is sent from a node
	// after the introduction node to be handled elsewhere as this is part
	// of a more general class of errors where the introduction node has
	// failed to convert errors for the blinded route.
	case *lnwire.FailInvalidBlinding:
		introIdx, isBlinded := introductionPointIndex(route)

		// Deal with cases where a node has incorrectly returned a
		// blinding error:
		// 1. A node before the introduction point returned it.
		// 2. A node in a non-blinded route returned it.
		if errorSourceIdx < introIdx || !isBlinded {
			reportNode()
			return
		}

		// Otherwise, the error was at the introduction node. All
		// nodes up until the introduction node forwarded correctly,
		// so we award them as successful.
		if introIdx >= 1 {
			i.successPairRange(route, 0, introIdx-1)
		}

		// If the hop after the introduction node that sent us an
		// error is the final recipient, then we finally fail the
		// payment because the receiver has generated a blinded route
		// that they're unable to use. We have this special case so
		// that we don't penalize the introduction node, and there is
		// no point in retrying the payment while LND only supports
		// one blinded route per payment.
		//
		// Note that if LND is extended to support multiple blinded
		// routes, this will terminate the payment without re-trying
		// the other routes.
		if errorSourceIdx == len(route.Hops)-1 {
			i.finalFailureReason = &reasonError
		} else {
			// If there are other hops between the recipient and
			// introduction node, then we just penalize the last
			// hop in the blinded route to minimize the storage of
			// results for ephemeral keys.
			i.failPairBalance(
				route, len(route.Hops)-1,
			)
		}

	// In all other cases, we penalize the reporting node. These are all
	// failures that should not happen.
	default:
		i.failNode(route, errorSourceIdx)
	}
}

// introductionPointIndex returns the index of an introduction point in a
// route, using the same indexing in the route that we use for errorSourceIdx
// (i.e., that we consider our own node to be at index zero). A boolean is
// returned to indicate whether the route contains a blinded portion at all.
func introductionPointIndex(route *route.Route) (int, bool) {
	for i, hop := range route.Hops {
		if hop.BlindingPoint != nil {
			return i + 1, true
		}
	}

	return 0, false
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

// failNode marks the node indicated by idx in the route as failed. It also
// marks the incoming and outgoing channels of the node as failed. This function
// intentionally panics when the self node is failed.
func (i *interpretedResult) failNode(rt *route.Route, idx int) {
	// Mark the node as failing.
	i.nodeFailure = &rt.Hops[idx-1].PubKeyBytes

	// Mark the incoming connection as failed for the node. We intent to
	// penalize as much as we can for a node level failure, including future
	// outgoing traffic for this connection. The pair as it is returned by
	// getPair is penalized in the original and the reversed direction. Note
	// that this will also affect the score of the failing node's peers.
	// This is necessary to prevent future routes from keep going into the
	// same node again.
	incomingChannelIdx := idx - 1
	inPair, _ := getPair(rt, incomingChannelIdx)
	i.pairResults[inPair] = failPairResult(0)
	i.pairResults[inPair.Reverse()] = failPairResult(0)

	// If not the ultimate node, mark the outgoing connection as failed for
	// the node.
	if idx < len(rt.Hops) {
		outgoingChannelIdx := idx
		outPair, _ := getPair(rt, outgoingChannelIdx)
		i.pairResults[outPair] = failPairResult(0)
		i.pairResults[outPair.Reverse()] = failPairResult(0)
	}
}

// failPairRange marks the node pairs from node fromIdx to node toIdx as failed
// in both direction.
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
	i.pairResults[pair] = failPairResult(0)
	i.pairResults[pair.Reverse()] = failPairResult(0)
}

// failPairBalance marks a pair as failed with a minimum penalization amount.
func (i *interpretedResult) failPairBalance(
	rt *route.Route, channelIdx int) {

	pair, amt := getPair(rt, channelIdx)

	i.pairResults[pair] = failPairResult(amt)
}

// successPairRange marks the node pairs from node fromIdx to node toIdx as
// succeeded.
func (i *interpretedResult) successPairRange(
	rt *route.Route, fromIdx, toIdx int) {

	for idx := fromIdx; idx <= toIdx; idx++ {
		pair, amt := getPair(rt, idx)

		i.pairResults[pair] = successPairResult(amt)
	}
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
