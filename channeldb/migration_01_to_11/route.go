package migration_01_to_11

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec"
	sphinx "github.com/lightningnetwork/lightning-onion"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// VertexSize is the size of the array to store a vertex.
const VertexSize = 33

// ErrNoRouteHopsProvided is returned when a caller attempts to construct a new
// sphinx packet, but provides an empty set of hops for each route.
var ErrNoRouteHopsProvided = fmt.Errorf("empty route hops provided")

// Vertex is a simple alias for the serialization of a compressed Bitcoin
// public key.
type Vertex [VertexSize]byte

// NewVertex returns a new Vertex given a public key.
func NewVertex(pub *btcec.PublicKey) Vertex {
	var v Vertex
	copy(v[:], pub.SerializeCompressed())
	return v
}

// NewVertexFromBytes returns a new Vertex based on a serialized pubkey in a
// byte slice.
func NewVertexFromBytes(b []byte) (Vertex, error) {
	vertexLen := len(b)
	if vertexLen != VertexSize {
		return Vertex{}, fmt.Errorf("invalid vertex length of %v, "+
			"want %v", vertexLen, VertexSize)
	}

	var v Vertex
	copy(v[:], b)
	return v, nil
}

// NewVertexFromStr returns a new Vertex given its hex-encoded string format.
func NewVertexFromStr(v string) (Vertex, error) {
	// Return error if hex string is of incorrect length.
	if len(v) != VertexSize*2 {
		return Vertex{}, fmt.Errorf("invalid vertex string length of "+
			"%v, want %v", len(v), VertexSize*2)
	}

	vertex, err := hex.DecodeString(v)
	if err != nil {
		return Vertex{}, err
	}

	return NewVertexFromBytes(vertex)
}

// String returns a human readable version of the Vertex which is the
// hex-encoding of the serialized compressed public key.
func (v Vertex) String() string {
	return fmt.Sprintf("%x", v[:])
}

// Hop represents an intermediate or final node of the route. This naming
// is in line with the definition given in BOLT #4: Onion Routing Protocol.
// The struct houses the channel along which this hop can be reached and
// the values necessary to create the HTLC that needs to be sent to the
// next hop. It is also used to encode the per-hop payload included within
// the Sphinx packet.
type Hop struct {
	// PubKeyBytes is the raw bytes of the public key of the target node.
	PubKeyBytes Vertex

	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// OutgoingTimeLock is the timelock value that should be used when
	// crafting the _outgoing_ HTLC from this hop.
	OutgoingTimeLock uint32

	// AmtToForward is the amount that this hop will forward to the next
	// hop. This value is less than the value that the incoming HTLC
	// carries as a fee will be subtracted by the hop.
	AmtToForward lnwire.MilliSatoshi

	// TLVRecords if non-nil are a set of additional TLV records that
	// should be included in the forwarding instructions for this node.
	TLVRecords []tlv.Record

	// LegacyPayload if true, then this signals that this node doesn't
	// understand the new TLV payload, so we must instead use the legacy
	// payload.
	LegacyPayload bool
}

// PackHopPayload writes to the passed io.Writer, the series of byes that can
// be placed directly into the per-hop payload (EOB) for this hop. This will
// include the required routing fields, as well as serializing any of the
// passed optional TLVRecords.  nextChanID is the unique channel ID that
// references the _outgoing_ channel ID that follows this hop. This field
// follows the same semantics as the NextAddress field in the onion: it should
// be set to zero to indicate the terminal hop.
func (h *Hop) PackHopPayload(w io.Writer, nextChanID uint64) error {
	// If this is a legacy payload, then we'll exit here as this method
	// shouldn't be called.
	if h.LegacyPayload == true {
		return fmt.Errorf("cannot pack hop payloads for legacy " +
			"payloads")
	}

	// Otherwise, we'll need to make a new stream that includes our
	// required routing fields, as well as these optional values.
	var records []tlv.Record

	// Every hop must have an amount to forward and CLTV expiry.
	amt := uint64(h.AmtToForward)
	records = append(records,
		record.NewAmtToFwdRecord(&amt),
		record.NewLockTimeRecord(&h.OutgoingTimeLock),
	)

	// BOLT 04 says the next_hop_id should be omitted for the final hop,
	// but present for all others.
	//
	// TODO(conner): test using hop.Exit once available
	if nextChanID != 0 {
		records = append(records,
			record.NewNextHopIDRecord(&nextChanID),
		)
	}

	// Append any custom types destined for this hop.
	records = append(records, h.TLVRecords...)

	// To ensure we produce a canonical stream, we'll sort the records
	// before encoding them as a stream in the hop payload.
	tlv.SortRecords(records)

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// Route represents a path through the channel graph which runs over one or
// more channels in succession. This struct carries all the information
// required to craft the Sphinx onion packet, and send the payment along the
// first hop in the path. A route is only selected as valid if all the channels
// have sufficient capacity to carry the initial payment amount after fees are
// accounted for.
type Route struct {
	// TotalTimeLock is the cumulative (final) time lock across the entire
	// route. This is the CLTV value that should be extended to the first
	// hop in the route. All other hops will decrement the time-lock as
	// advertised, leaving enough time for all hops to wait for or present
	// the payment preimage to complete the payment.
	TotalTimeLock uint32

	// TotalAmount is the total amount of funds required to complete a
	// payment over this route. This value includes the cumulative fees at
	// each hop. As a result, the HTLC extended to the first-hop in the
	// route will need to have at least this many satoshis, otherwise the
	// route will fail at an intermediate node due to an insufficient
	// amount of fees.
	TotalAmount lnwire.MilliSatoshi

	// SourcePubKey is the pubkey of the node where this route originates
	// from.
	SourcePubKey Vertex

	// Hops contains details concerning the specific forwarding details at
	// each hop.
	Hops []*Hop
}

// HopFee returns the fee charged by the route hop indicated by hopIndex.
func (r *Route) HopFee(hopIndex int) lnwire.MilliSatoshi {
	var incomingAmt lnwire.MilliSatoshi
	if hopIndex == 0 {
		incomingAmt = r.TotalAmount
	} else {
		incomingAmt = r.Hops[hopIndex-1].AmtToForward
	}

	// Fee is calculated as difference between incoming and outgoing amount.
	return incomingAmt - r.Hops[hopIndex].AmtToForward
}

// TotalFees is the sum of the fees paid at each hop within the final route. In
// the case of a one-hop payment, this value will be zero as we don't need to
// pay a fee to ourself.
func (r *Route) TotalFees() lnwire.MilliSatoshi {
	if len(r.Hops) == 0 {
		return 0
	}

	return r.TotalAmount - r.Hops[len(r.Hops)-1].AmtToForward
}

// NewRouteFromHops creates a new Route structure from the minimally required
// information to perform the payment. It infers fee amounts and populates the
// node, chan and prev/next hop maps.
func NewRouteFromHops(amtToSend lnwire.MilliSatoshi, timeLock uint32,
	sourceVertex Vertex, hops []*Hop) (*Route, error) {

	if len(hops) == 0 {
		return nil, ErrNoRouteHopsProvided
	}

	// First, we'll create a route struct and populate it with the fields
	// for which the values are provided as arguments of this function.
	// TotalFees is determined based on the difference between the amount
	// that is send from the source and the final amount that is received
	// by the destination.
	route := &Route{
		SourcePubKey:  sourceVertex,
		Hops:          hops,
		TotalTimeLock: timeLock,
		TotalAmount:   amtToSend,
	}

	return route, nil
}

// ToSphinxPath converts a complete route into a sphinx PaymentPath that
// contains the per-hop paylods used to encoding the HTLC routing data for each
// hop in the route. This method also accepts an optional EOB payload for the
// final hop.
func (r *Route) ToSphinxPath() (*sphinx.PaymentPath, error) {
	var path sphinx.PaymentPath

	// For each hop encoded within the route, we'll convert the hop struct
	// to an OnionHop with matching per-hop payload within the path as used
	// by the sphinx package.
	for i, hop := range r.Hops {
		pub, err := btcec.ParsePubKey(
			hop.PubKeyBytes[:], btcec.S256(),
		)
		if err != nil {
			return nil, err
		}

		// As a base case, the next hop is set to all zeroes in order
		// to indicate that the "last hop" as no further hops after it.
		nextHop := uint64(0)

		// If we aren't on the last hop, then we set the "next address"
		// field to be the channel that directly follows it.
		if i != len(r.Hops)-1 {
			nextHop = r.Hops[i+1].ChannelID
		}

		var payload sphinx.HopPayload

		// If this is the legacy payload, then we can just include the
		// hop data as normal.
		if hop.LegacyPayload {
			// Before we encode this value, we'll pack the next hop
			// into the NextAddress field of the hop info to ensure
			// we point to the right now.
			hopData := sphinx.HopData{
				ForwardAmount: uint64(hop.AmtToForward),
				OutgoingCltv:  hop.OutgoingTimeLock,
			}
			binary.BigEndian.PutUint64(
				hopData.NextAddress[:], nextHop,
			)

			payload, err = sphinx.NewHopPayload(&hopData, nil)
			if err != nil {
				return nil, err
			}
		} else {
			// For non-legacy payloads, we'll need to pack the
			// routing information, along with any extra TLV
			// information into the new per-hop payload format.
			// We'll also pass in the chan ID of the hop this
			// channel should be forwarded to so we can construct a
			// valid payload.
			var b bytes.Buffer
			err := hop.PackHopPayload(&b, nextHop)
			if err != nil {
				return nil, err
			}

			// TODO(roasbeef): make better API for NewHopPayload?
			payload, err = sphinx.NewHopPayload(nil, b.Bytes())
			if err != nil {
				return nil, err
			}
		}

		path[i] = sphinx.OnionHop{
			NodePub:    *pub,
			HopPayload: payload,
		}
	}

	return &path, nil
}

// String returns a human readable representation of the route.
func (r *Route) String() string {
	var b strings.Builder

	for i, hop := range r.Hops {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strconv.FormatUint(hop.ChannelID, 10))
	}

	return fmt.Sprintf("amt=%v, fees=%v, tl=%v, chans=%v",
		r.TotalAmount-r.TotalFees(), r.TotalFees(), r.TotalTimeLock,
		b.String(),
	)
}
