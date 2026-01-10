package migration1

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// Vertex is a frozen local type representing a 33-byte compressed public key,
// equivalent to route.Vertex at the time this migration was written.
type Vertex [33]byte

// NewVertex returns a new Vertex from a compressed public key.
func NewVertex(pub *btcec.PublicKey) Vertex {
	var v Vertex
	copy(v[:], pub.SerializeCompressed())

	return v
}

// NewVertexFromBytes returns a new Vertex from a serialized compressed public
// key byte slice.
func NewVertexFromBytes(b []byte) (Vertex, error) {
	if len(b) != 33 {
		return Vertex{}, fmt.Errorf("invalid vertex length %d, "+
			"expected 33", len(b))
	}

	var v Vertex
	copy(v[:], b)

	return v, nil
}

// Hop is a frozen local snapshot of route.Hop, containing exactly the fields
// that existed at the time this migration was written. This ensures the
// migration's serialization boundary is independent of future changes to
// route.Hop, in particular the planned removal of LegacyPayload once the KV
// backend is phased out.
type Hop struct {
	// PubKeyBytes is the raw bytes of the public key of the target node.
	PubKeyBytes Vertex

	// ChannelID is the unique channel ID for the channel.
	ChannelID uint64

	// OutgoingTimeLock is the timelock value that should be used when
	// crafting the _outgoing_ HTLC from this hop.
	OutgoingTimeLock uint32

	// AmtToForward is the amount that this hop will forward to the next
	// hop.
	AmtToForward lnwire.MilliSatoshi

	// MPP encapsulates the data required for option_mpp. This field should
	// only be set for the final hop.
	MPP *record.MPP

	// AMP encapsulates the data required for option_amp. This field should
	// only be set for the final hop.
	AMP *record.AMP

	// CustomRecords if non-nil are a set of additional TLV records that
	// should be included in the forwarding instructions for this node.
	CustomRecords record.CustomSet

	// LegacyPayload signals that this node doesn't understand the new TLV
	// payload, so the legacy payload format must be used.
	//
	// NOTE: This field is preserved here even though it is marked for
	// removal in the live route.Hop, because old KV data may have been
	// serialized with LegacyPayload=true and the migration must be able to
	// deserialize it correctly.
	LegacyPayload bool

	// Metadata is additional data sent along with the payment to the
	// payee.
	Metadata []byte

	// EncryptedData is an encrypted data blob included for hops that are
	// part of a blinded route.
	EncryptedData []byte

	// BlindingPoint is an ephemeral public key used by introduction nodes
	// in blinded routes.
	BlindingPoint *btcec.PublicKey

	// TotalAmtMsat is the total amount for a blinded payment. This field
	// should only be set for the final hop in a blinded path.
	TotalAmtMsat lnwire.MilliSatoshi
}

// Route is a frozen local snapshot of route.Route, containing exactly the
// fields that existed at the time this migration was written.
type Route struct {
	// TotalTimeLock is the cumulative (final) time lock across the entire
	// route.
	TotalTimeLock uint32

	// TotalAmount is the total amount of funds required to complete a
	// payment over this route, including fees.
	TotalAmount lnwire.MilliSatoshi

	// SourcePubKey is the pubkey of the node where this route originates.
	SourcePubKey Vertex

	// Hops contains details concerning the specific forwarding details at
	// each hop.
	Hops []*Hop

	// FirstHopAmount is the amount that should actually be sent to the
	// first hop. Only differs from TotalAmount for custom channels.
	FirstHopAmount tlv.RecordT[
		tlv.TlvType0, tlv.BigSizeT[lnwire.MilliSatoshi],
	]

	// FirstHopWireCustomRecords is a set of custom TLV records to include
	// in the wire message sent to the first hop.
	FirstHopWireCustomRecords lnwire.CustomRecords
}

// FinalHop returns the last hop in the route.
func (r *Route) FinalHop() *Hop {
	return r.Hops[len(r.Hops)-1]
}

// ReceiverAmt returns the amount forwarded to the final hop.
func (r *Route) ReceiverAmt() lnwire.MilliSatoshi {
	return r.FinalHop().AmtToForward
}

// TotalFees returns the total fees paid along the route.
func (r *Route) TotalFees() lnwire.MilliSatoshi {
	return r.TotalAmount - r.ReceiverAmt()
}
