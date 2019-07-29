package routing

import (
	"fmt"
	"strings"

	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

type pairResult struct {
	// minPenalizeAmt is the amount that was sent across the channel.
	minPenalizeAmt lnwire.MilliSatoshi

	// success indicates whether the payment attempt was successful through
	// this pair.
	success bool
}

func (p pairResult) String() string {
	if p.success {
		return "success"
	}

	return fmt.Sprintf("failed (minPenalizeAmt=%v)", p.minPenalizeAmt)
}

type interpretedResult struct {
	nodeFailures  map[route.Vertex]struct{}
	pairResults   map[DirectedNodePair]pairResult
	finalFailure  bool
	failureReason channeldb.FailureReason
	policyFailure *DirectedNodePair
}

func newInterpretedResult(rt *route.Route, success bool, failureSrcIdx *int,
	failure lnwire.FailureMessage) *interpretedResult {

	i := &interpretedResult{
		nodeFailures: make(map[route.Vertex]struct{}),
		pairResults:  make(map[DirectedNodePair]pairResult),
	}

	if success {
		i.processSuccess(rt)
	} else {
		i.processFail(rt, failureSrcIdx, failure)
	}
	return i
}

func (i *interpretedResult) processSuccess(route *route.Route) {
	i.successPairRange(route, 1, len(route.Hops)-1)
}

func (i *interpretedResult) processFail(
	route *route.Route, errSourceIdx *int,
	failure lnwire.FailureMessage) {

	if errSourceIdx == nil {
		i.processPaymentOutcomeUnknown(route)
		return
	}

	switch *errSourceIdx {

	// We don't keep a reputation for ourselves, as information about
	// channels should be available directly in links. Just retry with local
	// info that should now be updated.
	case 0:
		log.Warnf("Routing error for local channel %v occurred",
			route.Hops[0].ChannelID)

	// An error from the final hop was received.
	case len(route.Hops):
		i.processPaymentOutcomeFinal(
			route, failure,
		)

	// An intermediate hop failed. Interpret the outcome, update reputation
	// and try again.
	default:
		i.processPaymentOutcomeIntermediate(
			route, *errSourceIdx, failure,
		)
	}
}

func (i *interpretedResult) failNode(rt *route.Route, idx int) {
	i.nodeFailures[rt.Hops[idx].PubKeyBytes] = struct{}{}
}

func (i *interpretedResult) setFailure(final bool,
	reason channeldb.FailureReason) {

	i.finalFailure = final
	i.failureReason = reason
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
			i.failNode(route, n-1)
			i.setFailure(true, channeldb.FailureReasonError)

			return
		}

		// Otherwise penalize the last channel of the route and retry.
		i.failPair(route, n-1)

		// The other hops relayed corectly, so assign those pairs a
		// success result.
		if n > 2 {
			i.successPairRange(route, 1, n-2)
		}

	// We are using wrong payment hash or amount, fail the payment.
	case *lnwire.FailIncorrectPaymentAmount,
		*lnwire.FailUnknownPaymentHash:

		// Assign all pairs a success result, as the payment reached the
		// destination correctly.
		i.successPairRange(route, 1, n-1)

		i.setFailure(
			true, channeldb.FailureReasonIncorrectPaymentDetails,
		)

	// The HTLC that was extended to the final hop expires too soon. Fail
	// the payment, because we may be using the wrong final cltv delta.
	case *lnwire.FailFinalExpiryTooSoon:
		// TODO(roasbeef): can happen to to race condition, try again
		// with recent block height

		// TODO(joostjager): can also happen because a node delayed
		// deliberately. What to penalize?
		i.setFailure(
			true, channeldb.FailureReasonIncorrectPaymentDetails,
		)

	default:
		// All other errors are considered terminal if coming from the
		// final hop. They indicate that something is wrong at the
		// recipient, so we do apply a penalty.
		i.failNode(route, n-1)

		// Other channels in the route forwarded correctly.
		if n > 2 {
			i.successPairRange(route, 1, n-2)
		}

		i.setFailure(true, channeldb.FailureReasonError)
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

		if errorSourceIdx > 1 {
			i.successPairRange(route, 1, errorSourceIdx-1)
		}
	}

	reportIncoming := func() {
		// We trust ourselves. If the error comes from the first hop, we
		// can penalize the whole node. In that case there is no
		// uncertainty as to which node to blame.
		if errorSourceIdx == 1 {
			i.failNode(route, errorSourceIdx-1)
			return
		}

		// Otherwise report the incoming channel.
		i.failPair(
			route, errorSourceIdx-1,
		)

		if errorSourceIdx > 2 {
			i.successPairRange(route, 1, errorSourceIdx-2)
		}
	}

	reportAll := func() {
		// We trust ourselves. If the error comes from the first hop, we
		// can penalize the whole node. In that case there is no
		// uncertainty as to which node to blame.
		if errorSourceIdx == 1 {
			i.failNode(route, errorSourceIdx-1)
			return
		}

		// Otherwise report all channels up to the error source.
		i.failPairRange(
			route, 1, errorSourceIdx-1,
		)
	}

	switch failure.(type) {

	// If a hop reports onion payload corruption or an invalid version, we
	// will report the outgoing channel of that node. It may be either their
	// or the next node's fault.
	case *lnwire.FailInvalidOnionVersion,
		*lnwire.FailInvalidOnionHmac,
		*lnwire.FailInvalidOnionKey:

		reportOutgoing()

	// If the next hop in the route wasn't known or offline, we'll only
	// penalize the channel which we attempted to route over. This is
	// conservative, and it can handle faulty channels between nodes
	// properly. Additionally, this guards against routing nodes returning
	// errors in order to attempt to black list another node.
	case *lnwire.FailUnknownNextPeer:
		reportOutgoing()

	// If we get a permanent channel or node failure, then
	// we'll prune the channel in both directions and
	// continue with the rest of the routes.
	case *lnwire.FailPermanentChannelFailure:
		reportOutgoing()

	// If we get a failure due to violating the channel policy, we request a
	// second chance because our graph may be out of date. An attached
	// channel update should have been applied by now. If the second chance
	// is granted, we try again. Otherwise either the error source or its
	// predecessor sending an incorrect htlc is to blame.
	case *lnwire.FailAmountBelowMinimum,
		*lnwire.FailFeeInsufficient,
		*lnwire.FailIncorrectCltvExpiry,
		*lnwire.FailChannelDisabled:

		// Set the node pair that is responsible for this failure. The
		// second chance logic uses the policyFailure field.
		i.policyFailure = &DirectedNodePair{
			From: route.Hops[errorSourceIdx-1].PubKeyBytes,
			To:   route.Hops[errorSourceIdx].PubKeyBytes,
		}

		// Assuming no second chance, we report incoming channel.
		reportIncoming()

	// If the outgoing channel doesn't have enough capacity, we penalize.
	// But we penalize only in a single direction and only for amounts
	// greater than the attempted amount.
	case *lnwire.FailTemporaryChannelFailure:
		reportOutgoingBalance()

	// If FailExpiryTooSoon is received, there must have been some delay
	// along the path. We can't know which node is causing the delay, so we
	// penalize all of them up to the error source.
	case *lnwire.FailExpiryTooSoon:
		reportAll()

	// In all other cases, we report the whole node. These are all failures
	// that should not happen.
	default:
		i.failNode(route, errorSourceIdx-1)
	}
}

// processPaymentOutcomeUnknown processes a payment outcome for which no failure
// message or source is available.
func (i *interpretedResult) processPaymentOutcomeUnknown(route *route.Route) {
	n := len(route.Hops)

	// If this is a direct payment, the destination must be at fault.
	if n == 1 {
		i.failNode(route, n-1)
		i.setFailure(
			true, channeldb.FailureReasonError,
		)
		return
	}

	// Penalize all channels in the route to make sure the responsible node
	// is at least hit too. Start at one to not penalize our own channel.
	i.failPairRange(route, 1, n-1)
}

func (i *interpretedResult) failPairRange(
	rt *route.Route, fromIdx, toIdx int) {

	// Start at one because we don't penalize our own channels.
	for idx := fromIdx; idx <= toIdx; idx++ {
		i.failPair(rt, idx)
	}
}

// reportChannelFailure reports a bidirectional failure of a channel.
func (i *interpretedResult) failPair(
	rt *route.Route, channelIdx int) {

	pair, _ := getPair(rt, channelIdx)

	// Report pair in both directions without a minimum penalization amount.
	i.pairResults[pair] = pairResult{}
	i.pairResults[pair.Reverse()] = pairResult{}
}

func (i *interpretedResult) failPairBalance(
	rt *route.Route, channelIdx int) {

	pair, amt := getPair(rt, channelIdx)

	i.pairResults[pair] = pairResult{
		minPenalizeAmt: amt,
	}
}

func (i *interpretedResult) successPairRange(
	rt *route.Route, fromIdx, toIdx int) {

	for idx := fromIdx; idx <= toIdx; idx++ {
		pair, _ := getPair(rt, idx)

		i.pairResults[pair] = pairResult{
			success: true,
		}
	}
}

func (i interpretedResult) String() string {
	var b strings.Builder

	first := true

	for n := range i.nodeFailures {
		if !first {
			b.WriteString(",")
		} else {
			first = false
		}
		b.WriteString(n.String())
	}

	for p, r := range i.pairResults {
		if !first {
			b.WriteString(",")
		} else {
			first = false
		}
		b.WriteString(fmt.Sprintf(
			"(%x-%x,%v)", p.From[:6], p.To[:6], r,
		))
	}

	return b.String()
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
