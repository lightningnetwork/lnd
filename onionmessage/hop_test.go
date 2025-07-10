package onionmessage

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

func TestProcessOnionMessage(t *testing.T) {
	// Setup the local node (router).
	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey := nodeKey.PubKey()

	router := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: nodeKey},
		sphinx.NewMemoryReplayLog(),
	)
	router.Start()
	defer router.Stop()

	t.Run("Forward Action Success", func(t *testing.T) {
		// 1. Create the BlindedRouteData for this hop.
		// Since we are forwarding, we must specify the NextNodeID.
		nodeKeyB, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		pubKeyB := nodeKeyB.PubKey()
		nextNode := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
			pubKeyB,
		)

		routeDataA := record.NewNonFinalBlindedRouteDataOnionMessage(
			nextNode, nil, nil,
		)
		encodedRouteDataA, err := record.EncodeBlindedRouteData(
			routeDataA,
		)
		require.NoError(t, err)

		routeDataB := &record.BlindedRouteData{}

		encodedRouteDataB, err := record.EncodeBlindedRouteData(
			routeDataB,
		)
		require.NoError(t, err)

		// Create a set of 2 blinded hops for our path.
		hopsToBlind := make([]*sphinx.HopInfo, 2)

		// The first hop.
		hopsToBlind[0] = &sphinx.HopInfo{
			NodePub:   pubKey,
			PlainText: encodedRouteDataA,
		}

		// The second hop.
		hopsToBlind[1] = &sphinx.HopInfo{
			NodePub:   pubKeyB,
			PlainText: encodedRouteDataB,
		}

		// 2. Build blinded path.

		// Create a session key for the blinded path. This is used to
		// create the path keys.
		sessionKeyA, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		blindedPath, err := sphinx.BuildBlindedPath(
			sessionKeyA, hopsToBlind,
		)
		require.NoError(t, err)

		// 3. Create the final hop payload
		finalHopPayload := &lnwire.FinalHopPayload{
			TLVType: lnwire.InvoiceRequestNamespaceType,
			Value:   []byte{1, 2, 3},
		}

		// 4. Convert that blinded path to a sphinx path and add a final
		// payload.
		sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
			blindedPath.Path, nil, []*lnwire.FinalHopPayload{
				finalHopPayload,
			},
		)
		require.NoError(t, err)

		// 5. Create the Onion Packet.

		// Create a session key for the onion packet encryption.
		sessionKeyB, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		onionPkt, err := sphinx.NewOnionPacket(
			sphinxPath, sessionKeyB, nil,
			sphinx.DeterministicPacketFiller,
			sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
		)
		require.NoError(t, err, "new onion packet")

		// Encode the onion packet to bytes.
		var onionBlob bytes.Buffer
		err = onionPkt.Encode(&onionBlob)
		require.NoError(t, err)

		// 6. Construct the OnionMessage.
		msg := &lnwire.OnionMessage{
			PathKey:   sessionKeyA.PubKey(),
			OnionBlob: onionBlob.Bytes(),
		}

		// 7. Process the message.
		result := processOnionMessage(router, msg)
		require.True(t, result.IsOk())

		expectedCypherText := blindedPath.Path.BlindedHops[0].CipherText

		result.WhenOk(func(action routingAction) {
			// Should be forwardAction
			require.True(t, action.IsLeft())
			action.WhenLeft(func(fwdAction forwardAction) {
				require.Equal(t, pubKeyB, fwdAction.nextNodeID)
				require.NotNil(t, fwdAction.nextPathKey)
				require.NotEmpty(t, fwdAction.nextPacket)
				require.NoError(t, err)
				require.Equal(
					t,
					expectedCypherText,
					fwdAction.payload.EncryptedData,
				)
			})
		})
	})

	t.Run("Deliver Action Success", func(t *testing.T) {
		// 1. Create the BlindedRouteData for this hop.
		// For Delivery (final node) empty route data is fine.
		routeData := &record.BlindedRouteData{}

		encodedRouteData, err := record.EncodeBlindedRouteData(
			routeData,
		)
		require.NoError(t, err)

		// Create a set of 1 blinded hop for our path.
		hopsToBlind := make([]*sphinx.HopInfo, 1)

		// The first hop.
		hopsToBlind[0] = &sphinx.HopInfo{
			NodePub:   pubKey,
			PlainText: encodedRouteData,
		}

		// 2. Build blinded path.

		// Create a session key for the blinded path. This is used to
		// create the path keys.
		sessionKeyA, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		blindedPath, err := sphinx.BuildBlindedPath(
			sessionKeyA, hopsToBlind,
		)
		require.NoError(t, err)

		// 3. Create the final hop payload
		finalHopPayload := &lnwire.FinalHopPayload{
			TLVType: lnwire.InvoiceRequestNamespaceType,
			Value:   []byte{1, 2, 3},
		}

		// 4. Convert that blinded path to a sphinx path and add a final
		// payload.
		sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
			blindedPath.Path, nil, []*lnwire.FinalHopPayload{
				finalHopPayload,
			},
		)
		require.NoError(t, err)

		// 5. Create the Onion Packet.

		// Create a session key for the onion packet encryption.
		sessionKeyB, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		onionPkt, err := sphinx.NewOnionPacket(
			sphinxPath, sessionKeyB, nil,
			sphinx.DeterministicPacketFiller,
			sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
		)
		require.NoError(t, err, "new onion packet")

		// Encode the onion packet to bytes.
		var onionBlob bytes.Buffer
		err = onionPkt.Encode(&onionBlob)
		require.NoError(t, err)

		// 6. Construct the OnionMessage.
		msg := &lnwire.OnionMessage{
			PathKey:   sessionKeyA.PubKey(),
			OnionBlob: onionBlob.Bytes(),
		}

		// 7. Process the message.
		result := processOnionMessage(router, msg)
		require.True(t, result.IsOk())

		expectedCypherText := blindedPath.Path.BlindedHops[0].CipherText

		result.WhenOk(func(action routingAction) {
			// Should be deliverAction
			require.True(t, action.IsRight())
			action.WhenRight(func(dlvrAction deliverAction) {
				require.Equal(
					t,
					expectedCypherText,
					dlvrAction.payload.EncryptedData,
				)
			})
		})
	})
}

// TestIsForwarding tests the isForwarding function.
func TestIsForwarding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		packet   *sphinx.ProcessedPacket
		expected bool
	}{
		{
			name: "forwarding",
			packet: &sphinx.ProcessedPacket{
				Action: sphinx.MoreHops,
			},
			expected: true,
		},
		{
			name: "delivery",
			packet: &sphinx.ProcessedPacket{
				Action: sphinx.ExitNode,
			},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			result := isForwarding(test.packet)
			require.Equal(t, test.expected, result)
		})
	}
}

// TestDeriveNextPathKey tests the deriveNextPathKey function.
func TestDeriveNextPathKey(t *testing.T) {
	t.Parallel()

	// create a private key for the router.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// create a path key.
	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pathKey := sessionKey.PubKey()

	// Create a router. We don't need a replay log for this test as
	// NextEphemeral doesn't use it.
	router := sphinx.NewRouter(&sphinx.PrivKeyECDH{PrivKey: privKey}, nil)

	t.Run("override present", func(t *testing.T) {
		overrideKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		override := tlv.NewPrimitiveRecord[tlv.TlvType8](
			overrideKey.PubKey(),
		)
		optOverride := tlv.SomeRecordT(override)

		// Router can be nil as it shouldn't be used.
		result := deriveNextPathKey(nil, pathKey, optOverride)
		require.Equal(t, overrideKey.PubKey(), result)
	})

	t.Run("derive success", func(t *testing.T) {
		override := tlv.OptionalRecordT[tlv.TlvType8, *btcec.PublicKey]{}

		result := deriveNextPathKey(router, pathKey, override)
		require.NotNil(t, result)

		// Verify it matches manual derivation.
		expected, err := router.NextEphemeral(pathKey)
		require.NoError(t, err)
		require.Equal(t, expected, result)
	})

	// It's currently impossible to test derivation failure as there is no
	// way to make the key derivation fail with an error. You can only make
	// it panick by passing in a nil path key. This is due to how
	// PrivKeyECDH.ECDH is implemented in the keychain package.
}
