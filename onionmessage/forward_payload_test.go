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

// TestForwardPayloadFields exercises forwarding restrictions with blinded
// onions.
func TestForwardPayloadFields(t *testing.T) {
	tests := []struct {
		name      string
		outer     []byte
		pathID    []byte
		hasPathID bool
		reject    bool
	}{
		{name: "unknown odd empty", outer: []byte{3, 0}, reject: true},
		{
			name: "final payload empty", outer: []byte{65, 0},
			reject: true,
		},
		{
			name: "path id nonempty", pathID: []byte{1},
			hasPathID: true, reject: true,
		},
		{
			name: "path id empty", pathID: []byte{},
			hasPathID: true, reject: true,
		},
		{name: "valid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newActorHarness(t)
			next, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			nextNode := fn.NewLeft[*btcec.PublicKey,
				lnwire.ShortChannelID](next.PubKey())
			rd := record.NewNonFinalBlindedRouteDataOnionMessage(
				nextNode, nil, nil,
			)
			if tc.hasPathID {
				rd.PathID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						tc.pathID,
					),
				)
			}
			hops := []*sphinx.HopInfo{
				{
					NodePub: h.nodeKey.PubKey(),
					PlainText: EncodeBlindedRouteData(
						t, rd,
					),
				},
				{
					NodePub: next.PubKey(),
					PlainText: EncodeBlindedRouteData(
						t, &record.BlindedRouteData{},
					),
				},
			}
			blinded := BuildBlindedPath(t, hops)
			path, err := route.OnionMessageBlindedPathToSphinxPath(
				blinded.Path, nil, nil,
			)
			require.NoError(t, err)
			ciphertext := blinded.Path.BlindedHops[0].CipherText
			onionPayload := &lnwire.OnionMessagePayload{
				EncryptedData: ciphertext,
			}
			payload, err := onionPayload.Encode()
			require.NoError(t, err)
			if len(tc.outer) > 0 && tc.outer[0] < 4 {
				payload = append(
					bytes.Clone(tc.outer), payload...,
				)
			} else {
				payload = append(payload, tc.outer...)
			}
			path[0].HopPayload, err = sphinx.NewTLVHopPayload(
				payload,
			)
			require.NoError(t, err)
			session, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			packet, err := sphinx.NewOnionPacket(
				path, session, nil,
				sphinx.DeterministicPacketFiller,
				sphinx.WithMaxPayloadSize(
					sphinx.MaxRoutingPayloadSize,
				),
			)
			require.NoError(t, err)
			var buf bytes.Buffer
			require.NoError(t, packet.Encode(&buf))
			msg := lnwire.OnionMessage{
				PathKey:   blinded.SessionKey.PubKey(),
				OnionBlob: buf.Bytes(),
			}
			result := h.actor.Receive(
				t.Context(), &Request{msg: msg},
			)
			if tc.reject {
				require.True(t, result.IsErr())
				require.Empty(t, h.sender.sent)
				require.Empty(t, h.dispatcher.updates)
			} else {
				require.True(t, result.IsOk())
				require.Len(t, h.sender.sent, 1)
				require.Len(t, h.dispatcher.updates, 1)
			}
		})
	}
}

// TestFinalPathID verifies that path IDs remain valid at the final hop.
func TestFinalPathID(t *testing.T) {
	for _, id := range [][]byte{{}, {1, 2}} {
		h := newActorHarness(t)
		rd := &record.BlindedRouteData{PathID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType6](id),
		)}
		hops := []*sphinx.HopInfo{{
			NodePub:   h.nodeKey.PubKey(),
			PlainText: EncodeBlindedRouteData(t, rd),
		}}
		msg, _ := BuildOnionMessage(t, BuildBlindedPath(t, hops),
			[]*lnwire.FinalHopTLV{{TLVType: 65, Value: []byte{1}}})
		result := h.actor.Receive(t.Context(), &Request{msg: *msg})
		require.True(t, result.IsOk())
		require.Empty(t, h.sender.sent)
		require.Len(t, h.dispatcher.updates, 1)
	}
}
