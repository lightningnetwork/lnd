package route

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrNoRouteHopsProvided is returned when a caller attempts to construct a new
// sphinx packet, but provides an empty set of hops for each route.
var ErrNoRouteHopsProvided = fmt.Errorf("empty route hops provided")

// Vertex is a simple alias for the serialization of a compressed Bitcoin
// public key.
type Vertex [33]byte

// NewVertex returns a new Vertex given a public key.
func NewVertex(pub *btcec.PublicKey) Vertex {
	var v Vertex
	copy(v[:], pub.SerializeCompressed())
	return v
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
// hop in the route.
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

		path[i] = sphinx.OnionHop{
			NodePub: *pub,
			HopData: sphinx.HopData{
				// TODO(roasbeef): properly set realm, make
				// sphinx type an enum actually?
				Realm:         [1]byte{0},
				ForwardAmount: uint64(hop.AmtToForward),
				OutgoingCltv:  hop.OutgoingTimeLock,
			},
		}

		// As a base case, the next hop is set to all zeroes in order
		// to indicate that the "last hop" as no further hops after it.
		nextHop := uint64(0)

		// If we aren't on the last hop, then we set the "next address"
		// field to be the channel that directly follows it.
		if i != len(r.Hops)-1 {
			nextHop = r.Hops[i+1].ChannelID
		}

		binary.BigEndian.PutUint64(
			path[i].HopData.NextAddress[:], nextHop,
		)
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

// GetNodeIndex returns the index of the given pubkey in the route. The source
// node is index zero.
func (r *Route) GetNodeIndex(pubkey Vertex) (int, error) {
	if pubkey == r.SourcePubKey {
		return 0, nil
	}

	for i, hop := range r.Hops {
		if pubkey == hop.PubKeyBytes {
			return i + 1, nil
		}
	}

	return 0, errors.New("cannot find error source node in route")
}

// GetNodePubkeyByIndex gets the pubkey of the specified node in the route. The
// source node is returned when index is zero.
func (r *Route) GetNodePubkeyByIndex(index int) Vertex {
	if index == 0 {
		return r.SourcePubKey
	}

	return r.Hops[index-1].PubKeyBytes
}
