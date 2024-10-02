package models

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var byteOrder = binary.BigEndian

// MCRoute holds the bare minimum info about a payment attempt route that MC
// requires.
type MCRoute struct {
	SourcePubKey route.Vertex
	TotalAmount  lnwire.MilliSatoshi
	Hops         []*MCHop
}

// MCHop holds the bare minimum info about a payment attempt route hop that MC
// requires.
type MCHop struct {
	ChannelID        uint64
	PubKeyBytes      route.Vertex
	AmtToFwd         lnwire.MilliSatoshi
	HasBlindingPoint bool
	HasCustomRecords bool
}

// DeserializeRoute deserializes the MCRoute from the given io.Reader.
func DeserializeRoute(r io.Reader) (*MCRoute, error) {
	var (
		rt      MCRoute
		numHops uint32
	)

	if err := binary.Read(r, byteOrder, &rt.TotalAmount); err != nil {
		return nil, err
	}

	pub, err := wire.ReadVarBytes(r, 0, 66000, "[]byte")
	if err != nil {
		return nil, err
	}
	copy(rt.SourcePubKey[:], pub)

	if err := binary.Read(r, byteOrder, &numHops); err != nil {
		return nil, err
	}

	var hops []*MCHop
	for i := uint32(0); i < numHops; i++ {
		hop, err := deserializeHop(r)
		if err != nil {
			return nil, err
		}
		hops = append(hops, hop)
	}
	rt.Hops = hops

	return &rt, nil
}

// deserializeHop deserializes the MCHop from the given io.Reader.
func deserializeHop(r io.Reader) (*MCHop, error) {
	var h MCHop

	pub, err := wire.ReadVarBytes(r, 0, 66000, "[]byte")
	if err != nil {
		return nil, err
	}
	copy(h.PubKeyBytes[:], pub)

	if err := binary.Read(r, byteOrder, &h.ChannelID); err != nil {
		return nil, err
	}

	var a uint64
	if err := binary.Read(r, byteOrder, &a); err != nil {
		return nil, err
	}
	h.AmtToFwd = lnwire.MilliSatoshi(a)

	if err := binary.Read(r, byteOrder, &h.HasBlindingPoint); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &h.HasCustomRecords); err != nil {
		return nil, err
	}

	return &h, nil
}

// Serialize serializes a MCRoute and writes the resulting bytes to the given
// io.Writer.
func (r *MCRoute) Serialize(w io.Writer) error {
	err := binary.Write(w, byteOrder, uint64(r.TotalAmount))
	if err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, r.SourcePubKey[:]); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, uint32(len(r.Hops))); err != nil {
		return err
	}

	for _, h := range r.Hops {
		if err := serializeHop(w, h); err != nil {
			return err
		}
	}

	return nil
}

// serializeHop serializes a MCHop and writes the resulting bytes to the given
// io.Writer.
func serializeHop(w io.Writer, h *MCHop) error {
	if err := wire.WriteVarBytes(w, 0, h.PubKeyBytes[:]); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, h.ChannelID); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, uint64(h.AmtToFwd)); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, h.HasBlindingPoint); err != nil {
		return err
	}

	return binary.Write(w, byteOrder, h.HasCustomRecords)

}

// ToMCRoute extracts the fields required by MC from the Route struct to create
// the more minima MCRoute struct.
func ToMCRoute(route *route.Route) *MCRoute {
	return &MCRoute{
		SourcePubKey: route.SourcePubKey,
		TotalAmount:  route.TotalAmount,
		Hops:         extractMCHops(route.Hops),
	}
}

// extractMCHops extracts the Hop fields that MC actually uses from a slice of
// Hops.
func extractMCHops(hops []*route.Hop) []*MCHop {
	mcHops := make([]*MCHop, len(hops))
	for i, hop := range hops {
		mcHops[i] = extractMCHop(hop)
	}

	return mcHops
}

// extractMCHop extracts the Hop fields that MC actually uses from a Hop.
func extractMCHop(hop *route.Hop) *MCHop {
	return &MCHop{
		ChannelID:        hop.ChannelID,
		PubKeyBytes:      hop.PubKeyBytes,
		AmtToFwd:         hop.AmtToForward,
		HasBlindingPoint: hop.BlindingPoint != nil,
		HasCustomRecords: len(hop.CustomRecords) > 0,
	}
}
