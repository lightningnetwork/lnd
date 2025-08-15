package routing

import (
	"bytes"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

// Instantiate variables to allow taking a reference from the failure reason.
var (
	reasonError            = paymentsdb.FailureReasonError
	reasonIncorrectDetails = paymentsdb.FailureReasonPaymentDetails
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
	finalFailureReason *paymentsdb.FailureReason

	// policyFailure is set to a node pair if there is a policy failure on
	// that connection. This is used to control the second chance logic for
	// policy failures.
	policyFailure *DirectedNodePair
}

// interpretResult interprets a payment outcome and returns an object that
// contains information required to update mission control.
func interpretResult(rt *mcRoute,
	failure fn.Option[paymentFailure]) *interpretedResult {

	i := &interpretedResult{
		pairResults: make(map[DirectedNodePair]pairResult),
	}

	return fn.ElimOption(failure, func() *interpretedResult {
		i.processSuccess(rt)

		return i
	}, func(info paymentFailure) *interpretedResult {
		i.processFail(rt, info)

		return i
	})
}

// processSuccess processes a successful payment attempt.
func (i *interpretedResult) processSuccess(route *mcRoute) {
	// For successes, all nodes must have acted in the right way.
	// Therefore we mark all of them with a success result. However we need
	// to handle the blinded route part separately because for intermediate
	// blinded nodes the amount field is set to zero so we use the receiver
	// amount.
	introIdx, isBlinded := introductionPointIndex(route)
	if isBlinded {
		// Report success for all the pairs until the introduction
		// point.
		i.successPairRange(route, 0, introIdx-1)

		// Handle the blinded route part.
		//
		// NOTE: The introIdx index here does describe the node after
		// the introduction point.
		i.markBlindedRouteSuccess(route, introIdx)

		return
	}

	// Mark nodes as successful in the non-blinded case of the payment.
	i.successPairRange(route, 0, len(route.hops.Val)-1)
}

// processFail processes a failed payment attempt.
func (i *interpretedResult) processFail(rt *mcRoute, failure paymentFailure) {
	// Not having a source index means that we were unable to decrypt the
	// error message.
	if failure.sourceIdx.IsNone() {
		i.processPaymentOutcomeUnknown(rt)
		return
	}

	var (
		idx     int
		failMsg lnwire.FailureMessage
	)

	failure.sourceIdx.WhenSome(
		func(r tlv.RecordT[tlv.TlvType0, uint8]) {
			idx = int(r.Val)

			failure.msg.WhenSome(
				func(r tlv.RecordT[tlv.TlvType1,
					failureMessage]) {

					failMsg = r.Val.FailureMessage
				},
			)
		},
	)

	// If the payment was to a blinded route and we received an error from
	// after the introduction point, handle this error separately - there
	// has been a protocol violation from the introduction node. This
	// penalty applies regardless of the error code that is returned.
	introIdx, isBlinded := introductionPointIndex(rt)
	if isBlinded && introIdx < idx {
		i.processPaymentOutcomeBadIntro(rt, introIdx, idx)
		return
	}

	switch idx {
	// We are the source of the failure.
	case 0:
		i.processPaymentOutcomeSelf(rt, failMsg)

	// A failure from the final hop was received.
	case len(rt.hops.Val):
		i.processPaymentOutcomeFinal(rt, failMsg)

	// An intermediate hop failed. Interpret the outcome, update reputation
	// and try again.
	default:
		i.processPaymentOutcomeIntermediate(rt, idx, failMsg)
	}
}

// processPaymentOutcomeBadIntro handles the case where we have made payment
// to a blinded route, but received an error from a node after the introduction
// node. This indicates that the introduction node is not obeying the route
// blinding specification, as we expect all errors from the introduction node
// to be source from it.
func (i *interpretedResult) processPaymentOutcomeBadIntro(route *mcRoute,
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
	if errSourceIdx == len(route.hops.Val) {
		i.finalFailureReason = &reasonError
	}
}

// processPaymentOutcomeSelf handles failures sent by ourselves.
func (i *interpretedResult) processPaymentOutcomeSelf(rt *mcRoute,
	failure lnwire.FailureMessage) {

	switch failure.(type) {

	// We receive a malformed htlc failure from our peer. We trust ourselves
	// to send the correct htlc, so our peer must be at fault.
	case *lnwire.FailInvalidOnionVersion,
		*lnwire.FailInvalidOnionHmac,
		*lnwire.FailInvalidOnionKey:

		i.failNode(rt, 1)

		// If this was a payment to a direct peer, we can stop trying.
		if len(rt.hops.Val) == 1 {
			i.finalFailureReason = &reasonError
		}

	// Any other failure originating from ourselves should be temporary and
	// caused by changing conditions between path finding and execution of
	// the payment. We just retry and trust that the information locally
	// available in the link has been updated.
	default:
		log.Warnf("Routing failure for local channel %v occurred",
			rt.hops.Val[0].channelID)
	}
}

// processPaymentOutcomeFinal handles failures sent by the final hop.
func (i *interpretedResult) processPaymentOutcomeFinal(route *mcRoute,
	failure lnwire.FailureMessage) {

	n := len(route.hops.Val)

	failNode := func() {
		i.failNode(route, n)

		// Other channels in the route forwarded correctly.
		if n > 1 {
			i.successPairRange(route, 0, n-2)
		}

		i.finalFailureReason = &reasonError
	}

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

	// We do not expect to receive an invalid blinding error from the final
	// node in the route. This could erroneously happen in the following
	// cases:
	// 1. Unblinded node: misuses the error code.
	// 2. A receiving introduction node: erroneously sends the error code,
	//    as the spec indicates that receiving introduction nodes should
	//    use regular errors.
	//
	// Note that we expect the case where this error is sent from a node
	// after the introduction node to be handled elsewhere as this is part
	// of a more general class of errors where the introduction node has
	// failed to convert errors for the blinded route.
	case *lnwire.FailInvalidBlinding:
		failNode()

	// All other errors are considered terminal if coming from the
	// final hop. They indicate that something is wrong at the
	// recipient, so we do apply a penalty.
	default:
		failNode()
	}
}

// processPaymentOutcomeIntermediate handles failures sent by an intermediate
// hop.
//
//nolint:funlen
func (i *interpretedResult) processPaymentOutcomeIntermediate(route *mcRoute,
	errorSourceIdx int, failure lnwire.FailureMessage) {

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
			From: route.hops.Val[errorSourceIdx-1].pubKeyBytes.Val,
			To:   route.hops.Val[errorSourceIdx].pubKeyBytes.Val,
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
			From: route.hops.Val[errorSourceIdx-1].pubKeyBytes.Val,
			To:   route.hops.Val[errorSourceIdx].pubKeyBytes.Val,
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
		if introIdx == len(route.hops.Val)-1 {
			i.finalFailureReason = &reasonError
		} else {
			// We penalize the final hop of the blinded route which
			// is sufficient to not reuse this route again and is
			// also more memory efficient because the other hops
			// of the blinded path are ephemeral and will only be
			// used in conjunction with the final hop. Moreover we
			// don't want to punish the introduction node because
			// the blinded failure does not necessarily mean that
			// the introduction node was at fault.
			//
			// TODO(ziggie): Make sure we only keep mc data for
			// blinded paths, in both the success and failure case,
			// in memory during the time of the payment and remove
			// it afterwards. Blinded paths and their blinded hop
			// keys are always changing per blinded route so there
			// is no point in persisting this data.
			i.failBlindedRoute(route)
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
func introductionPointIndex(route *mcRoute) (int, bool) {
	for i, hop := range route.hops.Val {
		if hop.hasBlindingPoint.IsSome() {
			return i + 1, true
		}
	}

	return 0, false
}

// processPaymentOutcomeUnknown processes a payment outcome for which no failure
// message or source is available.
func (i *interpretedResult) processPaymentOutcomeUnknown(route *mcRoute) {
	n := len(route.hops.Val)

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

// extractMCRoute extracts the fields required by MC from the Route struct to
// create the more minimal mcRoute struct.
func extractMCRoute(r *route.Route) *mcRoute {
	return &mcRoute{
		sourcePubKey: tlv.NewRecordT[tlv.TlvType0](r.SourcePubKey),
		totalAmount:  tlv.NewRecordT[tlv.TlvType1](r.TotalAmount),
		hops: tlv.NewRecordT[tlv.TlvType2](
			extractMCHops(r.Hops),
		),
	}
}

// extractMCHops extracts the Hop fields that MC actually uses from a slice of
// Hops.
func extractMCHops(hops []*route.Hop) mcHops {
	return fn.Map(hops, extractMCHop)
}

// extractMCHop extracts the Hop fields that MC actually uses from a Hop.
func extractMCHop(hop *route.Hop) *mcHop {
	h := mcHop{
		channelID: tlv.NewPrimitiveRecord[tlv.TlvType0](
			hop.ChannelID,
		),
		pubKeyBytes: tlv.NewRecordT[tlv.TlvType1](hop.PubKeyBytes),
		amtToFwd:    tlv.NewRecordT[tlv.TlvType2](hop.AmtToForward),
	}

	if hop.BlindingPoint != nil {
		h.hasBlindingPoint = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType3](lnwire.TrueBoolean{}),
		)
	}

	if hop.CustomRecords != nil {
		h.hasCustomRecords = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType4](lnwire.TrueBoolean{}),
		)
	}

	return &h
}

// mcRoute holds the bare minimum info about a payment attempt route that MC
// requires.
type mcRoute struct {
	sourcePubKey tlv.RecordT[tlv.TlvType0, route.Vertex]
	totalAmount  tlv.RecordT[tlv.TlvType1, lnwire.MilliSatoshi]
	hops         tlv.RecordT[tlv.TlvType2, mcHops]
}

// Record returns a TLV record that can be used to encode/decode an mcRoute
// to/from a TLV stream.
func (r *mcRoute) Record() tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := encodeMCRoute(&b, r, &buf); err != nil {
			panic(err)
		}

		return uint64(len(b.Bytes()))
	}

	return tlv.MakeDynamicRecord(
		0, r, recordSize, encodeMCRoute, decodeMCRoute,
	)
}

func encodeMCRoute(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*mcRoute); ok {
		return serializeRoute(w, v)
	}

	return tlv.NewTypeForEncodingErr(val, "routing.mcRoute")
}

func decodeMCRoute(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if v, ok := val.(*mcRoute); ok {
		route, err := deserializeRoute(io.LimitReader(r, int64(l)))
		if err != nil {
			return err
		}

		*v = *route

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "routing.mcRoute", l, l)
}

// mcHops is a list of mcHop records.
type mcHops []*mcHop

// Record returns a TLV record that can be used to encode/decode a list of
// mcHop to/from a TLV stream.
func (h *mcHops) Record() tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := encodeMCHops(&b, h, &buf); err != nil {
			panic(err)
		}

		return uint64(len(b.Bytes()))
	}

	return tlv.MakeDynamicRecord(
		0, h, recordSize, encodeMCHops, decodeMCHops,
	)
}

func encodeMCHops(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*mcHops); ok {
		// Encode the number of hops as a var int.
		if err := tlv.WriteVarInt(w, uint64(len(*v)), buf); err != nil {
			return err
		}

		// With that written out, we'll now encode the entries
		// themselves as a sub-TLV record, which includes its _own_
		// inner length prefix.
		for _, hop := range *v {
			var hopBytes bytes.Buffer
			if err := serializeHop(&hopBytes, hop); err != nil {
				return err
			}

			// We encode the record with a varint length followed by
			// the _raw_ TLV bytes.
			tlvLen := uint64(len(hopBytes.Bytes()))
			if err := tlv.WriteVarInt(w, tlvLen, buf); err != nil {
				return err
			}

			if _, err := w.Write(hopBytes.Bytes()); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "routing.mcHops")
}

func decodeMCHops(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*mcHops); ok {
		// First, we'll decode the varint that encodes how many hops
		// are encoded in the stream.
		numHops, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		// Now that we know how many records we'll need to read, we can
		// iterate and read them all out in series.
		for i := uint64(0); i < numHops; i++ {
			// Read out the varint that encodes the size of this
			// inner TLV record.
			hopSize, err := tlv.ReadVarInt(r, buf)
			if err != nil {
				return err
			}

			// Using this information, we'll create a new limited
			// reader that'll return an EOF once the end has been
			// reached so the stream stops consuming bytes.
			innerTlvReader := &io.LimitedReader{
				R: r,
				N: int64(hopSize),
			}

			hop, err := deserializeHop(innerTlvReader)
			if err != nil {
				return err
			}

			*v = append(*v, hop)
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "routing.mcHops", l, l)
}

// mcHop holds the bare minimum info about a payment attempt route hop that MC
// requires.
type mcHop struct {
	channelID        tlv.RecordT[tlv.TlvType0, uint64]
	pubKeyBytes      tlv.RecordT[tlv.TlvType1, route.Vertex]
	amtToFwd         tlv.RecordT[tlv.TlvType2, lnwire.MilliSatoshi]
	hasBlindingPoint tlv.OptionalRecordT[tlv.TlvType3, lnwire.TrueBoolean]
	hasCustomRecords tlv.OptionalRecordT[tlv.TlvType4, lnwire.TrueBoolean]
}

// failNode marks the node indicated by idx in the route as failed. It also
// marks the incoming and outgoing channels of the node as failed. This function
// intentionally panics when the self node is failed.
func (i *interpretedResult) failNode(rt *mcRoute, idx int) {
	// Mark the node as failing.
	i.nodeFailure = &rt.hops.Val[idx-1].pubKeyBytes.Val

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
	if idx < len(rt.hops.Val) {
		outgoingChannelIdx := idx
		outPair, _ := getPair(rt, outgoingChannelIdx)
		i.pairResults[outPair] = failPairResult(0)
		i.pairResults[outPair.Reverse()] = failPairResult(0)
	}
}

// failPairRange marks the node pairs from node fromIdx to node toIdx as failed
// in both direction.
func (i *interpretedResult) failPairRange(rt *mcRoute, fromIdx, toIdx int) {
	for idx := fromIdx; idx <= toIdx; idx++ {
		i.failPair(rt, idx)
	}
}

// failPair marks a pair as failed in both directions.
func (i *interpretedResult) failPair(rt *mcRoute, idx int) {
	pair, _ := getPair(rt, idx)

	// Report pair in both directions without a minimum penalization amount.
	i.pairResults[pair] = failPairResult(0)
	i.pairResults[pair.Reverse()] = failPairResult(0)
}

// failPairBalance marks a pair as failed with a minimum penalization amount.
func (i *interpretedResult) failPairBalance(rt *mcRoute, channelIdx int) {
	pair, amt := getPair(rt, channelIdx)

	i.pairResults[pair] = failPairResult(amt)
}

// successPairRange marks the node pairs from node fromIdx to node toIdx as
// succeeded.
func (i *interpretedResult) successPairRange(rt *mcRoute, fromIdx, toIdx int) {
	for idx := fromIdx; idx <= toIdx; idx++ {
		pair, amt := getPair(rt, idx)

		i.pairResults[pair] = successPairResult(amt)
	}
}

// failBlindedRoute marks a blinded route as failed for the specific amount to
// send by only punishing the last pair.
func (i *interpretedResult) failBlindedRoute(rt *mcRoute) {
	// We fail the last pair of the route, in order to fail the complete
	// blinded route. This is because the combination of ephemeral pubkeys
	// is unique to the route. We fail the last pair in order to not punish
	// the introduction node, since we don't want to disincentivize them
	// from providing that service.
	pair, _ := getPair(rt, len(rt.hops.Val)-1)

	// Since all the hops along a blinded path don't have any amount set, we
	// extract the minimal amount to punish from the value that is tried to
	// be sent to the receiver.
	amt := rt.hops.Val[len(rt.hops.Val)-1].amtToFwd.Val

	i.pairResults[pair] = failPairResult(amt)
}

// markBlindedRouteSuccess marks the hops of the blinded route AFTER the
// introduction node as successful.
//
// NOTE: The introIdx must be the index of the first hop of the blinded route
// AFTER the introduction node.
func (i *interpretedResult) markBlindedRouteSuccess(rt *mcRoute, introIdx int) {
	// For blinded hops we do not have the forwarding amount so we take the
	// minimal amount which went through the route by looking at the last
	// hop.
	successAmt := rt.hops.Val[len(rt.hops.Val)-1].amtToFwd.Val
	for idx := introIdx; idx < len(rt.hops.Val); idx++ {
		pair, _ := getPair(rt, idx)

		i.pairResults[pair] = successPairResult(successAmt)
	}
}

// getPair returns a node pair from the route and the amount passed between that
// pair.
func getPair(rt *mcRoute, channelIdx int) (DirectedNodePair,
	lnwire.MilliSatoshi) {

	nodeTo := rt.hops.Val[channelIdx].pubKeyBytes.Val
	var (
		nodeFrom route.Vertex
		amt      lnwire.MilliSatoshi
	)

	if channelIdx == 0 {
		nodeFrom = rt.sourcePubKey.Val
		amt = rt.totalAmount.Val
	} else {
		nodeFrom = rt.hops.Val[channelIdx-1].pubKeyBytes.Val
		amt = rt.hops.Val[channelIdx-1].amtToFwd.Val
	}

	pair := NewDirectedNodePair(nodeFrom, nodeTo)

	return pair, amt
}
