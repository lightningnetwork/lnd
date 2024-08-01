package routing

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/amp"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/routing/shards"
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
}

// PaymentSessionSource is an interface that defines a source for the router to
// retrieve new payment sessions.
type PaymentSessionSource interface {
	// NewPaymentSession creates a new payment session that will produce
	// routes to the given target. An optional set of routing hints can be
	// provided in order to populate additional edges to explore when
	// finding a path to the payment's destination.
	NewPaymentSession(p *LightningPayment) (PaymentSession, error)

	// NewPaymentSessionEmpty creates a new paymentSession instance that is
	// empty, and will be exhausted immediately. Used for failure reporting
	// to missioncontrol for resumed payment we don't want to make more
	// attempts for.
	NewPaymentSessionEmpty() PaymentSession
}

// MissionController is an interface that exposes failure reporting and
// probability estimation.
type MissionController interface {
	// ReportPaymentFail reports a failed payment to mission control as
	// input for future probability estimates. It returns a bool indicating
	// whether this error is a final error and no further payment attempts
	// need to be made.
	ReportPaymentFail(attemptID uint64, rt *route.Route,
		failureSourceIdx *int, failure lnwire.FailureMessage) (
		*channeldb.FailureReason, error)

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
	MissionControl MissionController

	// SessionSource defines a source for the router to retrieve new payment
	// sessions.
	SessionSource PaymentSessionSource

	// QueryBandwidth is a method that allows the router to query the lower
	// link layer to determine the up-to-date available bandwidth at a
	// prospective link to be traversed. If the  link isn't available, then
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
	ApplyChannelUpdate func(msg *lnwire.ChannelUpdate) bool
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
	payments, err := r.cfg.Control.FetchInFlightPayments()
	if err != nil {
		return err
	}

	// Before we restart existing payments and start accepting more
	// payments to be made, we clean the network result store of the
	// Switch. We do this here at startup to ensure no more payments can be
	// made concurrently, so we know the toKeep map will be up-to-date
	// until the cleaning has finished.
	toKeep := make(map[uint64]struct{})
	for _, p := range payments {
		for _, a := range p.HTLCs {
			toKeep[a.AttemptID] = struct{}{}
		}
	}

	log.Debugf("Cleaning network result store.")
	if err := r.cfg.Payer.CleanStore(toKeep); err != nil {
		return err
	}

	for _, payment := range payments {
		log.Infof("Resuming payment %v", payment.Info.PaymentIdentifier)
		r.wg.Add(1)
		go func(payment *channeldb.MPPayment) {
			defer r.wg.Done()

			// Get the hashes used for the outstanding HTLCs.
			htlcs := make(map[uint64]lntypes.Hash)
			for _, a := range payment.HTLCs {
				a := a

				// We check whether the individual attempts
				// have their HTLC hash set, if not we'll fall
				// back to the overall payment hash.
				hash := payment.Info.PaymentIdentifier
				if a.Hash != nil {
					hash = *a.Hash
				}

				htlcs[a.AttemptID] = hash
			}

			// Since we are not supporting creating more shards
			// after a restart (only receiving the result of the
			// shards already outstanding), we create a simple
			// shard tracker that will map the attempt IDs to
			// hashes used for the HTLCs. This will be enough also
			// for AMP payments, since we only need the hashes for
			// the individual HTLCs to regenerate the circuits, and
			// we don't currently persist the root share necessary
			// to re-derive them.
			shardTracker := shards.NewSimpleShardTracker(
				payment.Info.PaymentIdentifier, htlcs,
			)

			// We create a dummy, empty payment session such that
			// we won't make another payment attempt when the
			// result for the in-flight attempt is received.
			paySession := r.cfg.SessionSource.NewPaymentSessionEmpty()

			// We pass in a non-timeout context, to indicate we
			// don't need it to timeout. It will stop immediately
			// after the existing attempt has finished anyway. We
			// also set a zero fee limit, as no more routes should
			// be tried.
			noTimeout := time.Duration(0)
			_, _, err := r.sendPayment(
				context.Background(), 0,
				payment.Info.PaymentIdentifier, noTimeout,
				paySession, shardTracker,
			)
			if err != nil {
				log.Errorf("Resuming payment %v failed: %v.",
					payment.Info.PaymentIdentifier, err)
				return
			}

			log.Infof("Resumed payment %v completed.",
				payment.Info.PaymentIdentifier)
		}(payment)
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
}

// FindBlindedPaths finds a selection of paths to the destination node that can
// be used in blinded payment paths.
func (r *ChannelRouter) FindBlindedPaths(destination route.Vertex,
	amt lnwire.MilliSatoshi, probabilitySrc probabilitySource,
	restrictions *BlindedPathRestrictions) ([]*route.Route, error) {

	// First, find a set of candidate paths given the destination node and
	// path length restrictions.
	paths, err := findBlindedPaths(
		r.cfg.RoutingGraph, destination, &blindedPathRestrictions{
			minNumHops: restrictions.MinDistanceFromIntroNode,
			maxNumHops: restrictions.NumHops,
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

		// Don't bother adding a route if its success probability less
		// minimum that can be assigned to any single pair.
		if totalRouteProbability <= DefaultMinRouteProbability {
			continue
		}

		routes = append(routes, &routeWithProbability{
			route: &route.Route{
				SourcePubKey: introNode,
				Hops:         hops,
			},
			probability: totalRouteProbability,
		})
	}

	// Sort the routes based on probability.
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].probability < routes[j].probability
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

// generateSphinxPacket generates then encodes a sphinx packet which encodes
// the onion route specified by the passed layer 3 route. The blob returned
// from this function can immediately be included within an HTLC add packet to
// be sent to the first hop within the route.
func generateSphinxPacket(rt *route.Route, paymentHash []byte,
	sessionKey *btcec.PrivateKey) ([]byte, *sphinx.Circuit, error) {

	// Now that we know we have an actual route, we'll map the route into a
	// sphinx payment path which includes per-hop payloads for each hop
	// that give each node within the route the necessary information
	// (fees, CLTV value, etc.) to properly forward the payment.
	sphinxPath, err := rt.ToSphinxPath()
	if err != nil {
		return nil, nil, err
	}

	log.Tracef("Constructed per-hop payloads for payment_hash=%x: %v",
		paymentHash, lnutils.NewLogClosure(func() string {
			path := make(
				[]sphinx.OnionHop, sphinxPath.TrueRouteLength(),
			)
			for i := range path {
				hopCopy := sphinxPath[i]
				path[i] = hopCopy
			}
			return spew.Sdump(path)
		}),
	)

	// Next generate the onion routing packet which allows us to perform
	// privacy preserving source routing across the network.
	sphinxPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, paymentHash,
		sphinx.DeterministicPacketFiller,
	)
	if err != nil {
		return nil, nil, err
	}

	// Finally, encode Sphinx packet using its wire representation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := sphinxPacket.Encode(&onionBlob); err != nil {
		return nil, nil, err
	}

	log.Tracef("Generated sphinx packet: %v",
		lnutils.NewLogClosure(func() string {
			// We make a copy of the ephemeral key and unset the
			// internal curve here in order to keep the logs from
			// getting noisy.
			key := *sphinxPacket.EphemeralKey
			packetCopy := *sphinxPacket
			packetCopy.EphemeralKey = &key
			return spew.Sdump(packetCopy)
		}),
	)

	return onionBlob.Bytes(), &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: sphinxPath.NodeKeys(),
	}, nil
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
	PaymentAddr *[32]byte

	// PaymentRequest is an optional payment request that this payment is
	// attempting to complete.
	PaymentRequest []byte

	// DestCustomRecords are TLV records that are to be sent to the final
	// hop in the new onion payload format. If the destination does not
	// understand this new onion payload format, then the payment will
	// fail.
	DestCustomRecords record.CustomSet

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
func (r *ChannelRouter) SendPayment(payment *LightningPayment) ([32]byte,
	*route.Route, error) {

	paySession, shardTracker, err := r.PreparePayment(payment)
	if err != nil {
		return [32]byte{}, nil, err
	}

	log.Tracef("Dispatching SendPayment for lightning payment: %v",
		spewPayment(payment))

	return r.sendPayment(
		context.Background(), payment.FeeLimit, payment.Identifier(),
		payment.PayAttemptTimeout, paySession, shardTracker,
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
		return spew.Sdump(p)
	})
}

// PreparePayment creates the payment session and registers the payment with the
// control tower.
func (r *ChannelRouter) PreparePayment(payment *LightningPayment) (
	PaymentSession, shards.ShardTracker, error) {

	// Before starting the HTLC routing attempt, we'll create a fresh
	// payment session which will report our errors back to mission
	// control.
	paySession, err := r.cfg.SessionSource.NewPaymentSession(payment)
	if err != nil {
		return nil, nil, err
	}

	// Record this payment hash with the ControlTower, ensuring it is not
	// already in-flight.
	//
	// TODO(roasbeef): store records as part of creation info?
	info := &channeldb.PaymentCreationInfo{
		PaymentIdentifier: payment.Identifier(),
		Value:             payment.Amount,
		CreationTime:      r.cfg.Clock.Now(),
		PaymentRequest:    payment.PaymentRequest,
	}

	// Create a new ShardTracker that we'll use during the life cycle of
	// this payment.
	var shardTracker shards.ShardTracker
	switch {
	// If this is an AMP payment, we'll use the AMP shard tracker.
	case payment.amp != nil:
		shardTracker = amp.NewShardTracker(
			payment.amp.RootShare, payment.amp.SetID,
			*payment.PaymentAddr, payment.Amount,
		)

	// Otherwise we'll use the simple tracker that will map each attempt to
	// the same payment hash.
	default:
		shardTracker = shards.NewSimpleShardTracker(
			payment.Identifier(), nil,
		)
	}

	err = r.cfg.Control.InitPayment(payment.Identifier(), info)
	if err != nil {
		return nil, nil, err
	}

	return paySession, shardTracker, nil
}

// SendToRoute sends a payment using the provided route and fails the payment
// when an error is returned from the attempt.
func (r *ChannelRouter) SendToRoute(htlcHash lntypes.Hash,
	rt *route.Route) (*channeldb.HTLCAttempt, error) {

	return r.sendToRoute(htlcHash, rt, false)
}

// SendToRouteSkipTempErr sends a payment using the provided route and fails
// the payment ONLY when a terminal error is returned from the attempt.
func (r *ChannelRouter) SendToRouteSkipTempErr(htlcHash lntypes.Hash,
	rt *route.Route) (*channeldb.HTLCAttempt, error) {

	return r.sendToRoute(htlcHash, rt, true)
}

// sendToRoute attempts to send a payment with the given hash through the
// provided route. This function is blocking and will return the attempt
// information as it is stored in the database. For a successful htlc, this
// information will contain the preimage. If an error occurs after the attempt
// was initiated, both return values will be non-nil. If skipTempErr is true,
// the payment won't be failed unless a terminal error has occurred.
func (r *ChannelRouter) sendToRoute(htlcHash lntypes.Hash, rt *route.Route,
	skipTempErr bool) (*channeldb.HTLCAttempt, error) {

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
	info := &channeldb.PaymentCreationInfo{
		PaymentIdentifier: paymentIdentifier,
		Value:             amt,
		CreationTime:      r.cfg.Clock.Now(),
		PaymentRequest:    nil,
	}

	err := r.cfg.Control.InitPayment(paymentIdentifier, info)
	switch {
	// If this is an MPP attempt and the hash is already registered with
	// the database, we can go on to launch the shard.
	case mpp != nil && errors.Is(err, channeldb.ErrPaymentInFlight):
	case mpp != nil && errors.Is(err, channeldb.ErrPaymentExists):

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
	p := newPaymentLifecycle(r, 0, paymentIdentifier, nil, shardTracker, 0)

	// We found a route to try, create a new HTLC attempt to try.
	//
	// NOTE: we use zero `remainingAmt` here to simulate the same effect of
	// setting the lastShard to be false, which is used by previous
	// implementation.
	attempt, err := p.registerAttempt(rt, 0)
	if err != nil {
		return nil, err
	}

	// Once the attempt is created, send it to the htlcswitch. Notice that
	// the `err` returned here has already been processed by
	// `handleSwitchErr`, which means if there's a terminal failure, the
	// payment has been failed.
	result, err := p.sendAttempt(attempt)
	if err != nil {
		return nil, err
	}

	// We now look up the payment to see if it's already failed.
	payment, err := p.router.cfg.Control.FetchPayment(p.identifier)
	if err != nil {
		return result.attempt, err
	}

	// Exit if the above error has caused the payment to be failed, we also
	// return the error from sending attempt to mimic the old behavior of
	// this method.
	_, failedReason := payment.TerminalInfo()
	if failedReason != nil {
		return result.attempt, result.err
	}

	// Since for SendToRoute we won't retry in case the shard fails, we'll
	// mark the payment failed with the control tower immediately if the
	// skipTempErr is false.
	reason := channeldb.FailureReasonError

	// If we failed to send the HTLC, we need to further decide if we want
	// to fail the payment.
	if result.err != nil {
		// If skipTempErr, we'll return the attempt and the temp error.
		if skipTempErr {
			return result.attempt, result.err
		}

		// Otherwise we need to fail the payment.
		err := r.cfg.Control.FailPayment(paymentIdentifier, reason)
		if err != nil {
			return nil, err
		}

		return result.attempt, result.err
	}

	// The attempt was successfully sent, wait for the result to be
	// available.
	result, err = p.collectResult(attempt)
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
		err := r.cfg.Control.FailPayment(paymentIdentifier, reason)
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
	shardTracker shards.ShardTracker) ([32]byte, *route.Route, error) {

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

	// Now set up a paymentLifecycle struct with these params, such that we
	// can resume the payment from the current state.
	p := newPaymentLifecycle(
		r, feeLimit, identifier, paySession, shardTracker,
		currentHeight,
	)

	return p.resumePayment(ctx)
}

// extractChannelUpdate examines the error and extracts the channel update.
func (r *ChannelRouter) extractChannelUpdate(
	failure lnwire.FailureMessage) *lnwire.ChannelUpdate {

	var update *lnwire.ChannelUpdate
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
	fromNode route.Vertex
}

// Error returns a human-readable string describing the error.
func (e ErrNoChannel) Error() string {
	return fmt.Sprintf("no matching outgoing channel available for "+
		"node %v (%v)", e.position, e.fromNode)
}

// BuildRoute returns a fully specified route based on a list of pubkeys. If
// amount is nil, the minimum routable amount is used. To force a specific
// outgoing channel, use the outgoingChan parameter.
func (r *ChannelRouter) BuildRoute(amt *lnwire.MilliSatoshi,
	hops []route.Vertex, outgoingChan *uint64,
	finalCltvDelta int32, payAddr *[32]byte) (*route.Route, error) {

	log.Tracef("BuildRoute called: hopsCount=%v, amt=%v",
		len(hops), amt)

	var outgoingChans map[uint64]struct{}
	if outgoingChan != nil {
		outgoingChans = map[uint64]struct{}{
			*outgoingChan: {},
		}
	}

	// If no amount is specified, we need to build a route for the minimum
	// amount that this route can carry.
	useMinAmt := amt == nil

	var runningAmt lnwire.MilliSatoshi
	if useMinAmt {
		// For minimum amount routes, aim to deliver at least 1 msat to
		// the destination. There are nodes in the wild that have a
		// min_htlc channel policy of zero, which could lead to a zero
		// amount payment being made.
		runningAmt = 1
	} else {
		// If an amount is specified, we need to build a route that
		// delivers exactly this amount to the final destination.
		runningAmt = *amt
	}

	// We'll attempt to obtain a set of bandwidth hints that helps us select
	// the best outgoing channel to use in case no outgoing channel is set.
	bandwidthHints, err := newBandwidthManager(
		r.cfg.RoutingGraph, r.cfg.SelfNode, r.cfg.GetLink,
	)
	if err != nil {
		return nil, err
	}

	// Fetch the current block height outside the routing transaction, to
	// prevent the rpc call blocking the database.
	_, height, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, err
	}

	sourceNode := r.cfg.SelfNode
	unifiers, senderAmt, err := getRouteUnifiers(
		sourceNode, hops, useMinAmt, runningAmt, outgoingChans,
		r.cfg.RoutingGraph, bandwidthHints,
	)
	if err != nil {
		return nil, err
	}

	pathEdges, receiverAmt, err := getPathEdges(
		sourceNode, senderAmt, unifiers, bandwidthHints, hops,
	)
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

// getRouteUnifiers returns a list of edge unifiers for the given route.
func getRouteUnifiers(source route.Vertex, hops []route.Vertex,
	useMinAmt bool, runningAmt lnwire.MilliSatoshi,
	outgoingChans map[uint64]struct{}, graph Graph,
	bandwidthHints *bandwidthManager) ([]*edgeUnifier, lnwire.MilliSatoshi,
	error) {

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

		localChan := i == 0

		// Build unified policies for this hop based on the channels
		// known in the graph. Don't use inbound fees.
		//
		// TODO: Add inbound fees support for BuildRoute.
		u := newNodeEdgeUnifier(
			source, toNode, false, outgoingChans,
		)

		err := u.addGraphPolicies(graph)
		if err != nil {
			return nil, 0, err
		}

		// Exit if there are no channels.
		edgeUnifier, ok := u.edgeUnifiers[fromNode]
		if !ok {
			log.Errorf("Cannot find policy for node %v", fromNode)
			return nil, 0, ErrNoChannel{
				fromNode: fromNode,
				position: i,
			}
		}

		// If using min amt, increase amt if needed.
		if useMinAmt {
			min := edgeUnifier.minAmt()
			if min > runningAmt {
				runningAmt = min
			}
		}

		// Get an edge for the specific amount that we want to forward.
		edge := edgeUnifier.getEdge(runningAmt, bandwidthHints, 0)
		if edge == nil {
			log.Errorf("Cannot find policy with amt=%v for node %v",
				runningAmt, fromNode)

			return nil, 0, ErrNoChannel{
				fromNode: fromNode,
				position: i,
			}
		}

		// Add fee for this hop.
		if !localChan {
			runningAmt += edge.policy.ComputeFee(runningAmt)
		}

		log.Tracef("Select channel %v at position %v",
			edge.policy.ChannelID, i)

		unifiers[i] = edgeUnifier
	}

	return unifiers, runningAmt, nil
}

// getPathEdges returns the edges that make up the path and the total amount,
// including fees, to send the payment.
func getPathEdges(source route.Vertex, receiverAmt lnwire.MilliSatoshi,
	unifiers []*edgeUnifier, bandwidthHints *bandwidthManager,
	hops []route.Vertex) ([]*unifiedEdge,
	lnwire.MilliSatoshi, error) {

	// Now that we arrived at the start of the route and found out the route
	// total amount, we make a forward pass. Because the amount may have
	// been increased in the backward pass, fees need to be recalculated and
	// amount ranges re-checked.
	var pathEdges []*unifiedEdge
	for i, unifier := range unifiers {
		edge := unifier.getEdge(receiverAmt, bandwidthHints, 0)
		if edge == nil {
			fromNode := source
			if i > 0 {
				fromNode = hops[i-1]
			}

			return nil, 0, ErrNoChannel{
				fromNode: fromNode,
				position: i,
			}
		}

		if i > 0 {
			// Decrease the amount to send while going forward.
			receiverAmt -= edge.policy.ComputeFeeFromIncoming(
				receiverAmt,
			)
		}

		pathEdges = append(pathEdges, edge)
	}

	return pathEdges, receiverAmt, nil
}
