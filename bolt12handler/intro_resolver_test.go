package bolt12handler

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// testSCID is the channel that testIntroResolver knows.
var testSCID = [8]byte{0, 0, 1, 0, 0, 2, 0, 3}

// errUnknownChannel is returned by fakeEdgeFetcher for an unknown channel.
var errUnknownChannel = errors.New("unknown channel")

// fakeEdgeFetcher is a channel graph that holds one channel.
type fakeEdgeFetcher struct {
	scid uint64
	edge *models.ChannelEdgeInfo
}

func (f *fakeEdgeFetcher) FetchChannelEdgesByID(_ context.Context,
	chanID uint64) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	if chanID != f.scid {
		return nil, nil, nil, errUnknownChannel
	}

	return f.edge, nil, nil, nil
}

// newFakeEdgeFetcher returns a graph with the channel testSCID between
// node1 and node2.
func newFakeEdgeFetcher(node1, node2 *btcec.PublicKey) *fakeEdgeFetcher {
	return &fakeEdgeFetcher{
		scid: lnwire.NewShortChanIDFromInt(
			0x0000010000020003,
		).ToUint64(),
		edge: &models.ChannelEdgeInfo{
			NodeKey1Bytes: route.NewVertex(node1),
			NodeKey2Bytes: route.NewVertex(node2),
		},
	}
}

// testIntroResolver returns a resolver that resolves direction 1 of testSCID
// to node.
func testIntroResolver(t *testing.T,
	node *btcec.PrivateKey) *GraphIntroResolver {

	t.Helper()

	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return NewGraphIntroResolver(
		newFakeEdgeFetcher(other.PubKey(), node.PubKey()),
	)
}

// TestGraphIntroResolver checks that the direction of a sciddir introduction
// node selects the node of the channel.
func TestGraphIntroResolver(t *testing.T) {
	t.Parallel()

	key1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	resolver := NewGraphIntroResolver(
		newFakeEdgeFetcher(key1.PubKey(), key2.PubKey()),
	)

	tests := []struct {
		name      string
		direction byte
		scid      [8]byte
		want      *btcec.PublicKey
		err       error
	}{
		{
			name:      "direction 0 is node 1",
			direction: 0,
			scid:      testSCID,
			want:      key1.PubKey(),
		},
		{
			name:      "direction 1 is node 2",
			direction: 1,
			scid:      testSCID,
			want:      key2.PubKey(),
		},
		{
			name:      "unknown channel",
			direction: 0,
			scid:      [8]byte{1},
			err:       errUnknownChannel,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			intro, err := lnwire.NewSciddirIntro(
				tc.direction, tc.scid,
			)
			require.NoError(t, err)

			got, err := resolver.ResolveSciddir(t.Context(), intro)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)

				return
			}

			require.NoError(t, err)
			require.True(t, tc.want.IsEqual(got))
		})
	}
}

// TestResolveIntroNode checks that resolveIntroNode replaces only a sciddir
// introduction node and keeps the caller's path.
func TestResolveIntroNode(t *testing.T) {
	t.Parallel()

	node := testKey(t)
	resolver := testIntroResolver(t, node)

	pubIntro, err := lnwire.NewPubkeyIntro(node.PubKey())
	require.NoError(t, err)
	sciddir, err := lnwire.NewSciddirIntro(1, testSCID)
	require.NoError(t, err)

	t.Run("pubkey intro", func(t *testing.T) {
		t.Parallel()

		path := &lnwire.BlindedPath{IntroductionNode: pubIntro}
		got, err := resolveIntroNode(t.Context(), path, resolver)
		require.NoError(t, err)
		require.Same(t, path, got)
	})

	t.Run("sciddir intro", func(t *testing.T) {
		t.Parallel()

		path := &lnwire.BlindedPath{IntroductionNode: sciddir}
		got, err := resolveIntroNode(t.Context(), path, resolver)
		require.NoError(t, err)
		require.Equal(t, pubIntro, got.IntroductionNode)
		require.Equal(t, sciddir, path.IntroductionNode)
	})
}
