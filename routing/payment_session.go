package routing

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/route"
)

// BlockPadding is used to increment the finalCltvDelta value for the last hop
// to prevent an HTLC being failed if some blocks are mined while it's in-flight.
const BlockPadding uint16 = 3

// ValidateCLTVLimit is a helper function that validates that the cltv limit is
// greater than the final cltv delta parameter, optionally including the
// BlockPadding in this calculation.
func ValidateCLTVLimit(limit uint32, delta uint16, includePad bool) error {
	if includePad {
		delta += BlockPadding
	}

	if limit <= uint32(delta) {
		return fmt.Errorf("cltv limit %v should be greater than %v",
			limit, delta)
	}

	return nil
}

// noRouteError encodes a non-critical error encountered during path finding.
type noRouteError uint8

const (
	// errNoTlvPayload is returned when the destination hop does not support
	// a tlv payload.
	errNoTlvPayload noRouteError = iota

	// errNoPaymentAddr is returned when the destination hop does not
	// support payment addresses.
	errNoPaymentAddr

	// errNoPathFound is returned when a path to the target destination does
	// not exist in the graph.
	errNoPathFound

	// errInsufficientLocalBalance is returned when none of the local
	// channels have enough balance for the payment.
	errInsufficientBalance

	// errEmptyPaySession is returned when the empty payment session is
	// queried for a route.
	errEmptyPaySession

	// errUnknownRequiredFeature is returned when the destination node
	// requires an unknown feature.
	errUnknownRequiredFeature

	// errMissingDependentFeature is returned when the destination node
	// misses a feature that a feature that we require depends on.
	errMissingDependentFeature
)

var (
	// DefaultShardMinAmt is the default amount beyond which we won't try to
	// further split the payment if no route is found. It is the minimum
	// amount that we use as the shard size when splitting.
	DefaultShardMinAmt = lnwire.NewMSatFromSatoshis(10000)
)

// adaptiveSplitLadder is the fixed ladder of fractions of the failing amount
// that the bound-aware split probes, largest first. Its length is the probe
// budget: a no-route result costs at most this many extra path finding calls,
// against the unbounded log2(amt/minShard) halvings the blind policy can
// spend.
var adaptiveSplitLadder = [...]float64{0.75, 0.5, 0.375, 0.25, 0.125}

// Error returns the string representation of the noRouteError.
func (e noRouteError) Error() string {
	switch e {
	case errNoTlvPayload:
		return "destination hop doesn't understand new TLV payloads"

	case errNoPaymentAddr:
		return "destination hop doesn't understand payment addresses"

	case errNoPathFound:
		return "unable to find a path to destination"

	case errEmptyPaySession:
		return "empty payment session"

	case errInsufficientBalance:
		return "insufficient local balance"

	case errUnknownRequiredFeature:
		return "unknown required feature"

	case errMissingDependentFeature:
		return "missing dependent feature"

	default:
		return "unknown no-route error"
	}
}

// FailureReason converts a path finding error into a payment-level failure.
func (e noRouteError) FailureReason() paymentsdb.FailureReason {
	switch e {
	case
		errNoTlvPayload,
		errNoPaymentAddr,
		errNoPathFound,
		errEmptyPaySession,
		errUnknownRequiredFeature,
		errMissingDependentFeature:

		return paymentsdb.FailureReasonNoRoute

	case errInsufficientBalance:
		return paymentsdb.FailureReasonInsufficientBalance

	default:
		return paymentsdb.FailureReasonError
	}
}

// PaymentSession is used during SendPayment attempts to provide routes to
// attempt. It also defines methods to give the PaymentSession additional
// information learned during the previous attempts.
type PaymentSession interface {
	// RequestRoute returns the next route to attempt for routing the
	// specified HTLC payment to the target node. The returned route should
	// carry at most maxAmt to the target node, and pay at most feeLimit in
	// fees. It can carry less if the payment is MPP. The activeShards
	// argument should be set to instruct the payment session about the
	// number of in flight HTLCS for the payment, such that it can choose
	// splitting strategy accordingly.
	//
	// A noRouteError is returned if a non-critical error is encountered
	// during path finding.
	RequestRoute(maxAmt, feeLimit lnwire.MilliSatoshi,
		activeShards, height uint32,
		firstHopCustomRecords lnwire.CustomRecords) (*route.Route,
		error)

	// UpdateAdditionalEdge takes an additional channel edge policy
	// (private channels) and applies the update from the message. Returns
	// a boolean to indicate whether the update has been applied without
	// error.
	UpdateAdditionalEdge(msg *lnwire.ChannelUpdate1,
		pubKey *btcec.PublicKey, policy *models.CachedEdgePolicy) bool

	// GetAdditionalEdgePolicy uses the public key and channel ID to query
	// the ephemeral channel edge policy for additional edges. Returns a nil
	// if nothing found.
	GetAdditionalEdgePolicy(pubKey *btcec.PublicKey,
		channelID uint64) *models.CachedEdgePolicy
}

// paymentSession is used during an HTLC routings session to prune the local
// chain view in response to failures, and also report those failures back to
// MissionController. The snapshot copied for this session will only ever grow,
// and will now be pruned after a decay like the main view within mission
// control. We do this as we want to avoid the case where we continually try a
// bad edge or route multiple times in a session. This can lead to an infinite
// loop if payment attempts take long enough. An additional set of edges can
// also be provided to assist in reaching the payment's destination.
type paymentSession struct {
	selfNode route.Vertex

	additionalEdges map[route.Vertex][]AdditionalEdge

	getBandwidthHints func(Graph) (bandwidthHints, error)

	payment *LightningPayment

	empty bool

	pathFinder pathFinder

	graphSessFactory GraphSessionFactory

	// pathFindingConfig defines global parameters that control the
	// trade-off in path finding between fees and probability.
	pathFindingConfig PathFindingConfig

	missionControl MissionControlQuerier

	// minShardAmt is the amount beyond which we won't try to further split
	// the payment if no route is found. If the maximum number of htlcs
	// specified in the payment is one, under no circumstances splitting
	// will happen and this value remains unused.
	minShardAmt lnwire.MilliSatoshi

	// log is a payment session-specific logger.
	log btclog.Logger
}

// newPaymentSession instantiates a new payment session.
func newPaymentSession(p *LightningPayment, selfNode route.Vertex,
	getBandwidthHints func(Graph) (bandwidthHints, error),
	graphSessFactory GraphSessionFactory,
	missionControl MissionControlQuerier,
	pathFindingConfig PathFindingConfig) (*paymentSession, error) {

	edges, err := RouteHintsToEdges(p.RouteHints, p.Target)
	if err != nil {
		return nil, err
	}

	if p.BlindedPathSet != nil {
		if len(edges) != 0 {
			return nil, fmt.Errorf("cannot have both route hints " +
				"and blinded path")
		}

		edges, err = p.BlindedPathSet.ToRouteHints()
		if err != nil {
			return nil, err
		}
	}

	logPrefix := fmt.Sprintf("PaymentSession(%x):", p.Identifier())

	return &paymentSession{
		selfNode:          selfNode,
		additionalEdges:   edges,
		getBandwidthHints: getBandwidthHints,
		payment:           p,
		pathFinder:        findPath,
		graphSessFactory:  graphSessFactory,
		pathFindingConfig: pathFindingConfig,
		missionControl:    missionControl,
		minShardAmt:       DefaultShardMinAmt,
		log:               log.WithPrefix(logPrefix),
	}, nil
}

// pathFindingError is a wrapper error type that is used to distinguish path
// finding errors from other errors in path finding loop.
type pathFindingError struct {
	error
}

// Unwrap returns the underlying error.
func (e *pathFindingError) Unwrap() error {
	return e.error
}

// RequestRoute returns a route which is likely to be capable for successfully
// routing the specified HTLC payment to the target node. Initially the first
// set of paths returned from this method may encounter routing failure along
// the way, however as more payments are sent, mission control will start to
// build an up to date view of the network itself. With each payment a new area
// will be explored, which feeds into the recommendations made for routing.
//
// NOTE: This function is safe for concurrent access.
// NOTE: Part of the PaymentSession interface.
func (p *paymentSession) RequestRoute(maxAmt, feeLimit lnwire.MilliSatoshi,
	activeShards, height uint32,
	firstHopCustomRecords lnwire.CustomRecords) (*route.Route, error) {

	if p.empty {
		return nil, errEmptyPaySession
	}

	// Add BlockPadding to the finalCltvDelta so that the receiving node
	// does not reject the HTLC if some blocks are mined while it's in-flight.
	finalCltvDelta := p.payment.FinalCLTVDelta
	finalCltvDelta += BlockPadding

	// We need to subtract the final delta before passing it into path
	// finding. The optimal path is independent of the final cltv delta and
	// the path finding algorithm is unaware of this value.
	cltvLimit := p.payment.CltvLimit - uint32(finalCltvDelta)

	// TODO(roasbeef): sync logic amongst dist sys

	// Taking into account this prune view, we'll attempt to locate a path
	// to our destination, respecting the recommendations from
	// MissionController.
	restrictions := &RestrictParams{
		ProbabilitySource:     p.missionControl.GetProbability,
		FeeLimit:              feeLimit,
		OutgoingChannelIDs:    p.payment.OutgoingChannelIDs,
		LastHop:               p.payment.LastHop,
		CltvLimit:             cltvLimit,
		DestCustomRecords:     p.payment.DestCustomRecords,
		DestFeatures:          p.payment.DestFeatures,
		PaymentAddr:           p.payment.PaymentAddr,
		Amp:                   p.payment.amp,
		Metadata:              p.payment.Metadata,
		FirstHopCustomRecords: firstHopCustomRecords,
	}

	finalHtlcExpiry := int32(height) + int32(finalCltvDelta)

	// Before we enter the loop below, we'll make sure to respect the max
	// payment shard size (if it's set), which is effectively our
	// client-side MTU that we'll attempt to respect at all times.
	maxShardActive := p.payment.MaxShardAmt != nil
	if maxShardActive && maxAmt > *p.payment.MaxShardAmt {
		p.log.Debugf("Clamping payment attempt from %v to %v due to "+
			"max shard size of %v", maxAmt, *p.payment.MaxShardAmt,
			maxAmt)

		maxAmt = *p.payment.MaxShardAmt
	}

	// probability is the success probability path finding assigns to the
	// route it returned. Stock lnd discards it here; the bound-aware split
	// is the first caller that needs it.
	var (
		path        []*unifiedEdge
		probability float64
	)
	findPath := func(graph graphdb.NodeTraverser) error {
		// We'll also obtain a set of bandwidthHints from the lower
		// layer for each of our outbound channels. This will allow the
		// path finding to skip any links that aren't active or just
		// don't have enough bandwidth to carry the payment. New
		// bandwidth hints are queried for every new path finding
		// attempt, because concurrent payments may change balances.
		bandwidthHints, err := p.getBandwidthHints(graph)
		if err != nil {
			return err
		}

		p.log.Debugf("pathfinding for amt=%v", maxAmt)

		// Find a route for the current amount.
		path, probability, err = p.pathFinder(
			&graphParams{
				additionalEdges: p.additionalEdges,
				bandwidthHints:  bandwidthHints,
				graph:           graph,
			},
			restrictions, &p.pathFindingConfig,
			p.selfNode, p.selfNode, p.payment.Target,
			maxAmt, p.payment.TimePref, finalHtlcExpiry,
		)
		if err != nil {
			// Wrap the error to distinguish path finding errors
			// from other errors in this closure.
			return &pathFindingError{err}
		}

		return nil
	}

	// runPathFinding executes a single path finding call for the current
	// value of maxAmt. It splits the two error classes apart: the first
	// return value is the unwrapped path finding error, which the caller
	// may recover from by trying another amount, while the second is a
	// critical error that ends the payment immediately.
	runPathFinding := func() (error, error) {
		err := p.graphSessFactory.GraphSession(
			context.TODO(),
			findPath, func() {
				path = nil
			},
		)
		// If there is an error, and it is not a path finding error, we
		// return it immediately.
		if err != nil && !lnutils.ErrorAs[*pathFindingError](err) {
			return nil, err
		} else if err != nil {
			// If the error is a path finding error, we'll unwrap it
			// to check the underlying error.
			//
			//nolint:errorlint
			pErr, _ := err.(*pathFindingError)

			return pErr.Unwrap(), nil
		}

		return nil, nil
	}

	// ladderSpent records that the bound-aware ladder has already been
	// priced once for this route request and found nothing, after which
	// the blind policy owns the rest of the descent.
	var ladderSpent bool

	for {
		err, critical := runPathFinding()
		if critical != nil {
			return nil, critical
		}

		// Otherwise, we'll switch on the path finding error.
		switch {
		case err == errNoPathFound:
			// Don't split if this is a legacy payment without mpp
			// record. If it has a blinded path though, then we
			// can split. Split payments to blinded paths won't have
			// MPP records.
			if p.payment.PaymentAddr.IsNone() &&
				p.payment.BlindedPathSet == nil {

				p.log.Debugf("not splitting because payment " +
					"address is unspecified")

				return nil, errNoPathFound
			}

			if p.payment.DestFeatures == nil {
				p.log.Debug("Not splitting because " +
					"destination DestFeatures is nil")
				return nil, errNoPathFound
			}

			destFeatures := p.payment.DestFeatures
			if !destFeatures.HasFeature(lnwire.MPPOptional) &&
				!destFeatures.HasFeature(lnwire.AMPOptional) {

				p.log.Debug("not splitting because " +
					"destination doesn't declare MPP or " +
					"AMP")

				return nil, errNoPathFound
			}

			// No splitting if this is the last shard.
			isLastShard := activeShards+1 >= p.payment.MaxParts
			if isLastShard {
				p.log.Debugf("not splitting because shard "+
					"limit %v has been reached",
					p.payment.MaxParts)

				return nil, errNoPathFound
			}

			// With the bound-aware policy active, we don't guess at
			// the next shard size at all: we price a ladder of
			// candidate shards and send the best one.
			if p.pathFindingConfig.Patch.AdaptiveSplit &&
				!ladderSpent {

				found, critical := p.searchShardAmt(
					runPathFinding, &maxAmt, &path,
					&probability,
				)
				if critical != nil {
					return nil, critical
				}
				if found {
					// The search left maxAmt at the chosen
					// shard and path at its route, so
					// we're done.
					break
				}

				// Belief rejected every rung. The ladder is a
				// fast path over the top of the range, not a
				// replacement for the range: hand what is left
				// below it back to the blind policy rather
				// than abandon a payment that policy could
				// still complete. maxAmt now sits just under
				// the bottom rung.
				ladderSpent = true

				if maxAmt < p.minShardAmt {
					p.log.Debugf("not splitting because "+
						"minimum shard amount %v has "+
						"been reached", p.minShardAmt)

					return nil, errNoPathFound
				}

				continue
			}

			// This is where the magic happens. If we can't find a
			// route, try it for half the amount.
			maxAmt /= 2

			// Put a lower bound on the minimum shard size.
			if maxAmt < p.minShardAmt {
				p.log.Debugf("not splitting because minimum "+
					"shard amount %v has been reached",
					p.minShardAmt)

				return nil, errNoPathFound
			}

			// Go pathfinding.
			continue

		// If there isn't enough local bandwidth, there is no point in
		// splitting. It won't be possible to create a complete set in
		// any case, but the sent out partial payments would be held by
		// the receiver until the mpp timeout.
		case err == errInsufficientBalance:
			p.log.Debug("not splitting because local balance " +
				"is insufficient")

			return nil, err

		case err != nil:
			return nil, err
		}

		// With the next candidate path found, we'll attempt to turn
		// this into a route by applying the time-lock and fee
		// requirements.
		route, err := newRoute(
			p.selfNode, path, height,
			finalHopParams{
				amt:         maxAmt,
				totalAmt:    p.payment.Amount,
				cltvDelta:   finalCltvDelta,
				records:     p.payment.DestCustomRecords,
				paymentAddr: p.payment.PaymentAddr,
				metadata:    p.payment.Metadata,
			}, p.payment.BlindedPathSet,
		)
		if err != nil {
			return nil, err
		}

		return route, err
	}
}

// searchShardAmt chooses the next shard by pricing a ladder of candidate
// amounts against path finding, and rewrites amt and path to the winner. It
// reports whether any rung was routable at all; when none was, amt is left
// just under the ladder's bottom rung so that the caller can resume the blind
// descent over the range the ladder does not cover.
//
// This is the inverted question: the blind policy fixes an amount and asks
// whether the graph can carry it, while the ladder asks which of several
// amounts is worth the most. Every rung is an ordinary path finding call, so
// every rung respects every bound mission control holds. No new state, no
// estimator change.
//
// The choice is by expected value, fraction times route probability, which is
// mx_c3's harvest ladder with lnd's own belief as the value model. That makes
// the estimator load bearing rather than incidental: under a miscalibrated
// belief, where everything below the last failure looks equally fine, the
// argmax degenerates to the largest rung and the policy becomes a blind
// geometric descent slower than halving. That degeneracy is measured, not
// hypothetical, which is why the estimator is an arm of the experiment.
//
// NOTE: amt must be an amount that path finding has just rejected, and runPath
// must run path finding for the current value of *amt, leaving its result in
// path and prob.
func (p *paymentSession) searchShardAmt(runPath func() (error, error),
	amt *lnwire.MilliSatoshi, path *[]*unifiedEdge,
	prob *float64) (bool, error) {

	// If the requested amount is already at or below the floor then there
	// is nothing left to split off, exactly as in the blind policy. Leave
	// the amount under the floor so that the caller stops there too.
	if *amt <= p.minShardAmt {
		*amt = p.minShardAmt - 1

		return false, nil
	}

	var (
		failingAmt = *amt
		lowestRung = *amt
		bestAmt    lnwire.MilliSatoshi
		bestPath   []*unifiedEdge
		bestValue  float64
	)

	for _, fraction := range adaptiveSplitLadder {
		rung := lnwire.MilliSatoshi(fraction * float64(failingAmt))

		// Rungs under the floor are not shards we are willing to send,
		// so they are skipped rather than clamped: clamping would
		// price the same amount several times.
		if rung < p.minShardAmt {
			continue
		}

		*amt, lowestRung = rung, rung

		err, critical := runPath()
		if critical != nil {
			return false, critical
		}
		if err != nil {
			continue
		}

		// Ties keep the earlier, larger rung: the ladder descends, so
		// this is the amount that completes the payment in fewer
		// shards when the value model cannot separate the two.
		if value := fraction * *prob; value > bestValue {
			bestValue, bestAmt, bestPath = value, rung, *path
		}
	}

	// No rung was routable. Leave the amount just under the bottom rung so
	// that the caller can resume the blind descent over the range the
	// ladder does not cover.
	if bestPath == nil {
		*amt = lowestRung - 1

		p.log.Debugf("no routable shard on the ladder below failing "+
			"amount %v, resuming blind descent at %v", failingAmt,
			*amt)

		return false, nil
	}

	p.log.Debugf("bound-aware split: shard %v of failing amount %v, "+
		"expected value %v", bestAmt, failingAmt, bestValue)

	*amt, *path = bestAmt, bestPath

	return true, nil
}

// UpdateAdditionalEdge updates the channel edge policy for a private edge. It
// validates the message signature and checks it's up to date, then applies the
// updates to the supplied policy. It returns a boolean to indicate whether
// there's an error when applying the updates.
func (p *paymentSession) UpdateAdditionalEdge(msg *lnwire.ChannelUpdate1,
	pubKey *btcec.PublicKey, policy *models.CachedEdgePolicy) bool {

	// Validate the message signature.
	if err := netann.VerifyChannelUpdateSignature(msg, pubKey); err != nil {
		log.Errorf(
			"Unable to validate channel update signature: %v", err,
		)
		return false
	}

	// Update channel policy for the additional edge.
	policy.TimeLockDelta = msg.TimeLockDelta
	policy.FeeBaseMSat = lnwire.MilliSatoshi(msg.BaseFee)
	policy.FeeProportionalMillionths = lnwire.MilliSatoshi(msg.FeeRate)

	log.Debugf("New private channel update applied: %v",
		lnutils.SpewLogClosure(msg))

	return true
}

// GetAdditionalEdgePolicy uses the public key and channel ID to query the
// ephemeral channel edge policy for additional edges. Returns a nil if nothing
// found.
func (p *paymentSession) GetAdditionalEdgePolicy(pubKey *btcec.PublicKey,
	channelID uint64) *models.CachedEdgePolicy {

	target := route.NewVertex(pubKey)

	edges, ok := p.additionalEdges[target]
	if !ok {
		return nil
	}

	for _, edge := range edges {
		policy := edge.EdgePolicy()
		if policy.ChannelID != channelID {
			continue
		}

		return policy
	}

	return nil
}
