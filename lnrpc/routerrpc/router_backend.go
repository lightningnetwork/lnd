package routerrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	math "math"
	"time"

	"github.com/btcsuite/btcd/btcec"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

// RouterBackend contains the backend implementation of the router rpc sub
// server calls.
type RouterBackend struct {
	// MaxPaymentMSat is the largest payment permitted by the backend.
	MaxPaymentMSat lnwire.MilliSatoshi

	// SelfNode is the vertex of the node sending the payment.
	SelfNode route.Vertex

	// FetchChannelCapacity is a closure that we'll use the fetch the total
	// capacity of a channel to populate in responses.
	FetchChannelCapacity func(chanID uint64) (btcutil.Amount, error)

	// FetchChannelEndpoints returns the pubkeys of both endpoints of the
	// given channel id.
	FetchChannelEndpoints func(chanID uint64) (route.Vertex,
		route.Vertex, error)

	// FindRoutes is a closure that abstracts away how we locate/query for
	// routes.
	FindRoute func(source, target route.Vertex,
		amt lnwire.MilliSatoshi, restrictions *routing.RestrictParams,
		destCustomRecords record.CustomSet,
		routeHints map[route.Vertex][]*channeldb.ChannelEdgePolicy,
		finalExpiry uint16) (*route.Route, error)

	MissionControl MissionControl

	// ActiveNetParams are the network parameters of the primary network
	// that the route is operating on. This is necessary so we can ensure
	// that we receive payment requests that send to destinations on our
	// network.
	ActiveNetParams *chaincfg.Params

	// Tower is the ControlTower instance that is used to track pending
	// payments.
	Tower routing.ControlTower

	// MaxTotalTimelock is the maximum total time lock a route is allowed to
	// have.
	MaxTotalTimelock uint32

	// DefaultFinalCltvDelta is the default value used as final cltv delta
	// when an RPC caller doesn't specify a value.
	DefaultFinalCltvDelta uint16
}

// MissionControl defines the mission control dependencies of routerrpc.
type MissionControl interface {
	// GetProbability is expected to return the success probability of a
	// payment from fromNode to toNode.
	GetProbability(fromNode, toNode route.Vertex,
		amt lnwire.MilliSatoshi) float64

	// ResetHistory resets the history of MissionControl returning it to a
	// state as if no payment attempts have been made.
	ResetHistory() error

	// GetHistorySnapshot takes a snapshot from the current mission control
	// state and actual probability estimates.
	GetHistorySnapshot() *routing.MissionControlSnapshot

	// GetPairHistorySnapshot returns the stored history for a given node
	// pair.
	GetPairHistorySnapshot(fromNode,
		toNode route.Vertex) routing.TimedPairResult
}

// QueryRoutes attempts to query the daemons' Channel Router for a possible
// route to a target destination capable of carrying a specific amount of
// satoshis within the route's flow. The retuned route contains the full
// details required to craft and send an HTLC, also including the necessary
// information that should be present within the Sphinx packet encapsulated
// within the HTLC.
//
// TODO(roasbeef): should return a slice of routes in reality * create separate
// PR to send based on well formatted route
func (r *RouterBackend) QueryRoutes(ctx context.Context,
	in *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse, error) {

	parsePubKey := func(key string) (route.Vertex, error) {
		pubKeyBytes, err := hex.DecodeString(key)
		if err != nil {
			return route.Vertex{}, err
		}

		return route.NewVertexFromBytes(pubKeyBytes)
	}

	// Parse the hex-encoded source and target public keys into full public
	// key objects we can properly manipulate.
	targetPubKey, err := parsePubKey(in.PubKey)
	if err != nil {
		return nil, err
	}

	var sourcePubKey route.Vertex
	if in.SourcePubKey != "" {
		var err error
		sourcePubKey, err = parsePubKey(in.SourcePubKey)
		if err != nil {
			return nil, err
		}
	} else {
		// If no source is specified, use self.
		sourcePubKey = r.SelfNode
	}

	// Currently, within the bootstrap phase of the network, we limit the
	// largest payment size allotted to (2^32) - 1 mSAT or 4.29 million
	// satoshis.
	amt, err := lnrpc.UnmarshallAmt(in.Amt, in.AmtMsat)
	if err != nil {
		return nil, err
	}
	if amt > r.MaxPaymentMSat {
		return nil, fmt.Errorf("payment of %v is too large, max payment "+
			"allowed is %v", amt, r.MaxPaymentMSat.ToSatoshis())
	}

	// Unmarshall restrictions from request.
	feeLimit := lnrpc.CalculateFeeLimit(in.FeeLimit, amt)

	ignoredNodes := make(map[route.Vertex]struct{})
	for _, ignorePubKey := range in.IgnoredNodes {
		ignoreVertex, err := route.NewVertexFromBytes(ignorePubKey)
		if err != nil {
			return nil, err
		}
		ignoredNodes[ignoreVertex] = struct{}{}
	}

	ignoredPairs := make(map[routing.DirectedNodePair]struct{})

	// Convert deprecated ignoredEdges to pairs.
	for _, ignoredEdge := range in.IgnoredEdges {
		pair, err := r.rpcEdgeToPair(ignoredEdge)
		if err != nil {
			log.Warnf("Ignore channel %v skipped: %v",
				ignoredEdge.ChannelId, err)

			continue
		}
		ignoredPairs[pair] = struct{}{}
	}

	// Add ignored pairs to set.
	for _, ignorePair := range in.IgnoredPairs {
		from, err := route.NewVertexFromBytes(ignorePair.From)
		if err != nil {
			return nil, err
		}

		to, err := route.NewVertexFromBytes(ignorePair.To)
		if err != nil {
			return nil, err
		}

		pair := routing.NewDirectedNodePair(from, to)
		ignoredPairs[pair] = struct{}{}
	}

	// Since QueryRoutes allows having a different source other than
	// ourselves, we'll only apply our max time lock if we are the source.
	maxTotalTimelock := r.MaxTotalTimelock
	if sourcePubKey != r.SelfNode {
		maxTotalTimelock = math.MaxUint32
	}
	cltvLimit, err := ValidateCLTVLimit(in.CltvLimit, maxTotalTimelock)
	if err != nil {
		return nil, err
	}

	// We need to subtract the final delta before passing it into path
	// finding. The optimal path is independent of the final cltv delta and
	// the path finding algorithm is unaware of this value.
	finalCLTVDelta := r.DefaultFinalCltvDelta
	if in.FinalCltvDelta != 0 {
		finalCLTVDelta = uint16(in.FinalCltvDelta)
	}
	cltvLimit -= uint32(finalCLTVDelta)

	// Parse destination feature bits.
	features, err := UnmarshalFeatures(in.DestFeatures)
	if err != nil {
		return nil, err
	}

	restrictions := &routing.RestrictParams{
		FeeLimit: feeLimit,
		ProbabilitySource: func(fromNode, toNode route.Vertex,
			amt lnwire.MilliSatoshi) float64 {

			if _, ok := ignoredNodes[fromNode]; ok {
				return 0
			}

			pair := routing.DirectedNodePair{
				From: fromNode,
				To:   toNode,
			}
			if _, ok := ignoredPairs[pair]; ok {
				return 0
			}

			if !in.UseMissionControl {
				return 1
			}

			return r.MissionControl.GetProbability(
				fromNode, toNode, amt,
			)
		},
		DestCustomRecords: record.CustomSet(in.DestCustomRecords),
		CltvLimit:         cltvLimit,
		DestFeatures:      features,
	}

	// Pass along an outgoing channel restriction if specified.
	if in.OutgoingChanId != 0 {
		restrictions.OutgoingChannelID = &in.OutgoingChanId
	}

	// Pass along a last hop restriction if specified.
	if len(in.LastHopPubkey) > 0 {
		lastHop, err := route.NewVertexFromBytes(
			in.LastHopPubkey,
		)
		if err != nil {
			return nil, err
		}
		restrictions.LastHop = &lastHop
	}

	// If we have any TLV records destined for the final hop, then we'll
	// attempt to decode them now into a form that the router can more
	// easily manipulate.
	customRecords := record.CustomSet(in.DestCustomRecords)
	if err := customRecords.Validate(); err != nil {
		return nil, err
	}

	// Convert route hints to an edge map.
	routeHints, err := unmarshallRouteHints(in.RouteHints)
	if err != nil {
		return nil, err
	}
	routeHintEdges, err := routing.RouteHintsToEdges(
		routeHints, targetPubKey,
	)
	if err != nil {
		return nil, err
	}

	// Query the channel router for a possible path to the destination that
	// can carry `in.Amt` satoshis _including_ the total fee required on
	// the route.
	route, err := r.FindRoute(
		sourcePubKey, targetPubKey, amt, restrictions,
		customRecords, routeHintEdges, finalCLTVDelta,
	)
	if err != nil {
		return nil, err
	}

	// For each valid route, we'll convert the result into the format
	// required by the RPC system.
	rpcRoute, err := r.MarshallRoute(route)
	if err != nil {
		return nil, err
	}

	// Calculate route success probability. Do not rely on a probability
	// that could have been returned from path finding, because mission
	// control may have been disabled in the provided ProbabilitySource.
	successProb := r.getSuccessProbability(route)

	routeResp := &lnrpc.QueryRoutesResponse{
		Routes:      []*lnrpc.Route{rpcRoute},
		SuccessProb: successProb,
	}

	return routeResp, nil
}

// getSuccessProbability returns the success probability for the given route
// based on the current state of mission control.
func (r *RouterBackend) getSuccessProbability(rt *route.Route) float64 {
	fromNode := rt.SourcePubKey
	amtToFwd := rt.TotalAmount
	successProb := 1.0
	for _, hop := range rt.Hops {
		toNode := hop.PubKeyBytes

		probability := r.MissionControl.GetProbability(
			fromNode, toNode, amtToFwd,
		)

		successProb *= probability

		amtToFwd = hop.AmtToForward
		fromNode = toNode
	}

	return successProb
}

// rpcEdgeToPair looks up the provided channel and returns the channel endpoints
// as a directed pair.
func (r *RouterBackend) rpcEdgeToPair(e *lnrpc.EdgeLocator) (
	routing.DirectedNodePair, error) {

	a, b, err := r.FetchChannelEndpoints(e.ChannelId)
	if err != nil {
		return routing.DirectedNodePair{}, err
	}

	var pair routing.DirectedNodePair
	if e.DirectionReverse {
		pair.From, pair.To = b, a
	} else {
		pair.From, pair.To = a, b
	}

	return pair, nil
}

// MarshallRoute marshalls an internal route to an rpc route struct.
func (r *RouterBackend) MarshallRoute(route *route.Route) (*lnrpc.Route, error) {
	resp := &lnrpc.Route{
		TotalTimeLock: route.TotalTimeLock,
		TotalFees:     int64(route.TotalFees().ToSatoshis()),
		TotalFeesMsat: int64(route.TotalFees()),
		TotalAmt:      int64(route.TotalAmount.ToSatoshis()),
		TotalAmtMsat:  int64(route.TotalAmount),
		Hops:          make([]*lnrpc.Hop, len(route.Hops)),
	}
	incomingAmt := route.TotalAmount
	for i, hop := range route.Hops {
		fee := route.HopFee(i)

		// Channel capacity is not a defining property of a route. For
		// backwards RPC compatibility, we retrieve it here from the
		// graph.
		chanCapacity, err := r.FetchChannelCapacity(hop.ChannelID)
		if err != nil {
			// If capacity cannot be retrieved, this may be a
			// not-yet-received or private channel. Then report
			// amount that is sent through the channel as capacity.
			chanCapacity = incomingAmt.ToSatoshis()
		}

		// Extract the MPP fields if present on this hop.
		var mpp *lnrpc.MPPRecord
		if hop.MPP != nil {
			addr := hop.MPP.PaymentAddr()

			mpp = &lnrpc.MPPRecord{
				PaymentAddr:  addr[:],
				TotalAmtMsat: int64(hop.MPP.TotalMsat()),
			}
		}

		resp.Hops[i] = &lnrpc.Hop{
			ChanId:           hop.ChannelID,
			ChanCapacity:     int64(chanCapacity),
			AmtToForward:     int64(hop.AmtToForward.ToSatoshis()),
			AmtToForwardMsat: int64(hop.AmtToForward),
			Fee:              int64(fee.ToSatoshis()),
			FeeMsat:          int64(fee),
			Expiry:           uint32(hop.OutgoingTimeLock),
			PubKey: hex.EncodeToString(
				hop.PubKeyBytes[:],
			),
			CustomRecords: hop.CustomRecords,
			TlvPayload:    !hop.LegacyPayload,
			MppRecord:     mpp,
		}
		incomingAmt = hop.AmtToForward
	}

	return resp, nil
}

// UnmarshallHopWithPubkey unmarshalls an rpc hop for which the pubkey has
// already been extracted.
func UnmarshallHopWithPubkey(rpcHop *lnrpc.Hop, pubkey route.Vertex) (*route.Hop,
	error) {

	customRecords := record.CustomSet(rpcHop.CustomRecords)
	if err := customRecords.Validate(); err != nil {
		return nil, err
	}

	mpp, err := UnmarshalMPP(rpcHop.MppRecord)
	if err != nil {
		return nil, err
	}

	return &route.Hop{
		OutgoingTimeLock: rpcHop.Expiry,
		AmtToForward:     lnwire.MilliSatoshi(rpcHop.AmtToForwardMsat),
		PubKeyBytes:      pubkey,
		ChannelID:        rpcHop.ChanId,
		CustomRecords:    customRecords,
		LegacyPayload:    !rpcHop.TlvPayload,
		MPP:              mpp,
	}, nil
}

// UnmarshallHop unmarshalls an rpc hop that may or may not contain a node
// pubkey.
func (r *RouterBackend) UnmarshallHop(rpcHop *lnrpc.Hop,
	prevNodePubKey [33]byte) (*route.Hop, error) {

	var pubKeyBytes [33]byte
	if rpcHop.PubKey != "" {
		// Unmarshall the provided hop pubkey.
		pubKey, err := hex.DecodeString(rpcHop.PubKey)
		if err != nil {
			return nil, fmt.Errorf("cannot decode pubkey %s",
				rpcHop.PubKey)
		}
		copy(pubKeyBytes[:], pubKey)
	} else {
		// If no pub key is given of the hop, the local channel graph
		// needs to be queried to complete the information necessary for
		// routing. Discard edge policies, because they may be nil.
		node1, node2, err := r.FetchChannelEndpoints(rpcHop.ChanId)
		if err != nil {
			return nil, err
		}

		switch {
		case prevNodePubKey == node1:
			pubKeyBytes = node2
		case prevNodePubKey == node2:
			pubKeyBytes = node1
		default:
			return nil, fmt.Errorf("channel edge does not match " +
				"expected node")
		}
	}

	return UnmarshallHopWithPubkey(rpcHop, pubKeyBytes)
}

// UnmarshallRoute unmarshalls an rpc route. For hops that don't specify a
// pubkey, the channel graph is queried.
func (r *RouterBackend) UnmarshallRoute(rpcroute *lnrpc.Route) (
	*route.Route, error) {

	prevNodePubKey := r.SelfNode

	hops := make([]*route.Hop, len(rpcroute.Hops))
	for i, hop := range rpcroute.Hops {
		routeHop, err := r.UnmarshallHop(hop, prevNodePubKey)
		if err != nil {
			return nil, err
		}

		if routeHop.AmtToForward > r.MaxPaymentMSat {
			return nil, fmt.Errorf("payment of %v is too large, "+
				"max payment allowed is %v",
				routeHop.AmtToForward,
				r.MaxPaymentMSat.ToSatoshis())
		}

		hops[i] = routeHop

		prevNodePubKey = routeHop.PubKeyBytes
	}

	route, err := route.NewRouteFromHops(
		lnwire.MilliSatoshi(rpcroute.TotalAmtMsat),
		rpcroute.TotalTimeLock,
		r.SelfNode,
		hops,
	)
	if err != nil {
		return nil, err
	}

	return route, nil
}

// extractIntentFromSendRequest attempts to parse the SendRequest details
// required to dispatch a client from the information presented by an RPC
// client.
func (r *RouterBackend) extractIntentFromSendRequest(
	rpcPayReq *SendPaymentRequest) (*routing.LightningPayment, error) {

	payIntent := &routing.LightningPayment{}

	// Pass along an outgoing channel restriction if specified.
	if rpcPayReq.OutgoingChanId != 0 {
		payIntent.OutgoingChannelID = &rpcPayReq.OutgoingChanId
	}

	// Pass along a last hop restriction if specified.
	if len(rpcPayReq.LastHopPubkey) > 0 {
		lastHop, err := route.NewVertexFromBytes(
			rpcPayReq.LastHopPubkey,
		)
		if err != nil {
			return nil, err
		}
		payIntent.LastHop = &lastHop
	}

	// Take the CLTV limit from the request if set, otherwise use the max.
	cltvLimit, err := ValidateCLTVLimit(
		uint32(rpcPayReq.CltvLimit), r.MaxTotalTimelock,
	)
	if err != nil {
		return nil, err
	}
	payIntent.CltvLimit = cltvLimit

	// Take fee limit from request.
	payIntent.FeeLimit, err = lnrpc.UnmarshallAmt(
		rpcPayReq.FeeLimitSat, rpcPayReq.FeeLimitMsat,
	)
	if err != nil {
		return nil, err
	}

	// Set payment attempt timeout.
	if rpcPayReq.TimeoutSeconds == 0 {
		return nil, errors.New("timeout_seconds must be specified")
	}

	customRecords := record.CustomSet(rpcPayReq.DestCustomRecords)
	if err := customRecords.Validate(); err != nil {
		return nil, err
	}
	payIntent.DestCustomRecords = customRecords

	payIntent.PayAttemptTimeout = time.Second *
		time.Duration(rpcPayReq.TimeoutSeconds)

	// Route hints.
	routeHints, err := unmarshallRouteHints(
		rpcPayReq.RouteHints,
	)
	if err != nil {
		return nil, err
	}
	payIntent.RouteHints = routeHints

	// Unmarshall either sat or msat amount from request.
	reqAmt, err := lnrpc.UnmarshallAmt(
		rpcPayReq.Amt, rpcPayReq.AmtMsat,
	)
	if err != nil {
		return nil, err
	}

	// If the payment request field isn't blank, then the details of the
	// invoice are encoded entirely within the encoded payReq.  So we'll
	// attempt to decode it, populating the payment accordingly.
	if rpcPayReq.PaymentRequest != "" {
		switch {

		case len(rpcPayReq.Dest) > 0:
			return nil, errors.New("dest and payment_request " +
				"cannot appear together")

		case len(rpcPayReq.PaymentHash) > 0:
			return nil, errors.New("dest and payment_hash " +
				"cannot appear together")

		case rpcPayReq.FinalCltvDelta != 0:
			return nil, errors.New("dest and final_cltv_delta " +
				"cannot appear together")
		}

		payReq, err := zpay32.Decode(
			rpcPayReq.PaymentRequest, r.ActiveNetParams,
		)
		if err != nil {
			return nil, err
		}

		// Next, we'll ensure that this payreq hasn't already expired.
		err = ValidatePayReqExpiry(payReq)
		if err != nil {
			return nil, err
		}

		// If the amount was not included in the invoice, then we let
		// the payee specify the amount of satoshis they wish to send.
		// We override the amount to pay with the amount provided from
		// the payment request.
		if payReq.MilliSat == nil {
			if reqAmt == 0 {
				return nil, errors.New("amount must be " +
					"specified when paying a zero amount " +
					"invoice")
			}

			payIntent.Amount = reqAmt
		} else {
			if reqAmt != 0 {
				return nil, errors.New("amount must not be " +
					"specified when paying a non-zero " +
					" amount invoice")
			}

			payIntent.Amount = *payReq.MilliSat
		}

		copy(payIntent.PaymentHash[:], payReq.PaymentHash[:])
		destKey := payReq.Destination.SerializeCompressed()
		copy(payIntent.Target[:], destKey)

		payIntent.FinalCLTVDelta = uint16(payReq.MinFinalCLTVExpiry())
		payIntent.RouteHints = append(
			payIntent.RouteHints, payReq.RouteHints...,
		)
		payIntent.DestFeatures = payReq.Features
		payIntent.PaymentAddr = payReq.PaymentAddr
	} else {
		// Otherwise, If the payment request field was not specified
		// (and a custom route wasn't specified), construct the payment
		// from the other fields.

		// Payment destination.
		target, err := route.NewVertexFromBytes(rpcPayReq.Dest)
		if err != nil {
			return nil, err
		}
		payIntent.Target = target

		// Final payment CLTV delta.
		if rpcPayReq.FinalCltvDelta != 0 {
			payIntent.FinalCLTVDelta =
				uint16(rpcPayReq.FinalCltvDelta)
		} else {
			payIntent.FinalCLTVDelta = r.DefaultFinalCltvDelta
		}

		// Amount.
		if reqAmt == 0 {
			return nil, errors.New("amount must be specified")
		}

		payIntent.Amount = reqAmt

		// Payment hash.
		copy(payIntent.PaymentHash[:], rpcPayReq.PaymentHash)

		// Parse destination feature bits.
		features, err := UnmarshalFeatures(rpcPayReq.DestFeatures)
		if err != nil {
			return nil, err
		}

		payIntent.DestFeatures = features
	}

	// Currently, within the bootstrap phase of the network, we limit the
	// largest payment size allotted to (2^32) - 1 mSAT or 4.29 million
	// satoshis.
	if payIntent.Amount > r.MaxPaymentMSat {
		// In this case, we'll send an error to the caller, but
		// continue our loop for the next payment.
		return payIntent, fmt.Errorf("payment of %v is too large, "+
			"max payment allowed is %v", payIntent.Amount,
			r.MaxPaymentMSat)

	}

	// Check for disallowed payments to self.
	if !rpcPayReq.AllowSelfPayment && payIntent.Target == r.SelfNode {
		return nil, errors.New("self-payments not allowed")
	}

	return payIntent, nil
}

// unmarshallRouteHints unmarshalls a list of route hints.
func unmarshallRouteHints(rpcRouteHints []*lnrpc.RouteHint) (
	[][]zpay32.HopHint, error) {

	routeHints := make([][]zpay32.HopHint, 0, len(rpcRouteHints))
	for _, rpcRouteHint := range rpcRouteHints {
		routeHint := make(
			[]zpay32.HopHint, 0, len(rpcRouteHint.HopHints),
		)
		for _, rpcHint := range rpcRouteHint.HopHints {
			hint, err := unmarshallHopHint(rpcHint)
			if err != nil {
				return nil, err
			}

			routeHint = append(routeHint, hint)
		}
		routeHints = append(routeHints, routeHint)
	}

	return routeHints, nil
}

// unmarshallHopHint unmarshalls a single hop hint.
func unmarshallHopHint(rpcHint *lnrpc.HopHint) (zpay32.HopHint, error) {
	pubBytes, err := hex.DecodeString(rpcHint.NodeId)
	if err != nil {
		return zpay32.HopHint{}, err
	}

	pubkey, err := btcec.ParsePubKey(pubBytes, btcec.S256())
	if err != nil {
		return zpay32.HopHint{}, err
	}

	return zpay32.HopHint{
		NodeID:                    pubkey,
		ChannelID:                 rpcHint.ChanId,
		FeeBaseMSat:               rpcHint.FeeBaseMsat,
		FeeProportionalMillionths: rpcHint.FeeProportionalMillionths,
		CLTVExpiryDelta:           uint16(rpcHint.CltvExpiryDelta),
	}, nil
}

// UnmarshalFeatures converts a list of uint32's into a valid feature vector.
// This method checks that feature bit pairs aren't assigned toegether, and
// validates transitive dependencies.
func UnmarshalFeatures(
	rpcFeatures []lnrpc.FeatureBit) (*lnwire.FeatureVector, error) {

	// If no destination features are specified we'll return nil to signal
	// that the router should try to use the graph as a fallback.
	if rpcFeatures == nil {
		return nil, nil
	}

	raw := lnwire.NewRawFeatureVector()
	for _, bit := range rpcFeatures {
		err := raw.SafeSet(lnwire.FeatureBit(bit))
		if err != nil {
			return nil, err
		}
	}

	return lnwire.NewFeatureVector(raw, lnwire.Features), nil
}

// ValidatePayReqExpiry checks if the passed payment request has expired. In
// the case it has expired, an error will be returned.
func ValidatePayReqExpiry(payReq *zpay32.Invoice) error {
	expiry := payReq.Expiry()
	validUntil := payReq.Timestamp.Add(expiry)
	if time.Now().After(validUntil) {
		return fmt.Errorf("invoice expired. Valid until %v", validUntil)
	}

	return nil
}

// ValidateCLTVLimit returns a valid CLTV limit given a value and a maximum. If
// the value exceeds the maximum, then an error is returned. If the value is 0,
// then the maximum is used.
func ValidateCLTVLimit(val, max uint32) (uint32, error) {
	switch {
	case val == 0:
		return max, nil
	case val > max:
		return 0, fmt.Errorf("total time lock of %v exceeds max "+
			"allowed %v", val, max)
	default:
		return val, nil
	}
}

// UnmarshalMPP accepts the mpp_total_amt_msat and mpp_payment_addr fields from
// an RPC request and converts into an record.MPP object. An error is returned
// if the payment address is not 0 or 32 bytes. If the total amount and payment
// address are zero-value, the return value will be nil signaling there is no
// MPP record to attach to this hop. Otherwise, a non-nil reocrd will be
// contained combining the provided values.
func UnmarshalMPP(reqMPP *lnrpc.MPPRecord) (*record.MPP, error) {
	// If no MPP record was submitted, assume the user wants to send a
	// regular payment.
	if reqMPP == nil {
		return nil, nil
	}

	reqTotal := reqMPP.TotalAmtMsat
	reqAddr := reqMPP.PaymentAddr

	switch {

	// No MPP fields were provided.
	case reqTotal == 0 && len(reqAddr) == 0:
		return nil, fmt.Errorf("missing total_msat and payment_addr")

	// Total is present, but payment address is missing.
	case reqTotal > 0 && len(reqAddr) == 0:
		return nil, fmt.Errorf("missing payment_addr")

	// Payment address is present, but total is missing.
	case reqTotal == 0 && len(reqAddr) > 0:
		return nil, fmt.Errorf("missing total_msat")
	}

	addr, err := lntypes.MakeHash(reqAddr)
	if err != nil {
		return nil, fmt.Errorf("unable to parse "+
			"payment_addr: %v", err)
	}

	total := lnwire.MilliSatoshi(reqTotal)

	return record.NewMPP(total, addr), nil
}

// MarshalHTLCAttempt constructs an RPC HTLCAttempt from the db representation.
func (r *RouterBackend) MarshalHTLCAttempt(
	htlc channeldb.HTLCAttempt) (*lnrpc.HTLCAttempt, error) {

	route, err := r.MarshallRoute(&htlc.Route)
	if err != nil {
		return nil, err
	}

	rpcAttempt := &lnrpc.HTLCAttempt{
		AttemptTimeNs: MarshalTimeNano(htlc.AttemptTime),
		Route:         route,
	}

	switch {
	case htlc.Settle != nil:
		rpcAttempt.Status = lnrpc.HTLCAttempt_SUCCEEDED
		rpcAttempt.ResolveTimeNs = MarshalTimeNano(
			htlc.Settle.SettleTime,
		)

	case htlc.Failure != nil:
		rpcAttempt.Status = lnrpc.HTLCAttempt_FAILED
		rpcAttempt.ResolveTimeNs = MarshalTimeNano(
			htlc.Failure.FailTime,
		)

		var err error
		rpcAttempt.Failure, err = marshallHtlcFailure(htlc.Failure)
		if err != nil {
			return nil, err
		}
	default:
		rpcAttempt.Status = lnrpc.HTLCAttempt_IN_FLIGHT
	}

	return rpcAttempt, nil
}

// marshallHtlcFailure marshalls htlc fail info from the database to its rpc
// representation.
func marshallHtlcFailure(failure *channeldb.HTLCFailInfo) (*lnrpc.Failure,
	error) {

	rpcFailure := &lnrpc.Failure{
		FailureSourceIndex: failure.FailureSourceIndex,
	}

	switch failure.Reason {

	case channeldb.HTLCFailUnknown:
		rpcFailure.Code = lnrpc.Failure_UNKNOWN_FAILURE

	case channeldb.HTLCFailUnreadable:
		rpcFailure.Code = lnrpc.Failure_UNREADABLE_FAILURE

	case channeldb.HTLCFailInternal:
		rpcFailure.Code = lnrpc.Failure_INTERNAL_FAILURE

	case channeldb.HTLCFailMessage:
		err := marshallWireError(failure.Message, rpcFailure)
		if err != nil {
			return nil, err
		}

	default:
		return nil, errors.New("unknown htlc failure reason")
	}

	return rpcFailure, nil
}

// MarshalTimeNano converts a time.Time into its nanosecond representation. If
// the time is zero, this method simply returns 0, since calling UnixNano() on a
// zero-valued time is undefined.
func MarshalTimeNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// marshallError marshall an error as received from the switch to rpc structs
// suitable for returning to the caller of an rpc method.
//
// Because of difficulties with using protobuf oneof constructs in some
// languages, the decision was made here to use a single message format for all
// failure messages with some fields left empty depending on the failure type.
func marshallError(sendError error) (*lnrpc.Failure, error) {
	response := &lnrpc.Failure{}

	if sendError == htlcswitch.ErrUnreadableFailureMessage {
		response.Code = lnrpc.Failure_UNREADABLE_FAILURE
		return response, nil
	}

	rtErr, ok := sendError.(htlcswitch.ClearTextError)
	if !ok {
		return nil, sendError
	}

	err := marshallWireError(rtErr.WireMessage(), response)
	if err != nil {
		return nil, err
	}

	// If the ClearTextError received is a ForwardingError, the error
	// originated from a node along the route, not locally on our outgoing
	// link. We set failureSourceIdx to the index of the node where the
	// failure occurred. If the error is not a ForwardingError, the failure
	// occurred at our node, so we leave the index as 0 to indicate that
	// we failed locally.
	fErr, ok := rtErr.(*htlcswitch.ForwardingError)
	if ok {
		response.FailureSourceIndex = uint32(fErr.FailureSourceIdx)
	}

	return response, nil
}

// marshallError marshall an error as received from the switch to rpc structs
// suitable for returning to the caller of an rpc method.
//
// Because of difficulties with using protobuf oneof constructs in some
// languages, the decision was made here to use a single message format for all
// failure messages with some fields left empty depending on the failure type.
func marshallWireError(msg lnwire.FailureMessage,
	response *lnrpc.Failure) error {

	switch onionErr := msg.(type) {

	case *lnwire.FailIncorrectDetails:
		response.Code = lnrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS
		response.Height = onionErr.Height()

	case *lnwire.FailIncorrectPaymentAmount:
		response.Code = lnrpc.Failure_INCORRECT_PAYMENT_AMOUNT

	case *lnwire.FailFinalIncorrectCltvExpiry:
		response.Code = lnrpc.Failure_FINAL_INCORRECT_CLTV_EXPIRY
		response.CltvExpiry = onionErr.CltvExpiry

	case *lnwire.FailFinalIncorrectHtlcAmount:
		response.Code = lnrpc.Failure_FINAL_INCORRECT_HTLC_AMOUNT
		response.HtlcMsat = uint64(onionErr.IncomingHTLCAmount)

	case *lnwire.FailFinalExpiryTooSoon:
		response.Code = lnrpc.Failure_FINAL_EXPIRY_TOO_SOON

	case *lnwire.FailInvalidRealm:
		response.Code = lnrpc.Failure_INVALID_REALM

	case *lnwire.FailExpiryTooSoon:
		response.Code = lnrpc.Failure_EXPIRY_TOO_SOON
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)

	case *lnwire.FailExpiryTooFar:
		response.Code = lnrpc.Failure_EXPIRY_TOO_FAR

	case *lnwire.FailInvalidOnionVersion:
		response.Code = lnrpc.Failure_INVALID_ONION_VERSION
		response.OnionSha_256 = onionErr.OnionSHA256[:]

	case *lnwire.FailInvalidOnionHmac:
		response.Code = lnrpc.Failure_INVALID_ONION_HMAC
		response.OnionSha_256 = onionErr.OnionSHA256[:]

	case *lnwire.FailInvalidOnionKey:
		response.Code = lnrpc.Failure_INVALID_ONION_KEY
		response.OnionSha_256 = onionErr.OnionSHA256[:]

	case *lnwire.FailAmountBelowMinimum:
		response.Code = lnrpc.Failure_AMOUNT_BELOW_MINIMUM
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)
		response.HtlcMsat = uint64(onionErr.HtlcMsat)

	case *lnwire.FailFeeInsufficient:
		response.Code = lnrpc.Failure_FEE_INSUFFICIENT
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)
		response.HtlcMsat = uint64(onionErr.HtlcMsat)

	case *lnwire.FailIncorrectCltvExpiry:
		response.Code = lnrpc.Failure_INCORRECT_CLTV_EXPIRY
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)
		response.CltvExpiry = onionErr.CltvExpiry

	case *lnwire.FailChannelDisabled:
		response.Code = lnrpc.Failure_CHANNEL_DISABLED
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)
		response.Flags = uint32(onionErr.Flags)

	case *lnwire.FailTemporaryChannelFailure:
		response.Code = lnrpc.Failure_TEMPORARY_CHANNEL_FAILURE
		response.ChannelUpdate = marshallChannelUpdate(onionErr.Update)

	case *lnwire.FailRequiredNodeFeatureMissing:
		response.Code = lnrpc.Failure_REQUIRED_NODE_FEATURE_MISSING

	case *lnwire.FailRequiredChannelFeatureMissing:
		response.Code = lnrpc.Failure_REQUIRED_CHANNEL_FEATURE_MISSING

	case *lnwire.FailUnknownNextPeer:
		response.Code = lnrpc.Failure_UNKNOWN_NEXT_PEER

	case *lnwire.FailTemporaryNodeFailure:
		response.Code = lnrpc.Failure_TEMPORARY_NODE_FAILURE

	case *lnwire.FailPermanentNodeFailure:
		response.Code = lnrpc.Failure_PERMANENT_NODE_FAILURE

	case *lnwire.FailPermanentChannelFailure:
		response.Code = lnrpc.Failure_PERMANENT_CHANNEL_FAILURE

	case *lnwire.FailMPPTimeout:
		response.Code = lnrpc.Failure_MPP_TIMEOUT

	case nil:
		response.Code = lnrpc.Failure_UNKNOWN_FAILURE

	default:
		return fmt.Errorf("cannot marshall failure %T", onionErr)
	}

	return nil
}

// marshallChannelUpdate marshalls a channel update as received over the wire to
// the router rpc format.
func marshallChannelUpdate(update *lnwire.ChannelUpdate) *lnrpc.ChannelUpdate {
	if update == nil {
		return nil
	}

	return &lnrpc.ChannelUpdate{
		Signature:       update.Signature[:],
		ChainHash:       update.ChainHash[:],
		ChanId:          update.ShortChannelID.ToUint64(),
		Timestamp:       update.Timestamp,
		MessageFlags:    uint32(update.MessageFlags),
		ChannelFlags:    uint32(update.ChannelFlags),
		TimeLockDelta:   uint32(update.TimeLockDelta),
		HtlcMinimumMsat: uint64(update.HtlcMinimumMsat),
		BaseFee:         update.BaseFee,
		FeeRate:         update.FeeRate,
		HtlcMaximumMsat: uint64(update.HtlcMaximumMsat),
		ExtraOpaqueData: update.ExtraOpaqueData,
	}
}
