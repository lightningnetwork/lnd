package migration1

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

// Vertex is a frozen local type representing a 33-byte compressed public key,
// equivalent to route.Vertex.
type Vertex [33]byte

// Hop is a frozen local snapshot of route.Hop, containing exactly the fields
// that existed at the time this migration was written. This ensures the
// migration serialization boundary is independent of future changes to
// route.Hop.
type Hop struct {
	PubKeyBytes      Vertex
	ChannelID        uint64
	OutgoingTimeLock uint32
	AmtToForward     lnwire.MilliSatoshi
	LegacyPayload    bool
	MPP              *record.MPP
	AMP              *record.AMP
	EncryptedData    []byte
	BlindingPoint    *btcec.PublicKey
	Metadata         []byte
	TotalAmtMsat     lnwire.MilliSatoshi
	CustomRecords    map[uint64][]byte
}

// Route is a frozen local snapshot of route.Route, containing exactly the
// fields that existed at the time this migration was written.
//
//nolint:ll
type Route struct {
	TotalTimeLock             uint32
	TotalAmount               lnwire.MilliSatoshi
	SourcePubKey              Vertex
	Hops                      []*Hop
	FirstHopAmount            tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[lnwire.MilliSatoshi]]
	FirstHopWireCustomRecords lnwire.CustomRecords
}

// FinalHop returns the last hop in the route.
func (r *Route) FinalHop() *Hop {
	return r.Hops[len(r.Hops)-1]
}

// ReceiverAmt returns the amount forwarded by the final hop.
func (r *Route) ReceiverAmt() lnwire.MilliSatoshi {
	return r.Hops[len(r.Hops)-1].AmtToForward
}

// TotalFees returns the total fees paid along the route.
func (r *Route) TotalFees() lnwire.MilliSatoshi {
	return r.TotalAmount - r.ReceiverAmt()
}

// toRouteRoute converts the local Route to a route.Route for use with live
// route package functionality such as sphinx path generation. This conversion
// is only used in the live payment code path, not during migration.
func (r *Route) toRouteRoute() route.Route {
	hops := make([]*route.Hop, len(r.Hops))
	for i, h := range r.Hops {
		hops[i] = h.toRouteHop()
	}

	return route.Route{
		TotalTimeLock:             r.TotalTimeLock,
		TotalAmount:               r.TotalAmount,
		SourcePubKey:              route.Vertex(r.SourcePubKey),
		Hops:                      hops,
		FirstHopAmount:            r.FirstHopAmount,
		FirstHopWireCustomRecords: r.FirstHopWireCustomRecords,
	}
}

// toRouteHop converts the local Hop to a route.Hop.
func (h *Hop) toRouteHop() *route.Hop {
	return &route.Hop{
		PubKeyBytes:      route.Vertex(h.PubKeyBytes),
		ChannelID:        h.ChannelID,
		OutgoingTimeLock: h.OutgoingTimeLock,
		AmtToForward:     h.AmtToForward,
		LegacyPayload:    h.LegacyPayload,
		MPP:              h.MPP,
		AMP:              h.AMP,
		EncryptedData:    h.EncryptedData,
		BlindingPoint:    h.BlindingPoint,
		Metadata:         h.Metadata,
		TotalAmtMsat:     h.TotalAmtMsat,
		CustomRecords:    record.CustomSet(h.CustomRecords),
	}
}

// routeToLocal converts a route.Route to the local Route type. Used when
// accepting route.Route from external callers (e.g. NewHtlcAttempt).
func routeToLocal(rt route.Route) Route {
	hops := make([]*Hop, len(rt.Hops))
	for i, h := range rt.Hops {
		hops[i] = hopToLocal(h)
	}

	return Route{
		TotalTimeLock:             rt.TotalTimeLock,
		TotalAmount:               rt.TotalAmount,
		SourcePubKey:              Vertex(rt.SourcePubKey),
		Hops:                      hops,
		FirstHopAmount:            rt.FirstHopAmount,
		FirstHopWireCustomRecords: rt.FirstHopWireCustomRecords,
	}
}

// hopToLocal converts a route.Hop to the local Hop type.
func hopToLocal(h *route.Hop) *Hop {
	return &Hop{
		PubKeyBytes:      Vertex(h.PubKeyBytes),
		ChannelID:        h.ChannelID,
		OutgoingTimeLock: h.OutgoingTimeLock,
		AmtToForward:     h.AmtToForward,
		LegacyPayload:    h.LegacyPayload,
		MPP:              h.MPP,
		AMP:              h.AMP,
		EncryptedData:    h.EncryptedData,
		BlindingPoint:    h.BlindingPoint,
		Metadata:         h.Metadata,
		TotalAmtMsat:     h.TotalAmtMsat,
		CustomRecords:    map[uint64][]byte(h.CustomRecords),
	}
}
