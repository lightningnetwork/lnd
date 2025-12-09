package routerrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	math "math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/protobuf/proto"
)

const (
	// DefaultMaxParts is the default number of splits we'll possibly use
	// for MPP when the user is attempting to send a payment.
	//
	// TODO(roasbeef): make this value dynamic based on expected number of
	// attempts for given amount.
	DefaultMaxParts = 16

	// MaxPartsUpperLimit defines the maximum allowable number of splits
	// for MPP/AMP when the user is attempting to send a payment.
	MaxPartsUpperLimit = 1000
)

// RouterBackend contains the backend implementation of the router rpc sub
// server calls.
type RouterBackend struct {
	// SelfNode is the vertex of the node sending the payment.
	SelfNode route.Vertex

	// FetchChannelCapacity is a closure that we'll use the fetch the total
	// capacity of a channel to populate in responses.
	FetchChannelCapacity func(chanID uint64) (btcutil.Amount, error)

	// FetchAmountPairCapacity determines the maximal channel capacity
	// between two nodes given a certain amount.
	FetchAmountPairCapacity func(nodeFrom, nodeTo route.Vertex,
		amount lnwire.MilliSatoshi) (btcutil.Amount, error)

	// FetchChannelEndpoints returns the pubkeys of both endpoints of the
	// given channel id.
	FetchChannelEndpoints func(chanID uint64) (route.Vertex,
		route.Vertex, error)

	// HasNode returns true if the node exists in the graph (i.e., has
	// public channels), false otherwise. This means the node is a public
	// node and should be reachable.
	HasNode func(nodePub route.Vertex) (bool, error)

	// FindRoute is a closure that abstracts away how we locate/query for
	// routes.
	FindRoute func(*routing.RouteRequest) (*route.Route, float64, error)

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

	// SubscribeHtlcEvents returns a subscription client for the node's
	// htlc events.
	SubscribeHtlcEvents func() (*subscribe.Client, error)

	// InterceptableForwarder exposes the ability to intercept forward events
	// by letting the router register a ForwardInterceptor.
	InterceptableForwarder htlcswitch.InterceptableHtlcForwarder

	// SetChannelEnabled exposes the ability to manually enable a channel.
	SetChannelEnabled func(wire.OutPoint) error

	// SetChannelDisabled exposes the ability to manually disable a channel
	SetChannelDisabled func(wire.OutPoint) error

	// SetChannelAuto exposes the ability to restore automatic channel state
	// management after manually setting channel status.
	SetChannelAuto func(wire.OutPoint) error

	// UseStatusInitiated is a boolean that indicates whether the router
	// should use the new status code `Payment_INITIATED`.
	//
	// TODO(yy): remove this config after the new status code is fully
	// deployed to the network(v0.20.0).
	UseStatusInitiated bool

	// ParseCustomChannelData is a function that can be used to parse custom
	// channel data from the first hop of a route.
	ParseCustomChannelData func(message proto.Message) error

	// ShouldSetExpEndorsement returns a boolean indicating whether the
	// experimental endorsement bit should be set.
	ShouldSetExpEndorsement func() bool

	// Clock is the clock used to validate payment requests expiry.
	// It is useful for testing.
	Clock clock.Clock
}

// MissionControl defines the mission control dependencies of routerrpc.
type MissionControl interface {
	// GetProbability is expected to return the success probability of a
	// payment from fromNode to toNode.
	GetProbability(fromNode, toNode route.Vertex,
		amt lnwire.MilliSatoshi, capacity btcutil.Amount) float64

	// ResetHistory resets the history of MissionControl returning it to a
	// state as if no payment attempts have been made.
	ResetHistory() error

	// GetHistorySnapshot takes a snapshot from the current mission control
	// state and actual probability estimates.
	GetHistorySnapshot() *routing.MissionControlSnapshot

	// ImportHistory imports the mission control snapshot to our internal
	// state. This import will only be applied in-memory, and will not be
	// persisted across restarts.
	ImportHistory(snapshot *routing.MissionControlSnapshot, force bool) error

	// GetPairHistorySnapshot returns the stored history for a given node
	// pair.
	GetPairHistorySnapshot(fromNode,
		toNode route.Vertex) routing.TimedPairResult

	// GetConfig gets mission control's current config.
	GetConfig() *routing.MissionControlConfig

	// SetConfig sets mission control's config to the values provided, if
	// they are valid.
	SetConfig(cfg *routing.MissionControlConfig) error
}

// QueryRoutes attempts to query the daemons' Channel Router for a possible
// route to a target destination capable of carrying a specific amount of
// satoshis within the route's flow. The returned route contains the full
// details required to craft and send an HTLC, also including the necessary
// information that should be present within the Sphinx packet encapsulated
// within the HTLC.
//
// TODO(roasbeef): should return a slice of routes in reality * create separate
// PR to send based on well formatted route
func (r *RouterBackend) QueryRoutes(ctx context.Context,
	in *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse, error) {

	routeReq, err := r.parseQueryRoutesRequest(in)
	if err != nil {
		return nil, err
	}

	// Query the channel router for a possible path to the destination that
	// can carry `in.Amt` satoshis _including_ the total fee required on
	// the route
	route, successProb, err := r.FindRoute(routeReq)
	if err != nil {
		return nil, err
	}

	// For each valid route, we'll convert the result into the format
	// required by the RPC system.
	rpcRoute, err := r.MarshallRoute(route)
	if err != nil {
		return nil, err
	}

	routeResp := &lnrpc.QueryRoutesResponse{
		Routes:      []*lnrpc.Route{rpcRoute},
		SuccessProb: successProb,
	}

	return routeResp, nil
}

func parsePubKey(key string) (route.Vertex, error) {
	pubKeyBytes, err := hex.DecodeString(key)
	if err != nil {
		return route.Vertex{}, err
	}

	return route.NewVertexFromBytes(pubKeyBytes)
}

func (r *RouterBackend) parseIgnored(in *lnrpc.QueryRoutesRequest) (
	map[route.Vertex]struct{}, map[routing.DirectedNodePair]struct{},
	error) {

	ignoredNodes := make(map[route.Vertex]struct{})
	for _, ignorePubKey := range in.IgnoredNodes {
		ignoreVertex, err := route.NewVertexFromBytes(ignorePubKey)
		if err != nil {
			return nil, nil, err
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
			return nil, nil, err
		}

		to, err := route.NewVertexFromBytes(ignorePair.To)
		if err != nil {
			return nil, nil, err
		}

		pair := routing.NewDirectedNodePair(from, to)
		ignoredPairs[pair] = struct{}{}
	}

	return ignoredNodes, ignoredPairs, nil
}

func (r *RouterBackend) parseQueryRoutesRequest(in *lnrpc.QueryRoutesRequest) (
	*routing.RouteRequest, error) {

	// Parse the hex-encoded source public key into a full public key that
	// we can properly manipulate.

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

	// Unmarshall restrictions from request.
	feeLimit := lnrpc.CalculateFeeLimit(in.FeeLimit, amt)

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

	// If we have a blinded path set, we'll get a few of our fields from
	// inside of the path rather than the request's fields.
	var (
		targetPubKey   *route.Vertex
		routeHintEdges map[route.Vertex][]routing.AdditionalEdge
		blindedPathSet *routing.BlindedPaymentPathSet

		// finalCLTVDelta varies depending on whether we're sending to
		// a blinded route or an unblinded node. For blinded paths,
		// our final cltv is already baked into the path so we restrict
		// this value to zero on the API. Bolt11 invoices have a
		// default, so we'll fill that in for the non-blinded case.
		finalCLTVDelta uint16

		// destinationFeatures is the set of features for the
		// destination node.
		destinationFeatures *lnwire.FeatureVector
	)

	// Validate that the fields provided in the request are sane depending
	// on whether it is using a blinded path or not.
	if len(in.BlindedPaymentPaths) > 0 {
		blindedPathSet, err = parseBlindedPaymentPaths(in)
		if err != nil {
			return nil, err
		}

		pathFeatures := blindedPathSet.Features()
		if pathFeatures != nil {
			destinationFeatures = pathFeatures.Clone()
		}
	} else {
		// If we do not have a blinded path, a target pubkey must be
		// set.
		pk, err := parsePubKey(in.PubKey)
		if err != nil {
			return nil, err
		}
		targetPubKey = &pk

		// Convert route hints to an edge map.
		routeHints, err := unmarshallRouteHints(in.RouteHints)
		if err != nil {
			return nil, err
		}

		routeHintEdges, err = routing.RouteHintsToEdges(
			routeHints, *targetPubKey,
		)
		if err != nil {
			return nil, err
		}

		// Set a non-zero final CLTV delta for payments that are not
		// to blinded paths, as bolt11 has a default final cltv delta
		// value that is used in the absence of a value.
		finalCLTVDelta = r.DefaultFinalCltvDelta
		if in.FinalCltvDelta != 0 {
			finalCLTVDelta = uint16(in.FinalCltvDelta)
		}

		// Do bounds checking without block padding so we don't give
		// routes that will leave the router in a zombie payment state.
		err = routing.ValidateCLTVLimit(
			cltvLimit, finalCLTVDelta, false,
		)
		if err != nil {
			return nil, err
		}

		// Parse destination feature bits.
		destinationFeatures = UnmarshalFeatures(in.DestFeatures)
	}

	// We need to subtract the final delta before passing it into path
	// finding. The optimal path is independent of the final cltv delta and
	// the path finding algorithm is unaware of this value.
	cltvLimit -= uint32(finalCLTVDelta)

	ignoredNodes, ignoredPairs, err := r.parseIgnored(in)
	if err != nil {
		return nil, err
	}

	restrictions := &routing.RestrictParams{
		FeeLimit: feeLimit,
		ProbabilitySource: func(fromNode, toNode route.Vertex,
			amt lnwire.MilliSatoshi,
			capacity btcutil.Amount) float64 {

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
				fromNode, toNode, amt, capacity,
			)
		},
		DestCustomRecords:     record.CustomSet(in.DestCustomRecords),
		CltvLimit:             cltvLimit,
		DestFeatures:          destinationFeatures,
		BlindedPaymentPathSet: blindedPathSet,
	}

	// We set the outgoing channel restrictions if the user provides a
	// list of channel ids. We also handle the case where the user
	// provides the deprecated `OutgoingChanId` field.
	switch {
	case len(in.OutgoingChanIds) > 0 && in.OutgoingChanId != 0:
		return nil, errors.New("outgoing_chan_id and " +
			"outgoing_chan_ids cannot both be set")

	case len(in.OutgoingChanIds) > 0:
		restrictions.OutgoingChannelIDs = in.OutgoingChanIds

	case in.OutgoingChanId != 0:
		restrictions.OutgoingChannelIDs = []uint64{in.OutgoingChanId}
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

	return routing.NewRouteRequest(
		sourcePubKey, targetPubKey, amt, in.TimePref, restrictions,
		customRecords, routeHintEdges, blindedPathSet,
		finalCLTVDelta,
	)
}

func parseBlindedPaymentPaths(in *lnrpc.QueryRoutesRequest) (
	*routing.BlindedPaymentPathSet, error) {

	if len(in.PubKey) != 0 {
		return nil, fmt.Errorf("target pubkey: %x should not be set "+
			"when blinded path is provided", in.PubKey)
	}

	if len(in.RouteHints) > 0 {
		return nil, errors.New("route hints and blinded path can't " +
			"both be set")
	}

	if in.FinalCltvDelta != 0 {
		return nil, errors.New("final cltv delta should be " +
			"zero for blinded paths")
	}

	// For blinded paths, we get one set of features for the relaying
	// intermediate nodes and the final destination. We don't allow the
	// destination feature bit field for regular payments to be set, as
	// this could lead to ambiguity.
	if len(in.DestFeatures) > 0 {
		return nil, errors.New("destination features should " +
			"be populated in blinded path")
	}

	paths := make([]*routing.BlindedPayment, len(in.BlindedPaymentPaths))
	for i, paymentPath := range in.BlindedPaymentPaths {
		blindedPmt, err := unmarshalBlindedPayment(paymentPath)
		if err != nil {
			return nil, fmt.Errorf("parse blinded payment: %w", err)
		}

		if err := blindedPmt.Validate(); err != nil {
			return nil, fmt.Errorf("invalid blinded path: %w", err)
		}

		paths[i] = blindedPmt
	}

	return routing.NewBlindedPaymentPathSet(paths)
}

func unmarshalBlindedPayment(rpcPayment *lnrpc.BlindedPaymentPath) (
	*routing.BlindedPayment, error) {

	if rpcPayment == nil {
		return nil, errors.New("nil blinded payment")
	}

	path, err := unmarshalBlindedPaymentPaths(rpcPayment.BlindedPath)
	if err != nil {
		return nil, err
	}

	features := UnmarshalFeatures(rpcPayment.Features)

	return &routing.BlindedPayment{
		BlindedPath:         path,
		CltvExpiryDelta:     uint16(rpcPayment.TotalCltvDelta),
		BaseFee:             uint32(rpcPayment.BaseFeeMsat),
		ProportionalFeeRate: rpcPayment.ProportionalFeeRate,
		HtlcMinimum:         rpcPayment.HtlcMinMsat,
		HtlcMaximum:         rpcPayment.HtlcMaxMsat,
		Features:            features,
	}, nil
}

func unmarshalBlindedPaymentPaths(rpcPath *lnrpc.BlindedPath) (
	*sphinx.BlindedPath, error) {

	if rpcPath == nil {
		return nil, errors.New("blinded path required when blinded " +
			"route is provided")
	}

	introduction, err := btcec.ParsePubKey(rpcPath.IntroductionNode)
	if err != nil {
		return nil, err
	}

	blinding, err := btcec.ParsePubKey(rpcPath.BlindingPoint)
	if err != nil {
		return nil, err
	}

	if len(rpcPath.BlindedHops) < 1 {
		return nil, errors.New("at least 1 blinded hops required")
	}

	path := &sphinx.BlindedPath{
		IntroductionPoint: introduction,
		BlindingPoint:     blinding,
		BlindedHops: make(
			[]*sphinx.BlindedHopInfo, len(rpcPath.BlindedHops),
		),
	}

	for i, hop := range rpcPath.BlindedHops {
		path.BlindedHops[i], err = unmarshalBlindedHop(hop)
		if err != nil {
			return nil, err
		}
	}

	return path, nil
}

func unmarshalBlindedHop(rpcHop *lnrpc.BlindedHop) (*sphinx.BlindedHopInfo,
	error) {

	pubkey, err := btcec.ParsePubKey(rpcHop.BlindedNode)
	if err != nil {
		return nil, err
	}

	if len(rpcHop.EncryptedData) == 0 {
		return nil, errors.New("empty encrypted data not allowed")
	}

	return &sphinx.BlindedHopInfo{
		BlindedNodePub: pubkey,
		CipherText:     rpcHop.EncryptedData,
	}, nil
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
		TotalTimeLock:      route.TotalTimeLock,
		TotalFees:          int64(route.TotalFees().ToSatoshis()),
		TotalFeesMsat:      int64(route.TotalFees()),
		TotalAmt:           int64(route.TotalAmount.ToSatoshis()),
		TotalAmtMsat:       int64(route.TotalAmount),
		Hops:               make([]*lnrpc.Hop, len(route.Hops)),
		FirstHopAmountMsat: int64(route.FirstHopAmount.Val.Int()),
	}

	// Encode the route's custom channel data (if available).
	if len(route.FirstHopWireCustomRecords) > 0 {
		customData, err := route.FirstHopWireCustomRecords.Serialize()
		if err != nil {
			return nil, err
		}

		resp.CustomChannelData = customData

		// Allow the aux data parser to parse the custom records into
		// a human-readable JSON (if available).
		if r.ParseCustomChannelData != nil {
			// Store the original custom data to check if parsing
			// changed it.
			originalCustomData := make([]byte, len(customData))
			copy(originalCustomData, customData)

			err := r.ParseCustomChannelData(resp)
			if err != nil {
				return nil, err
			}

			// We make sure we only set this field if the parser
			// changed the data otherwise we might mistakenly
			// show other tlv custom wire data as custom channel
			// data.
			if bytes.Equal(
				originalCustomData, resp.CustomChannelData,
			) {

				resp.CustomChannelData = nil
			}
		}
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

		var amp *lnrpc.AMPRecord
		if hop.AMP != nil {
			rootShare := hop.AMP.RootShare()
			setID := hop.AMP.SetID()

			amp = &lnrpc.AMPRecord{
				RootShare:  rootShare[:],
				SetId:      setID[:],
				ChildIndex: hop.AMP.ChildIndex(),
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
			AmpRecord:     amp,
			Metadata:      hop.Metadata,
			EncryptedData: hop.EncryptedData,
			TotalAmtMsat:  uint64(hop.TotalAmtMsat),
		}

		if hop.BlindingPoint != nil {
			blinding := hop.BlindingPoint.SerializeCompressed()
			resp.Hops[i].BlindingPoint = blinding
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

	amp, err := UnmarshalAMP(rpcHop.AmpRecord)
	if err != nil {
		return nil, err
	}

	hop := &route.Hop{
		OutgoingTimeLock: rpcHop.Expiry,
		AmtToForward:     lnwire.MilliSatoshi(rpcHop.AmtToForwardMsat),
		PubKeyBytes:      pubkey,
		ChannelID:        rpcHop.ChanId,
		CustomRecords:    customRecords,
		LegacyPayload:    false,
		MPP:              mpp,
		AMP:              amp,
		EncryptedData:    rpcHop.EncryptedData,
		TotalAmtMsat:     lnwire.MilliSatoshi(rpcHop.TotalAmtMsat),
	}

	haveBlindingPoint := len(rpcHop.BlindingPoint) != 0
	if haveBlindingPoint {
		hop.BlindingPoint, err = btcec.ParsePubKey(
			rpcHop.BlindingPoint,
		)
		if err != nil {
			return nil, fmt.Errorf("blinding point: %w", err)
		}
	}

	if haveBlindingPoint && len(rpcHop.EncryptedData) == 0 {
		return nil, errors.New("encrypted data should be present if " +
			"blinding point is provided")
	}

	return hop, nil
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

	// Pass along time preference.
	if rpcPayReq.TimePref < -1 || rpcPayReq.TimePref > 1 {
		return nil, errors.New("time preference out of range")
	}
	payIntent.TimePref = rpcPayReq.TimePref

	// Pass along restrictions on the outgoing channels that may be used.
	payIntent.OutgoingChannelIDs = rpcPayReq.OutgoingChanIds

	// Add the deprecated single outgoing channel restriction if present.
	if rpcPayReq.OutgoingChanId != 0 {
		if payIntent.OutgoingChannelIDs != nil {
			return nil, errors.New("outgoing_chan_id and " +
				"outgoing_chan_ids are mutually exclusive")
		}

		payIntent.OutgoingChannelIDs = append(
			payIntent.OutgoingChannelIDs, rpcPayReq.OutgoingChanId,
		)
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

	// Attempt to parse the max parts value set by the user, if this value
	// isn't set, then we'll use the current default value for this
	// setting.
	maxParts := rpcPayReq.MaxParts
	if maxParts == 0 {
		maxParts = DefaultMaxParts
	}
	payIntent.MaxParts = maxParts

	// If this payment had a max shard amount specified, then we'll apply
	// that now, which'll force us to always make payment splits smaller
	// than this.
	if rpcPayReq.MaxShardSizeMsat > 0 {
		shardAmtMsat := lnwire.MilliSatoshi(rpcPayReq.MaxShardSizeMsat)
		payIntent.MaxShardAmt = &shardAmtMsat

		// If the requested max_parts exceeds the allowed limit, then we
		// cannot send the payment amount.
		if payIntent.MaxParts > MaxPartsUpperLimit {
			return nil, fmt.Errorf("requested max_parts (%v) "+
				"exceeds the allowed upper limit of %v; cannot"+
				" send payment amount with max_shard_size_msat"+
				"=%v", payIntent.MaxParts, MaxPartsUpperLimit,
				*payIntent.MaxShardAmt)
		}
	}

	// Take fee limit from request.
	payIntent.FeeLimit, err = lnrpc.UnmarshallAmt(
		rpcPayReq.FeeLimitSat, rpcPayReq.FeeLimitMsat,
	)
	if err != nil {
		return nil, err
	}

	customRecords := record.CustomSet(rpcPayReq.DestCustomRecords)
	if err := customRecords.Validate(); err != nil {
		return nil, err
	}
	payIntent.DestCustomRecords = customRecords

	// Keysend payments do not support MPP payments.
	//
	// NOTE: There is no need to validate the `MaxParts` value here because
	// it is set to 1 somewhere else in case it's a keysend payment.
	if customRecords.IsKeysend() {
		if payIntent.MaxShardAmt != nil {
			return nil, errors.New("keysend payments cannot " +
				"specify a max shard amount - MPP not " +
				"supported with keysend payments")
		}
	}

	firstHopRecords := lnwire.CustomRecords(rpcPayReq.FirstHopCustomRecords)
	if err := firstHopRecords.Validate(); err != nil {
		return nil, err
	}
	payIntent.FirstHopCustomRecords = firstHopRecords

	// If the experimental endorsement signal is not already set, propagate
	// a zero value field if configured to set this signal.
	if r.ShouldSetExpEndorsement() {
		if payIntent.FirstHopCustomRecords == nil {
			payIntent.FirstHopCustomRecords = make(
				map[uint64][]byte,
			)
		}

		t := uint64(lnwire.ExperimentalEndorsementType)
		if _, set := payIntent.FirstHopCustomRecords[t]; !set {
			payIntent.FirstHopCustomRecords[t] = []byte{
				lnwire.ExperimentalUnendorsed,
			}
		}
	}

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
			return nil, errors.New("payment_hash and payment_request " +
				"cannot appear together")

		case rpcPayReq.FinalCltvDelta != 0:
			return nil, errors.New("final_cltv_delta and payment_request " +
				"cannot appear together")
		}

		payReq, err := zpay32.Decode(
			rpcPayReq.PaymentRequest, r.ActiveNetParams,
		)
		if err != nil {
			return nil, err
		}

		// Next, we'll ensure that this payreq hasn't already expired.
		err = ValidatePayReqExpiry(r.Clock, payReq)
		if err != nil {
			return nil, err
		}

		// An invoice must include either a payment address or
		// blinded paths.
		if payReq.PaymentAddr.IsNone() &&
			len(payReq.BlindedPaymentPaths) == 0 {

			return nil, errors.New("payment request must contain " +
				"either a payment address or blinded paths")
		}

		// If the amount was not included in the invoice, then we let
		// the payer specify the amount of satoshis they wish to send.
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
					"amount invoice")
			}

			payIntent.Amount = *payReq.MilliSat
		}

		if !payReq.Features.HasFeature(lnwire.MPPOptional) &&
			!payReq.Features.HasFeature(lnwire.AMPOptional) {

			payIntent.MaxParts = 1
		}

		payAddr := payReq.PaymentAddr
		if payReq.Features.HasFeature(lnwire.AMPOptional) {
			// The opt-in AMP flag is required to pay an AMP
			// invoice.
			if !rpcPayReq.Amp {
				return nil, fmt.Errorf("the AMP flag (--amp " +
					"or SendPaymentRequest.Amp) must be " +
					"set to pay an AMP invoice")
			}

			// Generate random SetID and root share.
			var setID [32]byte
			_, err = rand.Read(setID[:])
			if err != nil {
				return nil, err
			}

			var rootShare [32]byte
			_, err = rand.Read(rootShare[:])
			if err != nil {
				return nil, err
			}
			err := payIntent.SetAMP(&routing.AMPOptions{
				SetID:     setID,
				RootShare: rootShare,
			})
			if err != nil {
				return nil, err
			}

			// For AMP invoices, we'll allow users to override the
			// included payment addr to allow the invoice to be
			// pseudo-reusable, e.g. the invoice parameters are
			// reused (amt, cltv, hop hints, etc) even though the
			// payments will share different payment hashes.
			//
			// NOTE: This will only work when the peer has
			// spontaneous AMP payments enabled.
			if len(rpcPayReq.PaymentAddr) > 0 {
				var addr [32]byte
				copy(addr[:], rpcPayReq.PaymentAddr)
				payAddr = fn.Some(addr)
			}
		} else {
			err = payIntent.SetPaymentHash(*payReq.PaymentHash)
			if err != nil {
				return nil, err
			}
		}

		destKey := payReq.Destination.SerializeCompressed()
		copy(payIntent.Target[:], destKey)

		payIntent.FinalCLTVDelta = uint16(payReq.MinFinalCLTVExpiry())
		payIntent.RouteHints = append(
			payIntent.RouteHints, payReq.RouteHints...,
		)
		payIntent.DestFeatures = payReq.Features
		payIntent.PaymentAddr = payAddr
		payIntent.PaymentRequest = []byte(rpcPayReq.PaymentRequest)
		payIntent.Metadata = payReq.Metadata

		if len(payReq.BlindedPaymentPaths) > 0 {
			pathSet, err := BuildBlindedPathSet(
				payReq.BlindedPaymentPaths,
			)
			if err != nil {
				return nil, err
			}
			payIntent.BlindedPathSet = pathSet

			// Replace the target node with the target public key
			// of the blinded path set.
			copy(
				payIntent.Target[:],
				pathSet.TargetPubKey().SerializeCompressed(),
			)

			pathFeatures := pathSet.Features()
			if !pathFeatures.IsEmpty() {
				payIntent.DestFeatures = pathFeatures.Clone()
			}
		}
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

		// Parse destination feature bits.
		features := UnmarshalFeatures(rpcPayReq.DestFeatures)

		// Validate the features if any was specified.
		if features != nil {
			err = feature.ValidateDeps(features)
			if err != nil {
				return nil, err
			}
		}

		// If this is an AMP payment, we must generate the initial
		// randomness.
		if rpcPayReq.Amp {
			// If no destination features were specified, we set
			// those necessary for AMP payments.
			if features == nil {
				ampFeatures := []lnrpc.FeatureBit{
					lnrpc.FeatureBit_TLV_ONION_OPT,
					lnrpc.FeatureBit_PAYMENT_ADDR_OPT,
					lnrpc.FeatureBit_AMP_OPT,
				}

				features = UnmarshalFeatures(ampFeatures)
			}

			// First make sure the destination supports AMP.
			if !features.HasFeature(lnwire.AMPOptional) {
				return nil, fmt.Errorf("destination doesn't " +
					"support AMP payments")
			}

			// If no payment address is set, generate a random one.
			var payAddr [32]byte
			if len(rpcPayReq.PaymentAddr) == 0 {
				_, err = rand.Read(payAddr[:])
				if err != nil {
					return nil, err
				}
			} else {
				copy(payAddr[:], rpcPayReq.PaymentAddr)
			}
			payIntent.PaymentAddr = fn.Some(payAddr)

			// Generate random SetID and root share.
			var setID [32]byte
			_, err = rand.Read(setID[:])
			if err != nil {
				return nil, err
			}

			var rootShare [32]byte
			_, err = rand.Read(rootShare[:])
			if err != nil {
				return nil, err
			}
			err := payIntent.SetAMP(&routing.AMPOptions{
				SetID:     setID,
				RootShare: rootShare,
			})
			if err != nil {
				return nil, err
			}
		} else {
			// Payment hash.
			paymentHash, err := lntypes.MakeHash(rpcPayReq.PaymentHash)
			if err != nil {
				return nil, err
			}

			err = payIntent.SetPaymentHash(paymentHash)
			if err != nil {
				return nil, err
			}

			// If the payment addresses is specified, then we'll
			// also populate that now as well.
			if len(rpcPayReq.PaymentAddr) != 0 {
				var payAddr [32]byte
				copy(payAddr[:], rpcPayReq.PaymentAddr)

				payIntent.PaymentAddr = fn.Some(payAddr)
			}
		}

		payIntent.DestFeatures = features
	}

	// Validate that the MPP parameters are compatible with the
	// payment amount. In other words, the parameters are invalid if
	// they do not permit sending the full payment amount.
	if payIntent.MaxShardAmt != nil {
		maxPossibleAmount := (*payIntent.MaxShardAmt) *
			lnwire.MilliSatoshi(payIntent.MaxParts)

		if payIntent.Amount > maxPossibleAmount {
			return nil, fmt.Errorf("payment amount %v exceeds "+
				"maximum possible amount %v with max_parts=%v "+
				"and max_shard_size_msat=%v", payIntent.Amount,
				maxPossibleAmount, payIntent.MaxParts,
				*payIntent.MaxShardAmt,
			)
		}
	}

	// Do bounds checking with the block padding so the router isn't
	// left with a zombie payment in case the user messes up.
	err = routing.ValidateCLTVLimit(
		payIntent.CltvLimit, payIntent.FinalCLTVDelta, true,
	)
	if err != nil {
		return nil, err
	}

	// Check for disallowed payments to self.
	if !rpcPayReq.AllowSelfPayment && payIntent.Target == r.SelfNode {
		return nil, errors.New("self-payments not allowed")
	}

	return payIntent, nil
}

// BuildBlindedPathSet marshals a set of zpay32.BlindedPaymentPath and uses
// the result to build a new routing.BlindedPaymentPathSet.
func BuildBlindedPathSet(paths []*zpay32.BlindedPaymentPath) (
	*routing.BlindedPaymentPathSet, error) {

	marshalledPaths := make([]*routing.BlindedPayment, len(paths))
	for i, path := range paths {
		paymentPath := marshalBlindedPayment(path)

		err := paymentPath.Validate()
		if err != nil {
			return nil, err
		}

		marshalledPaths[i] = paymentPath
	}

	return routing.NewBlindedPaymentPathSet(marshalledPaths)
}

// marshalBlindedPayment marshals a zpay32.BLindedPaymentPath into a
// routing.BlindedPayment.
func marshalBlindedPayment(
	path *zpay32.BlindedPaymentPath) *routing.BlindedPayment {

	return &routing.BlindedPayment{
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: path.Hops[0].BlindedNodePub,
			BlindingPoint:     path.FirstEphemeralBlindingPoint,
			BlindedHops:       path.Hops,
		},
		BaseFee:             path.FeeBaseMsat,
		ProportionalFeeRate: path.FeeRate,
		CltvExpiryDelta:     path.CltvExpiryDelta,
		HtlcMinimum:         path.HTLCMinMsat,
		HtlcMaximum:         path.HTLCMaxMsat,
		Features:            path.Features,
	}
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

	pubkey, err := btcec.ParsePubKey(pubBytes)
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

// MarshalFeatures converts a feature vector into a list of uint32's.
func MarshalFeatures(feats *lnwire.FeatureVector) []lnrpc.FeatureBit {
	var featureBits []lnrpc.FeatureBit
	for feature := range feats.Features() {
		featureBits = append(featureBits, lnrpc.FeatureBit(feature))
	}

	return featureBits
}

// UnmarshalFeatures converts a list of uint32's into a valid feature vector.
// This method checks that feature bit pairs aren't assigned together, and
// validates transitive dependencies.
func UnmarshalFeatures(rpcFeatures []lnrpc.FeatureBit) *lnwire.FeatureVector {
	// If no destination features are specified we'll return nil to signal
	// that the router should try to use the graph as a fallback.
	if rpcFeatures == nil {
		return nil
	}

	raw := lnwire.NewRawFeatureVector()
	for _, bit := range rpcFeatures {
		// Even though the spec says that the writer of a feature vector
		// should never set both the required and optional bits of a
		// feature, it also says that if we receive a vector with both
		// bits set, then we should just treat the feature as required.
		// Therefore, we don't use SafeSet here when parsing a peer's
		// feature bits and just set the feature no matter what so that
		// if both are set then IsRequired returns true.
		raw.Set(lnwire.FeatureBit(bit))
	}

	return lnwire.NewFeatureVector(raw, lnwire.Features)
}

// ValidatePayReqExpiry checks if the passed payment request has expired. In
// the case it has expired, an error will be returned.
func ValidatePayReqExpiry(clock clock.Clock, payReq *zpay32.Invoice) error {
	expiry := payReq.Expiry()
	validUntil := payReq.Timestamp.Add(expiry)
	if clock.Now().After(validUntil) {
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

func UnmarshalAMP(reqAMP *lnrpc.AMPRecord) (*record.AMP, error) {
	if reqAMP == nil {
		return nil, nil
	}

	reqRootShare := reqAMP.RootShare
	reqSetID := reqAMP.SetId

	switch {
	case len(reqRootShare) != 32:
		return nil, errors.New("AMP root_share must be 32 bytes")

	case len(reqSetID) != 32:
		return nil, errors.New("AMP set_id must be 32 bytes")
	}

	var (
		rootShare [32]byte
		setID     [32]byte
	)
	copy(rootShare[:], reqRootShare)
	copy(setID[:], reqSetID)

	return record.NewAMP(rootShare, setID, reqAMP.ChildIndex), nil
}

// MarshalHTLCAttempt constructs an RPC HTLCAttempt from the db representation.
func (r *RouterBackend) MarshalHTLCAttempt(
	htlc paymentsdb.HTLCAttempt) (*lnrpc.HTLCAttempt, error) {

	route, err := r.MarshallRoute(&htlc.Route)
	if err != nil {
		return nil, err
	}

	rpcAttempt := &lnrpc.HTLCAttempt{
		AttemptId:     htlc.AttemptID,
		AttemptTimeNs: MarshalTimeNano(htlc.AttemptTime),
		Route:         route,
	}

	switch {
	case htlc.Settle != nil:
		rpcAttempt.Status = lnrpc.HTLCAttempt_SUCCEEDED
		rpcAttempt.ResolveTimeNs = MarshalTimeNano(
			htlc.Settle.SettleTime,
		)
		rpcAttempt.Preimage = htlc.Settle.Preimage[:]

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
func marshallHtlcFailure(failure *paymentsdb.HTLCFailInfo) (*lnrpc.Failure,
	error) {

	rpcFailure := &lnrpc.Failure{
		FailureSourceIndex: failure.FailureSourceIndex,
	}

	switch failure.Reason {
	case paymentsdb.HTLCFailUnknown:
		rpcFailure.Code = lnrpc.Failure_UNKNOWN_FAILURE

	case paymentsdb.HTLCFailUnreadable:
		rpcFailure.Code = lnrpc.Failure_UNREADABLE_FAILURE

	case paymentsdb.HTLCFailInternal:
		rpcFailure.Code = lnrpc.Failure_INTERNAL_FAILURE

	case paymentsdb.HTLCFailMessage:
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

	case *lnwire.InvalidOnionPayload:
		response.Code = lnrpc.Failure_INVALID_ONION_PAYLOAD

	case *lnwire.FailInvalidBlinding:
		response.Code = lnrpc.Failure_INVALID_ONION_BLINDING
		response.OnionSha_256 = onionErr.OnionSHA256[:]

	case nil:
		response.Code = lnrpc.Failure_UNKNOWN_FAILURE

	default:
		return fmt.Errorf("cannot marshall failure %T", onionErr)
	}

	return nil
}

// marshallChannelUpdate marshalls a channel update as received over the wire to
// the router rpc format.
func marshallChannelUpdate(update *lnwire.ChannelUpdate1) *lnrpc.ChannelUpdate {
	if update == nil {
		return nil
	}

	return &lnrpc.ChannelUpdate{
		Signature:       update.Signature.RawBytes(),
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

// MarshallPayment marshall a payment to its rpc representation.
func (r *RouterBackend) MarshallPayment(payment *paymentsdb.MPPayment) (
	*lnrpc.Payment, error) {

	// Fetch the payment's preimage and the total paid in fees.
	var (
		fee      lnwire.MilliSatoshi
		preimage lntypes.Preimage
	)
	for _, htlc := range payment.HTLCs {
		// If any of the htlcs have settled, extract a valid
		// preimage.
		if htlc.Settle != nil {
			preimage = htlc.Settle.Preimage
			fee += htlc.Route.TotalFees()
		}
	}

	msatValue := int64(payment.Info.Value)
	satValue := int64(payment.Info.Value.ToSatoshis())

	status, err := convertPaymentStatus(
		payment.Status, r.UseStatusInitiated,
	)
	if err != nil {
		return nil, err
	}

	htlcs := make([]*lnrpc.HTLCAttempt, 0, len(payment.HTLCs))
	for _, dbHTLC := range payment.HTLCs {
		htlc, err := r.MarshalHTLCAttempt(dbHTLC)
		if err != nil {
			return nil, err
		}

		htlcs = append(htlcs, htlc)
	}

	paymentID := payment.Info.PaymentIdentifier
	creationTimeNS := MarshalTimeNano(payment.Info.CreationTime)

	failureReason, err := marshallPaymentFailureReason(
		payment.FailureReason,
	)
	if err != nil {
		return nil, err
	}

	return &lnrpc.Payment{
		// TODO: set this to setID for AMP-payments?
		PaymentHash:           hex.EncodeToString(paymentID[:]),
		Value:                 satValue,
		ValueMsat:             msatValue,
		ValueSat:              satValue,
		CreationDate:          payment.Info.CreationTime.Unix(),
		CreationTimeNs:        creationTimeNS,
		Fee:                   int64(fee.ToSatoshis()),
		FeeSat:                int64(fee.ToSatoshis()),
		FeeMsat:               int64(fee),
		PaymentPreimage:       hex.EncodeToString(preimage[:]),
		PaymentRequest:        string(payment.Info.PaymentRequest),
		Status:                status,
		Htlcs:                 htlcs,
		PaymentIndex:          payment.SequenceNum,
		FailureReason:         failureReason,
		FirstHopCustomRecords: payment.Info.FirstHopCustomRecords,
	}, nil
}

// convertPaymentStatus converts a channeldb.PaymentStatus to the type expected
// by the RPC.
func convertPaymentStatus(dbStatus paymentsdb.PaymentStatus, useInit bool) (
	lnrpc.Payment_PaymentStatus, error) {

	switch dbStatus {
	case paymentsdb.StatusInitiated:
		// If the client understands the new status, return it.
		if useInit {
			return lnrpc.Payment_INITIATED, nil
		}

		// Otherwise remain the old behavior.
		return lnrpc.Payment_IN_FLIGHT, nil

	case paymentsdb.StatusInFlight:
		return lnrpc.Payment_IN_FLIGHT, nil

	case paymentsdb.StatusSucceeded:
		return lnrpc.Payment_SUCCEEDED, nil

	case paymentsdb.StatusFailed:
		return lnrpc.Payment_FAILED, nil

	default:
		return 0, fmt.Errorf("unhandled payment status %v", dbStatus)
	}
}

// marshallPaymentFailureReason marshalls the failure reason to the corresponding rpc
// type.
func marshallPaymentFailureReason(reason *paymentsdb.FailureReason) (
	lnrpc.PaymentFailureReason, error) {

	if reason == nil {
		return lnrpc.PaymentFailureReason_FAILURE_REASON_NONE, nil
	}

	switch *reason {
	case paymentsdb.FailureReasonTimeout:
		return lnrpc.PaymentFailureReason_FAILURE_REASON_TIMEOUT, nil

	case paymentsdb.FailureReasonNoRoute:
		return lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE, nil

	case paymentsdb.FailureReasonError:
		return lnrpc.PaymentFailureReason_FAILURE_REASON_ERROR, nil

	case paymentsdb.FailureReasonPaymentDetails:
		return lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS, nil

	case paymentsdb.FailureReasonInsufficientBalance:
		return lnrpc.PaymentFailureReason_FAILURE_REASON_INSUFFICIENT_BALANCE, nil

	case paymentsdb.FailureReasonCanceled:
		return lnrpc.PaymentFailureReason_FAILURE_REASON_CANCELED, nil
	}

	return 0, errors.New("unknown failure reason")
}
