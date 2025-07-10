package onionmessage

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// mockNodeIDResolver implements NodeIDResolver for tests.
type mockNodeIDResolver struct {
	peers map[lnwire.ShortChannelID]*btcec.PublicKey
}

// addPeer registers a single SCID to pubkey mapping for tests.
func (m *mockNodeIDResolver) addPeer(scid lnwire.ShortChannelID,
	pubKey *btcec.PublicKey) {

	m.peers[scid] = pubKey
}

// newMockNodeIDResolver creates a new instance of mockNodeIDResolver.
func newMockNodeIDResolver() *mockNodeIDResolver {
	return &mockNodeIDResolver{
		peers: make(map[lnwire.ShortChannelID]*btcec.PublicKey),
	}
}

// RemotePubFromSCID resolves a node public key from a short channel ID.
func (m *mockNodeIDResolver) RemotePubFromSCID(
	scid lnwire.ShortChannelID) (*btcec.PublicKey, error) {

	if pk, ok := m.peers[scid]; ok {
		return pk, nil
	}

	return nil, fmt.Errorf("unknown scid: %v", scid)
}

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
	finalHopTLVs []*lnwire.FinalHopTLV) (*lnwire.OnionMessage,
	[][]byte) {

	t.Helper()

	// Convert the blinded path to a sphinx path and add final payloads.
	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		blindedPath.Path, nil, finalHopTLVs,
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

// PeeledHop captures decrypted state for a single hop when peeling an onion.
type PeeledHop struct {
	EncryptedData []byte
	Payload       *lnwire.OnionMessagePayload
	IsFinal       bool
}

// PeelOnionLayers sequentially processes an onion message, creating a fresh
// router for each hop using the provided private keys (one per hop), returning
// the encrypted data and decoded payload for each hop until the final hop.
func PeelOnionLayers(t *testing.T, privKeys []*btcec.PrivateKey,
	msg *lnwire.OnionMessage) []PeeledHop {

	t.Helper()

	var onionPkt sphinx.OnionPacket
	require.NoError(t, onionPkt.Decode(bytes.NewReader(msg.OnionBlob)))

	currentPathKey := msg.PathKey
	var hops []PeeledHop

	for i := 0; ; i++ {
		require.Less(t, i, len(privKeys), "more hops than privKeys")

		router := sphinx.NewRouter(
			&sphinx.PrivKeyECDH{PrivKey: privKeys[i]},
			sphinx.NewMemoryReplayLog(),
		)
		require.NoError(t, router.Start())

		processedPkt, err := router.ProcessOnionPacket(
			&onionPkt, nil, 10,
			sphinx.WithBlindingPoint(currentPathKey),
		)
		require.NoError(t, err)

		payload := lnwire.NewOnionMessagePayload()
		_, err = payload.Decode(
			bytes.NewReader(processedPkt.Payload.Payload),
		)
		require.NoError(t, err)

		origPayload := *payload
		origPayload.EncryptedData = bytes.Clone(payload.EncryptedData)

		isFinal := processedPkt.Action == sphinx.ExitNode
		hops = append(hops, PeeledHop{
			EncryptedData: origPayload.EncryptedData,
			Payload:       &origPayload,
			IsFinal:       isFinal,
		})

		if isFinal {
			router.Stop()
			break
		}

		decrypted, err := router.DecryptBlindedHopData(
			currentPathKey, payload.EncryptedData,
		)
		require.NoError(t, err)

		routeData, err := record.DecodeBlindedRouteData(
			bytes.NewReader(decrypted),
		)
		require.NoError(t, err)

		nextPathKey := deriveNextPathKeyForTest(
			router, currentPathKey, routeData.NextBlindingOverride,
		)
		require.NotNil(t, nextPathKey)

		router.Stop()

		onionPkt = *processedPkt.NextPacket
		currentPathKey = nextPathKey
	}

	return hops
}

// deriveNextPathKeyForTest derives the next path key using the router and
// current path key. If an override is provided, it is used instead.
func deriveNextPathKeyForTest(router *sphinx.Router,
	currentPathKey *btcec.PublicKey,
	override tlv.OptionalRecordT[tlv.TlvType8,
		*btcec.PublicKey]) *btcec.PublicKey {

	// If an override is provided, use it.
	return override.UnwrapOrFunc(func() tlv.RecordT[tlv.TlvType8,
		*btcec.PublicKey] {

		// Otherwise, derive the next path key using the router.
		nextKey, err := router.NextEphemeral(currentPathKey)
		if err != nil {
			// If the derivation fails, return a zero key.
			return override.Zero()
		}

		return tlv.NewPrimitiveRecord[tlv.TlvType8](nextKey)
	}).Val
}
