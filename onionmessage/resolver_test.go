package onionmessage

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestMockNodeIDResolverRemotePubFromSCID(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		resolver := newMockNodeIDResolver()
		priv, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		pubKey := priv.PubKey()

		scid := lnwire.NewShortChanIDFromInt(1)
		resolver.addPeer(scid, pubKey)

		got, err := resolver.RemotePubFromSCID(t.Context(), scid)
		require.NoError(t, err)
		require.Equal(t, pubKey, got)
	})

	t.Run("unknown scid", func(t *testing.T) {
		t.Parallel()

		resolver := newMockNodeIDResolver()
		scid := lnwire.NewShortChanIDFromInt(2)

		got, err := resolver.RemotePubFromSCID(t.Context(), scid)
		require.Error(t, err)
		require.Nil(t, got)
	})
}

// TestRemotePubFromSCIDResolvesPrivateChannel covers a next hop that is only
// a private-channel SCID. The graph does not have it. The local lookup does.
func TestRemotePubFromSCIDResolvesPrivateChannel(t *testing.T) {
	t.Parallel()

	ourPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	peerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerPub := peerPriv.PubKey()

	realScid := lnwire.NewShortChanIDFromInt(42)
	aliasScid := lnwire.NewShortChanIDFromInt(1 << 55)
	private := map[lnwire.ShortChannelID]*btcec.PublicKey{
		realScid:  peerPub,
		aliasScid: peerPub,
	}

	// Seed one unrelated edge so a missing SCID is ErrEdgeNotFound, which
	// is what a private channel produces on a populated graph.
	graphStore := graphdb.NewTestDB(t)
	graph, err := graphdb.NewChannelGraph(graphStore)
	require.NoError(t, err)

	resolver := NewGraphNodeResolver(
		graph, ourPriv.PubKey(),
		func(scid lnwire.ShortChannelID) (*btcec.PublicKey, bool) {
			pub, ok := private[scid]
			return pub, ok
		},
	)

	for _, scid := range []lnwire.ShortChannelID{realScid, aliasScid} {
		got, err := resolver.RemotePubFromSCID(t.Context(), scid)
		require.NoError(t, err)
		require.True(t, got.IsEqual(peerPub))
	}

	_, err = resolver.RemotePubFromSCID(
		t.Context(), lnwire.NewShortChanIDFromInt(99),
	)
	require.Error(t, err)
}
