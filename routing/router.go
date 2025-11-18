package routing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/amp"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/routing/shards"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// DefaultPayAttemptTimeout is the default payment attempt timeout. The
	// payment attempt timeout defines the duration after which we stop
	// trying more routes for a payment.
	DefaultPayAttemptTimeout = time.Second * 60

	// MinCLTVDelta is the minimum CLTV value accepted by LND for all
	// timelock deltas. This includes both forwarding CLTV deltas set on
	// channel updates, as well as final CLTV deltas used to create BOLT 11
	// payment requests.
	//
	// NOTE: For payment requests, BOLT 11 stipulates that a final CLTV
	// delta of 9 should be used when no value is decoded. This however
	// leads to inflexibility in upgrading this default parameter, since it
	// can create inconsistencies around the assumed value between sender
	// and receiver. Specifically, if the receiver assumes a higher value
	// than the sender, the receiver will always see the received HTLCs as
	// invalid due to their timelock not meeting the required delta.
	//
	// We skirt this by always setting an explicit CLTV delta when creating
	// invoices. This allows LND nodes to freely update the minimum without
	// creating incompatibilities during the upgrade process. For some time
	// LND has used an explicit default final CLTV delta of 40 blocks for
	// bitcoin, though we now clamp the lower end of this
	// range for user-chosen deltas to 18 blocks to be conservative.
	MinCLTVDelta = 18

	// MaxCLTVDelta is the maximum CLTV value accepted by LND for all
	// timelock deltas.
	MaxCLTVDelta = math.MaxUint16
)

var (
	// ErrRouterShuttingDown is returned if the router is in the process of
	// shutting down.
	ErrRouterShuttingDown = fmt.Errorf("router shutting down")

	// ErrSelfIntro is a failure returned when the source node of a
	// route request is also the introduction node. This is not yet
	// supported because LND does not support blinded forwardingg.
	ErrSelfIntro = errors.New("introduction point as own node not " +
		"supported")

	// ErrHintsAndBlinded is returned if a route request has both
	// bolt 11 route hints and a blinded path set.
	ErrHintsAndBlinded = errors.New("bolt 11 route hints and blinded " +
		"paths are mutually exclusive")

	// ErrExpiryAndBlinded is returned if a final cltv and a blinded path
	// are provided, as the cltv should be provided within the blinded
	// path.
	ErrExpiryAndBlinded = errors.New("final cltv delta and blinded " +
		"paths are mutually exclusive")

	// ErrTargetAndBlinded is returned is a target destination and a
	// blinded path are both set (as the target is inferred from the
	// blinded path).
	ErrTargetAndBlinded = errors.New("target node and blinded paths " +
		"are mutually exclusive")

	// ErrNoTarget is returned when the target node for a route is not
	// provided by either a blinded route or a cleartext pubkey.
	ErrNoTarget = errors.New("destination not set in target or blinded " +
		"path")

	// ErrSkipTempErr is returned when a non-MPP is made yet the
	// skipTempErr flag is set.
	ErrSkipTempErr = errors.New("cannot skip temp error for non-MPP")
)

// PaymentAttemptDispatcher is used by the router to send payment attempts onto
// the network, and receive their results.
type PaymentAttemptDispatcher interface {
	// SendHTLC is a function that directs a link-layer switch to
	// forward a fully encoded payment to the first hop in the route
	// denoted by its public key. A non-nil error is to be returned if the
	// payment was unsuccessful.
	SendHTLC(firstHop lnwire.ShortChannelID,
		attemptID uint64,
		htlcAdd *lnwire.UpdateAddHTLC) error

	// GetAttemptResult returns the result of the payment attempt with
	// the given attemptID. The paymentHash should be set to the payment's
	// overall hash, or in case of AMP payments the payment's unique
	// identifier.
	//
	// The method returns a channel where the payment result will be sent
	// when available, or an error is encountered during forwarding. When a
	// result is received on the channel, the HTLC is guaranteed to no
	// longer be in flight.  The switch shutting down is signaled by
	// closing the channel. If the attemptID is unknown,
	// ErrPaymentIDNotFound will be returned.
	GetAttemptResult(attemptID uint64, paymentHash lntypes.Hash,
		deobfuscator htlcswitch.ErrorDecrypter) (
		<-chan *htlcswitch.PaymentResult, error)

	// CleanStore calls the underlying result store, telling it is safe to
	// delete all entries except the ones in the keepPids map. This should
	// be called periodically to let the switch clean up payment results
	// that we have handled.
	// NOTE: New payment attempts MUST NOT be made after the keepPids map
	// has been created and this method has returned.
	CleanStore(keepPids map[uint64]struct{}) error

	// HasAttemptResult reads the network result store to fetch the
	// specified attempt. Returns true if the attempt result exists.
	//
	// NOTE: This method is used and should only be used by the router to
	// resume payments during startup. It can be viewed as a subset of
	// `GetAttemptResult` in terms of its functionality, and can be removed
	// once we move the construction of `UpdateAddHTLC` and
	// `ErrorDecrypter` into `htlcswitch`.
	HasAttemptResult(attemptID uint64) (bool, error)
}

// PaymentSessionSource is an interface that defines a source for the router to
// retrieve new payment sessions.
type PaymentSessionSource interface {
	// NewPaymentSession creates a new payment session that will produce
	// routes to the given target. An optional set of routing hints can be
	// provided in order to populate additional edges to explore when
	// finding a path to the payment's destination.
	NewPaymentSession(p *LightningPayment,
		firstHopBlob fn.Option[tlv.Blob],
		ts fn.Option[htlcswitch.AuxTrafficShaper]) (PaymentSession,
		error)

	// NewPaymentSessionEmpty creates a new paymentSession instance that is
	// empty, and will be exhausted immediately. Used for failure reporting
	// to missioncontrol for resumed payment we don't want to make more
	// attempts for.
	NewPaymentSessionEmpty() PaymentSession
}

// MissionControlQuerier is an interface that exposes failure reporting and
// probability estimation.
type MissionControlQuerier interface {
	// ReportPaymentFail reports a failed payment to mission control as
	// input for future probability estimates. It returns a bool indicating
	// whether this error is a final error and no further payment attempts
	// need to be made.
	ReportPaymentFail(attemptID uint64, rt *route.Route,
		failureSourceIdx *int, failure lnwire.FailureMessage) (
		*paymentsdb.FailureReason, error)

	// ReportPaymentSuccess reports a successful payment to mission control
	// as input for future probability estimates.
	ReportPaymentSuccess(attemptID uint64, rt *route.Route) error

	// GetProbability is expected to return the success probability of a
	// payment from fromNode along edge.
	GetProbability(fromNode, toNode route.Vertex,
		amt lnwire.MilliSatoshi, capacity btcutil.Amount) float64
}

// FeeSchema is the set fee configuration for a Lightning Node on the network.
// Using the coefficients described within the schema, the required fee to
// forward outgoing payments can be derived.
type FeeSchema struct {
	// BaseFee is the base amount of milli-satoshis that will be chained
	// for ANY payment forwarded.
	BaseFee lnwire.MilliSatoshi

	// FeeRate is the rate that will be charged for forwarding payments.
	// This value should be interpreted as the numerator for a fraction
	// (fixed point arithmetic) whose denominator is 1 million. As a result
	// the effective fee rate charged per mSAT will be: (amount *
	// FeeRate/1,000,000).
	FeeRate uint32

	// InboundFee is the inbound fee schedule that applies to forwards
	// coming in through a channel to which this FeeSchema pertains.
	InboundFee fn.Option[models.InboundFee]
}

// ChannelPolicy holds the parameters that determine the policy we enforce
// when forwarding payments on a channel. These parameters are communicated
// to the rest of the network in ChannelUpdate messages.
type ChannelPolicy struct {
	// FeeSchema holds the fee configuration for a channel.
	FeeSchema

	// TimeLockDelta is the required HTLC timelock delta to be used
	// when forwarding payments.
	TimeLockDelta uint32

	// MaxHTLC is the maximum HTLC size including fees we are allowed to
	// forward over this channel.
	MaxHTLC lnwire.MilliSatoshi

	// MinHTLC is the minimum HTLC size including fees we are allowed to
	// forward over this channel.
	MinHTLC *lnwire.MilliSatoshi
}

// Config defines the configuration for the ChannelRouter. ALL elements within
// the configuration MUST be non-nil for the ChannelRouter to carry out its
// duties.
type Config struct {
	// SelfNode is the public key of the node that this channel router
	// belongs to.
	SelfNode route.Vertex

	// RoutingGraph is a graph source that will be used for pathfinding.
	RoutingGraph Graph

	// Chain is the router's source to the most up-to-date blockchain data.
	// All incoming advertised channels will be checked against the chain
	// to ensure that the channels advertised are still open.
	Chain lnwallet.BlockChainIO

	// Payer is an instance of a PaymentAttemptDispatcher and is used by
	// the router to send payment attempts onto the network, and receive
	// their results.
	Payer PaymentAttemptDispatcher

	// Control keeps track of the status of ongoing payments, ensuring we
	// can properly resume them across restarts.
	Control ControlTower

	// MissionControl is a shared memory of sorts that executions of
	// payment path finding use in order to remember which vertexes/edges
	// were pruned from prior attempts. During SendPayment execution,
	// errors sent by nodes are mapped into a vertex or edge to be pruned.
	// Each run will then take into account this set of pruned
	// vertexes/edges to reduce route failure and pass on graph information
	// gained to the next execution.
	MissionControl MissionControlQuerier

	// SessionSource defines a source for the router to retrieve new payment
	// sessions.
	SessionSource PaymentSessionSource

	// GetLink is a method that allows the router to query the lower link
	// layer to determine the up-to-date available bandwidth at a
	// prospective link to be traversed. If the link isn't available, then
	// a value of zero should be returned. Otherwise, the current up-to-
	// date knowledge of the available bandwidth of the link should be
	// returned.
	GetLink getLinkQuery

	// NextPaymentID is a method that guarantees to return a new, unique ID
	// each time it is called. This is used by the router to generate a
	// unique payment ID for each payment it attempts to send, such that
	// the switch can properly handle the HTLC.
	NextPaymentID func() (uint64, error)

	// PathFindingConfig defines global path finding parameters.
	PathFindingConfig PathFindingConfig

	// Clock is mockable time provider.
	Clock clock.Clock

	// ApplyChannelUpdate can be called to apply a new channel update to the
	// graph that we received from a payment failure.
	ApplyChannelUpdate func(msg *lnwire.ChannelUpdate1) bool

	// ClosedSCIDs is used by the router to fetch closed channels.
	//
	// TODO(yy): remove it once the root cause of stuck payments is found.
	ClosedSCIDs map[lnwire.ShortChannelID]struct{}

	// TrafficShaper is an optional traffic shaper that can be used to
	// control the outgoing channel of a payment.
	TrafficShaper fn.Option[htlcswitch.AuxTrafficShaper]

	// KeepFailedPaymentAttempts indicates whether to keep failed payment
	// attempts in the database.
	KeepFailedPaymentAttempts bool
}

// EdgeLocator is a struct used to identify a specific edge.
type EdgeLocator struct {
	// ChannelID is the channel of this edge.
	ChannelID uint64

	// Direction takes the value of 0 or 1 and is identical in definition to
	// the channel direction flag. A value of 0 means the direction from the
	// lower node pubkey to the higher.
	Direction uint8
}

// String returns a human-readable version of the edgeLocator values.
func (e *EdgeLocator) String() string {
	return fmt.Sprintf("%v:%v", e.ChannelID, e.Direction)
}

// ChannelRouter is the layer 3 router within the Lightning stack. Below the
// ChannelRouter is the HtlcSwitch, and below that is the Bitcoin blockchain
// itself. The primary role of the ChannelRouter is to respond to queries for
// potential routes that can support a payment amount, and also general graph
// reachability questions. The router will prune the channel graph
// automatically as new blocks are discovered which spend certain known funding
// outpoints, thereby closing their respective channels.
type ChannelRouter struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	// cfg is a copy of the configuration struct that the ChannelRouter was
	// initialized with.
	cfg *Config

	quit chan struct{}
	wg   sync.WaitGroup
}

// New creates a new instance of the ChannelRouter with the specified
// configuration parameters. As part of initialization, if the router detects
// that the channel graph isn't fully in sync with the latest UTXO (since the
// channel graph is a subset of the UTXO set) set, then the router will proceed
// to fully sync to the latest state of the UTXO set.
func New(cfg Config) (*ChannelRouter, error) {
	return &ChannelRouter{
		cfg:  &cfg,
		quit: make(chan struct{}),
	}, nil
}

// Start launches all the goroutines the ChannelRouter requires to carry out
// its duties. If the router has already been started, then this method is a
// noop.
func (r *ChannelRouter) Start() error {
	if !atomic.CompareAndSwapUint32(&r.started, 0, 1) {
		return nil
	}

	log.Info("Channel Router starting")

	// If any payments are still in flight, we resume, to make sure their
	// results are properly handled.
	if err := r.resumePayments(); err != nil {
		log.Error("Failed to resume payments during startup")
	}

	return nil
}

// Stop signals the ChannelRouter to gracefully halt all routines. This method
// will *block* until all goroutines have excited. If the channel router has
// already stopped then this method will return immediately.
func (r *ChannelRouter) Stop() error {
	if !atomic.CompareAndSwapUint32(&r.stopped, 0, 1) {
		return nil
	}

	log.Info("Channel Router shutting down...")
	defer log.Debug("Channel Router shutdown complete")

	close(r.quit)
	r.wg.Wait()

	return nil
}

// RouteRequest contains the parameters for a pathfinding request. It may
// describe a request to make a regular payment or one to a blinded path
// (incdicated by a non-nil BlindedPayment field).
type RouteRequest struct {
	// Source is the node that the path originates from.
	Source route.Vertex

	// Target is the node that the path terminates at. If the route
	// includes a blinded path, target will be the blinded node id of the
	// final hop in the blinded route.
	Target route.Vertex

	// Amount is the Amount in millisatoshis to be delivered to the target
	// node.
	Amount lnwire.MilliSatoshi

	// TimePreference expresses the caller's time preference for
	// pathfinding.
	TimePreference float64

	// Restrictions provides a set of additional Restrictions that the
	// route must adhere to.
	Restrictions *RestrictParams

	// CustomRecords is a set of custom tlv records to include for the
	// final hop.
	CustomRecords record.CustomSet

	// RouteHints contains an additional set of edges to include in our
	// view of the graph. This may either be a set of hints for private
	// channels or a "virtual" hop hint that represents a blinded route.
	RouteHints RouteHints

	// FinalExpiry is the cltv delta for the final hop. If paying to a
	// blinded path, this value is a duplicate of the delta provided
	// in blinded payment.
	FinalExpiry uint16

	// BlindedPathSet contains a set of optional blinded paths and
	// parameters used to reach a target node blinded paths. This field is
	// mutually exclusive with the Target field.
	BlindedPathSet *BlindedPaymentPathSet
}

// RouteHints is an alias type for a set of route hints, with the source node
// as the map's key and the details of the hint(s) in the edge policy.
type RouteHints map[route.Vertex][]AdditionalEdge

// NewRouteRequest produces a new route request for a regular payment or one
// to a blinded route, validating that the target, routeHints and finalExpiry
// parameters are mutually exclusive with the blindedPayment parameter (which
// contains these values for blinded payments).
func NewRouteRequest(source route.Vertex, target *route.Vertex,
	amount lnwire.MilliSatoshi, timePref float64,
	restrictions *RestrictParams, customRecords record.CustomSet,
	routeHints RouteHints, blindedPathSet *BlindedPaymentPathSet,
	finalExpiry uint16) (*RouteRequest, error) {

	var (
		// Assume that we're starting off with a regular payment.
		requestHints  = routeHints
		requestExpiry = finalExpiry
		err           error
	)

	if blindedPathSet != nil {
		if blindedPathSet.IsIntroNode(source) {
			return nil, ErrSelfIntro
		}

		// Check that the values for a clear path have not been set,
		// as this is an ambiguous signal from the caller.
		if routeHints != nil {
			return nil, ErrHintsAndBlinded
		}

		if finalExpiry != 0 {
			return nil, ErrExpiryAndBlinded
		}

		requestExpiry = blindedPathSet.FinalCLTVDelta()

		requestHints, err = blindedPathSet.ToRouteHints()
		if err != nil {
			return nil, err
		}
	}

	requestTarget, err := getTargetNode(target, blindedPathSet)
	if err != nil {
		return nil, err
	}

	return &RouteRequest{
		Source:         source,
		Target:         requestTarget,
		Amount:         amount,
		TimePreference: timePref,
		Restrictions:   restrictions,
		CustomRecords:  customRecords,
		RouteHints:     requestHints,
		FinalExpiry:    requestExpiry,
		BlindedPathSet: blindedPathSet,
	}, nil
}

func getTargetNode(target *route.Vertex,
	blindedPathSet *BlindedPaymentPathSet) (route.Vertex, error) {

	var (
		blinded   = blindedPathSet != nil
		targetSet = target != nil
	)

	switch {
	case blinded && targetSet:
		return route.Vertex{}, ErrTargetAndBlinded

	case blinded:
		return route.NewVertex(blindedPathSet.TargetPubKey()), nil

	case targetSet:
		return *target, nil

	default:
		return route.Vertex{}, ErrNoTarget
	}
}

// FindRoute attempts to query the ChannelRouter for the optimum path to a
// particular target destination to which it is able to send `amt` after
// factoring in channel capacities and cumulative fees along the route.
func (r *ChannelRouter) FindRoute(req *RouteRequest) (*route.Route, float64,
	error) {

	log.Debugf("Searching for path to %v, sending %v", req.Target,
		req.Amount)

	// We'll attempt to obtain a set of bandwidth hints that can help us
	// eliminate certain routes early on in the path finding process.
	bandwidthHints, err := newBandwidthManager(
		r.cfg.RoutingGraph, r.cfg.SelfNode, r.cfg.GetLink,
		fn.None[tlv.Blob](), r.cfg.TrafficShaper,
	)
	if err != nil {
		return nil, 0, err
	}

	// We'll fetch the current block height, so we can properly calculate
	// the required HTLC time locks within the route.
	_, currentHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, 0, err
	}

	// Now that we know the destination is reachable within the graph, we'll
	// execute our path finding algorithm.
	finalHtlcExpiry := currentHeight + int32(req.FinalExpiry)

	// Validate time preference.
	timePref := req.TimePreference
	if timePref < -1 || timePref > 1 {
		return nil, 0, errors.New("time preference out of range")
	}

	path, probability, err := findPath(
		&graphParams{
			additionalEdges: req.RouteHints,
			bandwidthHints:  bandwidthHints,
			graph:           r.cfg.RoutingGraph,
		},
		req.Restrictions, &r.cfg.PathFindingConfig,
		r.cfg.SelfNode, req.Source, req.Target, req.Amount,
		req.TimePreference, finalHtlcExpiry,
	)
	if err != nil {
		return nil, 0, err
	}

	// Create the route with absolute time lock values.
	route, err := newRoute(
		req.Source, path, uint32(currentHeight),
		finalHopParams{
			amt:       req.Amount,
			totalAmt:  req.Amount,
			cltvDelta: req.FinalExpiry,
			records:   req.CustomRecords,
		}, req.BlindedPathSet,
	)
	if err != nil {
		return nil, 0, err
	}

	go log.Tracef("Obtained path to send %v to %x: %v",
		req.Amount, req.Target, lnutils.SpewLogClosure(route))

	return route, probability, nil
}

// probabilitySource defines the signature of a function that can be used to
// query the success probability of sending a given amount between the two
// given vertices.
type probabilitySource func(route.Vertex, route.Vertex, lnwire.MilliSatoshi,
	btcutil.Amount) float64

// BlindedPathRestrictions are a set of constraints to adhere to when
// choosing a set of blinded paths to this node.
type BlindedPathRestrictions struct {
	// MinDistanceFromIntroNode is the minimum number of _real_ (non-dummy)
	// hops to include in a blinded path. Since we post-fix dummy hops, this
	// is the minimum distance between our node and the introduction node
	// of the path. This doesn't include our node, so if the minimum is 1,
	// then the path will contain at minimum our node along with an
	// introduction node hop.
	MinDistanceFromIntroNode uint8

	// NumHops is the number of hops that each blinded path should consist
	// of. If paths are found with a number of hops less that NumHops, then
	// dummy hops will be padded on to the route. This value doesn't
	// include our node, so if the maximum is 1, then the path will contain
	// our node along with an introduction node hop.
	NumHops uint8

	// MaxNumPaths is the maximum number of blinded paths to select.
	MaxNumPaths uint8

	// NodeOmissionSet is a set of nodes that should not be used within any
	// of the blinded paths that we generate.
	NodeOmissionSet fn.Set[route.Vertex]

	// IncomingChainedChannels holds the chained channels list (specified
	// via channel id) starting from a channel which points to the receiver
	// node.
	IncomingChainedChannels []uint64
}

// FindBlindedPaths finds a selection of paths to the destination node that can
// be used in blinded payment paths.
func (r *ChannelRouter) FindBlindedPaths(destination route.Vertex,
	amt lnwire.MilliSatoshi, probabilitySrc probabilitySource,
	restrictions *BlindedPathRestrictions) ([]*route.Route, error) {

	// First, find a set of candidate paths given the destination node and
	// path length restrictions.
	incomingChainedChannels := restrictions.IncomingChainedChannels
	minDistanceFromIntroNode := restrictions.MinDistanceFromIntroNode
	paths, err := findBlindedPaths(
		r.cfg.RoutingGraph, destination, &blindedPathRestrictions{
			minNumHops:              minDistanceFromIntroNode,
			maxNumHops:              restrictions.NumHops,
			nodeOmissionSet:         restrictions.NodeOmissionSet,
			incomingChainedChannels: incomingChainedChannels,
		},
	)
	if err != nil {
		return nil, err
	}

	// routeWithProbability groups a route with the probability of a
	// payment of the given amount succeeding on that path.
	type routeWithProbability struct {
		route       *route.Route
		probability float64
	}

	// Iterate over all the candidate paths and determine the success
	// probability of each path given the data we have about forwards
	// between any two nodes on a path.
	routes := make([]*routeWithProbability, 0, len(paths))
	for _, path := range paths {
		if len(path) < 1 {
			return nil, fmt.Errorf("a blinded path must have at " +
				"least one hop")
		}

		var (
			introNode = path[0].vertex
			prevNode  = introNode
			hops      = make(
				[]*route.Hop, 0, len(path)-1,
			)
			totalRouteProbability = float64(1)
		)

		// For each set of hops on the path, get the success probability
		// of a forward between those two vertices and use that to
		// update the overall route probability.
		for j := 1; j < len(path); j++ {
			probability := probabilitySrc(
				prevNode, path[j].vertex, amt,
				path[j-1].edgeCapacity,
			)

			totalRouteProbability *= probability

			hops = append(hops, &route.Hop{
				PubKeyBytes: path[j].vertex,
				ChannelID:   path[j-1].channelID,
			})

			prevNode = path[j].vertex
		}

		routeWithProbability := &routeWithProbability{
			route: &route.Route{
				SourcePubKey: introNode,
				Hops:         hops,
			},
			probability: totalRouteProbability,
		}

		// Don't bother adding a route if its success probability less
		// minimum that can be assigned to any single pair.
		if totalRouteProbability <= DefaultMinRouteProbability {
			log.Debugf("Not using route (%v) as a blinded "+
				"path since it resulted in an low "+
				"probability path(%.3f)",
				route.ChanIDString(routeWithProbability.route),
				routeWithProbability.probability)

			continue
		}

		routes = append(routes, routeWithProbability)
	}

	// Sort the routes based on probability.
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].probability > routes[j].probability
	})

	// Now just choose the best paths up until the maximum number of allowed
	// paths.
	bestRoutes := make([]*route.Route, 0, restrictions.MaxNumPaths)
	for _, route := range routes {
		if len(bestRoutes) >= int(restrictions.MaxNumPaths) {
			break
		}

		bestRoutes = append(bestRoutes, route.route)
	}

	return bestRoutes, nil
}

// generateNewSessionKey generates a new ephemeral private key to be used for a
// payment attempt.
func generateNewSessionKey() (*btcec.PrivateKey, error) {
	// Generate a new random session key to ensure that we don't trigger
	// any replay.
	//
	// TODO(roasbeef): add more sources of randomness?
	return btcec.NewPrivateKey()
}

// LightningPayment describes a payment to be sent through the network to the
// final destination.
type LightningPayment struct {
	// Target is the node in which the payment should be routed towards.
	Target route.Vertex

	// Amount is the value of the payment to send through the network in
	// milli-satoshis.
	Amount lnwire.MilliSatoshi

	// FeeLimit is the maximum fee in millisatoshis that the payment should
	// accept when sending it through the network. The payment will fail
	// if there isn't a route with lower fees than this limit.
	FeeLimit lnwire.MilliSatoshi

	// CltvLimit is the maximum time lock that is allowed for attempts to
	// complete this payment.
	CltvLimit uint32

	// paymentHash is the r-hash value to use within the HTLC extended to
	// the first hop. This won't be set for AMP payments.
	paymentHash *lntypes.Hash

	// amp is an optional field that is set if and only if this is am AMP
	// payment.
	amp *AMPOptions

	// FinalCLTVDelta is the CTLV expiry delta to use for the _final_ hop
	// in the route. This means that the final hop will have a CLTV delta
	// of at least: currentHeight + FinalCLTVDelta.
	FinalCLTVDelta uint16

	// PayAttemptTimeout is a timeout value that we'll use to determine
	// when we should should abandon the payment attempt after consecutive
	// payment failure. This prevents us from attempting to send a payment
	// indefinitely. A zero value means the payment will never time out.
	//
	// TODO(halseth): make wallclock time to allow resume after startup.
	PayAttemptTimeout time.Duration

	// RouteHints represents the different routing hints that can be used to
	// assist a payment in reaching its destination successfully. These
	// hints will act as intermediate hops along the route.
	//
	// NOTE: This is optional unless required by the payment. When providing
	// multiple routes, ensure the hop hints within each route are chained
	// together and sorted in forward order in order to reach the
	// destination successfully. This is mutually exclusive to the
	// BlindedPayment field.
	RouteHints [][]zpay32.HopHint

	// BlindedPathSet holds the information about a set of blinded paths to
	// the payment recipient. This is mutually exclusive to the RouteHints
	// field.
	BlindedPathSet *BlindedPaymentPathSet

	// OutgoingChannelIDs is the list of channels that are allowed for the
	// first hop. If nil, any channel may be used.
	OutgoingChannelIDs []uint64

	// LastHop is the pubkey of the last node before the final destination
	// is reached. If nil, any node may be used.
	LastHop *route.Vertex

	// DestFeatures specifies the set of features we assume the final node
	// has for pathfinding. Typically, these will be taken directly from an
	// invoice, but they can also be manually supplied or assumed by the
	// sender. If a nil feature vector is provided, the router will try to
	// fall back to the graph in order to load a feature vector for a node
	// in the public graph.
	DestFeatures *lnwire.FeatureVector

	// PaymentAddr is the payment address specified by the receiver. This
	// field should be a random 32-byte nonce presented in the receiver's
	// invoice to prevent probing of the destination.
	PaymentAddr fn.Option[[32]byte]

	// PaymentRequest is an optional payment request that this payment is
	// attempting to complete.
	PaymentRequest []byte

	// DestCustomRecords are TLV records that are to be sent to the final
	// hop in the new onion payload format. If the destination does not
	// understand this new onion payload format, then the payment will
	// fail.
	DestCustomRecords record.CustomSet

	// FirstHopCustomRecords are the TLV records that are to be sent to the
	// first hop of this payment. These records will be transmitted via the
	// wire message and therefore do not affect the onion payload size.
	FirstHopCustomRecords lnwire.CustomRecords

	// MaxParts is the maximum number of partial payments that may be used
	// to complete the full amount.
	MaxParts uint32

	// MaxShardAmt is the largest shard that we'll attempt to split using.
	// If this field is set, and we need to split, rather than attempting
	// half of the original payment amount, we'll use this value if half
	// the payment amount is greater than it.
	//
	// NOTE: This field is _optional_.
	MaxShardAmt *lnwire.MilliSatoshi

	// TimePref is the time preference for this payment. Set to -1 to
	// optimize for fees only, to 1 to optimize for reliability only or a
	// value in between for a mix.
	TimePref float64

	// Metadata is additional data that is sent along with the payment to
	// the payee.
	Metadata []byte
}

// AMPOptions houses information that must be known in order to send an AMP
// payment.
type AMPOptions struct {
	SetID     [32]byte
	RootShare [32]byte
}

// SetPaymentHash sets the given hash as the payment's overall hash. This
// should only be used for non-AMP payments.
func (l *LightningPayment) SetPaymentHash(hash lntypes.Hash) error {
	if l.amp != nil {
		return fmt.Errorf("cannot set payment hash for AMP payment")
	}

	l.paymentHash = &hash
	return nil
}

// SetAMP sets the given AMP options for the payment.
func (l *LightningPayment) SetAMP(amp *AMPOptions) error {
	if l.paymentHash != nil {
		return fmt.Errorf("cannot set amp options for payment " +
			"with payment hash")
	}

	l.amp = amp
	return nil
}

// Identifier returns a 32-byte slice that uniquely identifies this single
// payment. For non-AMP payments this will be the payment hash, for AMP
// payments this will be the used SetID.
func (l *LightningPayment) Identifier() [32]byte {
	if l.amp != nil {
		return l.amp.SetID
	}

	return *l.paymentHash
}

// SendPayment attempts to send a payment as described within the passed
// LightningPayment. This function is blocking and will return either: when the
// payment is successful, or all candidates routes have been attempted and
// resulted in a failed payment. If the payment succeeds, then a non-nil Route
// will be returned which describes the path the successful payment traversed
// within the network to reach the destination. Additionally, the payment
// preimage will also be returned.
func (r *ChannelRouter) SendPayment(ctx context.Context,
	payment *LightningPayment) ([32]byte, *route.Route, error) {

	paySession, shardTracker, err := r.PreparePayment(payment)
	if err != nil {
		return [32]byte{}, nil, err
	}

	log.Tracef("Dispatching SendPayment for lightning payment: %v",
		spewPayment(payment))

	return r.sendPayment(
		ctx, payment.FeeLimit, payment.Identifier(),
		payment.PayAttemptTimeout, paySession, shardTracker,
		payment.FirstHopCustomRecords,
	)
}

// SendPaymentAsync is the non-blocking version of SendPayment. The payment
// result needs to be retrieved via the control tower.
func (r *ChannelRouter) SendPaymentAsync(ctx context.Context,
	payment *LightningPayment, ps PaymentSession, st shards.ShardTracker) {

	// Since this is the first time this payment is being made, we pass nil
	// for the existing attempt.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		log.Tracef("Dispatching SendPayment for lightning payment: %v",
			spewPayment(payment))

		_, _, err := r.sendPayment(
			ctx, payment.FeeLimit, payment.Identifier(),
			payment.PayAttemptTimeout, ps, st,
			payment.FirstHopCustomRecords,
		)
		if err != nil {
			log.Errorf("Payment %x failed: %v",
				payment.Identifier(), err)
		}
	}()
}

// spewPayment returns a log closures that provides a spewed string
// representation of the passed payment.
func spewPayment(payment *LightningPayment) lnutils.LogClosure {
	return lnutils.NewLogClosure(func() string {
		// Make a copy of the payment with a nilled Curve
		// before spewing.
		var routeHints [][]zpay32.HopHint
		for _, routeHint := range payment.RouteHints {
			var hopHints []zpay32.HopHint
			for _, hopHint := range routeHint {
				h := hopHint.Copy()
				hopHints = append(hopHints, h)
			}
			routeHints = append(routeHints, hopHints)
		}
		p := *payment
		p.RouteHints = routeHints

		return lnutils.SpewLogClosure(p)()
	})
}

// PreparePayment creates the payment session and registers the payment with the
// control tower.
func (r *ChannelRouter) PreparePayment(payment *LightningPayment) (
	PaymentSession, shards.ShardTracker, error) {

	ctx := context.TODO()

	// Assemble any custom data we want to send to the first hop only.
	var firstHopData fn.Option[tlv.Blob]
	if len(payment.FirstHopCustomRecords) > 0 {
		if err := payment.FirstHopCustomRecords.Validate(); err != nil {
			return nil, nil, fmt.Errorf("invalid first hop custom "+
				"records: %w", err)
		}

		firstHopBlob, err := payment.FirstHopCustomRecords.Serialize()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to serialize "+
				"first hop custom records: %w", err)
		}

		firstHopData = fn.Some(firstHopBlob)
	}

	// Before starting the HTLC routing attempt, we'll create a fresh
	// payment session which will report our errors back to mission
	// control.
	paySession, err := r.cfg.SessionSource.NewPaymentSession(
		payment, firstHopData, r.cfg.TrafficShaper,
	)
	if err != nil {
		return nil, nil, err
	}

	// Record this payment hash with the ControlTower, ensuring it is not
	// already in-flight.
	//
	// TODO(roasbeef): store records as part of creation info?
	info := &paymentsdb.PaymentCreationInfo{
		PaymentIdentifier:     payment.Identifier(),
		Value:                 payment.Amount,
		CreationTime:          r.cfg.Clock.Now(),
		PaymentRequest:        payment.PaymentRequest,
		FirstHopCustomRecords: payment.FirstHopCustomRecords,
	}

	// Create a new ShardTracker that we'll use during the life cycle of
	// this payment.
	var shardTracker shards.ShardTracker
	switch {
	// If this is an AMP payment, we'll use the AMP shard tracker.
	case payment.amp != nil:
		addr := payment.PaymentAddr.UnwrapOr([32]byte{})
		shardTracker = amp.NewShardTracker(
			payment.amp.RootShare, payment.amp.SetID, addr,
			payment.Amount,
		)

	// Otherwise we'll use the simple tracker that will map each attempt to
	// the same payment hash.
	default:
		shardTracker = shards.NewSimpleShardTracker(
			payment.Identifier(), nil,
		)
	}

	err = r.cfg.Control.InitPayment(ctx, payment.Identifier(), info)
	if err != nil {
		return nil, nil, err
	}

	return paySession, shardTracker, nil
}

// SendToRoute sends a payment using the provided route and fails the payment
// when an error is returned from the attempt.
func (r *ChannelRouter) SendToRoute(_ context.Context, htlcHash lntypes.Hash,
	rt *route.Route,
	firstHopCustomRecords lnwire.CustomRecords) (*paymentsdb.HTLCAttempt,
	error) {

	return r.sendToRoute(htlcHash, rt, false, firstHopCustomRecords)
}

// SendToRouteSkipTempErr sends a payment using the provided route and fails
// the payment ONLY when a terminal error is returned from the attempt.
func (r *ChannelRouter) SendToRouteSkipTempErr(_ context.Context,
	htlcHash lntypes.Hash, rt *route.Route,
	firstHopCustomRecords lnwire.CustomRecords) (*paymentsdb.HTLCAttempt,
	error) {

	return r.sendToRoute(htlcHash, rt, true, firstHopCustomRecords)
}

// sendToRoute attempts to send a payment with the given hash through the
// provided route. This function is blocking and will return the attempt
// information as it is stored in the database. For a successful htlc, this
// information will contain the preimage. If an error occurs after the attempt
// was initiated, both return values will be non-nil. If skipTempErr is true,
// the payment won't be failed unless a terminal error has occurred.
func (r *ChannelRouter) sendToRoute(htlcHash lntypes.Hash, rt *route.Route,
	skipTempErr bool,
	firstHopCustomRecords lnwire.CustomRecords) (*paymentsdb.HTLCAttempt,
	error) {

	// TODO(ziggie): We cannot easily thread the context from the caller
	// of this method because the payment lifecycle depends on the context
	// to update the db. The Sending and Receiving of results is currently
	// not cleanly separated which is the reason that we cannot easily
	// cancel the context and therefore cancel the ongoing payment.
	ctx := context.TODO()

	// Helper function to fail a payment. It makes sure the payment is only
	// failed once so that the failure reason is not overwritten.
	failPayment := func(paymentIdentifier lntypes.Hash,
		reason paymentsdb.FailureReason) error {

		payment, fetchErr := r.cfg.Control.FetchPayment(
			ctx, paymentIdentifier,
		)
		if fetchErr != nil {
			return fetchErr
		}

		// NOTE: We cannot rely on the payment status to be failed here
		// because it can still be in-flight although the payment is
		// already failed.
		_, failedReason := payment.TerminalInfo()
		if failedReason != nil {
			return nil
		}

		return r.cfg.Control.FailPayment(
			ctx, paymentIdentifier, reason,
		)
	}

	log.Debugf("SendToRoute for payment %v with skipTempErr=%v",
		htlcHash, skipTempErr)

	// Calculate amount paid to receiver.
	amt := rt.ReceiverAmt()

	// If this is meant as an MP payment shard, we set the amount for the
	// creating info to the total amount of the payment.
	finalHop := rt.Hops[len(rt.Hops)-1]
	mpp := finalHop.MPP
	if mpp != nil {
		amt = mpp.TotalMsat()
	}

	// For non-MPP, there's no such thing as temp error as there's only one
	// HTLC attempt being made. When this HTLC is failed, the payment is
	// failed hence cannot be retried.
	if skipTempErr && mpp == nil {
		return nil, ErrSkipTempErr
	}

	// For non-AMP payments the overall payment identifier will be the same
	// hash as used for this HTLC.
	paymentIdentifier := htlcHash

	// For AMP-payments, we'll use the setID as the unique ID for the
	// overall payment.
	amp := finalHop.AMP
	if amp != nil {
		paymentIdentifier = amp.SetID()
	}

	// Record this payment hash with the ControlTower, ensuring it is not
	// already in-flight.
	info := &paymentsdb.PaymentCreationInfo{
		PaymentIdentifier:     paymentIdentifier,
		Value:                 amt,
		CreationTime:          r.cfg.Clock.Now(),
		PaymentRequest:        nil,
		FirstHopCustomRecords: firstHopCustomRecords,
	}

	err := r.cfg.Control.InitPayment(ctx, paymentIdentifier, info)
	switch {
	// If this is an MPP attempt and the hash is already registered with
	// the database, we can go on to launch the shard.
	case mpp != nil && errors.Is(err, paymentsdb.ErrPaymentInFlight):
	case mpp != nil && errors.Is(err, paymentsdb.ErrPaymentExists):

	// Any other error is not tolerated.
	case err != nil:
		return nil, err
	}

	log.Tracef("Dispatching SendToRoute for HTLC hash %v: %v", htlcHash,
		lnutils.SpewLogClosure(rt))

	// Since the HTLC hashes and preimages are specified manually over the
	// RPC for SendToRoute requests, we don't have to worry about creating
	// a ShardTracker that can generate hashes for AMP payments. Instead, we
	// create a simple tracker that can just return the hash for the single
	// shard we'll now launch.
	shardTracker := shards.NewSimpleShardTracker(htlcHash, nil)

	// Create a payment lifecycle using the given route with,
	// - zero fee limit as we are not requesting routes.
	// - nil payment session (since we already have a route).
	// - no payment timeout.
	// - no current block height.
	p := newPaymentLifecycle(
		r, 0, paymentIdentifier, nil, shardTracker, 0,
		firstHopCustomRecords,
	)

	// Allow the traffic shaper to add custom records to the outgoing HTLC
	// and also adjust the amount if needed.
	err = p.amendFirstHopData(rt)
	if err != nil {
		return nil, err
	}

	// We found a route to try, create a new HTLC attempt to try.
	//
	// NOTE: we use zero `remainingAmt` here to simulate the same effect of
	// setting the lastShard to be false, which is used by previous
	// implementation.
	attempt, err := p.registerAttempt(ctx, rt, 0)
	if err != nil {
		return nil, err
	}

	// Once the attempt is created, send it to the htlcswitch. Notice that
	// the `err` returned here has already been processed by
	// `handleSwitchErr`, which means if there's a terminal failure, the
	// payment has been failed.
	result, err := p.sendAttempt(ctx, attempt)
	if err != nil {
		return nil, err
	}

	// Since for SendToRoute we won't retry in case the shard fails, we'll
	// mark the payment failed with the control tower immediately if the
	// skipTempErr is false.
	reason := paymentsdb.FailureReasonError

	// If we failed to send the HTLC, we need to further decide if we want
	// to fail the payment.
	if result.err != nil {
		// If skipTempErr, we'll return the attempt and the temp error.
		if skipTempErr {
			return result.attempt, result.err
		}

		err := failPayment(paymentIdentifier, reason)
		if err != nil {
			return nil, err
		}

		return result.attempt, result.err
	}

	// The attempt was successfully sent, wait for the result to be
	// available.
	result, err = p.collectAndHandleResult(ctx, attempt)
	if err != nil {
		return nil, err
	}

	// We got a successful result.
	if result.err == nil {
		return result.attempt, nil
	}

	// An error returned from collecting the result, we'll mark the payment
	// as failed if we don't skip temp error.
	if !skipTempErr {
		err := failPayment(paymentIdentifier, reason)
		if err != nil {
			return nil, err
		}
	}

	return result.attempt, result.err
}

// sendPayment attempts to send a payment to the passed payment hash. This
// function is blocking and will return either: when the payment is successful,
// or all candidates routes have been attempted and resulted in a failed
// payment. If the payment succeeds, then a non-nil Route will be returned
// which describes the path the successful payment traversed within the network
// to reach the destination. Additionally, the payment preimage will also be
// returned.
//
// This method relies on the ControlTower's internal payment state machine to
// carry out its execution. After restarts, it is safe, and assumed, that the
// router will call this method for every payment still in-flight according to
// the ControlTower.
func (r *ChannelRouter) sendPayment(ctx context.Context,
	feeLimit lnwire.MilliSatoshi, identifier lntypes.Hash,
	paymentAttemptTimeout time.Duration, paySession PaymentSession,
	shardTracker shards.ShardTracker,
	firstHopCustomRecords lnwire.CustomRecords) ([32]byte, *route.Route,
	error) {

	// If the user provides a timeout, we will additionally wrap the context
	// in a deadline.
	cancel := func() {}
	if paymentAttemptTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, paymentAttemptTimeout)
	}

	// Since resumePayment is a blocking call, we'll cancel this
	// context if the payment completes before the optional
	// deadline.
	defer cancel()

	// We'll also fetch the current block height, so we can properly
	// calculate the required HTLC time locks within the route.
	_, currentHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return [32]byte{}, nil, err
	}

	// Validate the custom records before we attempt to send the payment.
	// TODO(ziggie): Move this check before registering the payment in the
	// db (InitPayment).
	if err := firstHopCustomRecords.Validate(); err != nil {
		return [32]byte{}, nil, err
	}

	// Now set up a paymentLifecycle struct with these params, such that we
	// can resume the payment from the current state.
	p := newPaymentLifecycle(
		r, feeLimit, identifier, paySession, shardTracker,
		currentHeight, firstHopCustomRecords,
	)

	return p.resumePayment(ctx)
}

// extractChannelUpdate examines the error and extracts the channel update.
func (r *ChannelRouter) extractChannelUpdate(
	failure lnwire.FailureMessage) *lnwire.ChannelUpdate1 {

	var update *lnwire.ChannelUpdate1
	switch onionErr := failure.(type) {
	case *lnwire.FailExpiryTooSoon:
		update = &onionErr.Update
	case *lnwire.FailAmountBelowMinimum:
		update = &onionErr.Update
	case *lnwire.FailFeeInsufficient:
		update = &onionErr.Update
	case *lnwire.FailIncorrectCltvExpiry:
		update = &onionErr.Update
	case *lnwire.FailChannelDisabled:
		update = &onionErr.Update
	case *lnwire.FailTemporaryChannelFailure:
		update = onionErr.Update
	}

	return update
}

// ErrNoChannel is returned when a route cannot be built because there are no
// channels that satisfy all requirements.
type ErrNoChannel struct {
	position int
}

// Error returns a human-readable string describing the error.
func (e ErrNoChannel) Error() string {
	return fmt.Sprintf("no matching outgoing channel available for "+
		"node index %v", e.position)
}

// BuildRoute returns a fully specified route based on a list of pubkeys. If
// amount is nil, the minimum routable amount is used. To force a specific
// outgoing channel, use the outgoingChan parameter.
func (r *ChannelRouter) BuildRoute(amt fn.Option[lnwire.MilliSatoshi],
	hops []route.Vertex, outgoingChan *uint64, finalCltvDelta int32,
	payAddr fn.Option[[32]byte], firstHopBlob fn.Option[[]byte]) (
	*route.Route, error) {

	log.Tracef("BuildRoute called: hopsCount=%v, amt=%v", len(hops), amt)

	var outgoingChans map[uint64]struct{}
	if outgoingChan != nil {
		outgoingChans = map[uint64]struct{}{
			*outgoingChan: {},
		}
	}

	// We'll attempt to obtain a set of bandwidth hints that helps us select
	// the best outgoing channel to use in case no outgoing channel is set.
	bandwidthHints, err := newBandwidthManager(
		r.cfg.RoutingGraph, r.cfg.SelfNode, r.cfg.GetLink, firstHopBlob,
		r.cfg.TrafficShaper,
	)
	if err != nil {
		return nil, err
	}

	sourceNode := r.cfg.SelfNode

	// We check that each node in the route has a connection to others that
	// can forward in principle.
	unifiers, err := getEdgeUnifiers(
		r.cfg.SelfNode, hops, outgoingChans, r.cfg.RoutingGraph,
	)
	if err != nil {
		return nil, err
	}

	var (
		receiverAmt lnwire.MilliSatoshi
		senderAmt   lnwire.MilliSatoshi
		pathEdges   []*unifiedEdge
	)

	// We determine the edges compatible with the requested amount, as well
	// as the amount to send, which can be used to determine the final
	// receiver amount, if a minimal amount was requested.
	pathEdges, senderAmt, err = senderAmtBackwardPass(
		unifiers, amt, bandwidthHints,
	)
	if err != nil {
		return nil, err
	}

	// For the minimal amount search, we need to do a forward pass to find a
	// larger receiver amount due to possible min HTLC bumps, otherwise we
	// just use the requested amount.
	receiverAmt, err = fn.ElimOption(
		amt,
		func() fn.Result[lnwire.MilliSatoshi] {
			return fn.NewResult(
				receiverAmtForwardPass(senderAmt, pathEdges),
			)
		},
		fn.Ok[lnwire.MilliSatoshi],
	).Unpack()
	if err != nil {
		return nil, err
	}

	// Fetch the current block height outside the routing transaction, to
	// prevent the rpc call blocking the database.
	_, height, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// Build and return the final route.
	return newRoute(
		sourceNode, pathEdges, uint32(height),
		finalHopParams{
			amt:         receiverAmt,
			totalAmt:    receiverAmt,
			cltvDelta:   uint16(finalCltvDelta),
			records:     nil,
			paymentAddr: payAddr,
		}, nil,
	)
}

// resumePayments fetches inflight payments and resumes their payment
// lifecycles.
func (r *ChannelRouter) resumePayments() error {
	ctx := context.TODO()

	// Get all payments that are inflight.
	log.Debugf("Scanning for inflight payments")
	payments, err := r.cfg.Control.FetchInFlightPayments(ctx)
	if err != nil {
		return err
	}

	log.Debugf("Scanning finished, found %d inflight payments",
		len(payments))

	// TODO(ziggie): Also check for payments which have no HTLCs at all
	// this can happen because we register an attempt after initializing the
	// payment, so there is a small chance that we init a payment but never
	// register an attempt for it.

	// Before we restart existing payments and start accepting more
	// payments to be made, we clean the network result store of the
	// Switch. We do this here at startup to ensure no more payments can be
	// made concurrently, so we know the toKeep map will be up-to-date
	// until the cleaning has finished.
	toKeep := make(map[uint64]struct{})
	for _, p := range payments {
		for _, a := range p.HTLCs {
			toKeep[a.AttemptID] = struct{}{}

			// Try to fail the attempt if the route contains a dead
			// channel.
			r.failStaleAttempt(a, p.Info.PaymentIdentifier)
		}
	}

	log.Debugf("Cleaning network result store.")
	if err := r.cfg.Payer.CleanStore(toKeep); err != nil {
		return err
	}

	// launchPayment is a helper closure that handles resuming the payment.
	launchPayment := func(payment *paymentsdb.MPPayment) {
		defer r.wg.Done()

		// Get the hashes used for the outstanding HTLCs.
		htlcs := make(map[uint64]lntypes.Hash)
		for _, a := range payment.HTLCs {
			a := a

			// We check whether the individual attempts have their
			// HTLC hash set, if not we'll fall back to the overall
			// payment hash.
			hash := payment.Info.PaymentIdentifier
			if a.Hash != nil {
				hash = *a.Hash
			}

			htlcs[a.AttemptID] = hash
		}

		payHash := payment.Info.PaymentIdentifier

		// Since we are not supporting creating more shards after a
		// restart (only receiving the result of the shards already
		// outstanding), we create a simple shard tracker that will map
		// the attempt IDs to hashes used for the HTLCs. This will be
		// enough also for AMP payments, since we only need the hashes
		// for the individual HTLCs to regenerate the circuits, and we
		// don't currently persist the root share necessary to
		// re-derive them.
		shardTracker := shards.NewSimpleShardTracker(payHash, htlcs)

		// We create a dummy, empty payment session such that we won't
		// make another payment attempt when the result for the
		// in-flight attempt is received.
		paySession := r.cfg.SessionSource.NewPaymentSessionEmpty()

		// We pass in a non-timeout context, to indicate we don't need
		// it to timeout. It will stop immediately after the existing
		// attempt has finished anyway. We also set a zero fee limit,
		// as no more routes should be tried.
		noTimeout := time.Duration(0)
		_, _, err := r.sendPayment(
			context.Background(), 0, payHash, noTimeout, paySession,
			shardTracker, payment.Info.FirstHopCustomRecords,
		)
		if err != nil {
			log.Errorf("Resuming payment %v failed: %v", payHash,
				err)

			return
		}

		log.Infof("Resumed payment %v completed", payHash)
	}

	for _, payment := range payments {
		log.Infof("Resuming payment %v", payment.Info.PaymentIdentifier)

		r.wg.Add(1)
		go launchPayment(payment)
	}

	return nil
}

// failStaleAttempt will fail an HTLC attempt if it's using an unknown channel
// in its route. It first consults the switch to see if there's already a
// network result stored for this attempt. If not, it will check whether the
// first hop of this attempt is using an active channel known to us. If
// inactive, this attempt will be failed.
//
// NOTE: there's an unknown bug that caused the network result for a particular
// attempt to NOT be saved, resulting a payment being stuck forever. More info:
// - https://github.com/lightningnetwork/lnd/issues/8146
// - https://github.com/lightningnetwork/lnd/pull/8174
func (r *ChannelRouter) failStaleAttempt(a paymentsdb.HTLCAttempt,
	payHash lntypes.Hash) {

	ctx := context.TODO()

	// We can only fail inflight HTLCs so we skip the settled/failed ones.
	if a.Failure != nil || a.Settle != nil {
		return
	}

	// First, check if we've already had a network result for this attempt.
	// If no result is found, we'll check whether the reference link is
	// still known to us.
	ok, err := r.cfg.Payer.HasAttemptResult(a.AttemptID)
	if err != nil {
		log.Errorf("Failed to fetch network result for attempt=%v",
			a.AttemptID)
		return
	}

	// There's already a network result, no need to fail it here as the
	// payment lifecycle will take care of it, so we can exit early.
	if ok {
		log.Debugf("Already have network result for attempt=%v",
			a.AttemptID)
		return
	}

	// We now need to decide whether this attempt should be failed here.
	// For very old payments, it's possible that the network results were
	// never saved, causing the payments to be stuck inflight. We now check
	// whether the first hop is referencing an active channel ID and, if
	// not, we will fail the attempt as it has no way to be retried again.
	var shouldFail bool

	// Validate that the attempt has hop info. If this attempt has no hop
	// info it indicates an error in our db.
	if len(a.Route.Hops) == 0 {
		log.Errorf("Found empty hop for attempt=%v", a.AttemptID)

		shouldFail = true
	} else {
		// Get the short channel ID.
		chanID := a.Route.Hops[0].ChannelID
		scid := lnwire.NewShortChanIDFromInt(chanID)

		// Check whether this link is active. If so, we won't fail the
		// attempt but keep waiting for its result.
		_, err := r.cfg.GetLink(scid)
		if err == nil {
			return
		}

		// We should get the link not found err. If not, we will log an
		// error and skip failing this attempt since an unknown error
		// occurred.
		if !errors.Is(err, htlcswitch.ErrChannelLinkNotFound) {
			log.Errorf("Failed to get link for attempt=%v for "+
				"payment=%v: %v", a.AttemptID, payHash, err)

			return
		}

		// The channel link is not active, we now check whether this
		// channel is already closed. If so, we fail the HTLC attempt
		// as there's no need to wait for its network result because
		// there's no link. If the channel is still pending, we'll keep
		// waiting for the result as we may get a contract resolution
		// for this HTLC.
		if _, ok := r.cfg.ClosedSCIDs[scid]; ok {
			shouldFail = true
		}
	}

	// Exit if there's no need to fail.
	if !shouldFail {
		return
	}

	log.Errorf("Failing stale attempt=%v for payment=%v", a.AttemptID,
		payHash)

	// Fail the attempt in db. If there's an error, there's nothing we can
	// do here but logging it.
	failInfo := &paymentsdb.HTLCFailInfo{
		Reason:   paymentsdb.HTLCFailUnknown,
		FailTime: r.cfg.Clock.Now(),
	}
	_, err = r.cfg.Control.FailAttempt(ctx, payHash, a.AttemptID, failInfo)
	if err != nil {
		log.Errorf("Fail attempt=%v got error: %v", a.AttemptID, err)
	}
}

// getEdgeUnifiers returns a list of edge unifiers for the given route.
func getEdgeUnifiers(source route.Vertex, hops []route.Vertex,
	outgoingChans map[uint64]struct{},
	graph Graph) ([]*edgeUnifier, error) {

	// Allocate a list that will contain the edge unifiers for this route.
	unifiers := make([]*edgeUnifier, len(hops))

	// Traverse hops backwards to accumulate fees in the running amounts.
	for i := len(hops) - 1; i >= 0; i-- {
		toNode := hops[i]

		var fromNode route.Vertex
		if i == 0 {
			fromNode = source
		} else {
			fromNode = hops[i-1]
		}

		// Build unified policies for this hop based on the channels
		// known in the graph. Inbound fees are only active if the edge
		// is not the last hop.
		isExitHop := i == len(hops)-1
		u := newNodeEdgeUnifier(
			source, toNode, !isExitHop, outgoingChans,
		)

		err := u.addGraphPolicies(graph)
		if err != nil {
			return nil, err
		}

		// Exit if there are no channels.
		edgeUnifier, ok := u.edgeUnifiers[fromNode]
		if !ok {
			log.Errorf("Cannot find policy for node %v", fromNode)
			return nil, ErrNoChannel{position: i}
		}

		unifiers[i] = edgeUnifier
	}

	return unifiers, nil
}

// senderAmtBackwardPass returns a list of unified edges for the given route and
// determines the amount that should be sent to fulfill min HTLC requirements.
// The minimal sender amount can be searched for by using amt=None.
func senderAmtBackwardPass(unifiers []*edgeUnifier,
	amt fn.Option[lnwire.MilliSatoshi],
	bandwidthHints bandwidthHints) ([]*unifiedEdge, lnwire.MilliSatoshi,
	error) {

	if len(unifiers) == 0 {
		return nil, 0, fmt.Errorf("no unifiers provided")
	}

	var unifiedEdges = make([]*unifiedEdge, len(unifiers))

	// We traverse the route backwards and handle the last hop separately.
	edgeUnifier := unifiers[len(unifiers)-1]

	// incomingAmt tracks the amount that is forwarded on the edges of a
	// route. The last hop only forwards the amount that the receiver should
	// receive, as there are no fees paid to the last node.
	// For minimum amount routes, aim to deliver at least 1 msat to
	// the destination. There are nodes in the wild that have a
	// min_htlc channel policy of zero, which could lead to a zero
	// amount payment being made.
	incomingAmt := amt.UnwrapOr(1)

	// If using min amt, increase the amount if needed to fulfill min HTLC
	// requirements.
	if amt.IsNone() {
		min := edgeUnifier.minAmt()
		if min > incomingAmt {
			incomingAmt = min
		}
	}

	// Get an edge for the specific amount that we want to forward.
	edge := edgeUnifier.getEdge(incomingAmt, bandwidthHints, 0)
	if edge == nil {
		log.Errorf("Cannot find policy with amt=%v "+
			"for hop %v", incomingAmt, len(unifiers)-1)

		return nil, 0, ErrNoChannel{position: len(unifiers) - 1}
	}

	unifiedEdges[len(unifiers)-1] = edge

	// Handle the rest of the route except the last hop.
	for i := len(unifiers) - 2; i >= 0; i-- {
		edgeUnifier = unifiers[i]

		// If using min amt, increase the amount if needed to fulfill
		// min HTLC requirements.
		if amt.IsNone() {
			min := edgeUnifier.minAmt()
			if min > incomingAmt {
				incomingAmt = min
			}
		}

		// A --current hop-- B --next hop: incomingAmt-- C
		// The outbound fee paid to the current end node B is based on
		// the amount that the next hop forwards and B's policy for that
		// hop.
		outboundFee := unifiedEdges[i+1].policy.ComputeFee(
			incomingAmt,
		)

		netAmount := incomingAmt + outboundFee

		// We need to select an edge that can forward the requested
		// amount.
		edge = edgeUnifier.getEdge(
			netAmount, bandwidthHints, outboundFee,
		)
		if edge == nil {
			return nil, 0, ErrNoChannel{position: i}
		}

		// The fee paid to B depends on the current hop's inbound fee
		// policy and on the outbound fee for the next hop as any
		// inbound fee discount is capped by the outbound fee such that
		// the total fee for B can't become negative.
		inboundFee := calcCappedInboundFee(edge, netAmount, outboundFee)

		fee := lnwire.MilliSatoshi(int64(outboundFee) + inboundFee)

		log.Tracef("Select channel %v at position %v",
			edge.policy.ChannelID, i)

		// Finally, we update the amount that needs to flow into node B
		// from A, which is the next hop's forwarding amount plus the
		// fee for B: A --current hop: incomingAmt-- B --next hop-- C
		incomingAmt += fee

		unifiedEdges[i] = edge
	}

	return unifiedEdges, incomingAmt, nil
}

// receiverAmtForwardPass returns the amount that a receiver will receive after
// deducting all fees from the sender amount.
func receiverAmtForwardPass(runningAmt lnwire.MilliSatoshi,
	unifiedEdges []*unifiedEdge) (lnwire.MilliSatoshi, error) {

	if len(unifiedEdges) == 0 {
		return 0, fmt.Errorf("no edges to forward through")
	}

	inEdge := unifiedEdges[0]
	if !inEdge.amtInRange(runningAmt) {
		log.Errorf("Amount %v not in range for hop index %v",
			runningAmt, 0)

		return 0, ErrNoChannel{position: 0}
	}

	// Now that we arrived at the start of the route and found out the route
	// total amount, we make a forward pass. Because the amount may have
	// been increased in the backward pass, fees need to be recalculated and
	// amount ranges re-checked.
	for i := 1; i < len(unifiedEdges); i++ {
		inEdge := unifiedEdges[i-1]
		outEdge := unifiedEdges[i]

		// Decrease the amount to send while going forward.
		runningAmt = outgoingFromIncoming(runningAmt, inEdge, outEdge)

		if !outEdge.amtInRange(runningAmt) {
			log.Errorf("Amount %v not in range for hop index %v",
				runningAmt, i)

			return 0, ErrNoChannel{position: i}
		}
	}

	return runningAmt, nil
}

// incomingFromOutgoing computes the incoming amount based on the outgoing
// amount by adding fees to the outgoing amount, replicating the path finding
// and routing process, see also CheckHtlcForward.
func incomingFromOutgoing(outgoingAmt lnwire.MilliSatoshi,
	incoming, outgoing *unifiedEdge) lnwire.MilliSatoshi {

	outgoingFee := outgoing.policy.ComputeFee(outgoingAmt)

	// Net amount is the amount the inbound fees are calculated with.
	netAmount := outgoingAmt + outgoingFee

	inboundFee := incoming.inboundFees.CalcFee(netAmount)

	// The inbound fee is not allowed to reduce the incoming amount below
	// the outgoing amount.
	if int64(outgoingFee)+inboundFee < 0 {
		return outgoingAmt
	}

	return netAmount + lnwire.MilliSatoshi(inboundFee)
}

// outgoingFromIncoming computes the outgoing amount based on the incoming
// amount by subtracting fees from the incoming amount. Note that this is not
// exactly the inverse of incomingFromOutgoing, because of some rounding.
func outgoingFromIncoming(incomingAmt lnwire.MilliSatoshi,
	incoming, outgoing *unifiedEdge) lnwire.MilliSatoshi {

	// Convert all quantities to big.Int to be able to hande negative
	// values. The formulas to compute the outgoing amount involve terms
	// with PPM*PPM*A, which can easily overflow an int64.
	A := big.NewInt(int64(incomingAmt))
	Ro := big.NewInt(int64(outgoing.policy.FeeProportionalMillionths))
	Bo := big.NewInt(int64(outgoing.policy.FeeBaseMSat))
	Ri := big.NewInt(int64(incoming.inboundFees.Rate))
	Bi := big.NewInt(int64(incoming.inboundFees.Base))
	PPM := big.NewInt(1_000_000)

	// The following discussion was contributed by user feelancer21, see
	//nolint:ll
	// https://github.com/feelancer21/lnd/commit/f6f05fa930985aac0d27c3f6681aada1b599162a.

	// The incoming amount Ai based on the outgoing amount Ao is computed by
	// Ai = max(Ai(Ao), Ao), which caps the incoming amount such that the
	// total node fee (Ai - Ao) is non-negative. This is commonly enforced
	// by routing nodes.

	// The function Ai(Ao) is given by:
	// Ai(Ao) = (Ao + Bo + Ro/PPM) + (Bi + (Ao + Ro/PPM + Bo)*Ri/PPM), where
	// the first term is the net amount (the outgoing amount plus the
	// outbound fee), and the second is the inbound fee computed based on
	// the net amount.

	// Ai(Ao) can potentially become more negative in absolute value than
	// Ao, which is why the above mentioned capping is needed. We can
	// abbreviate Ai(Ao) with Ai(Ao) = m*Ao + n, where m and n are:
	// m := (1 + Ro/PPM) * (1 + Ri/PPM)
	// n := Bi + Bo*(1 + Ri/PPM)

	// If we know that m > 0, this is equivalent of Ri/PPM > -1, because Ri
	// is the only factor that can become negative. A value or Ri/PPM = -1,
	// means that the routing node is willing to give up on 100% of the
	// net amount (based on the fee rate), which is likely to not happen in
	// practice. This condition will be important for a later trick.

	// If we want to compute the incoming amount based on the outgoing
	// amount, which is the reverse problem, we need to solve Ai =
	// max(Ai(Ao), Ao) for Ao(Ai). Given an incoming amount A,
	// we look for an Ao such that A = max(Ai(Ao), Ao).

	// The max function separates this into two cases. The case to take is
	// not clear yet, because we don't know Ao, but later we see a trick
	// how to determine which case is the one to take.

	// first case: Ai(Ao) <= Ao:
	// Therefore, A = max(Ai(Ao), Ao) = Ao, we find Ao = A.
	// This also leads to Ai(A) <= A by substitution into the condition.

	// second case: Ai(Ao) > Ao:
	// Therefore, A = max(Ai(Ao), Ao) = Ai(Ao) = m*Ao + n. Solving for Ao
	// gives Ao = (A - n)/m.
	//
	// We know
	// Ai(Ao) > Ao  <=>  A = Ai(Ao) > Ao = (A - n)/m,
	// so A > (A - n)/m.
	//
	// **Assuming m > 0**, by multiplying with m, we can transform this to
	// A * m + n > A.
	//
	// We know Ai(A) = A*m + n, therefore Ai(A) > A.
	//
	// This means that if we apply the incoming amount calculation to the
	// **incoming** amount, and this condition holds, then we know that we
	// deal with the second case, being able to compute the outgoing amount
	// based off the formula Ao = (A - n)/m, otherwise we will just return
	// the incoming amount.

	// In case the inbound fee rate is less than -1 (-100%), we fail to
	// compute the outbound amount and return the incoming amount. This also
	// protects against zero division later.

	// We compute m in terms of big.Int to be safe from overflows and to be
	// consistent with later calculations.
	// m := (PPM*PPM + Ri*PPM + Ro*PPM + Ro*Ri)/(PPM*PPM)

	// Compute terms in (PPM*PPM + Ri*PPM + Ro*PPM + Ro*Ri).
	m1 := new(big.Int).Mul(PPM, PPM)
	m2 := new(big.Int).Mul(Ri, PPM)
	m3 := new(big.Int).Mul(Ro, PPM)
	m4 := new(big.Int).Mul(Ro, Ri)

	// Add up terms m1..m4.
	m := big.NewInt(0)
	m.Add(m, m1)
	m.Add(m, m2)
	m.Add(m, m3)
	m.Add(m, m4)

	// Since we compare to 0, we can multiply by PPM*PPM to avoid the
	// division.
	if m.Int64() <= 0 {
		return incomingAmt
	}

	// In order to decide if the total fee is negative, we apply the fee
	// to the *incoming* amount as mentioned before.

	// We compute the test amount in terms of big.Int to be safe from
	// overflows and to be consistent later calculations.
	// testAmtF := A*m + n =
	// = A + Bo + Bi + (PPM*(A*Ri + A*Ro + Ro*Ri) + A*Ri*Ro)/(PPM*PPM)

	// Compute terms in (A*Ri + A*Ro + Ro*Ri).
	t1 := new(big.Int).Mul(A, Ri)
	t2 := new(big.Int).Mul(A, Ro)
	t3 := new(big.Int).Mul(Ro, Ri)

	// Sum up terms t1-t3.
	t4 := big.NewInt(0)
	t4.Add(t4, t1)
	t4.Add(t4, t2)
	t4.Add(t4, t3)

	// Compute PPM*(A*Ri + A*Ro + Ro*Ri).
	t6 := new(big.Int).Mul(PPM, t4)

	// Compute A*Ri*Ro.
	t7 := new(big.Int).Mul(A, Ri)
	t7.Mul(t7, Ro)

	// Compute (PPM*(A*Ri + A*Ro + Ro*Ri) + A*Ri*Ro)/(PPM*PPM).
	num := new(big.Int).Add(t6, t7)
	denom := new(big.Int).Mul(PPM, PPM)
	fraction := new(big.Int).Div(num, denom)

	// Sum up all terms.
	testAmt := big.NewInt(0)
	testAmt.Add(testAmt, A)
	testAmt.Add(testAmt, Bo)
	testAmt.Add(testAmt, Bi)
	testAmt.Add(testAmt, fraction)

	// Protect against negative values for the integer cast to Msat.
	if testAmt.Int64() < 0 {
		return incomingAmt
	}

	// If the second case holds, we have to compute the outgoing amount.
	if lnwire.MilliSatoshi(testAmt.Int64()) > incomingAmt {
		// Compute the outgoing amount by integer ceiling division. This
		// precision is needed because PPM*PPM*A and other terms can
		// easily overflow with int64, which happens with about
		// A = 10_000 sat.

		// out := (A - n) / m = numerator / denominator
		// numerator := PPM*(PPM*(A - Bo - Bi) - Bo*Ri)
		// denominator := PPM*(PPM + Ri + Ro) + Ri*Ro

		var numerator big.Int

		// Compute (A - Bo - Bi).
		temp1 := new(big.Int).Sub(A, Bo)
		temp2 := new(big.Int).Sub(temp1, Bi)

		// Compute terms in (PPM*(A - Bo - Bi) - Bo*Ri).
		temp3 := new(big.Int).Mul(PPM, temp2)
		temp4 := new(big.Int).Mul(Bo, Ri)

		// Compute PPM*(PPM*(A - Bo - Bi) - Bo*Ri)
		temp5 := new(big.Int).Sub(temp3, temp4)
		numerator.Mul(PPM, temp5)

		var denominator big.Int

		// Compute (PPM + Ri + Ro).
		temp1 = new(big.Int).Add(PPM, Ri)
		temp2 = new(big.Int).Add(temp1, Ro)

		// Compute PPM*(PPM + Ri + Ro) + Ri*Ro.
		temp3 = new(big.Int).Mul(PPM, temp2)
		temp4 = new(big.Int).Mul(Ri, Ro)
		denominator.Add(temp3, temp4)

		// We overestimate the outgoing amount by taking the ceiling of
		// the division. This means that we may round slightly up by a
		// MilliSatoshi, but this helps to ensure that we don't hit min
		// HTLC constrains in the context of finding the minimum amount
		// of a route.
		// ceil = floor((numerator + denominator - 1) / denominator)
		ceil := new(big.Int).Add(&numerator, &denominator)
		ceil.Sub(ceil, big.NewInt(1))
		ceil.Div(ceil, &denominator)

		return lnwire.MilliSatoshi(ceil.Int64())
	}

	// Otherwise the inbound fee made up for the outbound fee, which is why
	// we just return the incoming amount.
	return incomingAmt
}
