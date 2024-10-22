package routing

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrNoPayLoadSizeFunc is returned when no payload size function is
	// definied.
	ErrNoPayLoadSizeFunc = errors.New("no payloadSizeFunc defined for " +
		"additional edge")
)

// AdditionalEdge is an interface which specifies additional edges which can
// be appended to an existing route. Compared to normal edges of a route they
// provide an explicit payload size function and are introduced because blinded
// paths differ in their payload structure.
type AdditionalEdge interface {
	// IntermediatePayloadSize returns the size of the payload for the
	// additional edge when being an intermediate hop in a route NOT the
	// final hop.
	IntermediatePayloadSize(amount lnwire.MilliSatoshi, expiry uint32,
		channelID uint64) uint64

	// EdgePolicy returns the policy of the additional edge.
	EdgePolicy() *models.CachedEdgePolicy

	// BlindedPayment returns the BlindedPayment that this additional edge
	// info was derived from. It will return nil if this edge was not
	// derived from a blinded route.
	BlindedPayment() *BlindedPayment
}

// PayloadSizeFunc defines the interface for the payload size function.
type PayloadSizeFunc func(amount lnwire.MilliSatoshi, expiry uint32,
	channelID uint64) uint64

// PrivateEdge implements the AdditionalEdge interface. As the name implies it
// is used for private route hints that the receiver adds for example to an
// invoice.
type PrivateEdge struct {
	policy *models.CachedEdgePolicy
}

// EdgePolicy return the policy of the PrivateEdge.
func (p *PrivateEdge) EdgePolicy() *models.CachedEdgePolicy {
	return p.policy
}

// IntermediatePayloadSize returns the sphinx payload size defined in BOLT04 if
// this edge were to be included in a route.
func (p *PrivateEdge) IntermediatePayloadSize(amount lnwire.MilliSatoshi,
	expiry uint32, channelID uint64) uint64 {

	hop := route.Hop{
		AmtToForward:     amount,
		OutgoingTimeLock: expiry,
	}

	return hop.PayloadSize(channelID)
}

// BlindedPayment is a no-op for a PrivateEdge since it is not associated with
// a blinded payment. This will thus return nil.
func (p *PrivateEdge) BlindedPayment() *BlindedPayment {
	return nil
}

// BlindedEdge implements the AdditionalEdge interface. Blinded hops are viewed
// as additional edges because they are appended at the end of a normal route.
type BlindedEdge struct {
	policy *models.CachedEdgePolicy

	// blindedPayment is the BlindedPayment that this blinded edge was
	// derived from.
	blindedPayment *BlindedPayment

	// hopIndex is the index of the hop in the blinded payment path that
	// this edge is associated with.
	hopIndex int
}

// NewBlindedEdge constructs a new BlindedEdge which packages the policy info
// for a specific hop within the given blinded payment path. The hop index
// should correspond to the hop within the blinded payment that this edge is
// associated with.
func NewBlindedEdge(policy *models.CachedEdgePolicy, payment *BlindedPayment,
	hopIndex int) (*BlindedEdge, error) {

	if payment == nil {
		return nil, fmt.Errorf("blinded payment cannot be nil for " +
			"blinded edge")
	}

	if hopIndex < 0 || hopIndex >= len(payment.BlindedPath.BlindedHops) {
		return nil, fmt.Errorf("the hop index %d is outside the "+
			"valid range between 0 and %d", hopIndex,
			len(payment.BlindedPath.BlindedHops)-1)
	}

	return &BlindedEdge{
		policy:         policy,
		hopIndex:       hopIndex,
		blindedPayment: payment,
	}, nil
}

// EdgePolicy return the policy of the BlindedEdge.
func (b *BlindedEdge) EdgePolicy() *models.CachedEdgePolicy {
	return b.policy
}

// IntermediatePayloadSize returns the sphinx payload size defined in BOLT04 if
// this edge were to be included in a route.
func (b *BlindedEdge) IntermediatePayloadSize(_ lnwire.MilliSatoshi, _ uint32,
	_ uint64) uint64 {

	blindedPath := b.blindedPayment.BlindedPath

	hop := route.Hop{
		BlindingPoint: blindedPath.BlindingPoint,
		EncryptedData: blindedPath.BlindedHops[b.hopIndex].CipherText,
	}

	// For blinded paths the next chanID is in the encrypted data tlv.
	return hop.PayloadSize(0)
}

// BlindedPayment returns the blinded payment that this edge is associated
// with.
func (b *BlindedEdge) BlindedPayment() *BlindedPayment {
	return b.blindedPayment
}

// Compile-time constraints to ensure the PrivateEdge and the BlindedEdge
// implement the AdditionalEdge interface.
var _ AdditionalEdge = (*PrivateEdge)(nil)
var _ AdditionalEdge = (*BlindedEdge)(nil)

// defaultHopPayloadSize is the default payload size of a normal (not-blinded)
// hop in the route.
func defaultHopPayloadSize(amount lnwire.MilliSatoshi, expiry uint32,
	channelID uint64) uint64 {

	// The payload size of a cleartext intermediate hop is equal to the
	// payload size of a private edge therefore we reuse its size function.
	edge := PrivateEdge{}

	return edge.IntermediatePayloadSize(amount, expiry, channelID)
}
