package routerrpc

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	context "golang.org/x/net/context"
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

	// FindRoutes is a closure that abstracts away how we locate/query for
	// routes.
	FindRoutes func(source, target route.Vertex,
		amt lnwire.MilliSatoshi, restrictions *routing.RestrictParams,
		numPaths uint32, finalExpiry ...uint16) (
		[]*route.Route, error)
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

		if len(pubKeyBytes) != 33 {
			return route.Vertex{},
				errors.New("invalid key length")
		}

		var v route.Vertex
		copy(v[:], pubKeyBytes)

		return v, nil
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
	amt := btcutil.Amount(in.Amt)
	amtMSat := lnwire.NewMSatFromSatoshis(amt)
	if amtMSat > r.MaxPaymentMSat {
		return nil, fmt.Errorf("payment of %v is too large, max payment "+
			"allowed is %v", amt, r.MaxPaymentMSat.ToSatoshis())
	}

	// Unmarshall restrictions from request.
	feeLimit := calculateFeeLimit(in.FeeLimit, amtMSat)

	ignoredNodes := make(map[route.Vertex]struct{})
	for _, ignorePubKey := range in.IgnoredNodes {
		if len(ignorePubKey) != 33 {
			return nil, fmt.Errorf("invalid ignore node pubkey")
		}
		var ignoreVertex route.Vertex
		copy(ignoreVertex[:], ignorePubKey)
		ignoredNodes[ignoreVertex] = struct{}{}
	}

	ignoredEdges := make(map[routing.EdgeLocator]struct{})
	for _, ignoredEdge := range in.IgnoredEdges {
		locator := routing.EdgeLocator{
			ChannelID: ignoredEdge.ChannelId,
		}
		if ignoredEdge.DirectionReverse {
			locator.Direction = 1
		}
		ignoredEdges[locator] = struct{}{}
	}

	restrictions := &routing.RestrictParams{
		FeeLimit:     feeLimit,
		IgnoredNodes: ignoredNodes,
		IgnoredEdges: ignoredEdges,
	}

	// numRoutes will default to 10 if not specified explicitly.
	numRoutesIn := uint32(in.NumRoutes)
	if numRoutesIn == 0 {
		numRoutesIn = 10
	}

	// Query the channel router for a possible path to the destination that
	// can carry `in.Amt` satoshis _including_ the total fee required on
	// the route.
	var (
		routes  []*route.Route
		findErr error
	)

	if in.FinalCltvDelta == 0 {
		routes, findErr = r.FindRoutes(
			sourcePubKey, targetPubKey, amtMSat, restrictions,
			numRoutesIn,
		)
	} else {
		routes, findErr = r.FindRoutes(
			sourcePubKey, targetPubKey, amtMSat, restrictions,
			numRoutesIn, uint16(in.FinalCltvDelta),
		)
	}
	if findErr != nil {
		return nil, findErr
	}

	// As the number of returned routes can be less than the number of
	// requested routes, we'll clamp down the length of the response to the
	// minimum of the two.
	numRoutes := uint32(len(routes))
	if numRoutesIn < numRoutes {
		numRoutes = numRoutesIn
	}

	// For each valid route, we'll convert the result into the format
	// required by the RPC system.
	routeResp := &lnrpc.QueryRoutesResponse{
		Routes: make([]*lnrpc.Route, 0, in.NumRoutes),
	}
	for i := uint32(0); i < numRoutes; i++ {
		routeResp.Routes = append(
			routeResp.Routes,
			r.MarshallRoute(routes[i]),
		)
	}

	return routeResp, nil
}

// calculateFeeLimit returns the fee limit in millisatoshis. If a percentage
// based fee limit has been requested, we'll factor in the ratio provided with
// the amount of the payment.
func calculateFeeLimit(feeLimit *lnrpc.FeeLimit,
	amount lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	switch feeLimit.GetLimit().(type) {
	case *lnrpc.FeeLimit_Fixed:
		return lnwire.NewMSatFromSatoshis(
			btcutil.Amount(feeLimit.GetFixed()),
		)
	case *lnrpc.FeeLimit_Percent:
		return amount * lnwire.MilliSatoshi(feeLimit.GetPercent()) / 100
	default:
		// If a fee limit was not specified, we'll use the payment's
		// amount as an upper bound in order to avoid payment attempts
		// from incurring fees higher than the payment amount itself.
		return amount
	}
}

// MarshallRoute marshalls an internal route to an rpc route struct.
func (r *RouterBackend) MarshallRoute(route *route.Route) *lnrpc.Route {
	resp := &lnrpc.Route{
		TotalTimeLock: route.TotalTimeLock,
		TotalFees:     int64(route.TotalFees.ToSatoshis()),
		TotalFeesMsat: int64(route.TotalFees),
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
		}
		incomingAmt = hop.AmtToForward
	}

	return resp
}
