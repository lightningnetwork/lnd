package models

import (
	"image/color"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Node represents an individual vertex/node within the channel graph.
// A node is connected to other nodes by one or more channel edges emanating
// from it. As the graph is directed, a node will also have an incoming edge
// attached to it for each outgoing edge.
type Node struct {
	// Version is the gossip version that this node was advertised on.
	Version lnwire.GossipVersion

	// PubKeyBytes is the raw bytes of the public key of the target node.
	PubKeyBytes [33]byte
	pubKey      *btcec.PublicKey

	// LastUpdate is the last time the vertex information for this node has
	// been updated.
	LastUpdate time.Time

	// Address is the TCP address this node is reachable over.
	Addresses []net.Addr

	// Color is the selected color for the node.
	Color fn.Option[color.RGBA]

	// Alias is a nick-name for the node. The alias can be used to confirm
	// a node's identity or to serve as a short ID for an address book.
	Alias fn.Option[string]

	// AuthSigBytes is the raw signature under the advertised public key
	// which serves to authenticate the attributes announced by this node.
	AuthSigBytes []byte

	// Features is the list of protocol features supported by this node.
	Features *lnwire.FeatureVector

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte
}

// NodeV1Fields houses the fields that are specific to a version 1 node
// announcement.
type NodeV1Fields struct {
	// Address is the TCP address this node is reachable over.
	Addresses []net.Addr

	// AuthSigBytes is the raw signature under the advertised public key
	// which serves to authenticate the attributes announced by this node.
	AuthSigBytes []byte

	// Features is the list of protocol features supported by this node.
	Features *lnwire.RawFeatureVector

	// Color is the selected color for the node.
	Color color.RGBA

	// Alias is a nick-name for the node. The alias can be used to confirm
	// a node's identity or to serve as a short ID for an address book.
	Alias string

	// LastUpdate is the last time the vertex information for this node has
	// been updated.
	LastUpdate time.Time

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte
}

// NewV1Node creates a new version 1 node from the passed fields.
func NewV1Node(pub route.Vertex, n *NodeV1Fields) *Node {
	return &Node{
		Version:      lnwire.GossipVersion1,
		PubKeyBytes:  pub,
		Addresses:    n.Addresses,
		AuthSigBytes: n.AuthSigBytes,
		Features: lnwire.NewFeatureVector(
			n.Features, lnwire.Features,
		),
		Color:           fn.Some(n.Color),
		Alias:           fn.Some(n.Alias),
		LastUpdate:      n.LastUpdate,
		ExtraOpaqueData: n.ExtraOpaqueData,
	}
}

// NewV1ShellNode creates a new shell version 1 node.
func NewV1ShellNode(pubKey route.Vertex) *Node {
	return NewShellNode(lnwire.GossipVersion1, pubKey)
}

// NewShellNode creates a new shell node with the given gossip version and
// public key.
func NewShellNode(v lnwire.GossipVersion, pubKey route.Vertex) *Node {
	return &Node{
		Version:     v,
		PubKeyBytes: pubKey,
		Features:    lnwire.EmptyFeatureVector(),
		LastUpdate:  time.Unix(0, 0),
	}
}

// HaveAnnouncement returns true if we have received a node announcement for
// this node. We determine this by checking if we have a signature for the
// announcement.
func (n *Node) HaveAnnouncement() bool {
	return len(n.AuthSigBytes) > 0
}

// PubKey is the node's long-term identity public key. This key will be used to
// authenticated any advertisements/updates sent by the node.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (n *Node) PubKey() (*btcec.PublicKey, error) {
	if n.pubKey != nil {
		return n.pubKey, nil
	}

	key, err := btcec.ParsePubKey(n.PubKeyBytes[:])
	if err != nil {
		return nil, err
	}
	n.pubKey = key

	return key, nil
}
