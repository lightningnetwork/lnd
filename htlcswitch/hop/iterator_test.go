package hop

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestSphinxHopIteratorForwardingInstructions tests that we're able to
// properly decode an onion payload, no matter the payload type, into the
// original set of forwarding instructions.
func TestSphinxHopIteratorForwardingInstructions(t *testing.T) {
	t.Parallel()

	// First, we'll make the hop data that the sender would create to send
	// an HTLC through our imaginary route.
	hopData := sphinx.HopData{
		ForwardAmount: 100000,
		OutgoingCltv:  4343,
	}
	copy(hopData.NextAddress[:], bytes.Repeat([]byte("a"), 8))

	// Next, we'll make the hop forwarding information that we should
	// extract each type, no matter the payload type.
	nextAddrInt := binary.BigEndian.Uint64(hopData.NextAddress[:])
	expectedFwdInfo := ForwardingInfo{
		NextHop: NewChannelNextHop(
			lnwire.NewShortChanIDFromInt(nextAddrInt),
		),
		AmountToForward: lnwire.MilliSatoshi(hopData.ForwardAmount),
		OutgoingCLTV:    hopData.OutgoingCltv,
	}

	// For our TLV payload, we'll serialize the hop into into a TLV stream
	// as we would normally in the routing network.
	var b bytes.Buffer
	tlvRecords := []tlv.Record{
		record.NewAmtToFwdRecord(&hopData.ForwardAmount),
		record.NewLockTimeRecord(&hopData.OutgoingCltv),
		record.NewNextHopIDRecord(&nextAddrInt),
	}
	tlvStream, err := tlv.NewStream(tlvRecords...)
	require.NoError(t, err, "unable to create stream")
	if err := tlvStream.Encode(&b); err != nil {
		t.Fatalf("unable to encode stream: %v", err)
	}

	var testCases = []struct {
		sphinxPacket    *sphinx.ProcessedPacket
		expectedFwdInfo ForwardingInfo
	}{
		// A regular legacy payload that signals more hops.
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type: sphinx.PayloadLegacy,
				},
				Action:                 sphinx.MoreHops,
				ForwardingInstructions: &hopData,
			},
			expectedFwdInfo: expectedFwdInfo,
		},
		// A TLV payload, which includes the sphinx action as
		// cid may be zero for blinded routes (thus we require the
		// action to signal whether we are at the final hop).
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type:    sphinx.PayloadTLV,
					Payload: b.Bytes(),
				},
				Action: sphinx.MoreHops,
			},
			expectedFwdInfo: expectedFwdInfo,
		},
	}

	// Finally, we'll test that we get the same set of
	// ForwardingInstructions for each payload type.
	iterator := sphinxHopIterator{}
	for i, testCase := range testCases {
		iterator.processedPacket = testCase.sphinxPacket

		pld, _, pldErr := iterator.HopPayload()
		if pldErr != nil {
			t.Fatalf("#%v: unable to extract forwarding "+
				"instructions: %v", i, pldErr)
		}

		fwdInfo := pld.ForwardingInfo()
		if fwdInfo != testCase.expectedFwdInfo {
			t.Fatalf("#%v: wrong fwding info: expected %v, got %v",
				i, spew.Sdump(testCase.expectedFwdInfo),
				spew.Sdump(fwdInfo))
		}
	}
}

// TestForwardingAmountCalc tests calculation of forwarding amounts from the
// hop's forwarding parameters.
func TestForwardingAmountCalc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		incomingAmount lnwire.MilliSatoshi
		baseFee        lnwire.MilliSatoshi
		proportional   uint32
		forwardAmount  lnwire.MilliSatoshi
		expectErr      bool
	}{
		{
			name:           "overflow",
			incomingAmount: 10,
			baseFee:        100,
			expectErr:      true,
		},
		{
			name:           "trivial proportional",
			incomingAmount: 100_000,
			baseFee:        1000,
			proportional:   10,
			forwardAmount:  99000,
		},
		{
			name:           "both fees charged",
			incomingAmount: 10_002_020,
			baseFee:        1000,
			proportional:   1,
			forwardAmount:  10_001_010,
		},
	}

	for _, testCase := range tests {

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			actual, err := calculateForwardingAmount(
				testCase.incomingAmount, testCase.baseFee,
				testCase.proportional,
			)

			require.Equal(t, testCase.expectErr, err != nil)
			require.Equal(t, testCase.forwardAmount.ToSatoshis(),
				actual.ToSatoshis())
		})
	}
}

// mockProcessor is a mocked blinding point processor that just returns the
// data that it is called with when "decrypting".
type mockProcessor struct {
	decryptErr error
}

// DecryptBlindedHopData mocks blob decryption, returning the same data that
// it was called with and an optionally configured error.
func (m *mockProcessor) DecryptBlindedHopData(_ *btcec.PublicKey,
	data []byte) ([]byte, error) {

	return data, m.decryptErr
}

// NextEphemeral mocks getting our next ephemeral key.
func (m *mockProcessor) NextEphemeral(*btcec.PublicKey) (*btcec.PublicKey,
	error) {

	return nil, nil
}

// TestParseAndValidateRecipientData tests deriving forwarding info using a
// blinding kit. This test does not cover assertions on the calculations of
// forwarding information, because this is covered in a test dedicated to those
// calculations.
func TestParseAndValidateRecipientData(t *testing.T) {
	t.Parallel()

	// Encode valid blinding data that we'll fake decrypting for our test.
	maxCltv := 1000
	blindedData := record.NewNonFinalBlindedRouteData(
		lnwire.NewShortChanIDFromInt(1500), nil,
		record.PaymentRelayInfo{
			CltvExpiryDelta: 10,
			BaseFee:         100,
			FeeRate:         0,
		},
		&record.PaymentConstraints{
			MaxCltvExpiry:   1000,
			HtlcMinimumMsat: lnwire.MilliSatoshi(1),
		},
		nil,
	)

	validData, err := record.EncodeBlindedRouteData(blindedData)
	require.NoError(t, err)

	// Mocked error.
	errDecryptFailed := errors.New("could not decrypt")

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	tests := []struct {
		name              string
		data              []byte
		incomingCLTV      uint32
		updateAddBlinding *btcec.PublicKey
		payloadBlinding   *btcec.PublicKey
		processor         *mockProcessor
		expectedErr       error
	}{
		{
			name:        "no blinding point",
			data:        validData,
			processor:   &mockProcessor{},
			expectedErr: ErrNoBlindingPoint,
		},
		{
			name:              "decryption failed",
			data:              validData,
			updateAddBlinding: &btcec.PublicKey{},
			incomingCLTV:      500,
			processor: &mockProcessor{
				decryptErr: errDecryptFailed,
			},
			expectedErr: errDecryptFailed,
		},
		{
			name:              "decode fails",
			data:              []byte{1, 2, 3},
			updateAddBlinding: &btcec.PublicKey{},
			incomingCLTV:      500,
			processor:         &mockProcessor{},
			expectedErr:       ErrDecodeFailed,
		},
		{
			name:              "validation fails",
			data:              validData,
			updateAddBlinding: &btcec.PublicKey{},
			incomingCLTV:      uint32(maxCltv) + 10,
			processor:         &mockProcessor{},
			expectedErr: ErrInvalidPayload{
				Type:      record.LockTimeOnionType,
				Violation: InsufficientViolation,
			},
		},
		{
			name:              "valid using update add",
			updateAddBlinding: &btcec.PublicKey{},
			data:              validData,
			processor:         &mockProcessor{},
			expectedErr:       nil,
		},
		{
			name:            "valid using payload",
			payloadBlinding: &btcec.PublicKey{},
			data:            validData,
			processor:       &mockProcessor{},
			expectedErr:     nil,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// We don't actually use blinding keys due to our
			// mocking so they can be nil.
			kit := BlindingKit{
				Processor:      testCase.processor,
				IncomingAmount: 10000,
				IncomingCltv:   testCase.incomingCLTV,
			}

			if testCase.updateAddBlinding != nil {
				kit.UpdateAddBlinding = tlv.SomeRecordT(
					//nolint:ll
					tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](testCase.updateAddBlinding),
				)
			}
			iterator := &sphinxHopIterator{
				blindingKit: kit,
				router: sphinx.NewRouter(
					&sphinx.PrivKeyECDH{PrivKey: nodeKey},
					sphinx.NewMemoryReplayLog(),
				),
			}

			_, _, err = parseAndValidateRecipientData(
				iterator, &Payload{
					encryptedData: testCase.data,
					blindingPoint: testCase.payloadBlinding,
				}, false, RouteRoleCleartext,
			)
			require.ErrorIs(t, err, testCase.expectedErr)
		})
	}
}

// TestDeriveBlindedRouteNextHop asserts how a non-final blinded hop's next hop
// is derived from the recipient data: a short channel ID becomes a Left, a
// next_node_id becomes a Right, having both set is rejected with an error, and
// the absence of both is also an error.
func TestDeriveBlindedRouteNextHop(t *testing.T) {
	t.Parallel()

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	var nextNodeRaw [33]byte
	copy(nextNodeRaw[:], nextNodePub.SerializeCompressed())

	scid := lnwire.NewShortChanIDFromInt(1500)

	relayInfo := tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType10](
		record.PaymentRelayInfo{
			CltvExpiryDelta: 10,
			BaseFee:         100,
			FeeRate:         0,
		},
	))
	constraints := tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType12](
		record.PaymentConstraints{
			MaxCltvExpiry:   1000,
			HtlcMinimumMsat: lnwire.MilliSatoshi(1),
		},
	))
	scidRecord := tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType2](scid))
	nodeIDRecord := tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType4](nextNodePub),
	)

	tests := []struct {
		name        string
		data        *record.BlindedRouteData
		expectedHop fn.Either[lnwire.ShortChannelID, [33]byte]
		expectedErr string
	}{
		{
			name: "short channel id only",
			data: &record.BlindedRouteData{
				ShortChannelID: scidRecord,
				RelayInfo:      relayInfo,
				Constraints:    constraints,
			},
			expectedHop: NewChannelNextHop(scid),
		},
		{
			name: "next node id only",
			data: &record.BlindedRouteData{
				NextNodeID:  nodeIDRecord,
				RelayInfo:   relayInfo,
				Constraints: constraints,
			},
			expectedHop: NewNodeNextHop(nextNodeRaw),
		},
		{
			// BOLT 4 requires a non-final blinded hop to set
			// exactly one of short_channel_id or next_node_id, so
			// setting both must be rejected.
			name: "both present is an error",
			data: &record.BlindedRouteData{
				ShortChannelID: scidRecord,
				NextNodeID:     nodeIDRecord,
				RelayInfo:      relayInfo,
				Constraints:    constraints,
			},
			expectedErr: "both short channel ID and next node ID",
		},
		{
			name: "neither present",
			data: &record.BlindedRouteData{
				RelayInfo:   relayInfo,
				Constraints: constraints,
			},
			expectedErr: "next hop not set",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			data, err := record.EncodeBlindedRouteData(
				testCase.data,
			)
			require.NoError(t, err)

			kit := BlindingKit{
				Processor:      &mockProcessor{},
				IncomingAmount: 10000,
				IncomingCltv:   500,
				UpdateAddBlinding: tlv.SomeRecordT(
					//nolint:ll
					tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](&btcec.PublicKey{}),
				),
			}
			iterator := &sphinxHopIterator{
				blindingKit: kit,
				router: sphinx.NewRouter(
					&sphinx.PrivKeyECDH{PrivKey: nodeKey},
					sphinx.NewMemoryReplayLog(),
				),
			}

			payload, _, err := parseAndValidateRecipientData(
				iterator, &Payload{encryptedData: data},
				false, RouteRoleCleartext,
			)

			if testCase.expectedErr != "" {
				require.ErrorContains(
					t, err, testCase.expectedErr,
				)

				return
			}

			require.NoError(t, err)
			require.Equal(
				t, testCase.expectedHop,
				payload.FwdInfo.NextHop,
			)
		})
	}
}

// TestBlindedHopBothNextHopFieldsRejected asserts that a blinded hop setting
// both short_channel_id and next_node_id is rejected for a final hop and for a
// dummy hop (next_node_id == our own pubkey), not just an intermediate hop. The
// mutual-exclusivity check runs before the final-hop and dummy-hop branches, so
// none of them accept a hop that violates BOLT 4. The intermediate case is
// already covered by TestDeriveBlindedRouteNextHop.
func TestBlindedHopBothNextHopFieldsRejected(t *testing.T) {
	t.Parallel()

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nodePub := nodeKey.PubKey()

	// Route data that sets both short_channel_id and next_node_id. The node
	// ID is our own pubkey, which for a non-final hop would otherwise
	// signal a dummy hop; the both-set check must still fire first.
	bothData := &record.BlindedRouteData{
		ShortChannelID: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](
				lnwire.NewShortChanIDFromInt(1500),
			),
		),
		NextNodeID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType4](nodePub),
		),
		RelayInfo: tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType10](
			record.PaymentRelayInfo{
				CltvExpiryDelta: 10,
				BaseFee:         100,
				FeeRate:         0,
			},
		)),
		Constraints: tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType12](
			record.PaymentConstraints{
				MaxCltvExpiry:   1000,
				HtlcMinimumMsat: lnwire.MilliSatoshi(1),
			},
		)),
	}
	data, err := record.EncodeBlindedRouteData(bothData)
	require.NoError(t, err)

	// Both the dummy/forwarding path (isFinal=false, next_node_id points at
	// us) and the final path (isFinal=true) must reject the hop.
	for _, isFinal := range []bool{false, true} {
		name := "forwarding hop"
		if isFinal {
			name = "final hop"
		}

		t.Run(name, func(t *testing.T) {
			kit := BlindingKit{
				Processor:      &mockProcessor{},
				IncomingAmount: 10000,
				IncomingCltv:   500,
				UpdateAddBlinding: tlv.SomeRecordT(
					//nolint:ll
					tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](&btcec.PublicKey{}),
				),
			}
			iterator := &sphinxHopIterator{
				blindingKit: kit,
				router: sphinx.NewRouter(
					&sphinx.PrivKeyECDH{PrivKey: nodeKey},
					sphinx.NewMemoryReplayLog(),
				),
			}

			_, _, err := parseAndValidateRecipientData(
				iterator, &Payload{encryptedData: data},
				isFinal, RouteRoleCleartext,
			)
			require.ErrorContains(
				t, err,
				"both short channel ID and next node ID",
			)
		})
	}
}

// TestBlindedRouteDummyHopPeeledLocally asserts that a blinded route hop where
// next_node_id is our own public key is recognized as a dummy hop and is peeled
// locally rather than falling through to the generic next_node_id forwarding
// branch.
func TestBlindedRouteDummyHopPeeledLocally(t *testing.T) {
	t.Parallel()

	// Construct a realistic onion packet that contains a blinded final hop.
	// We'll use this to test that we can peel a dummy hop locally and
	// extract the forwarding information from the decrypted final hop's
	// payload.
	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nodePub := nodeKey.PubKey()

	relayInfo := tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType10](
		record.PaymentRelayInfo{
			CltvExpiryDelta: 10,
			BaseFee:         100,
			FeeRate:         0,
		},
	))
	constraints := tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType12](
		record.PaymentConstraints{
			MaxCltvExpiry:   1000,
			HtlcMinimumMsat: lnwire.MilliSatoshi(1),
		},
	))

	// Set next_node_id to our own public key. This signals a dummy hop.
	nodeIDRecord := tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType4](nodePub),
	)

	// We'll generate a valid, cryptographically blinded final hop's payload
	// using sphinx.BuildBlindedPath. This contains the PathID.
	secret := make([]byte, 32)
	secret[0] = 2
	finalHopData := &record.BlindedRouteData{
		PathID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType6](secret),
		),
	}
	finalHopDataBytes, err := record.EncodeBlindedRouteData(finalHopData)
	require.NoError(t, err)

	hopInfo := &sphinx.HopInfo{
		NodePub:   nodePub,
		PlainText: finalHopDataBytes,
	}

	blindingSessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	blindedPathInfo, err := sphinx.BuildBlindedPath(
		blindingSessionKey, []*sphinx.HopInfo{hopInfo},
	)
	require.NoError(t, err)

	// Since we are peeling a dummy hop locally, we want the next blinding
	// override to be the blinding point generated for our blinded final
	// hop.
	dummyHopData := &record.BlindedRouteData{
		NextNodeID:  nodeIDRecord,
		RelayInfo:   relayInfo,
		Constraints: constraints,
		NextBlindingOverride: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType8](
				blindedPathInfo.Path.BlindingPoint,
			),
		),
	}

	data, err := record.EncodeBlindedRouteData(dummyHopData)
	require.NoError(t, err)

	// Encode a valid TLV payload for the next hop (which we will peel).
	var hop2Buffer bytes.Buffer
	amt := uint64(10000)
	cltv := uint32(500)
	encryptedDataRecord := record.NewEncryptedDataRecord(
		&blindedPathInfo.Path.BlindedHops[0].CipherText,
	)
	tlvRecords := []tlv.Record{
		record.NewAmtToFwdRecord(&amt),
		record.NewLockTimeRecord(&cltv),
		encryptedDataRecord,
	}
	tlvStream, err := tlv.NewStream(tlvRecords...)
	require.NoError(t, err)
	err = tlvStream.Encode(&hop2Buffer)
	require.NoError(t, err)

	hopPayload, err := sphinx.NewTLVHopPayload(hop2Buffer.Bytes())
	require.NoError(t, err)

	// Create a valid 1-hop onion path using our blinded public key.
	var paymentPath sphinx.PaymentPath
	paymentPath[0] = sphinx.OnionHop{
		NodePub:    *blindedPathInfo.Path.BlindedHops[0].BlindedNodePub,
		HopPayload: hopPayload,
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rHash := [32]byte{1}

	// Generate a cryptographically valid onion packet for this path.
	onionPacket, err := sphinx.NewOnionPacket(
		&paymentPath, sessionKey, rHash[:],
		sphinx.DeterministicPacketFiller,
	)
	require.NoError(t, err)

	// Simulate an incoming HTLC with a blinding point and a valid onion
	// packet. The blinding point is used to decrypt the dummy hop's
	// payload, which contains the blinding point for the next hop (the
	// blinded final hop).
	kit := BlindingKit{
		Processor:      &mockProcessor{},
		IncomingAmount: 12000,
		IncomingCltv:   510,
		UpdateAddBlinding: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
				nodePub,
			),
		),
	}

	iterator := &sphinxHopIterator{
		blindingKit: kit,
		rHash:       rHash[:],
		router: sphinx.NewRouter(
			&sphinx.PrivKeyECDH{PrivKey: nodeKey},
			sphinx.NewMemoryReplayLog(),
		),
		// Set our valid onion packet to be peeled.
		processedPacket: &sphinx.ProcessedPacket{
			NextPacket: onionPacket,
		},
	}

	// When we parse and validate the recipient data, it should enter the
	// dummy-hop peeling path. Since our onion packet is valid and matches
	// our private key, it should be successfully peeled and parsed.
	pld, _, err := parseAndValidateRecipientData(
		iterator, &Payload{encryptedData: data},
		false, RouteRoleCleartext,
	)

	// Assert that we successfully peeled the dummy hop and extracted the
	// decrypted final payload.
	require.NoError(t, err)
	require.NotNil(t, pld)

	fwdInfo := pld.ForwardingInfo()
	require.Equal(t, lnwire.MilliSatoshi(0), fwdInfo.AmountToForward)
	require.Equal(t, uint32(0), fwdInfo.OutgoingCLTV)
	require.NotNil(t, fwdInfo.PathID)
	require.Equal(t, secret, fwdInfo.PathID[:])
}
