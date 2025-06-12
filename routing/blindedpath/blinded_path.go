package blindedpath

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// oneMillion is a constant used frequently in fee rate calculations.
	oneMillion = uint32(1_000_000)
)

// errInvalidBlindedPath indicates that the chosen real path is not usable as
// a blinded path.
var errInvalidBlindedPath = errors.New("the chosen path results in an " +
	"unusable blinded path")

// BuildBlindedPathCfg defines the various resources and configuration values
// required to build a blinded payment path to this node.
type BuildBlindedPathCfg struct {
	// FindRoutes returns a set of routes to us that can be used for the
	// construction of blinded paths. These routes will consist of real
	// nodes advertising the route blinding feature bit. They may be of
	// various lengths and may even contain only a single hop. Any route
	// shorter than MinNumHops will be padded with dummy hops during route
	// construction.
	FindRoutes func(value lnwire.MilliSatoshi) ([]*route.Route, error)

	// FetchChannelEdgesByID attempts to look up the two directed edges for
	// the channel identified by the channel ID.
	FetchChannelEdgesByID func(chanID uint64) (*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error)

	// FetchOurOpenChannels fetches this node's set of open channels.
	FetchOurOpenChannels func() ([]*channeldb.OpenChannel, error)

	// BestHeight can be used to fetch the best block height that this node
	// is aware of.
	BestHeight func() (uint32, error)

	// AddPolicyBuffer is a function that can be used to alter the policy
	// values of the given channel edge. The main reason for doing this is
	// to add a safety buffer so that if the node makes small policy changes
	// during the lifetime of the blinded path, then the path remains valid
	// and so probing is more difficult. Note that this will only be called
	// for the policies of real nodes and won't be applied to
	// DefaultDummyHopPolicy.
	AddPolicyBuffer func(policy *BlindedHopPolicy) (*BlindedHopPolicy,
		error)

	// PathID is the secret data to embed in the blinded path data that we
	// will receive back as the recipient. This is the equivalent of the
	// payment address used in normal payments. It lets the recipient check
	// that the path is being used in the correct context.
	PathID []byte

	// ValueMsat is the payment amount in milli-satoshis that must be
	// routed. This will be used for selecting appropriate routes to use for
	// the blinded path.
	ValueMsat lnwire.MilliSatoshi

	// MinFinalCLTVExpiryDelta is the minimum CLTV delta that the recipient
	// requires for the final hop of the payment.
	//
	// NOTE that the caller is responsible for adding additional block
	// padding to this value to account for blocks being mined while the
	// payment is in-flight.
	MinFinalCLTVExpiryDelta uint32

	// BlocksUntilExpiry is the number of blocks that this blinded path
	// should remain valid for. This is a relative number of blocks. This
	// number in addition with a potential minimum cltv delta for the last
	// hop and some block padding will be the payment constraint which is
	// part of the blinded hop info. Every htlc using the provided blinded
	// hops cannot have a higher cltv delta otherwise it will get rejected
	// by the forwarding nodes or the final node.
	//
	// This number should at least be greater than the invoice expiry time
	// so that the blinded route is always valid as long as the invoice is
	// valid.
	BlocksUntilExpiry uint32

	// MinNumHops is the minimum number of hops that each blinded path
	// should be. If the number of hops in a path returned by FindRoutes is
	// less than this number, then dummy hops will be post-fixed to the
	// route.
	MinNumHops uint8

	// DefaultDummyHopPolicy holds the policy values that should be used for
	// dummy hops in the cases where it cannot be derived via other means
	// such as averaging the policy values of other hops on the path. This
	// would happen in the case where the introduction node is also the
	// introduction node. If these default policy values are used, then
	// the MaxHTLCMsat value must be carefully chosen.
	DefaultDummyHopPolicy *BlindedHopPolicy
}

// BuildBlindedPaymentPaths uses the passed config to construct a set of blinded
// payment paths that can be added to the invoice.
func BuildBlindedPaymentPaths(cfg *BuildBlindedPathCfg) (
	[]*zpay32.BlindedPaymentPath, error) {

	// Find some appropriate routes for the value to be routed. This will
	// return a set of routes made up of real nodes.
	routes, err := cfg.FindRoutes(cfg.ValueMsat)
	if err != nil {
		return nil, err
	}

	if len(routes) == 0 {
		return nil, fmt.Errorf("could not find any routes to self to " +
			"use for blinded route construction")
	}

	// Not every route returned will necessarily result in a usable blinded
	// path and so the number of paths returned might be less than the
	// number of real routes returned by FindRoutes above.
	paths := make([]*zpay32.BlindedPaymentPath, 0, len(routes))

	// For each route returned, we will construct the associated blinded
	// payment path.
	for _, route := range routes {
		// Extract the information we need from the route.
		candidatePath := extractCandidatePath(route)

		// Pad the given route with dummy hops until the minimum number
		// of hops is met.
		candidatePath.padWithDummyHops(cfg.MinNumHops)

		path, err := buildBlindedPaymentPath(cfg, candidatePath)
		if errors.Is(err, errInvalidBlindedPath) {
			log.Debugf("Not using route (%s) as a blinded path "+
				"since it resulted in an invalid blinded path",
				route)

			continue
		} else if err != nil {
			log.Errorf("Not using route (%s) as a blinded path: %v",
				route, err)

			continue
		}

		log.Debugf("Route selected for blinded path: %s", candidatePath)

		paths = append(paths, path)
	}

	if len(paths) == 0 {
		return nil, fmt.Errorf("could not build any blinded paths")
	}

	return paths, nil
}

// buildBlindedPaymentPath takes a route from an introduction node to this node
// and uses the given config to convert it into a blinded payment path.
func buildBlindedPaymentPath(cfg *BuildBlindedPathCfg, path *candidatePath) (
	*zpay32.BlindedPaymentPath, error) {

	hops, minHTLC, maxHTLC, err := collectRelayInfo(cfg, path)
	if err != nil {
		return nil, fmt.Errorf("could not collect blinded path relay "+
			"info: %w", err)
	}

	relayInfo := make([]*record.PaymentRelayInfo, len(hops))
	for i, hop := range hops {
		relayInfo[i] = hop.relayInfo
	}

	// Using the collected relay info, we can calculate the aggregated
	// policy values for the route.
	baseFee, feeRate, cltvDelta := calcBlindedPathPolicies(
		relayInfo, uint16(cfg.MinFinalCLTVExpiryDelta),
	)

	currentHeight, err := cfg.BestHeight()
	if err != nil {
		return nil, err
	}

	// The next step is to calculate the payment constraints to communicate
	// to each hop and to package up the hop info for each hop. We will
	// handle the final hop first since its payload looks a bit different,
	// and then we will iterate backwards through the remaining hops.
	//
	// Note that the +1 here is required because the route won't have the
	// introduction node included in the "Hops". But since we want to create
	// payloads for all the hops as well as the introduction node, we add 1
	// here to get the full hop length along with the introduction node.
	hopDataSet := make([]*hopData, 0, len(path.hops)+1)

	// Determine the maximum CLTV expiry for the destination node.
	cltvExpiry := currentHeight + cfg.BlocksUntilExpiry +
		cfg.MinFinalCLTVExpiryDelta

	constraints := &record.PaymentConstraints{
		MaxCltvExpiry:   cltvExpiry,
		HtlcMinimumMsat: minHTLC,
	}

	// If the blinded route has only a source node (introduction node) and
	// no hops, then the destination node is also the source node.
	finalHopPubKey := path.introNode
	if len(path.hops) > 0 {
		finalHopPubKey = path.hops[len(path.hops)-1].pubKey
	}

	// For the final hop, we only send it the path ID and payment
	// constraints.
	info, err := buildFinalHopRouteData(
		finalHopPubKey, cfg.PathID, constraints,
	)
	if err != nil {
		return nil, err
	}

	hopDataSet = append(hopDataSet, info)

	// Iterate through the remaining (non-final) hops, back to front.
	for i := len(hops) - 1; i >= 0; i-- {
		hop := hops[i]

		cltvExpiry += uint32(hop.relayInfo.CltvExpiryDelta)

		constraints = &record.PaymentConstraints{
			MaxCltvExpiry:   cltvExpiry,
			HtlcMinimumMsat: minHTLC,
		}

		var info *hopData
		if hop.nextHopIsDummy {
			info, err = buildDummyRouteData(
				hop.hopPubKey, hop.relayInfo, constraints,
			)
		} else {
			info, err = buildHopRouteData(
				hop.hopPubKey, hop.nextSCID, hop.relayInfo,
				constraints,
			)
		}
		if err != nil {
			return nil, err
		}

		hopDataSet = append(hopDataSet, info)
	}

	// Sort the hop info list in reverse order so that the data for the
	// introduction node is first.
	sort.Slice(hopDataSet, func(i, j int) bool {
		return j < i
	})

	// Add padding to each route data instance until the encrypted data
	// blobs are all the same size.
	paymentPath, _, err := padHopInfo(
		hopDataSet, true, record.AverageDummyHopPayloadSize,
	)
	if err != nil {
		return nil, err
	}

	// Derive an ephemeral session key.
	sessionKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	// Encrypt the hop info.
	blindedPathInfo, err := sphinx.BuildBlindedPath(sessionKey, paymentPath)
	if err != nil {
		return nil, err
	}
	blindedPath := blindedPathInfo.Path

	if len(blindedPath.BlindedHops) < 1 {
		return nil, fmt.Errorf("blinded path must have at least one " +
			"hop")
	}

	// Overwrite the introduction point's blinded pub key with the real
	// pub key since then we can use this more compact format in the
	// invoice without needing to encode the un-used blinded node pub key of
	// the intro node.
	blindedPath.BlindedHops[0].BlindedNodePub =
		blindedPath.IntroductionPoint

	// Now construct a z32 blinded path.
	return &zpay32.BlindedPaymentPath{
		FeeBaseMsat:                 uint32(baseFee),
		FeeRate:                     feeRate,
		CltvExpiryDelta:             cltvDelta,
		HTLCMinMsat:                 uint64(minHTLC),
		HTLCMaxMsat:                 uint64(maxHTLC),
		Features:                    lnwire.EmptyFeatureVector(),
		FirstEphemeralBlindingPoint: blindedPath.BlindingPoint,
		Hops:                        blindedPath.BlindedHops,
	}, nil
}

// hopRelayInfo packages together the relay info to send to hop on a blinded
// path along with the pub key of that hop and the SCID that the hop should
// forward the payment on to.
type hopRelayInfo struct {
	hopPubKey      route.Vertex
	nextSCID       lnwire.ShortChannelID
	relayInfo      *record.PaymentRelayInfo
	nextHopIsDummy bool
}

// collectRelayInfo collects the relay policy rules for each relay hop on the
// route and applies any policy buffers.
//
// For the blinded route:
//
//	C --chan(CB)--> B --chan(BA)--> A
//
// where C is the introduction node, the route.Route struct we are given will
// have SourcePubKey set to C's pub key, and then it will have the following
// route.Hops:
//
//   - PubKeyBytes: B, ChannelID: chan(CB)
//   - PubKeyBytes: A, ChannelID: chan(BA)
//
// We, however, want to collect the channel policies for the following PubKey
// and ChannelID pairs:
//
//   - PubKey: C, ChannelID: chan(CB)
//   - PubKey: B, ChannelID: chan(BA)
//
// Therefore, when we go through the route and its hops to collect policies, our
// index for collecting public keys will be trailing that of the channel IDs by
// 1.
//
// For any dummy hops on the route, this function also decides what to use as
// policy values for the dummy hops. If there are other real hops, then the
// dummy hop policy values are derived by taking the average of the real
// policy values. If there are no real hops (in other words we are the
// introduction node), then we use some default routing values and we use the
// average of our channel capacities for the MaxHTLC value.
func collectRelayInfo(cfg *BuildBlindedPathCfg, path *candidatePath) (
	[]*hopRelayInfo, lnwire.MilliSatoshi, lnwire.MilliSatoshi, error) {

	var (
		// The first pub key is that of the introduction node.
		hopSource = path.introNode

		// A collection of the policy values of real hops on the path.
		policies = make(map[uint64]*BlindedHopPolicy)

		hasDummyHops bool
	)

	// On this first iteration, we just collect policy values of the real
	// hops on the path.
	for _, hop := range path.hops {
		// Once we have hit a dummy hop, all hops after will be dummy
		// hops too.
		if hop.isDummy {
			hasDummyHops = true

			break
		}

		// For real hops, retrieve the channel policy for this hop's
		// channel ID in the direction pointing away from the hopSource
		// node.
		policy, err := getNodeChannelPolicy(
			cfg, hop.channelID, hopSource,
		)
		if err != nil {
			return nil, 0, 0, err
		}

		policies[hop.channelID] = policy

		// This hop's pub key will be the policy creator for the next
		// hop.
		hopSource = hop.pubKey
	}

	var (
		dummyHopPolicy *BlindedHopPolicy
		err            error
	)

	// If the path does have dummy hops, we need to decide which policy
	// values to use for these hops.
	if hasDummyHops {
		dummyHopPolicy, err = computeDummyHopPolicy(
			cfg.DefaultDummyHopPolicy, cfg.FetchOurOpenChannels,
			policies,
		)
		if err != nil {
			return nil, 0, 0, err
		}
	}

	// We iterate through the hops one more time. This time it is to
	// buffer the policy values, collect the payment relay info to send to
	// each hop, and to compute the min and max HTLC values for the path.
	var (
		hops    = make([]*hopRelayInfo, 0, len(path.hops))
		minHTLC lnwire.MilliSatoshi
		maxHTLC lnwire.MilliSatoshi
	)
	// The first pub key is that of the introduction node.
	hopSource = path.introNode
	for _, hop := range path.hops {
		var (
			policy = dummyHopPolicy
			ok     bool
			err    error
		)

		if !hop.isDummy {
			policy, ok = policies[hop.channelID]
			if !ok {
				return nil, 0, 0, fmt.Errorf("no cached "+
					"policy found for channel ID: %d",
					hop.channelID)
			}
		}

		if policy.MinHTLCMsat > cfg.ValueMsat {
			return nil, 0, 0, fmt.Errorf("%w: minHTLC of hop "+
				"policy larger than payment amt: sentAmt(%v), "+
				"minHTLC(%v)", errInvalidBlindedPath,
				cfg.ValueMsat, policy.MinHTLCMsat)
		}

		bufferPolicy, err := cfg.AddPolicyBuffer(policy)
		if err != nil {
			return nil, 0, 0, err
		}

		// We only use the new buffered policy if the new minHTLC value
		// does not violate the sender amount.
		//
		// NOTE: We don't check this for maxHTLC, because the payment
		// amount can always be splitted using MPP.
		if bufferPolicy.MinHTLCMsat <= cfg.ValueMsat {
			policy = bufferPolicy
		}

		// If this is the first policy we are collecting, then use this
		// policy to set the base values for min/max htlc.
		if len(hops) == 0 {
			minHTLC = policy.MinHTLCMsat
			maxHTLC = policy.MaxHTLCMsat
		} else {
			if policy.MinHTLCMsat > minHTLC {
				minHTLC = policy.MinHTLCMsat
			}

			if policy.MaxHTLCMsat < maxHTLC {
				maxHTLC = policy.MaxHTLCMsat
			}
		}

		// From the policy values for this hop, we can collect the
		// payment relay info that we will send to this hop.
		hops = append(hops, &hopRelayInfo{
			hopPubKey: hopSource,
			nextSCID:  lnwire.NewShortChanIDFromInt(hop.channelID),
			relayInfo: &record.PaymentRelayInfo{
				FeeRate:         policy.FeeRate,
				BaseFee:         policy.BaseFee,
				CltvExpiryDelta: policy.CLTVExpiryDelta,
			},
			nextHopIsDummy: hop.isDummy,
		})

		// This hop's pub key will be the policy creator for the next
		// hop.
		hopSource = hop.pubKey
	}

	// It can happen that there is no HTLC-range overlap between the various
	// hops along the path. We return errInvalidBlindedPath to indicate that
	// this route was not usable
	if minHTLC > maxHTLC {
		return nil, 0, 0, fmt.Errorf("%w: resulting blinded path min "+
			"HTLC value is larger than the resulting max HTLC "+
			"value", errInvalidBlindedPath)
	}

	return hops, minHTLC, maxHTLC, nil
}

// buildDummyRouteData constructs the record.BlindedRouteData struct for the
// given a hop in a blinded route where the following hop is a dummy hop.
func buildDummyRouteData(node route.Vertex, relayInfo *record.PaymentRelayInfo,
	constraints *record.PaymentConstraints) (*hopData, error) {

	nodeID, err := btcec.ParsePubKey(node[:])
	if err != nil {
		return nil, err
	}

	return &hopData{
		data: record.NewDummyHopRouteData(
			nodeID, *relayInfo, *constraints,
		),
		nodeID: nodeID,
	}, nil
}

// computeDummyHopPolicy determines policy values to use for a dummy hop on a
// blinded path. If other real policy values exist, then we use the average of
// those values for the dummy hop policy values. Otherwise, in the case were
// there are no real policy values due to this node being the introduction node,
// we use the provided default policy values, and we get the average capacity of
// this node's channels to compute a MaxHTLC value.
func computeDummyHopPolicy(defaultPolicy *BlindedHopPolicy,
	fetchOurChannels func() ([]*channeldb.OpenChannel, error),
	policies map[uint64]*BlindedHopPolicy) (*BlindedHopPolicy, error) {

	numPolicies := len(policies)

	// If there are no real policies to calculate an average policy from,
	// then we use the default. The only thing we need to calculate here
	// though is the MaxHTLC value.
	if numPolicies == 0 {
		chans, err := fetchOurChannels()
		if err != nil {
			return nil, err
		}

		if len(chans) == 0 {
			return nil, fmt.Errorf("node has no channels to " +
				"receive on")
		}

		// Calculate the average channel capacity and use this as the
		// MaxHTLC value.
		var maxHTLC btcutil.Amount
		for _, c := range chans {
			maxHTLC += c.Capacity
		}

		maxHTLC = btcutil.Amount(float64(maxHTLC) / float64(len(chans)))

		return &BlindedHopPolicy{
			CLTVExpiryDelta: defaultPolicy.CLTVExpiryDelta,
			FeeRate:         defaultPolicy.FeeRate,
			BaseFee:         defaultPolicy.BaseFee,
			MinHTLCMsat:     defaultPolicy.MinHTLCMsat,
			MaxHTLCMsat:     lnwire.NewMSatFromSatoshis(maxHTLC),
		}, nil
	}

	var avgPolicy BlindedHopPolicy

	for _, policy := range policies {
		avgPolicy.MinHTLCMsat += policy.MinHTLCMsat
		avgPolicy.MaxHTLCMsat += policy.MaxHTLCMsat
		avgPolicy.BaseFee += policy.BaseFee
		avgPolicy.FeeRate += policy.FeeRate
		avgPolicy.CLTVExpiryDelta += policy.CLTVExpiryDelta
	}

	avgPolicy.MinHTLCMsat = lnwire.MilliSatoshi(
		float64(avgPolicy.MinHTLCMsat) / float64(numPolicies),
	)
	avgPolicy.MaxHTLCMsat = lnwire.MilliSatoshi(
		float64(avgPolicy.MaxHTLCMsat) / float64(numPolicies),
	)
	avgPolicy.BaseFee = lnwire.MilliSatoshi(
		float64(avgPolicy.BaseFee) / float64(numPolicies),
	)
	avgPolicy.FeeRate = uint32(
		float64(avgPolicy.FeeRate) / float64(numPolicies),
	)
	avgPolicy.CLTVExpiryDelta = uint16(
		float64(avgPolicy.CLTVExpiryDelta) / float64(numPolicies),
	)

	return &avgPolicy, nil
}

// buildHopRouteData constructs the record.BlindedRouteData struct for the given
// non-final hop on a blinded path and packages it with the node's ID.
func buildHopRouteData(node route.Vertex, scid lnwire.ShortChannelID,
	relayInfo *record.PaymentRelayInfo,
	constraints *record.PaymentConstraints) (*hopData, error) {

	// Wrap up the data we want to send to this hop.
	blindedRouteHopData := record.NewNonFinalBlindedRouteData(
		scid, nil, *relayInfo, constraints, nil,
	)

	nodeID, err := btcec.ParsePubKey(node[:])
	if err != nil {
		return nil, err
	}

	return &hopData{
		data:   blindedRouteHopData,
		nodeID: nodeID,
	}, nil
}

// buildFinalHopRouteData constructs the record.BlindedRouteData struct for the
// final hop and packages it with the real node ID of the node it is intended
// for.
func buildFinalHopRouteData(node route.Vertex, pathID []byte,
	constraints *record.PaymentConstraints) (*hopData, error) {

	blindedRouteHopData := record.NewFinalHopBlindedRouteData(
		constraints, pathID,
	)
	nodeID, err := btcec.ParsePubKey(node[:])
	if err != nil {
		return nil, err
	}

	return &hopData{
		data:   blindedRouteHopData,
		nodeID: nodeID,
	}, nil
}

// getNodeChanPolicy fetches the routing policy info for the given channel and
// node pair.
func getNodeChannelPolicy(cfg *BuildBlindedPathCfg, chanID uint64,
	nodeID route.Vertex) (*BlindedHopPolicy, error) {

	// Attempt to fetch channel updates for the given channel. We will have
	// at most two updates for a given channel.
	_, update1, update2, err := cfg.FetchChannelEdgesByID(chanID)
	if err != nil {
		return nil, err
	}

	// Now we need to determine which of the updates was created by the
	// node in question. We know the update is the correct one if the
	// "ToNode" for the fetched policy is _not_ equal to the node ID in
	// question.
	var policy *models.ChannelEdgePolicy
	switch {
	case update1 != nil && !bytes.Equal(update1.ToNode[:], nodeID[:]):
		policy = update1

	case update2 != nil && !bytes.Equal(update2.ToNode[:], nodeID[:]):
		policy = update2

	default:
		return nil, fmt.Errorf("no channel updates found from node "+
			"%s for channel %d", nodeID, chanID)
	}

	return &BlindedHopPolicy{
		CLTVExpiryDelta: policy.TimeLockDelta,
		FeeRate:         uint32(policy.FeeProportionalMillionths),
		BaseFee:         policy.FeeBaseMSat,
		MinHTLCMsat:     policy.MinHTLC,
		MaxHTLCMsat:     policy.MaxHTLC,
	}, nil
}

// candidatePath holds all the information about a route to this node that we
// need in order to build a blinded route.
type candidatePath struct {
	introNode   route.Vertex
	finalNodeID route.Vertex
	hops        []*blindedPathHop
}

// String returns a string representation of the candidatePath which can be
// useful for logging and debugging.
func (c *candidatePath) String() string {
	str := fmt.Sprintf("[%s (intro node)]", c.introNode)

	for _, hop := range c.hops {
		if hop.isDummy {
			str += "--->[dummy hop]"
			continue
		}

		str += fmt.Sprintf("--<%d>-->[%s]", hop.channelID, hop.pubKey)
	}

	return str
}

// padWithDummyHops will append n dummy hops to the candidatePath hop set. The
// pub key for the dummy hop will be the same as the pub key for the final hop
// of the path. That way, the final hop will be able to decrypt the data
// encrypted for each dummy hop.
func (c *candidatePath) padWithDummyHops(n uint8) {
	for len(c.hops) < int(n) {
		c.hops = append(c.hops, &blindedPathHop{
			pubKey:  c.finalNodeID,
			isDummy: true,
		})
	}
}

// blindedPathHop holds the information we need to know about a hop in a route
// in order to use it in the construction of a blinded path.
type blindedPathHop struct {
	// pubKey is the real pub key of a node on a blinded path.
	pubKey route.Vertex

	// channelID is the channel along which the previous hop should forward
	// their HTLC in order to reach this hop.
	channelID uint64

	// isDummy is true if this hop is an appended dummy hop.
	isDummy bool
}

// extractCandidatePath extracts the data it needs from the given route.Route in
// order to construct a candidatePath.
func extractCandidatePath(path *route.Route) *candidatePath {
	var (
		hops      = make([]*blindedPathHop, len(path.Hops))
		finalNode = path.SourcePubKey
	)
	for i, hop := range path.Hops {
		hops[i] = &blindedPathHop{
			pubKey:    hop.PubKeyBytes,
			channelID: hop.ChannelID,
		}

		if i == len(path.Hops)-1 {
			finalNode = hop.PubKeyBytes
		}
	}

	return &candidatePath{
		introNode:   path.SourcePubKey,
		finalNodeID: finalNode,
		hops:        hops,
	}
}

// BlindedHopPolicy holds the set of relay policy values to use for a channel
// in a blinded path.
type BlindedHopPolicy struct {
	CLTVExpiryDelta uint16
	FeeRate         uint32
	BaseFee         lnwire.MilliSatoshi
	MinHTLCMsat     lnwire.MilliSatoshi
	MaxHTLCMsat     lnwire.MilliSatoshi
}

// AddPolicyBuffer constructs the bufferedChanPolicies for a path hop by taking
// its actual policy values and multiplying them by the given multipliers.
// The base fee, fee rate and minimum HTLC msat values are adjusted via the
// incMultiplier while the maximum HTLC msat value is adjusted via the
// decMultiplier. If adjustments of the HTLC values no longer make sense
// then the original HTLC value is used.
func AddPolicyBuffer(policy *BlindedHopPolicy, incMultiplier,
	decMultiplier float64) (*BlindedHopPolicy, error) {

	if incMultiplier < 1 {
		return nil, fmt.Errorf("blinded path policy increase " +
			"multiplier must be greater than or equal to 1")
	}

	if decMultiplier < 0 || decMultiplier > 1 {
		return nil, fmt.Errorf("blinded path policy decrease " +
			"multiplier must be in the range [0;1]")
	}

	var (
		minHTLCMsat = lnwire.MilliSatoshi(
			float64(policy.MinHTLCMsat) * incMultiplier,
		)
		maxHTLCMsat = lnwire.MilliSatoshi(
			float64(policy.MaxHTLCMsat) * decMultiplier,
		)
	)

	// Make sure the new minimum is not more than the original maximum.
	// If it is, then just stick to the original minimum.
	if minHTLCMsat > policy.MaxHTLCMsat {
		minHTLCMsat = policy.MinHTLCMsat
	}

	// Make sure the new maximum is not less than the original minimum.
	// If it is, then just stick to the original maximum.
	if maxHTLCMsat < policy.MinHTLCMsat {
		maxHTLCMsat = policy.MaxHTLCMsat
	}

	// Also ensure that the new htlc bounds make sense. If the new minimum
	// is greater than the new maximum, then just let both to their original
	// values.
	if minHTLCMsat > maxHTLCMsat {
		minHTLCMsat = policy.MinHTLCMsat
		maxHTLCMsat = policy.MaxHTLCMsat
	}

	return &BlindedHopPolicy{
		CLTVExpiryDelta: uint16(
			float64(policy.CLTVExpiryDelta) * incMultiplier,
		),
		FeeRate: uint32(
			float64(policy.FeeRate) * incMultiplier,
		),
		BaseFee: lnwire.MilliSatoshi(
			float64(policy.BaseFee) * incMultiplier,
		),
		MinHTLCMsat: minHTLCMsat,
		MaxHTLCMsat: maxHTLCMsat,
	}, nil
}

// calcBlindedPathPolicies computes the accumulated policy values for the path.
// These values include the total base fee, the total proportional fee and the
// total CLTV delta. This function assumes that all the passed relay infos have
// already been adjusted with a buffer to account for easy probing attacks.
func calcBlindedPathPolicies(relayInfo []*record.PaymentRelayInfo,
	ourMinFinalCLTVDelta uint16) (lnwire.MilliSatoshi, uint32, uint16) {

	var (
		totalFeeBase lnwire.MilliSatoshi
		totalFeeProp uint32
		totalCLTV    = ourMinFinalCLTVDelta
	)
	// Use the algorithms defined in BOLT 4 to calculate the accumulated
	// relay fees for the route:
	//nolint:ll
	// https://github.com/lightning/bolts/blob/db278ab9b2baa0b30cfe79fb3de39280595938d3/04-onion-routing.md?plain=1#L255
	for i := len(relayInfo) - 1; i >= 0; i-- {
		info := relayInfo[i]

		totalFeeBase = calcNextTotalBaseFee(
			totalFeeBase, info.BaseFee, info.FeeRate,
		)

		totalFeeProp = calcNextTotalFeeRate(totalFeeProp, info.FeeRate)

		totalCLTV += info.CltvExpiryDelta
	}

	return totalFeeBase, totalFeeProp, totalCLTV
}

// calcNextTotalBaseFee takes the current total accumulated base fee of a
// blinded path at hop `n` along with the fee rate and base fee of the hop at
// `n+1` and uses these to calculate the accumulated base fee at hop `n+1`.
func calcNextTotalBaseFee(currentTotal, hopBaseFee lnwire.MilliSatoshi,
	hopFeeRate uint32) lnwire.MilliSatoshi {

	numerator := (uint32(hopBaseFee) * oneMillion) +
		(uint32(currentTotal) * (oneMillion + hopFeeRate)) +
		oneMillion - 1

	return lnwire.MilliSatoshi(numerator / oneMillion)
}

// calculateNextTotalFeeRate takes the current total accumulated fee rate of a
// blinded path at hop `n` along with the fee rate of the hop at `n+1` and uses
// these to calculate the accumulated fee rate at hop `n+1`.
func calcNextTotalFeeRate(currentTotal, hopFeeRate uint32) uint32 {
	numerator := (currentTotal+hopFeeRate)*oneMillion +
		currentTotal*hopFeeRate + oneMillion - 1

	return numerator / oneMillion
}

// hopData packages the record.BlindedRouteData for a hop on a blinded path with
// the real node ID of that hop.
type hopData struct {
	data   *record.BlindedRouteData
	nodeID *btcec.PublicKey
}

// padStats can be used to keep track of various pieces of data that we collect
// during a call to padHopInfo. This is useful for logging and for test
// assertions.
type padStats struct {
	minPayloadSize  int
	maxPayloadSize  int
	finalPaddedSize int
	numIterations   int
}

// padHopInfo iterates over a set of record.BlindedRouteData and adds padding
// where needed until the resulting encrypted data blobs are all the same size.
// This may take a few iterations due to the fact that a TLV field is used to
// add this padding. For example, if we want to add a 1 byte padding to a
// record.BlindedRouteData when it does not yet have any padding, then adding
// a 1 byte padding will actually add 3 bytes due to the bytes required when
// adding the initial type and length bytes. However, on the next iteration if
// we again add just 1 byte, then only a single byte will be added. The same
// iteration is required for padding values on the BigSize encoding bucket
// edges. The number of iterations that this function takes is also returned for
// testing purposes. If prePad is true, then zero byte padding is added to each
// payload that does not yet have padding. This will save some iterations for
// the majority of cases. minSize can be used to specify a minimum size that all
// payloads should be.
func padHopInfo(hopInfo []*hopData, prePad bool, minSize int) (
	[]*sphinx.HopInfo, *padStats, error) {

	var (
		paymentPath = make([]*sphinx.HopInfo, len(hopInfo))
		stats       = padStats{finalPaddedSize: minSize}
	)

	// Pre-pad each payload with zero byte padding (if it does not yet have
	// padding) to save a couple of iterations in the majority of cases.
	if prePad {
		for _, info := range hopInfo {
			if info.data.Padding.IsSome() {
				continue
			}

			info.data.PadBy(0)
		}
	}

	for {
		stats.numIterations++

		// On each iteration of the loop, we first determine the
		// current largest encoded data blob size. This will be the
		// size we aim to get the others to match.
		var (
			maxLen = minSize
			minLen = math.MaxInt8
		)
		for i, hop := range hopInfo {
			plainText, err := record.EncodeBlindedRouteData(
				hop.data,
			)
			if err != nil {
				return nil, nil, err
			}

			if len(plainText) > maxLen {
				maxLen = len(plainText)

				// Update the stats to take note of this new
				// max since this may be the final max that all
				// payloads will be padded to.
				stats.finalPaddedSize = maxLen
			}
			if len(plainText) < minLen {
				minLen = len(plainText)
			}

			paymentPath[i] = &sphinx.HopInfo{
				NodePub:   hop.nodeID,
				PlainText: plainText,
			}
		}

		// If this is our first iteration, then we take note of the min
		// and max lengths of the payloads pre-padding for logging
		// later.
		if stats.numIterations == 1 {
			stats.minPayloadSize = minLen
			stats.maxPayloadSize = maxLen
		}

		// Now we iterate over them again and determine which ones we
		// need to add padding to.
		var numEqual int
		for i, hop := range hopInfo {
			plainText := paymentPath[i].PlainText

			// If the plaintext length is equal to the desired
			// length, then we can continue. We use numEqual to
			// keep track of how many have the same length.
			if len(plainText) == maxLen {
				numEqual++

				continue
			}

			// If we previously added padding to this hop, we keep
			// the length of that initial padding too.
			var existingPadding int
			hop.data.Padding.WhenSome(
				func(p tlv.RecordT[tlv.TlvType1, []byte]) {
					existingPadding = len(p.Val)
				},
			)

			// Add some padding bytes to the hop.
			hop.data.PadBy(
				existingPadding + maxLen - len(plainText),
			)
		}

		// If all the payloads have the same length, we can exit the
		// loop.
		if numEqual == len(hopInfo) {
			break
		}
	}

	log.Debugf("Finished padding %d blinded path payloads to %d bytes "+
		"each where the pre-padded min and max sizes were %d and %d "+
		"bytes respectively", len(hopInfo), stats.finalPaddedSize,
		stats.minPayloadSize, stats.maxPayloadSize)

	return paymentPath, &stats, nil
}
