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

// NewHopID creates new instance of hop form node public key.
func NewHopID(pubKey []byte) HopID {
	var routeID HopID
	copy(routeID[:], btcutil.Hash160(pubKey))
	return routeID
}

// String returns string representation of hop id.
func (h HopID) String() string {
	return hex.EncodeToString(h[:])
}

// IsEqual checks does the two hop ids are equal.
func (h HopID) IsEqual(h2 HopID) bool {
	return bytes.Equal(h[:], h2[:])
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

// NewSphinxBlob creates new instance of sphinx hop iterator.
func NewSphinxBlob(route *routing.Route, paymentHash []byte) ([]byte, error) {

	// First obtain all the public keys along the route which are contained
	// in each hop.
	nodes := make([]*btcec.PublicKey, len(route.Hops))
	for i, hop := range route.Hops {
		// We create a new instance of the public key to avoid possibly
		// mutating the curve parameters, which are unset in a higher
		// level in order to avoid spamming the logs.
		pub := btcec.PublicKey{
			Curve: btcec.S256(),
			X:     hop.Channel.Node.PubKey.X,
			Y:     hop.Channel.Node.PubKey.Y,
		}
		nodes[i] = &pub
	}

	// Next we generate the per-hop payload which gives each node within
	// the route the necessary information (fees, CLTV value, etc) to
	// properly forward the payment.
	// TODO(roasbeef): properly set CLTV value, payment amount, and chain
	// within hop payloads.
	var hopPayloads [][]byte
	for i := 0; i < len(route.Hops); i++ {
		payload := bytes.Repeat([]byte{byte('A' + i)},
			sphinx.HopPayloadSize)
		hopPayloads = append(hopPayloads, payload)
	}

	sessionKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	// Next generate the onion routing packet which allows us to perform
	// privacy preserving source routing across the network.
	onionPacket, err := sphinx.NewOnionPacket(nodes, sessionKey,
		hopPayloads, paymentHash)
	if err != nil {
		return nil, err
	}

	// Finally, encode Sphinx packet using it's wire representation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := onionPacket.Encode(&onionBlob); err != nil {
		return nil, err
	}

	return onionBlob.Bytes(), nil
}

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
