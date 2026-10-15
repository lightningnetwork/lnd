package onionmessage

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/aliasmgr"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
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

// TestRemotePubFromSCIDAnnouncedOrAlias checks BOLT 4 next-hop resolution.
// An announced edge resolves from the graph. An edge with no auth proof, and
// a missing edge, resolve only when the SCID is one of our local aliases.
func TestRemotePubFromSCIDAnnouncedOrAlias(t *testing.T) {
	t.Parallel()

	ourPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ourPub := ourPriv.PubKey()

	peerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerPub := peerPriv.PubKey()

	alias := func(txIndex uint32) lnwire.ShortChannelID {
		return lnwire.ShortChannelID{
			BlockHeight: aliasmgr.AliasStartBlockHeight,
			TxIndex:     txIndex,
		}
	}
	announced := lnwire.NewShortChanIDFromInt(42)
	unannounced := lnwire.NewShortChanIDFromInt(43)
	zeroConfAlias := alias(1)
	localAlias := alias(2)
	unknownAlias := alias(3)

	graph, err := graphdb.NewChannelGraph(graphdb.NewTestDB(t))
	require.NoError(t, err)
	require.NoError(t, graph.Start())
	t.Cleanup(func() {
		require.NoError(t, graph.Stop())
	})

	// Our own unannounced channels are in the graph without an auth
	// proof. An unconfirmed zero-conf channel is there under its alias.
	ourVertex := route.NewVertex(ourPub)
	peerVertex := route.NewVertex(peerPub)
	addEdge := func(scid lnwire.ShortChannelID, announce bool) {
		var opts []models.EdgeModifier
		if announce {
			opts = append(opts, models.WithChanProof(
				models.NewV1ChannelAuthProof(
					[]byte{1}, []byte{2}, []byte{3},
					[]byte{4},
				),
			))
		}
		edge, err := models.NewV1Channel(
			scid.ToUint64(), chainhash.Hash{}, ourVertex,
			peerVertex, &models.ChannelV1Fields{
				BitcoinKey1Bytes: ourVertex,
				BitcoinKey2Bytes: peerVertex,
			}, opts...,
		)
		require.NoError(t, err)
		require.NoError(t, graph.AddChannelEdge(t.Context(), edge))
	}
	addEdge(announced, true)
	addEdge(unannounced, false)
	addEdge(zeroConfAlias, false)

	// The switch knows the links of both alias cases.
	links := map[lnwire.ShortChannelID]*btcec.PublicKey{
		zeroConfAlias: peerPub,
		localAlias:    peerPub,
	}

	tests := []struct {
		name      string
		scid      lnwire.ShortChannelID
		found     bool
		askSwitch bool
	}{
		{
			name:  "announced channel",
			scid:  announced,
			found: true,
		},
		{
			name: "unannounced channel confirmed scid",
			scid: unannounced,
		},
		{
			name: "confirmed scid not in graph",
			scid: lnwire.NewShortChanIDFromInt(44),
		},
		{
			name:      "unconfirmed zero-conf alias in graph",
			scid:      zeroConfAlias,
			found:     true,
			askSwitch: true,
		},
		{
			name:      "local alias not in graph",
			scid:      localAlias,
			found:     true,
			askSwitch: true,
		},
		{
			name:      "unknown local alias",
			scid:      unknownAlias,
			askSwitch: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var askedSwitch bool
			resolver := NewGraphNodeResolver(
				graph, ourPub,
				func(scid lnwire.ShortChannelID) (
					*btcec.PublicKey, bool) {

					askedSwitch = true
					pub, ok := links[scid]

					return pub, ok
				},
			)

			got, err := resolver.RemotePubFromSCID(
				t.Context(), test.scid,
			)
			require.Equal(t, test.askSwitch, askedSwitch)
			if !test.found {
				require.ErrorIs(t, err, ErrSCIDNotResolved)
				require.Nil(t, got)

				return
			}

			require.NoError(t, err)
			require.True(t, got.IsEqual(peerPub))

			// A hit is cached. The second call must not ask again.
			askedSwitch = false
			got, err = resolver.RemotePubFromSCID(
				t.Context(), test.scid,
			)
			require.NoError(t, err)
			require.True(t, got.IsEqual(peerPub))
			require.False(t, askedSwitch)
		})
	}
}

// TestRemotePubFromSCIDGraphErrorSkipsPrivate checks that a graph error other
// than ErrEdgeNotFound is returned without the private-channel lookup.
func TestRemotePubFromSCIDGraphErrorSkipsPrivate(t *testing.T) {
	t.Parallel()

	ourPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	peerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	graph, err := graphdb.NewChannelGraph(graphdb.NewTestDB(t))
	require.NoError(t, err)

	scid := lnwire.NewShortChanIDFromInt(42)
	var ourKey, peerKey [33]byte
	copy(ourKey[:], ourPriv.PubKey().SerializeCompressed())
	copy(peerKey[:], peerPriv.PubKey().SerializeCompressed())
	require.NoError(t, graph.MarkEdgeZombie(
		t.Context(), lnwire.GossipVersion1, scid.ToUint64(), ourKey,
		peerKey,
	))

	var called bool
	resolver := NewGraphNodeResolver(
		graph, ourPriv.PubKey(),
		func(lnwire.ShortChannelID) (*btcec.PublicKey, bool) {
			called = true
			return peerPriv.PubKey(), true
		},
	)

	_, err = resolver.RemotePubFromSCID(t.Context(), scid)
	require.ErrorIs(t, err, graphdb.ErrZombieEdge)
	require.False(t, called)
}
