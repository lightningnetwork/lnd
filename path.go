package sphinx

import "github.com/btcsuite/btcd/btcec"

// NumMaxHops is the maximum path length. This should be set to an estimate of
// the upper limit of the diameter of the node graph.
const NumMaxHops = 20

// PaymentPath represents a series of hops within the Lightning Network
// starting at a sender and terminating at a receiver. Each hop contains a set
// of mandatory data which contains forwarding instructions for that hop.
// Additionally, we can also transmit additional data to each hop by utilizing
// the un-used hops (see TrueRouteLength()) to pack in additional data. In
// order to do this, we encrypt the several hops with the same node public key,
// and unroll the extra data into the space used for route forwarding
// information.
type PaymentPath [NumMaxHops]OnionHop

// OnionHop represents an abstract hop (a link between two nodes) within the
// Lightning Network. A hop is composed of the incoming node (able to decrypt
// the encrypted routing information), and the routing information itself.
// Optionally, the crafter of a route can indicate that additional data aside
// from the routing information is be delivered, which will manifest as
// additional hops to pack the data.
type OnionHop struct {
	// NodePub is the target node for this hop. The payload will enter this
	// hop, it'll decrypt the routing information, and hand off the
	// internal packet to the next hop.
	NodePub btcec.PublicKey

	// HopData are the plaintext routing instructions that should be
	// delivered to this hop.
	HopData HopData
}

// IsEmpty returns true if the hop isn't populated.
func (o OnionHop) IsEmpty() bool {
	return o.NodePub.X == nil || o.NodePub.Y == nil
}

// NodeKeys returns a slice pointing to node keys that this route comprises of.
// The size of the returned slice will be TrueRouteLength().
func (p *PaymentPath) NodeKeys() []*btcec.PublicKey {
	var nodeKeys [NumMaxHops]*btcec.PublicKey

	routeLen := p.TrueRouteLength()
	for i := 0; i < routeLen; i++ {
		nodeKeys[i] = &p[i].NodePub
	}

	return nodeKeys[:routeLen]
}

// TrueRouteLength returns the "true" length of the PaymentPath. The max
// payment path is NumMaxHops size, but in practice routes are much smaller.
// This method will return the number of actual hops (nodes) involved in this
// route. For references, a direct path has a length of 1, path through an
// intermediate node has a length of 2 (3 nodes involved).
func (p *PaymentPath) TrueRouteLength() int {
	var routeLength int
	for _, hop := range p {
		// When we hit the first empty hop, we know we're now in the
		// zero'd out portion of the array.
		if hop.IsEmpty() {
			return routeLength
		}

		routeLength++
	}

	return routeLength
}
