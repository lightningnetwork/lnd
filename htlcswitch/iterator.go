package htlcswitch

import (
	"bytes"
	"encoding/hex"
	"io"

	"github.com/btcsuite/golangcrypto/ripemd160"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// HopID represents the id which is used by propagation subsystem in order to
// identify lightning network node.
// TODO(andrew.shvv) remove after switching to the using channel id.
type HopID [ripemd160.Size]byte
// NetworkHop indicates the blockchain network that is intended to be the next
// hop for a forwarded HTLC. The existnce of this field within the
// ForwardingInfo struct enables the ability for HTLC to cross chain-boundaries
// at will.
type NetworkHop uint8

// NewHopID creates new instance of hop form node public key.
func NewHopID(pubKey []byte) HopID {
	var routeID HopID
	copy(routeID[:], btcutil.Hash160(pubKey))
	return routeID
}
const (
	// BitcoinHop denotes that an HTLC is to be forwarded along the Bitcoin
	// link with the specified short channel ID.
	BitcoinHop NetworkHop = iota

	// LitecoinHop denotes that an HTLC is to be forwarded along the
	// Litecoin link with the specified short channel ID.
	LitecoinHop
)

// String returns string representation of hop id.
func (h HopID) String() string {
	return hex.EncodeToString(h[:])
// String returns the string representation of the target NetworkHop.
func (c NetworkHop) String() string {
	switch c {
	case BitcoinHop:
		return "Bitcoin"
	case LitecoinHop:
		return "Litecoin"
	default:
		return "Kekcoin"
	}
}

// IsEqual checks does the two hop ids are equal.
func (h HopID) IsEqual(h2 HopID) bool {
	return bytes.Equal(h[:], h2[:])
var (
	// exitHop is a special "hop" which denotes that an incoming HTLC is
	// meant to pay finally to the receiving node.
	exitHop lnwire.ShortChannelID
)

// ForwardingInfo contains all the information that is necessary to forward and
// incoming HTLC to the next hop encoded within a valid HopIterator instance.
// Forwarding links are to use this information to authenticate the information
// received within the incoming HTLC, to ensure that the prior hop didn't
// tamper with the end-to-end routing information at all.
type ForwardingInfo struct {
	// Network is the target blockchain network that the HTLC will travel
	// over next.
	Network NetworkHop

	// NextHop is the channel ID of the next hop. The received HTLC should
	// be forwarded to this particular channel in order to continue the
	// end-to-end route.
	NextHop lnwire.ShortChannelID

	// AmountToForward is the amount that the receiving node should forward
	// to the next hop.
	AmountToForward btcutil.Amount

	// OutgoingCTLV is the specified value of the CTLV timelock to be used
	// in the outgoing HTLC.
	OutgoingCTLV uint32

	// TODO(roasbeef): modify sphinx logic to not just discard the
	// remaining bytes, instead should include the rest as excess
}

// HopIterator interface represent the entity which is able to give route
// hops one by one. This interface is used to have an abstraction over the
// algorithm which we use to determine the next hope in htlc route.
type HopIterator interface {
	// Next returns next hop if exist and nil if route is ended.
	Next() *HopID

	// Encode encodes iterator and writes it to the writer.
	Encode(w io.Writer) error
}

// sphinxHopIterator is the Sphinx implementation of hop iterator which uses
// onion routing to encode route in such a way so that node might see only the
// next hop in the route, after retrieving hop iterator will behave as if
// there is no hop in path.
type sphinxHopIterator struct {
	onionPacket  *sphinx.OnionPacket
	sphinxPacket *sphinx.ProcessedPacket
}

// // A compile time check to ensure sphinxHopIterator implements the HopIterator
// interface.
var _ HopIterator = (*sphinxHopIterator)(nil)

// Encode encodes iterator and writes it to the writer.
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) Encode(w io.Writer) error {
	return r.onionPacket.Encode(w)
}

// Next returns next hop if exist and nil if route is ended.
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) Next() *HopID {
	// If next node was already given than behave as if no hops in route.
	if r.sphinxPacket == nil {
		return nil
	}

	switch r.sphinxPacket.Action {
	case sphinx.ExitNode:
		return nil

	case sphinx.MoreHops:
		id := (*HopID)(&r.sphinxPacket.NextHop)
		r.sphinxPacket = nil
		return id
	}

	return nil
}

// SphinxDecoder is responsible for keeping all sphinx dependent parts inside
// and expose only decoding function. With such approach we give freedom for
// subsystems which wants to decode sphinx path to not be dependable from sphinx
// at all.
//
// NOTE: The reason for keeping decoder separated from hop iterator is too
// maintain the hop iterator abstraction. Without it the structures which using
// the hop iterator should contain sphinx router which makes their
// creations in tests dependent from the sphinx internal parts.
type SphinxDecoder struct {
	router *sphinx.Router
}

// NewSphinxDecoder creates new instance of decoder.
func NewSphinxDecoder(router *sphinx.Router) *SphinxDecoder {
	return &SphinxDecoder{router}
}

// Decode takes byte stream as input and decodes the route/ hop iterator.
func (p *SphinxDecoder) Decode(r io.Reader, rHash []byte) (HopIterator, error) {
	// Before adding the new HTLC to the state machine, parse the
	// onion object in order to obtain the routing information.
	onionPkt := &sphinx.OnionPacket{}
	if err := onionPkt.Decode(r); err != nil {
		return nil, errors.Errorf("unable to decode onion pkt: %v",
			err)

	}

	// Attempt to process the Sphinx packet. We include the payment
	// hash of the HTLC as it's authenticated within the Sphinx
	// packet itself as associated data in order to thwart attempts
	// a replay attacks. In the case of a replay, an attacker is
	// *forced* to use the same payment hash twice, thereby losing
	// their money entirely.
	sphinxPacket, err := p.router.ProcessOnionPacket(onionPkt, rHash)
	if err != nil {
		return nil, errors.Errorf("unable to process onion pkt: "+
			"%v", err)
	}

	return HopIterator(&sphinxHopIterator{
		onionPacket:  sphinxPacket.Packet,
		sphinxPacket: sphinxPacket,
	}), nil
}
