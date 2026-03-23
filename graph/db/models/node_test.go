package models

import (
	"image/color"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// newTestKey generates a fresh private key for tests.
func newTestKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	return key
}

// pubVertex returns the compressed-public-key route.Vertex for a key.
func pubVertex(key *btcec.PrivateKey) route.Vertex {
	var v route.Vertex
	copy(v[:], key.PubKey().SerializeCompressed())
	return v
}

// ecdsaSigBytes signs a 32-byte hash with an ECDSA key and returns DER bytes.
func ecdsaSigBytes(t *testing.T, key *btcec.PrivateKey, hash [32]byte) []byte {
	t.Helper()
	sig := ecdsa.Sign(key, hash[:])
	return sig.Serialize()
}

// schnorrSigBytes signs a 32-byte hash with a Schnorr key and returns 64 bytes.
func schnorrSigBytes(t *testing.T, key *btcec.PrivateKey, hash [32]byte) []byte {
	t.Helper()
	sig, err := schnorr.Sign(key, hash[:])
	require.NoError(t, err)
	return sig.Serialize()
}

// TestNodeAnnouncementV1UnsignedRoundTrip verifies that a v1 Node can
// reconstruct an unsigned NodeAnnouncement1 with all fields intact.
func TestNodeAnnouncementV1UnsignedRoundTrip(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	pub := pubVertex(key)
	addr := &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 9735}
	ts := time.Unix(1_000_000, 0).UTC()

	node := NewV1Node(pub, &NodeV1Fields{
		LastUpdate:      ts,
		Addresses:       []net.Addr{addr},
		Alias:           "alice",
		Color:           color.RGBA{R: 1, G: 2, B: 3},
		Features:        lnwire.EmptyFeatureVector().RawFeatureVector,
		ExtraOpaqueData: []byte{0xde, 0xad},
	})

	ann, err := node.WireNodeAnnouncement(false)
	require.NoError(t, err)

	ann1, ok := ann.(*lnwire.NodeAnnouncement1)
	require.True(t, ok, "expected *NodeAnnouncement1")
	require.Equal(t, pub, route.Vertex(ann1.NodeID))
	require.Equal(t, "alice", ann1.Alias.String())
	require.Equal(t, color.RGBA{R: 1, G: 2, B: 3}, ann1.RGBColor)
	require.Equal(t, uint32(ts.Unix()), ann1.Timestamp)
	require.EqualValues(t, []byte{0xde, 0xad}, ann1.ExtraOpaqueData)
	require.Len(t, ann1.Addresses, 1)
}

// TestNodeAnnouncementV1SignedRoundTrip verifies that NodeAnnouncement includes
// the stored ECDSA signature for v1 when signed=true.
func TestNodeAnnouncementV1SignedRoundTrip(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	pub := pubVertex(key)
	sigBytes := ecdsaSigBytes(t, key, [32]byte{0x01})

	node := NewV1Node(pub, &NodeV1Fields{
		LastUpdate:   time.Now(),
		AuthSigBytes: sigBytes,
		Features:     lnwire.EmptyFeatureVector().RawFeatureVector,
	})

	ann, err := node.WireNodeAnnouncement(true)
	require.NoError(t, err)

	ann1, ok := ann.(*lnwire.NodeAnnouncement1)
	require.True(t, ok)
	require.Equal(t, sigBytes, ann1.Signature.ToSignatureBytes())
}

// TestNodeAnnouncementV2UnsignedRoundTrip verifies that a v2 Node can
// reconstruct an unsigned NodeAnnouncement2 with all core fields intact.
func TestNodeAnnouncementV2UnsignedRoundTrip(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	pub := pubVertex(key)

	node := NewV2Node(pub, &NodeV2Fields{
		LastBlockHeight:   42,
		Alias:             fn.Some("bob"),
		Color:             fn.Some(color.RGBA{R: 10, G: 20, B: 30}),
		Features:          lnwire.EmptyFeatureVector().RawFeatureVector,
		ExtraSignedFields: map[uint64][]byte{9999: {0xab}},
	})

	ann, err := node.WireNodeAnnouncement(false)
	require.NoError(t, err)

	ann2, ok := ann.(*lnwire.NodeAnnouncement2)
	require.True(t, ok, "expected *NodeAnnouncement2")
	require.Equal(t, pub, route.Vertex(ann2.NodeID.Val))
	require.EqualValues(t, 42, ann2.BlockHeight.Val)
	require.EqualValues(t, map[uint64][]byte{9999: {0xab}}, ann2.ExtraSignedFields)

	aliasOpt := ann2.Alias.ValOpt()
	require.True(t, aliasOpt.IsSome())
	aliasOpt.WhenSome(func(a lnwire.NodeAlias2) {
		require.Equal(t, "bob", string(a))
	})

	colorOpt := ann2.Color.ValOpt()
	require.True(t, colorOpt.IsSome())
	colorOpt.WhenSome(func(c lnwire.Color) {
		require.Equal(t, color.RGBA{R: 10, G: 20, B: 30}, color.RGBA(c))
	})
}

// TestNodeAnnouncementV2SignedRoundTrip verifies that NodeAnnouncement2
// includes the stored Schnorr signature when signed=true.
func TestNodeAnnouncementV2SignedRoundTrip(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	pub := pubVertex(key)
	sigBytes := schnorrSigBytes(t, key, [32]byte{0x02})

	node := NewV2Node(pub, &NodeV2Fields{
		LastBlockHeight: 10,
		Signature:       sigBytes,
		Features:        lnwire.EmptyFeatureVector().RawFeatureVector,
	})

	ann, err := node.WireNodeAnnouncement(true)
	require.NoError(t, err)

	ann2, ok := ann.(*lnwire.NodeAnnouncement2)
	require.True(t, ok)
	require.Equal(t, sigBytes, ann2.Signature.Val.ToSignatureBytes())
}

// TestNodeAnnouncementV2AddressCategorization verifies that addresses of
// different types are placed into the correct TLV fields on NodeAnnouncement2.
func TestNodeAnnouncementV2AddressCategorization(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	pub := pubVertex(key)

	ipv4Addr := &net.TCPAddr{IP: net.ParseIP("1.2.3.4").To4(), Port: 9735}
	ipv6Addr := &net.TCPAddr{IP: net.ParseIP("::1"), Port: 9736}
	torAddr := &tor.OnionAddr{
		OnionService: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.onion",
		Port:         9737,
	}
	dnsAddr := &lnwire.DNSAddress{Hostname: "example.com", Port: 9738}

	node := NewV2Node(pub, &NodeV2Fields{
		Features:  lnwire.EmptyFeatureVector().RawFeatureVector,
		Addresses: []net.Addr{ipv4Addr, ipv6Addr, torAddr, dnsAddr},
	})

	ann, err := node.WireNodeAnnouncement(false)
	require.NoError(t, err)

	ann2, ok := ann.(*lnwire.NodeAnnouncement2)
	require.True(t, ok)

	ann2.IPV4Addrs.ValOpt().WhenSome(func(addrs lnwire.IPV4Addrs) {
		require.Len(t, addrs, 1)
		require.Equal(t, ipv4Addr.Port, addrs[0].Port)
	})
	require.True(t, ann2.IPV4Addrs.ValOpt().IsSome(), "expected ipv4 addrs")

	ann2.IPV6Addrs.ValOpt().WhenSome(func(addrs lnwire.IPV6Addrs) {
		require.Len(t, addrs, 1)
		require.Equal(t, ipv6Addr.Port, addrs[0].Port)
	})
	require.True(t, ann2.IPV6Addrs.ValOpt().IsSome(), "expected ipv6 addrs")

	ann2.TorV3Addrs.ValOpt().WhenSome(func(addrs lnwire.TorV3Addrs) {
		require.Len(t, addrs, 1)
		require.Equal(t, torAddr.Port, addrs[0].Port)
	})
	require.True(t, ann2.TorV3Addrs.ValOpt().IsSome(), "expected tor addrs")

	ann2.DNSHostName.ValOpt().WhenSome(func(d lnwire.DNSAddress) {
		require.Equal(t, dnsAddr.Hostname, d.Hostname)
		require.Equal(t, dnsAddr.Port, d.Port)
	})
	require.True(t, ann2.DNSHostName.ValOpt().IsSome(), "expected dns addr")
}

// TestNodeFromWireAnnouncementV1 verifies a NodeAnnouncement1 is parsed into
// a v1 Node with all fields correctly mapped.
func TestNodeFromWireAnnouncementV1(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	var pubBytes [33]byte
	copy(pubBytes[:], key.PubKey().SerializeCompressed())

	rawSig := ecdsaSigBytes(t, key, [32]byte{0x03})
	wireSig, err := lnwire.NewSigFromECDSARawSignature(rawSig)
	require.NoError(t, err)

	alias, err := lnwire.NewNodeAlias("carol")
	require.NoError(t, err)

	ann1 := &lnwire.NodeAnnouncement1{
		Signature:       wireSig,
		Features:        lnwire.EmptyFeatureVector().RawFeatureVector,
		Timestamp:       uint32(1_000_000),
		NodeID:          pubBytes,
		RGBColor:        color.RGBA{R: 5, G: 6, B: 7},
		Alias:           alias,
		Addresses:       []net.Addr{&net.TCPAddr{IP: net.ParseIP("9.8.7.6"), Port: 9735}},
		ExtraOpaqueData: []byte{0xff},
	}

	node, err := NodeFromWireAnnouncement(ann1)
	require.NoError(t, err)
	require.Equal(t, lnwire.GossipVersion1, node.Version)
	require.EqualValues(t, pubBytes, node.PubKeyBytes)
	require.Equal(t, "carol", node.Alias.UnwrapOr(""))
	require.Equal(t, color.RGBA{R: 5, G: 6, B: 7}, node.Color.UnwrapOr(color.RGBA{}))
	require.Equal(t, rawSig, node.AuthSigBytes)
	require.True(t, node.HaveAnnouncement())

	// Round-trip back to wire should yield a v1 announcement.
	roundTripped, err := node.WireNodeAnnouncement(true)
	require.NoError(t, err)
	_, ok := roundTripped.(*lnwire.NodeAnnouncement1)
	require.True(t, ok, "round-trip should produce *NodeAnnouncement1")
}

// TestNodeFromWireAnnouncementV2 verifies a NodeAnnouncement2 is parsed into
// a v2 Node with all fields correctly mapped.
func TestNodeFromWireAnnouncementV2(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	var pubBytes [33]byte
	copy(pubBytes[:], key.PubKey().SerializeCompressed())

	rawSig := schnorrSigBytes(t, key, [32]byte{0x04})
	wireSig, err := lnwire.NewSigFromSchnorrRawSignature(rawSig)
	require.NoError(t, err)

	ann2 := &lnwire.NodeAnnouncement2{
		Features: tlv.NewRecordT[tlv.TlvType0](
			*lnwire.EmptyFeatureVector().RawFeatureVector,
		),
		BlockHeight: tlv.NewPrimitiveRecord[tlv.TlvType2](uint32(100)),
		NodeID:      tlv.NewPrimitiveRecord[tlv.TlvType4, [33]byte](pubBytes),
		Signature:   tlv.NewRecordT[tlv.TlvType160](wireSig),
	}

	node, err := NodeFromWireAnnouncement(ann2)
	require.NoError(t, err)
	require.Equal(t, lnwire.GossipVersion2, node.Version)
	require.EqualValues(t, pubBytes, node.PubKeyBytes)
	require.EqualValues(t, 100, node.LastBlockHeight)
	require.Equal(t, rawSig, node.AuthSigBytes)
	require.True(t, node.HaveAnnouncement())

	// Round-trip back to wire should yield a v2 announcement.
	roundTripped, err := node.WireNodeAnnouncement(true)
	require.NoError(t, err)
	_, ok := roundTripped.(*lnwire.NodeAnnouncement2)
	require.True(t, ok, "round-trip should produce *NodeAnnouncement2")
}

// TestNodeHaveAnnouncementAndPubKeyCache verifies HaveAnnouncement behaviour
// and that PubKey caches the parsed key across repeated calls.
func TestNodeHaveAnnouncementAndPubKeyCache(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	pub := pubVertex(key)

	shell := NewShellNode(lnwire.GossipVersion1, pub)
	require.False(t, shell.HaveAnnouncement())

	// First PubKey call parses bytes and caches the result.
	first, err := shell.PubKey()
	require.NoError(t, err)
	require.True(t, key.PubKey().IsEqual(first))

	// Second call returns the same cached pointer.
	second, err := shell.PubKey()
	require.NoError(t, err)
	require.Same(t, first, second)

	// After adding a signature, HaveAnnouncement returns true.
	shell.AuthSigBytes = []byte{0x01}
	require.True(t, shell.HaveAnnouncement())
}

// TestNodeAnnouncementUnsignedNoSigRequired verifies that unsigned
// announcements succeed even when no signature is stored, and that
// signed=true correctly fails in that case.
func TestNodeAnnouncementUnsignedNoSigRequired(t *testing.T) {
	t.Parallel()

	key := newTestKey(t)
	pub := pubVertex(key)

	// Shell node has no signature.
	node := NewShellNode(lnwire.GossipVersion1, pub)
	require.False(t, node.HaveAnnouncement())

	_, err := node.WireNodeAnnouncement(false)
	require.NoError(t, err)

	// Requesting signed=true must fail when no sig is stored.
	_, err = node.WireNodeAnnouncement(true)
	require.ErrorContains(t, err, "does not have node announcement")
}
