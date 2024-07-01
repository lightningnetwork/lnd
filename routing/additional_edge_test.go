package routing

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestIntermediatePayloadSize tests the payload size functions of the
// PrivateEdge and the BlindedEdge.
func TestIntermediatePayloadSize(t *testing.T) {
	t.Parallel()

	testPrivKeyBytes, _ := hex.DecodeString("e126f68f7eafcc8b74f54d269fe" +
		"206be715000f94dac067d1c04a8ca3b2db734")
	_, blindedPoint := btcec.PrivKeyFromBytes(testPrivKeyBytes)

	testCases := []struct {
		name    string
		hop     route.Hop
		nextHop uint64
		edge    AdditionalEdge
	}{
		{
			name: "Tlv payload private edge",
			hop: route.Hop{
				AmtToForward:     1000,
				OutgoingTimeLock: 600000,
				ChannelID:        3432483437438,
			},
			nextHop: 1,
			edge:    &PrivateEdge{},
		},
		{
			name: "Blinded edge",
			hop: route.Hop{
				EncryptedData: []byte{12, 13},
			},
			edge: &BlindedEdge{blindedPayment: &BlindedPayment{
				BlindedPath: &sphinx.BlindedPath{
					BlindedHops: []*sphinx.BlindedHopInfo{
						{CipherText: []byte{12, 13}},
					},
				},
			}},
		},
		{
			name: "Blinded edge - introduction point",
			hop: route.Hop{
				EncryptedData: []byte{12, 13},
				BlindingPoint: blindedPoint,
			},
			edge: &BlindedEdge{blindedPayment: &BlindedPayment{
				BlindedPath: &sphinx.BlindedPath{
					BlindingPoint: blindedPoint,
					BlindedHops: []*sphinx.BlindedHopInfo{
						{CipherText: []byte{12, 13}},
					},
				},
			}},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			payLoad, err := createHopPayload(
				testCase.hop, testCase.nextHop, false,
			)
			require.NoErrorf(t, err, "failed to create hop payload")

			expectedPayloadSize := testCase.edge.
				IntermediatePayloadSize(
					testCase.hop.AmtToForward,
					testCase.hop.OutgoingTimeLock,
					testCase.nextHop,
				)

			require.Equal(
				t, expectedPayloadSize,
				uint64(payLoad.NumBytes()),
			)
		})
	}
}

// createHopPayload creates the hop payload of the sphinx package to facilitate
// the testing of the payload size.
func createHopPayload(hop route.Hop, nextHop uint64,
	finalHop bool) (sphinx.HopPayload, error) {

	// If this is the legacy payload, then we can just include the
	// hop data as normal.
	if hop.LegacyPayload {
		// Before we encode this value, we'll pack the next hop
		// into the NextAddress field of the hop info to ensure
		// we point to the right now.
		hopData := sphinx.HopData{
			ForwardAmount: uint64(hop.AmtToForward),
			OutgoingCltv:  hop.OutgoingTimeLock,
		}
		binary.BigEndian.PutUint64(
			hopData.NextAddress[:], nextHop,
		)

		return sphinx.NewLegacyHopPayload(&hopData)
	}

	// For non-legacy payloads, we'll need to pack the
	// routing information, along with any extra TLV
	// information into the new per-hop payload format.
	// We'll also pass in the chan ID of the hop this
	// channel should be forwarded to so we can construct a
	// valid payload.
	var b bytes.Buffer
	err := hop.PackHopPayload(&b, nextHop, finalHop)
	if err != nil {
		return sphinx.HopPayload{}, err
	}

	return sphinx.NewTLVHopPayload(b.Bytes())
}
