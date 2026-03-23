package models

import (
	"fmt"
	"image/color"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/tor"
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

	// LastBlockHeight is the block height that timestamps the last update
	// we received for this node. This is only used if this is a V2 node
	// announcement.
	LastBlockHeight uint32

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
	// compatible manner. This is only used for V1 node announcements.
	ExtraOpaqueData []byte

	// ExtraSignedFields is a map of extra fields that are covered by the
	// node announcement's signature that we have not explicitly parsed.
	// This is only used for version 2 node announcements and beyond.
	ExtraSignedFields map[uint64][]byte
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

// NodeV2Fields houses the fields that are specific to a version 2 node
// announcement.
type NodeV2Fields struct {
	// LastBlockHeight is the block height that timestamps the last update
	// we received for this node.
	LastBlockHeight uint32

	// Address is the TCP address this node is reachable over.
	Addresses []net.Addr

	// Color is the selected color for the node.
	Color fn.Option[color.RGBA]

	// Alias is a nick-name for the node. The alias can be used to confirm
	// a node's identity or to serve as a short ID for an address book.
	Alias fn.Option[string]

	// Signature is the schnorr signature under the advertised public key
	// which serves to authenticate the attributes announced by this node.
	Signature []byte

	// Features is the list of protocol features supported by this node.
	Features *lnwire.RawFeatureVector

	// ExtraSignedFields is a map of extra fields that are covered by the
	// node announcement's signature that we have not explicitly parsed.
	ExtraSignedFields map[uint64][]byte
}

// NewV2Node creates a new version 2 node from the passed fields.
func NewV2Node(pub route.Vertex, n *NodeV2Fields) *Node {
	return &Node{
		Version:      lnwire.GossipVersion2,
		PubKeyBytes:  pub,
		Addresses:    n.Addresses,
		AuthSigBytes: n.Signature,
		Features: lnwire.NewFeatureVector(
			n.Features, lnwire.Features,
		),
		LastBlockHeight:   n.LastBlockHeight,
		Color:             n.Color,
		Alias:             n.Alias,
		LastUpdate:        time.Unix(0, 0),
		ExtraSignedFields: n.ExtraSignedFields,
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
// NOTE: By having this method to access the attribute, we ensure we only need
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

// NodeAnnouncement retrieves the v1 node announcement for this node.
func (n *Node) NodeAnnouncement(signed bool) (*lnwire.NodeAnnouncement1,
	error) {

	return n.toNodeAnnouncement1(signed)
}

// WireNodeAnnouncement reconstructs the version-appropriate wire node
// announcement for this node. If signed is true, the returned announcement
// will include the node's signature. Returns an error if signed is true but
// no signature is stored.
func (n *Node) WireNodeAnnouncement(signed bool) (lnwire.NodeAnnouncement,
	error) {

	if signed && !n.HaveAnnouncement() {
		return nil, fmt.Errorf("node does not have node announcement")
	}

	switch n.Version {
	case lnwire.GossipVersion1:
		return n.toNodeAnnouncement1(signed)

	case lnwire.GossipVersion2:
		return n.toNodeAnnouncement2(signed)

	default:
		return nil, fmt.Errorf("unsupported node version: %d",
			n.Version)
	}
}

// toNodeAnnouncement1 constructs a v1 node announcement from the node's
// stored fields.
func (n *Node) toNodeAnnouncement1(signed bool) (*lnwire.NodeAnnouncement1,
	error) {

	alias, err := lnwire.NewNodeAlias(n.Alias.UnwrapOr(""))
	if err != nil {
		return nil, err
	}

	nodeAnn := &lnwire.NodeAnnouncement1{
		Features:        n.Features.RawFeatureVector,
		NodeID:          n.PubKeyBytes,
		RGBColor:        n.Color.UnwrapOr(color.RGBA{}),
		Alias:           alias,
		Addresses:       n.Addresses,
		Timestamp:       uint32(n.LastUpdate.Unix()),
		ExtraOpaqueData: n.ExtraOpaqueData,
	}

	if !signed {
		return nodeAnn, nil
	}

	nodeAnn.Signature, err = lnwire.NewSigFromECDSARawSignature(
		n.AuthSigBytes,
	)
	if err != nil {
		return nil, err
	}

	return nodeAnn, nil
}

// toNodeAnnouncement2 constructs a v2 node announcement from the node's
// stored fields.
func (n *Node) toNodeAnnouncement2(signed bool) (*lnwire.NodeAnnouncement2,
	error) {

	nodeAnn := &lnwire.NodeAnnouncement2{
		Features: tlv.NewRecordT[tlv.TlvType0](
			*n.Features.RawFeatureVector,
		),
		BlockHeight: tlv.NewPrimitiveRecord[tlv.TlvType2](
			n.LastBlockHeight,
		),
		NodeID: tlv.NewPrimitiveRecord[tlv.TlvType4, [33]byte](
			n.PubKeyBytes,
		),
		ExtraSignedFields: n.ExtraSignedFields,
	}

	n.Alias.WhenSome(func(s string) {
		aliasRecord := tlv.ZeroRecordT[tlv.TlvType3, lnwire.NodeAlias2]()
		aliasRecord.Val = lnwire.NodeAlias2(s)
		nodeAnn.Alias = tlv.SomeRecordT(aliasRecord)
	})

	n.Color.WhenSome(func(rgba color.RGBA) {
		colorRecord := tlv.ZeroRecordT[tlv.TlvType1, lnwire.Color]()
		colorRecord.Val = lnwire.Color(rgba)
		nodeAnn.Color = tlv.SomeRecordT(colorRecord)
	})

	// Categorise addresses by type for the separate TLV fields.
	var (
		ipv4  lnwire.IPV4Addrs
		ipv6  lnwire.IPV6Addrs
		torV3 lnwire.TorV3Addrs
	)
	for _, addr := range n.Addresses {
		switch a := addr.(type) {
		case *net.TCPAddr:
			if a.IP.To4() != nil {
				ipv4 = append(ipv4, a)
			} else {
				ipv6 = append(ipv6, a)
			}

		case *tor.OnionAddr:
			// Only v3 onion addresses are supported in gossip v2.
			if len(a.OnionService) == tor.V3Len {
				torV3 = append(torV3, a)
			}

		case *lnwire.DNSAddress:
			nodeAnn.DNSHostName = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType11](*a),
			)
		}
	}
	if len(ipv4) > 0 {
		nodeAnn.IPV4Addrs = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType5](ipv4),
		)
	}
	if len(ipv6) > 0 {
		nodeAnn.IPV6Addrs = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType7](ipv6),
		)
	}
	if len(torV3) > 0 {
		nodeAnn.TorV3Addrs = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType9](torV3),
		)
	}

	if !signed {
		return nodeAnn, nil
	}

	var err error
	nodeAnn.Signature.Val, err = lnwire.NewSigFromSchnorrRawSignature(
		n.AuthSigBytes,
	)
	if err != nil {
		return nil, err
	}

	return nodeAnn, nil
}

// NodeFromWireAnnouncement creates a Node instance from a node announcement
// wire message.
func NodeFromWireAnnouncement(msg lnwire.NodeAnnouncement) (*Node, error) {
	switch msg := msg.(type) {
	case *lnwire.NodeAnnouncement1:
		timestamp := time.Unix(int64(msg.Timestamp), 0)
		authSig := msg.Signature.ToSignatureBytes()

		return NewV1Node(
			msg.NodeID,
			&NodeV1Fields{
				LastUpdate:      timestamp,
				Addresses:       msg.Addresses,
				Alias:           msg.Alias.String(),
				AuthSigBytes:    authSig,
				Features:        msg.Features,
				Color:           msg.RGBColor,
				ExtraOpaqueData: msg.ExtraOpaqueData,
			},
		), nil

	case *lnwire.NodeAnnouncement2:
		var addrs []net.Addr
		ipv4Opt := msg.IPV4Addrs.ValOpt()
		ipv4Opt.WhenSome(func(ipv4Addrs lnwire.IPV4Addrs) {
			for _, addr := range ipv4Addrs {
				addrs = append(addrs, addr)
			}
		})
		ipv6Opt := msg.IPV6Addrs.ValOpt()
		ipv6Opt.WhenSome(func(ipv6Addrs lnwire.IPV6Addrs) {
			for _, addr := range ipv6Addrs {
				addrs = append(addrs, addr)
			}
		})
		torOpt := msg.TorV3Addrs.ValOpt()
		torOpt.WhenSome(func(torAddrs lnwire.TorV3Addrs) {
			for _, addr := range torAddrs {
				addrs = append(addrs, addr)
			}
		})
		dnsOpt := msg.DNSHostName.ValOpt()
		dnsOpt.WhenSome(func(dnsAddr lnwire.DNSAddress) {
			dns := dnsAddr
			addrs = append(addrs, &dns)
		})

		nodeColor := fn.MapOption(func(c lnwire.Color) color.RGBA {
			return color.RGBA(c)
		})(msg.Color.ValOpt())

		nodeAlias := fn.MapOption(func(a lnwire.NodeAlias2) string {
			return string(a)
		})(msg.Alias.ValOpt())

		sig := msg.Signature.Val.ToSignatureBytes()

		return NewV2Node(
			msg.NodeID.Val, &NodeV2Fields{
				LastBlockHeight:   msg.BlockHeight.Val,
				Addresses:         addrs,
				Color:             nodeColor,
				Alias:             nodeAlias,
				Signature:         sig,
				Features:          &msg.Features.Val,
				ExtraSignedFields: msg.ExtraSignedFields,
			},
		), nil
	}

	return nil, fmt.Errorf("unsupported node announcement: %T", msg)
}
