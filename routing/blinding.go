package routing

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// BlindedPathNUMSHex is the hex encoded version of the blinded path target
// NUMs key (in compressed format) which has no known private key.
// This was generated using the following script:
// https://github.com/lightninglabs/lightning-node-connect/tree/master/
// mailbox/numsgen, with the seed phrase "Lightning Blinded Path".
const BlindedPathNUMSHex = "02667a98ef82ecb522f803b17a74f14508a48b25258f9831" +
	"dd6e95f5e299dfd54e"

var (
	// ErrNoBlindedPath is returned when the blinded path in a blinded
	// payment is missing.
	ErrNoBlindedPath = errors.New("blinded path required")

	// ErrInsufficientBlindedHops is returned when a blinded path does
	// not have enough blinded hops.
	ErrInsufficientBlindedHops = errors.New("blinded path requires " +
		"at least one hop")

	// ErrHTLCRestrictions is returned when a blinded path has invalid
	// HTLC maximum and minimum values.
	ErrHTLCRestrictions = errors.New("invalid htlc minimum and maximum")

	// BlindedPathNUMSKey is a NUMS key (nothing up my sleeves number) that
	// has no known private key.
	BlindedPathNUMSKey = input.MustParsePubKey(BlindedPathNUMSHex)

	// CompressedBlindedPathNUMSKey is the compressed version of the
	// BlindedPathNUMSKey.
	CompressedBlindedPathNUMSKey = BlindedPathNUMSKey.SerializeCompressed()
)

// BlindedPaymentPathSet groups the data we need to handle sending to a set of
// blinded paths provided by the recipient of a payment.
//
// NOTE: for now this only holds a single BlindedPayment. By the end of the PR
// series, it will handle multiple paths.
type BlindedPaymentPathSet struct {
	// paths is the set of blinded payment paths for a single payment.
	// NOTE: For now this will always only have a single entry. By the end
	// of this PR, it can hold multiple.
	paths []*BlindedPayment

	// targetPubKey is the ephemeral node pub key that we will inject into
	// each path as the last hop. This is only for the sake of path finding.
	// Once the path has been found, the original destination pub key is
	// used again. In the edge case where there is only a single hop in the
	// path (the introduction node is the destination node), then this will
	// just be the introduction node's real public key.
	targetPubKey *btcec.PublicKey

	// features is the set of relay features available for the payment.
	// This is extracted from the set of blinded payment paths. At the
	// moment we require that all paths for the same payment have the
	// same feature set.
	features *lnwire.FeatureVector

	// finalCLTV is the final hop's expiry delta of _any_ path in the set.
	// For any multi-hop path, the final CLTV delta should be seen as zero
	// since the final hop's final CLTV delta is accounted for in the
	// accumulated path policy values. The only edge case is for when the
	// final hop in the path is also the introduction node in which case
	// that path's FinalCLTV must be the non-zero min CLTV of the final hop
	// so that it is accounted for in path finding. For this reason, if
	// we have any single path in the set with only one hop, then we throw
	// away all the other paths. This should be fine to do since if there is
	// a path where the intro node is also the destination node, then there
	// isn't any need to try any other longer blinded path. In other words,
	// if this value is non-zero, then there is only one path in this
	// blinded path set and that path only has a single hop: the
	// introduction node.
	finalCLTV uint16
}

// NewBlindedPaymentPathSet constructs a new BlindedPaymentPathSet from a set of
// BlindedPayments. For blinded paths which have more than one single hop a
// dummy hop via a NUMS key is appeneded to allow for MPP path finding via
// multiple blinded paths.
func NewBlindedPaymentPathSet(paths []*BlindedPayment) (*BlindedPaymentPathSet,
	error) {

	if len(paths) == 0 {
		return nil, ErrNoBlindedPath
	}

	// For now, we assert that all the paths have the same set of features.
	features := paths[0].Features
	noFeatures := features == nil || features.IsEmpty()
	for i := 1; i < len(paths); i++ {
		noFeats := paths[i].Features == nil ||
			paths[i].Features.IsEmpty()

		if noFeatures && !noFeats {
			return nil, fmt.Errorf("all blinded paths must have " +
				"the same set of features")
		}

		if noFeatures {
			continue
		}

		if !features.RawFeatureVector.Equals(
			paths[i].Features.RawFeatureVector,
		) {

			return nil, fmt.Errorf("all blinded paths must have " +
				"the same set of features")
		}
	}

	// Deep copy the paths to avoid mutating the original paths.
	pathSet := make([]*BlindedPayment, len(paths))
	for i, path := range paths {
		pathSet[i] = path.deepCopy()
	}

	// For blinded paths we use the NUMS key as a target if the blinded
	// path has more hops than just the introduction node.
	targetPub := &BlindedPathNUMSKey

	var finalCLTVDelta uint16

	// In case the paths do NOT include a single hop route we append a
	// dummy hop via a NUMS key to allow for MPP path finding via multiple
	// blinded paths. A unified target is needed to use all blinded paths
	// during the payment lifecycle. A dummy hop is solely added for the
	// path finding process and is removed after the path is found. This
	// ensures that we still populate the mission control with the correct
	// data and also respect these mc entries when looking for a path.
	for _, path := range pathSet {
		pathLength := len(path.BlindedPath.BlindedHops)

		// If any provided blinded path only has a single hop (ie, the
		// destination node is also the introduction node), then we
		// discard all other paths since we know the real pub key of the
		// destination node. We also then set the final CLTV delta to
		// the path's delta since there are no other edge hints that
		// will account for it.
		if pathLength == 1 {
			pathSet = []*BlindedPayment{path}
			finalCLTVDelta = path.CltvExpiryDelta
			targetPub = path.BlindedPath.IntroductionPoint

			break
		}

		lastHop := path.BlindedPath.BlindedHops[pathLength-1]
		path.BlindedPath.BlindedHops = append(
			path.BlindedPath.BlindedHops,
			&sphinx.BlindedHopInfo{
				BlindedNodePub: &BlindedPathNUMSKey,
				// We add the last hop's cipher text so that
				// the payload size of the final hop is equal
				// to the real last hop.
				CipherText: lastHop.CipherText,
			},
		)
	}

	return &BlindedPaymentPathSet{
		paths:        pathSet,
		targetPubKey: targetPub,
		features:     features,
		finalCLTV:    finalCLTVDelta,
	}, nil
}

// TargetPubKey returns the public key to be used as the destination node's
// public key during pathfinding.
func (s *BlindedPaymentPathSet) TargetPubKey() *btcec.PublicKey {
	return s.targetPubKey
}

// Features returns the set of relay features available for the payment.
func (s *BlindedPaymentPathSet) Features() *lnwire.FeatureVector {
	return s.features
}

// IntroNodeOnlyPath can be called if it is expected that the path set only
// contains a single payment path which itself only has one hop. It errors if
// this is not the case.
func (s *BlindedPaymentPathSet) IntroNodeOnlyPath() (*BlindedPayment, error) {
	if len(s.paths) != 1 {
		return nil, fmt.Errorf("expected only a single path in the "+
			"blinded payment set, got %d", len(s.paths))
	}

	if len(s.paths[0].BlindedPath.BlindedHops) > 1 {
		return nil, fmt.Errorf("an intro node only path cannot have " +
			"more than one hop")
	}

	return s.paths[0], nil
}

// IsIntroNode returns true if the given vertex is an introduction node for one
// of the paths in the blinded payment path set.
func (s *BlindedPaymentPathSet) IsIntroNode(source route.Vertex) bool {
	for _, path := range s.paths {
		introVertex := route.NewVertex(
			path.BlindedPath.IntroductionPoint,
		)
		if source == introVertex {
			return true
		}
	}

	return false
}

// FinalCLTVDelta is the minimum CLTV delta to use for the final hop on the
// route. In most cases this will return zero since the value is accounted for
// in the path's accumulated CLTVExpiryDelta. Only in the edge case of the path
// set only including a single path which only includes an introduction node
// will this return a non-zero value.
func (s *BlindedPaymentPathSet) FinalCLTVDelta() uint16 {
	return s.finalCLTV
}

// LargestLastHopPayloadPath returns the BlindedPayment in the set that has the
// largest last-hop payload. This is to be used for onion size estimation in
// path finding.
func (s *BlindedPaymentPathSet) LargestLastHopPayloadPath() (*BlindedPayment,
	error) {

	var (
		largestPath *BlindedPayment
		currentMax  int
	)

	if len(s.paths) == 0 {
		return nil, fmt.Errorf("no blinded paths in the set")
	}

	// We set the largest path to make sure we always return a path even
	// if the cipher text is empty.
	largestPath = s.paths[0]

	for _, path := range s.paths {
		numHops := len(path.BlindedPath.BlindedHops)
		lastHop := path.BlindedPath.BlindedHops[numHops-1]

		if len(lastHop.CipherText) > currentMax {
			largestPath = path
			currentMax = len(lastHop.CipherText)
		}
	}

	return largestPath, nil
}

// ToRouteHints converts the blinded path payment set into a RouteHints map so
// that the blinded payment paths can be treated like route hints throughout the
// code base.
func (s *BlindedPaymentPathSet) ToRouteHints() (RouteHints, error) {
	hints := make(RouteHints)

	for _, path := range s.paths {
		pathHints, err := path.toRouteHints()
		if err != nil {
			return nil, err
		}

		for from, edges := range pathHints {
			hints[from] = append(hints[from], edges...)
		}
	}

	if len(hints) == 0 {
		return nil, nil
	}

	return hints, nil
}

// IsBlindedRouteNUMSTargetKey returns true if the given public key is the
// NUMS key used as a target for blinded path final hops.
func IsBlindedRouteNUMSTargetKey(pk []byte) bool {
	return bytes.Equal(pk, CompressedBlindedPathNUMSKey)
}

// BlindedPayment provides the path and payment parameters required to send a
// payment along a blinded path.
type BlindedPayment struct {
	// BlindedPath contains the unblinded introduction point and blinded
	// hops for the blinded section of the payment.
	BlindedPath *sphinx.BlindedPath

	// BaseFee is the total base fee to be paid for payments made over the
	// blinded path.
	BaseFee uint32

	// ProportionalFeeRate is the aggregated proportional fee rate for
	// payments made over the blinded path.
	ProportionalFeeRate uint32

	// CltvExpiryDelta is the total expiry delta for the blinded path. This
	// field includes the CLTV for the blinded hops *and* the final cltv
	// delta for the receiver.
	CltvExpiryDelta uint16

	// HtlcMinimum is the highest HLTC minimum supported along the blinded
	// path (while some hops may have lower values, we're effectively
	// bounded by the highest minimum).
	HtlcMinimum uint64

	// HtlcMaximum is the lowest HTLC maximum supported along the blinded
	// path (while some hops may have higher values, we're effectively
	// bounded by the lowest maximum).
	HtlcMaximum uint64

	// Features is the set of relay features available for the payment.
	Features *lnwire.FeatureVector
}

// Validate performs validation on a blinded payment.
func (b *BlindedPayment) Validate() error {
	if b.BlindedPath == nil {
		return ErrNoBlindedPath
	}

	// The sphinx library inserts the introduction node as the first hop,
	// so we expect at least one hop.
	if len(b.BlindedPath.BlindedHops) < 1 {
		return fmt.Errorf("%w got: %v", ErrInsufficientBlindedHops,
			len(b.BlindedPath.BlindedHops))
	}

	if b.HtlcMaximum < b.HtlcMinimum {
		return fmt.Errorf("%w: %v < %v", ErrHTLCRestrictions,
			b.HtlcMaximum, b.HtlcMinimum)
	}

	for _, hop := range b.BlindedPath.BlindedHops {
		// The first hop of the blinded path does not necessarily have
		// blinded node pub key because it is the introduction point.
		if hop.BlindedNodePub == nil {
			continue
		}

		if IsBlindedRouteNUMSTargetKey(
			hop.BlindedNodePub.SerializeCompressed(),
		) {

			return fmt.Errorf("blinded path cannot include NUMS "+
				"key: %s", BlindedPathNUMSHex)
		}
	}

	return nil
}

// toRouteHints produces a set of chained route hints that represent a blinded
// path. In the case of a single hop blinded route (which is paying directly
// to the introduction point), no hints will be returned. In this case callers
// *must* account for the blinded route's CLTV delta elsewhere (as this is
// effectively the final_cltv_delta for the receiving introduction node). In
// the case of multiple blinded hops, CLTV delta is fully accounted for in the
// hints (both for intermediate hops and the final_cltv_delta for the receiving
// node).
func (b *BlindedPayment) toRouteHints() (RouteHints, error) {
	// If we just have a single hop in our blinded route, it just contains
	// an introduction node (this is a valid path according to the spec).
	// Since we have the un-blinded node ID for the introduction node, we
	// don't need to add any route hints.
	if len(b.BlindedPath.BlindedHops) == 1 {
		return nil, nil
	}

	hintCount := len(b.BlindedPath.BlindedHops) - 1
	hints := make(
		RouteHints, hintCount,
	)

	// Start at the unblinded introduction node, because our pathfinding
	// will be able to locate this point in the graph.
	fromNode := route.NewVertex(b.BlindedPath.IntroductionPoint)

	features := lnwire.EmptyFeatureVector()
	if b.Features != nil {
		features = b.Features.Clone()
	}

	// Use the total aggregate relay parameters for the entire blinded
	// route as the policy for the hint from our introduction node. This
	// will ensure that pathfinding provides sufficient fees/delay for the
	// blinded portion to the introduction node.
	firstBlindedHop := b.BlindedPath.BlindedHops[1].BlindedNodePub
	edgePolicy := &models.CachedEdgePolicy{
		TimeLockDelta: b.CltvExpiryDelta,
		MinHTLC:       lnwire.MilliSatoshi(b.HtlcMinimum),
		MaxHTLC:       lnwire.MilliSatoshi(b.HtlcMaximum),
		FeeBaseMSat:   lnwire.MilliSatoshi(b.BaseFee),
		FeeProportionalMillionths: lnwire.MilliSatoshi(
			b.ProportionalFeeRate,
		),
		ToNodePubKey: func() route.Vertex {
			return route.NewVertex(
				// The first node in this slice is
				// the introduction node, so we start
				// at index 1 to get the first blinded
				// relaying node.
				firstBlindedHop,
			)
		},
		ToNodeFeatures: features,
	}

	lastEdge, err := NewBlindedEdge(edgePolicy, b, 0)
	if err != nil {
		return nil, err
	}

	hints[fromNode] = []AdditionalEdge{lastEdge}

	// Start at an offset of 1 because the first node in our blinded hops
	// is the introduction node and terminate at the second-last node
	// because we're dealing with hops as pairs.
	for i := 1; i < hintCount; i++ {
		// Set our origin node to the current
		fromNode = route.NewVertex(
			b.BlindedPath.BlindedHops[i].BlindedNodePub,
		)

		// Create a hint which has no fee or cltv delta. We
		// specifically want zero values here because our relay
		// parameters are expressed in encrypted blobs rather than the
		// route itself for blinded routes.
		nextHopIdx := i + 1
		nextNode := route.NewVertex(
			b.BlindedPath.BlindedHops[nextHopIdx].BlindedNodePub,
		)

		edgePolicy := &models.CachedEdgePolicy{
			ToNodePubKey: func() route.Vertex {
				return nextNode
			},
			ToNodeFeatures: features,
		}

		lastEdge, err = NewBlindedEdge(edgePolicy, b, i)
		if err != nil {
			return nil, err
		}

		hints[fromNode] = []AdditionalEdge{lastEdge}
	}

	return hints, nil
}

// deepCopy returns a deep copy of the BlindedPayment.
func (b *BlindedPayment) deepCopy() *BlindedPayment {
	if b == nil {
		return nil
	}

	cpyPayment := &BlindedPayment{
		BaseFee:             b.BaseFee,
		ProportionalFeeRate: b.ProportionalFeeRate,
		CltvExpiryDelta:     b.CltvExpiryDelta,
		HtlcMinimum:         b.HtlcMinimum,
		HtlcMaximum:         b.HtlcMaximum,
	}

	// Deep copy the BlindedPath if it exists
	if b.BlindedPath != nil {
		cpyPayment.BlindedPath = &sphinx.BlindedPath{
			BlindedHops: make([]*sphinx.BlindedHopInfo,
				len(b.BlindedPath.BlindedHops)),
		}

		if b.BlindedPath.IntroductionPoint != nil {
			cpyPayment.BlindedPath.IntroductionPoint =
				copyPublicKey(b.BlindedPath.IntroductionPoint)
		}

		if b.BlindedPath.BlindingPoint != nil {
			cpyPayment.BlindedPath.BlindingPoint =
				copyPublicKey(b.BlindedPath.BlindingPoint)
		}

		// Copy each blinded hop info.
		for i, hop := range b.BlindedPath.BlindedHops {
			if hop == nil {
				continue
			}

			cpyHop := &sphinx.BlindedHopInfo{
				CipherText: hop.CipherText,
			}

			if hop.BlindedNodePub != nil {
				cpyHop.BlindedNodePub =
					copyPublicKey(hop.BlindedNodePub)
			}

			cpyHop.CipherText = make([]byte, len(hop.CipherText))
			copy(cpyHop.CipherText, hop.CipherText)

			cpyPayment.BlindedPath.BlindedHops[i] = cpyHop
		}
	}

	// Deep copy the Features if they exist
	if b.Features != nil {
		cpyPayment.Features = b.Features.Clone()
	}

	return cpyPayment
}

// copyPublicKey makes a deep copy of a public key.
//
// TODO(ziggie): Remove this function if this is available in the btcec library.
func copyPublicKey(pk *btcec.PublicKey) *btcec.PublicKey {
	var result secp256k1.JacobianPoint
	pk.AsJacobian(&result)
	result.ToAffine()

	return btcec.NewPublicKey(&result.X, &result.Y)
}
