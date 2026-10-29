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

// newReplyPath returns a blinded path to carry in an onion message's
// reply_path record.
func newReplyPath(t *testing.T) *lnwire.BlindedPath {
	t.Helper()

	intro, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	blinding, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hop, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &lnwire.BlindedPath{
		IntroductionNode: lnwire.PubkeyIntro{Pubkey: intro.PubKey()},
		BlindingPoint:    blinding.PubKey(),
		Hops: []lnwire.BlindedHop{{
			BlindedNodeID: hop.PubKey(),
			EncryptedData: []byte{1, 2, 3},
		}},
	}
}

// encodePayload encodes an onion message payload, failing the test on error.
func encodePayload(t *testing.T, payload *lnwire.OnionMessagePayload) []byte {
	t.Helper()

	b, err := payload.Encode()
	require.NoError(t, err)

	return b
}

// TestForwardPayloadFields exercises the BOLT 4 payload rules for onion
// messages with blinded onions: a forwarding hop accepts only
// encrypted_recipient_data in its outer payload and no path_id in its
// decrypted route data, while the final hop accepts all of these records.
func TestForwardPayloadFields(t *testing.T) {
	tests := []struct {
		name string

		// final builds a path that ends at our node instead of one
		// that forwards to a next node.
		final bool

		// replyPath adds a reply_path record (type 2) to the outer
		// payload.
		replyPath bool

		// lowRecords are raw TLV records with types between 2 and 4,
		// placed before encrypted_recipient_data.
		lowRecords []byte

		// noEncData omits the encrypted_recipient_data record.
		noEncData bool

		// highRecords are raw TLV records with types above 4, placed
		// after encrypted_recipient_data.
		highRecords []byte

		// pathID, if set, is placed in the decrypted route data.
		pathID fn.Option[[]byte]

		// err is the error expected from processing the message, or
		// nil if it should be accepted.
		err error
	}{
		{
			name:       "unknown odd empty",
			lowRecords: []byte{3, 0},
			err:        ErrInvalidForwardPayload,
		},
		{
			name:       "unknown odd without encrypted data",
			lowRecords: []byte{3, 0},
			noEncData:  true,
			err:        ErrMissingEncryptedData,
		},
		{
			name:      "reply path",
			replyPath: true,
			err:       ErrInvalidForwardPayload,
		},
		{
			name:        "final payload empty",
			highRecords: []byte{65, 0},
			err:         ErrInvalidForwardPayload,
		},
		{
			name:   "path id nonempty",
			pathID: fn.Some([]byte{1}),
			err:    ErrForwardPathID,
		},
		{
			name:   "path id empty",
			pathID: fn.Some([]byte{}),
			err:    ErrForwardPathID,
		},
		{
			name: "valid",
		},
		{
			name:        "final hop accepts all records",
			final:       true,
			replyPath:   true,
			lowRecords:  []byte{3, 0},
			highRecords: []byte{65, 0},
			pathID:      fn.Some([]byte{1}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newActorHarness(t)

			// Build the route data for our hop, forwarding to a
			// fresh next node unless this is the final hop.
			rd := &record.BlindedRouteData{}
			hops := []*sphinx.HopInfo{}
			if !tc.final {
				next, err := btcec.NewPrivateKey()
				require.NoError(t, err)

				nextNode := fn.NewLeft[*btcec.PublicKey,
					lnwire.ShortChannelID](next.PubKey())

				//nolint:ll
				rd = record.NewNonFinalBlindedRouteDataOnionMessage(
					nextNode, nil, nil,
				)

				hops = append(hops, &sphinx.HopInfo{
					NodePub: next.PubKey(),
					PlainText: EncodeBlindedRouteData(
						t, &record.BlindedRouteData{},
					),
				})
			}
			tc.pathID.WhenSome(func(id []byte) {
				rd.PathID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						id,
					),
				)
			})
			hops = append([]*sphinx.HopInfo{{
				NodePub:   h.nodeKey.PubKey(),
				PlainText: EncodeBlindedRouteData(t, rd),
			}}, hops...)

			blinded := BuildBlindedPath(t, hops)
			path, err := route.OnionMessageBlindedPathToSphinxPath(
				blinded.Path, nil, nil,
			)
			require.NoError(t, err)

			// Assemble our hop's outer payload record by record,
			// in ascending type order.
			var payload []byte
			if tc.replyPath {
				payload = append(payload, encodePayload(
					t, &lnwire.OnionMessagePayload{
						ReplyPath: newReplyPath(t),
					},
				)...)
			}
			payload = append(payload, tc.lowRecords...)
			if !tc.noEncData {
				ciphertext := blinded.Path.BlindedHops[0].
					CipherText

				payload = append(payload, encodePayload(
					t, &lnwire.OnionMessagePayload{
						EncryptedData: ciphertext,
					},
				)...)
			}
			payload = append(payload, tc.highRecords...)

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

			// A rejected message must fail with the rule it
			// breaks, and must be neither forwarded nor
			// dispatched.
			if tc.err != nil {
				require.ErrorIs(t, result.Err(), tc.err)
				require.Empty(t, h.sender.sent)
				require.Empty(t, h.dispatcher.updates)

				return
			}

			require.NoError(t, result.Err())
			require.Len(t, h.dispatcher.updates, 1)

			// Only a forwarding hop sends the message on.
			if tc.final {
				require.Empty(t, h.sender.sent)
			} else {
				require.Len(t, h.sender.sent, 1)
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

		require.NoError(t, result.Err())
		require.Empty(t, h.sender.sent)
		require.Len(t, h.dispatcher.updates, 1)
	}
}
