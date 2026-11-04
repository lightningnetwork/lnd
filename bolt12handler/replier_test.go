package bolt12handler

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/stretchr/testify/require"
)

// TestServerOnionReplierPathChecks verifies that the replier refuses a reply
// path that it cannot send to. Otherwise it sends the invoice as a type-66
// payload to the introduction node.
func TestServerOnionReplierPathChecks(t *testing.T) {
	t.Parallel()

	introKey := testKey(t)
	hopKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pathKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	newPath := func() *sphinx.BlindedPath {
		return &sphinx.BlindedPath{
			IntroductionPoint: introKey.PubKey(),
			BlindingPoint:     pathKey.PubKey(),
			BlindedHops: []*sphinx.BlindedHopInfo{{
				BlindedNodePub: hopKey.PubKey(),
				CipherText:     []byte{1, 2, 3},
			}},
		}
	}

	noHops := newPath()
	noHops.BlindedHops = nil

	noIntroKey := newPath()
	noIntroKey.IntroductionPoint = nil

	invoiceBytes := []byte{0x01, 0x02}

	tests := []struct {
		name    string
		path    *sphinx.BlindedPath
		wantErr string
	}{
		{name: "nil path", wantErr: "no reply path"},
		{name: "no hops", path: noHops, wantErr: "no hops"},
		{
			name:    "no introduction node key",
			path:    noIntroKey,
			wantErr: "no introduction node key",
		},
		{name: "valid path", path: newPath()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sender := &mockOnionSender{}
			replier := NewServerOnionReplier(sender)

			err := replier.SendInvoiceReply(
				t.Context(), invoiceBytes, tc.path,
			)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Empty(t, sender.peers)

				return
			}

			require.NoError(t, err)
			require.Len(t, sender.peers, 1)

			var want [33]byte
			copy(want[:], introKey.PubKey().SerializeCompressed())
			require.Equal(t, want, sender.peers[0])

			// The path has no real blinding, so the hop key alone
			// peels the onion.
			msg := &lnwire.OnionMessage{
				OnionBlob: sender.onions[0],
			}
			hops := onionmessage.PeelOnionLayers(
				t, []*btcec.PrivateKey{hopKey}, msg,
			)
			require.Len(t, hops, 1)
			require.Equal(t, []*lnwire.FinalHopTLV{{
				TLVType: lnwire.InvoiceNamespaceType,
				Value:   invoiceBytes,
			}}, hops[0].Payload.FinalHopTLVs)
		})
	}
}

// mockOnionSender records the peer and the onion of each onion message it
// sends.
type mockOnionSender struct {
	peers  [][33]byte
	onions [][]byte
}

func (m *mockOnionSender) SendOnionMessage(_ context.Context,
	peerPub [33]byte, _ *btcec.PublicKey, onion []byte) error {

	m.peers = append(m.peers, peerPub)
	m.onions = append(m.onions, onion)

	return nil
}
