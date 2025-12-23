package testhelpers

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

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
		payload, _, err = payload.Decode(
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

		nextPathKey := deriveNextPathKey(
			router, currentPathKey, routeData.NextBlindingOverride,
		)
		require.NotNil(t, nextPathKey)

		router.Stop()

		onionPkt = *processedPkt.NextPacket
		currentPathKey = nextPathKey
	}

	return hops
}

// deriveNextPathKey derives the next path key using the router and current
// path key. If an override is provided, it is used instead.
func deriveNextPathKey(router *sphinx.Router, currentPathKey *btcec.PublicKey,
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
