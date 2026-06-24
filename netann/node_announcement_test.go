package netann_test

import (
	"bytes"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// TestNodeAnnSignatureWithLegacyV2OnionAddr signs a [v3, v2, ipv4]
// announcement, round-trips it through the wire codec, and verifies that
// the signature still validates. If Decode or NodeFromWireAnnouncement ever
// starts filtering v2, DataToSign no longer reproduces the signed bytes and
// this test catches the regression.
func TestNodeAnnSignatureWithLegacyV2OnionAddr(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var nodeID [33]byte
	copy(nodeID[:], privKey.PubKey().SerializeCompressed())

	v3 := &tor.OnionAddr{
		OnionService: "abcdefghijabcdefghijabcdefghij" +
			"abcdefghijabcdefghij234567.onion",
		Port: 9735,
	}
	v2 := &tor.OnionAddr{
		OnionService: "abcdefghijklmnop.onion",
		Port:         9735,
	}
	tcp := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735}
	addrs := []net.Addr{v3, v2, tcp}

	ann := &lnwire.NodeAnnouncement1{
		Features:  lnwire.NewRawFeatureVector(),
		Timestamp: 1700000000,
		NodeID:    nodeID,
		Addresses: addrs,
	}
	copy(ann.Alias[:], "regression-test")

	dataToSign, err := ann.DataToSign()
	require.NoError(t, err)

	rawSig := ecdsa.Sign(privKey, chainhash.DoubleHashB(dataToSign))
	ann.Signature, err = lnwire.NewSigFromSignature(rawSig)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, ann.Encode(&buf, 0))

	var decoded lnwire.NodeAnnouncement1
	require.NoError(t, decoded.Decode(&buf, 0))

	// All three addresses must survive Decode for DataToSign to
	// reproduce the signed bytes.
	require.Len(t, decoded.Addresses, 3)
	require.NoError(t, netann.ValidateNodeAnnSignature(&decoded))

	node := models.NodeFromWireAnnouncement(&decoded)
	require.Len(t, node.Addresses, 3)

	var sawV2, sawV3, sawTCP bool
	for _, addr := range node.Addresses {
		switch a := addr.(type) {
		case *tor.OnionAddr:
			switch len(a.OnionService) {
			case tor.V2Len:
				sawV2 = true
			case tor.V3Len:
				sawV3 = true
			}
		case *net.TCPAddr:
			sawTCP = true
		}
	}
	require.True(t, sawV2, "v2 onion address must be preserved on the Node")
	require.True(t, sawV3, "v3 onion address must be preserved on the Node")
	require.True(t, sawTCP, "ipv4 address must be preserved on the Node")
}
