package routing

import (
	"errors"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrNoPayLoadSizeFunc is returned when no payload size function is
	// definied.
	ErrNoPayLoadSizeFunc = errors.New("no payloadSizeFunc definied for " +
		"DirectedEdge")
)

// customEdgeSizeFunc is a size function that is used to allow edges to specify
// a custom hop size for the forwarding vars provided.
type customEdgeSizeFunc func(amount lnwire.MilliSatoshi, expiry uint32,
	legacy bool, channelID uint64) uint64

// hopPayloadSizeFunc calculates the size of the payload for a clear hop of
// a route (non-blinded). This size function must only be used for intermediate
// hops. The exit hop payload has to be treated differently.
func hopPayloadSizeFunc(amount lnwire.MilliSatoshi, expiry uint32,
	legacy bool, channelID uint64) uint64 {

	hop := route.Hop{
		AmtToForward:     amount,
		OutgoingTimeLock: expiry,
		LegacyPayload:    legacy,
	}

	return hop.PayloadSize(channelID)
}

// AdditionalEdge encapsulates the policy and the payload size function for
// additional edges which are amended to a path. These can either be blinded
// hops or additonal routing hints (unannounced channels).
// TODO(ziggie): Do not need to export this type.
type AdditionalEdge struct {
	policy          *channeldb.CachedEdgePolicy
	payloadSizeFunc customEdgeSizeFunc
}

// EdgePolicy return the policy of the DirectedEdge.
func (edge *AdditionalEdge) edgePolicy() *channeldb.CachedEdgePolicy {
	return edge.policy
}

// hopPayloadFunc returns the payload size function of an edge.
func (edge *AdditionalEdge) hopPayloadSize() (customEdgeSizeFunc, error) {

	// Because the payload size function is optional we need to make sure
	// that it is definied.
	if edge.payloadSizeFunc == nil {
		return nil, ErrNoPayLoadSizeFunc
	}
	return edge.payloadSizeFunc, nil
}
