package netann_test

import (
	"bytes"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// TestNodeAnnSignatureWithLegacyV2OnionAddr is a regression test for a
// signature-verification failure on inbound NodeAnnouncement1 messages that
// carry a legacy v2 onion address alongside still-valid entries.
//
// The wire codec must stay wire-faithful so DataToSign() reproduces the exact
// bytes the remote peer signed. Any pre-validation filtering of Addresses
// (e.g. dropping v2 in Decode) mutates a signed field and causes
// ValidateNodeAnnSignature to reject otherwise valid announcements. v2 is
// filtered later, when the message is converted to a models.Node.
func TestNodeAnnSignatureWithLegacyV2OnionAddr(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var nodeID [33]byte
	copy(nodeID[:], privKey.PubKey().SerializeCompressed())

	v3 := &tor.OnionAddr{
		// 56 base32 chars + ".onion" = 62 chars = tor.V3Len.
		OnionService: "abcdefghijabcdefghijabcdefghij" +
			"abcdefghijabcdefghij234567.onion",
		Port: 9735,
	}
	v2 := &tor.OnionAddr{
		// 16 base32 chars + ".onion" = 22 chars = tor.V2Len.
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

	// Round-trip through the wire codec to mirror what a peer would
	// hand the gossiper.
	var buf bytes.Buffer
	require.NoError(t, ann.Encode(&buf, 0))

	var decoded lnwire.NodeAnnouncement1
	require.NoError(t, decoded.Decode(&buf, 0))

	// All three addresses must survive Decode so the signed bytes can
	// be reproduced; stripping v2 here would flip the next call into
	// an invalid-signature error.
	require.Len(t, decoded.Addresses, 3)
	require.NoError(t, netann.ValidateNodeAnnSignature(&decoded))

	// Conversion to a models.Node preserves v2 so the announcement can
	// be re-broadcast after a restart with the same signed bytes.
	// Consumption-side filtering (dial logic, RPC display) is where
	// v2 is dropped, not at storage entry.
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
