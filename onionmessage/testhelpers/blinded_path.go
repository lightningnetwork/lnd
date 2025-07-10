// Package testhelpers provides shared test utilities for onion message tests.
// These helpers are used by both unit tests in the onionmessage package and
// integration tests in the itest package.
package testhelpers

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// EncodeBlindedRouteData encodes BlindedRouteData to bytes for use in test
// hop payloads.
func EncodeBlindedRouteData(t *testing.T,
	data *record.BlindedRouteData) []byte {

	t.Helper()

	buf, err := record.EncodeBlindedRouteData(data)
	require.NoError(t, err)

	return buf
}

// BuildBlindedPath creates a BlindedPathInfo from a list of HopInfo. This is a
// test helper that wraps sphinx.BuildBlindedPath with a fresh session key.
func BuildBlindedPath(t *testing.T,
	hops []*sphinx.HopInfo) *sphinx.BlindedPathInfo {

	t.Helper()

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	blindedPath, err := sphinx.BuildBlindedPath(sessionKey, hops)
	require.NoError(t, err)

	return blindedPath
}

// ConcatBlindedPaths concatenates two blinded paths. The sender's path points
// TO the introduction node (with NextBlindingOverride), and the receiver's
// path starts AT the introduction node. The concatenated path includes all
// hops from both paths - the sender's last hop instructs forwarding to the
// intro node, and all receiver hops follow.
func ConcatBlindedPaths(t *testing.T, senderPath,
	receiverPath *sphinx.BlindedPathInfo) *sphinx.BlindedPathInfo {

	t.Helper()

	// The resulting path uses the sender's session key and introduction
	// point but concatenates all blinded hops.
	concatenated := &sphinx.BlindedPath{
		IntroductionPoint: senderPath.Path.IntroductionPoint,
		BlindingPoint:     senderPath.Path.BlindingPoint,
		BlindedHops: append(
			senderPath.Path.BlindedHops,
			receiverPath.Path.BlindedHops...,
		),
	}

	return &sphinx.BlindedPathInfo{
		Path:             concatenated,
		SessionKey:       senderPath.SessionKey,
		LastEphemeralKey: receiverPath.LastEphemeralKey,
	}
}

// BuildOnionMessage builds an onion message from a BlindedPathInfo and returns
// the message along with the ciphertexts for each blinded hop (in hop order).
// If finalPayloads is nil or empty, no final hop payload data is included.
func BuildOnionMessage(t *testing.T, blindedPath *sphinx.BlindedPathInfo,
	finalPayloads []*lnwire.FinalHopPayload) (*lnwire.OnionMessage,
	[][]byte) {

	t.Helper()

	// Convert the blinded path to a sphinx path and add final payloads.
	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		blindedPath.Path, nil, finalPayloads,
	)
	require.NoError(t, err)

	onionSessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create an onion packet with no associated data.
	onionPkt, err := sphinx.NewOnionPacket(
		sphinxPath, onionSessionKey, nil,
		sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	require.NoError(t, err)

	// Encode the onion message packet.
	var buf bytes.Buffer
	require.NoError(t, onionPkt.Encode(&buf))

	onionMsg := &lnwire.OnionMessage{
		PathKey:   blindedPath.SessionKey.PubKey(),
		OnionBlob: buf.Bytes(),
	}

	var ctexts [][]byte
	for _, bh := range blindedPath.Path.BlindedHops {
		ctexts = append(ctexts, bh.CipherText)
	}

	return onionMsg, ctexts
}
