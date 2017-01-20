package routing

import (
	"bytes"
	"encoding/hex"
	"github.com/btcsuite/golangcrypto/ripemd160"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// HopID represents the id which is used by propagation subsystem in order to
// identify lightning network node.
type HopID [ripemd160.Size]byte

// NewHopID...
func NewHopID(pubKey []byte) *HopID {
	var routeId HopID
	copy(routeId[:], btcutil.Hash160(pubKey))
	return &routeId
}

func (id *HopID) String() string {
	return hex.EncodeToString(id[:])
}

func (first *HopID) Equal(second *HopID) bool {
	return bytes.Equal(first[:], second[:])
}

// HopIterator interface represent the entity which is able to give route
// hops one by one. This interface is used to have an abstraction over the
// algorithm which we use to determine the next hope in htlc route and also
// help the unit test to create mock representation of such algorithm.
type HopIterator interface {
	// Next...
	Next() *HopID

	// ToBytes...
	ToBytes() ([]byte, error)
}

// SphinxHopIterator...
type SphinxHopIterator struct {
	onionPacket  *sphinx.OnionPacket
	sphinxPacket *sphinx.ProcessedPacket
}

var _ HopIterator = (*SphinxHopIterator)(nil)

// NewSphinxHopIterator...
func NewSphinxHopIterator(route *Route,
	paymentHash []byte) (*SphinxHopIterator, error) {
	// First obtain all the public keys along the route which are contained
	// in each hop.
	nodes := make([]*btcec.PublicKey, len(route.Hops))
	for i, hop := range route.Hops {
		// We create a new instance of the public key to avoid possibly
		// mutating the curve parameters, which are unset in a higher
		// level in order to avoid spamming the logs.
		pub := btcec.PublicKey{
			btcec.S256(),
			hop.Channel.Node.PubKey.X,
			hop.Channel.Node.PubKey.Y,
		}
		nodes[i] = &pub
	}

	// Next we generate the per-hop payload which gives each node within
	// the route the necessary information (fees, CLTV value, etc) to
	// properly forward the payment.
	// TODO(roasbeef): properly set CLTV value, payment amount, and chain
	// within hop paylods.
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

	// Next generate the onion routing packet which allows
	// us to perform privacy preserving source routing
	// across the network.
	packet, err := sphinx.NewOnionPacket(nodes, sessionKey,
		hopPayloads, paymentHash)
	if err != nil {
		return nil, err
	}

	return &SphinxHopIterator{
		onionPacket:  packet,
		sphinxPacket: nil,
	}, nil
}

// ToBytes...
func (r *SphinxHopIterator) ToBytes() ([]byte, error) {
	var data bytes.Buffer
	if err := r.onionPacket.Encode(&data); err != nil {
		return nil, err
	}

	return data.Bytes(), nil
}

// Next...
func (r *SphinxHopIterator) Next() *HopID {
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

// SphinxDecoder...
type SphinxDecoder struct {
	router *sphinx.Router
}

// NewSphinxDecoder...
func NewSphinxDecoder(router *sphinx.Router) *SphinxDecoder {
	return &SphinxDecoder{router}
}

// Decode...
func (p *SphinxDecoder) Decode(data []byte, rHash []byte) (HopIterator, error) {
	// Before adding the new HTLC to the state machine, parse the
	// onion object in order to obtain the routing information.
	blobReader := bytes.NewReader(data)
	onionPkt := &sphinx.OnionPacket{}
	if err := onionPkt.Decode(blobReader); err != nil {
		return nil, errors.Errorf("unable to decode onion pkt: %v", err)

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

	return HopIterator(&SphinxHopIterator{
		onionPacket:  sphinxPacket.Packet,
		sphinxPacket: sphinxPacket,
	}), nil
}
