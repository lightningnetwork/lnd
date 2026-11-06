package onionmessage

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// newTestRouter returns a started onion router for key.
func newTestRouter(t *testing.T, key *btcec.PrivateKey) *sphinx.Router {
	t.Helper()

	router := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: key}, sphinx.NewNoOpReplayLog(),
	)
	require.NoError(t, router.Start())
	t.Cleanup(router.Stop)

	return router
}

// forwardHop returns a hop of node that forwards to the next node.
func forwardHop(t *testing.T, node *btcec.PublicKey,
	next fn.Either[*btcec.PublicKey, lnwire.ShortChannelID],
	override *btcec.PublicKey) *sphinx.HopInfo {

	return &sphinx.HopInfo{
		NodePub: node,
		PlainText: EncodeBlindedRouteData(
			t, record.NewNonFinalBlindedRouteDataOnionMessage(
				next, override, nil,
			),
		),
	}
}

// finalHop returns the last hop of a path, at node.
func finalHop(t *testing.T, node *btcec.PublicKey) *sphinx.HopInfo {
	return &sphinx.HopInfo{
		NodePub: node,
		PlainText: EncodeBlindedRouteData(
			t, &record.BlindedRouteData{},
		),
	}
}

// requireDecryptable checks that the introduction node of path can decrypt
// its hop, which needs the right blinding point.
func requireDecryptable(t *testing.T, path *sphinx.BlindedPath,
	introKey *btcec.PrivateKey) {

	t.Helper()

	router := newTestRouter(t, introKey)
	cipherText := bytes.Clone(path.BlindedHops[0].CipherText)
	plainText, err := router.DecryptBlindedHopData(
		path.BlindingPoint, cipherText,
	)
	require.NoError(t, err)

	_, err = record.DecodeBlindedRouteData(bytes.NewReader(plainText))
	require.NoError(t, err)
}

// TestHopPeeler checks that the peeler removes the hops of this node at
// the start of a blinded path, and that the next node can use the result.
func TestHopPeeler(t *testing.T) {
	t.Parallel()

	ourKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ourPub, otherPub := ourKey.PubKey(), otherKey.PubKey()

	scid := lnwire.NewShortChanIDFromInt(42)
	resolver := newMockNodeIDResolver()
	resolver.addPeer(scid, otherPub)

	peeler := NewHopPeeler(
		newTestRouter(t, ourKey), resolver, ourPub,
	)

	toOther := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		otherPub,
	)
	toOtherByChannel := fn.NewRight[*btcec.PublicKey](scid)
	toUs := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](ourPub)

	t.Run("next node by node id", func(t *testing.T) {
		t.Parallel()

		path := BuildBlindedPath(t, []*sphinx.HopInfo{
			forwardHop(t, ourPub, toOther, nil),
			finalHop(t, otherPub),
		}).Path
		ownCipherText := bytes.Clone(path.BlindedHops[0].CipherText)

		got, err := peeler.PeelOwnHops(t.Context(), path)
		require.NoError(t, err)
		require.True(t, otherPub.IsEqual(got.IntroductionPoint))
		require.Equal(t, path.BlindedHops[1:], got.BlindedHops)
		requireDecryptable(t, got, otherKey)

		// The caller's path is unchanged.
		require.Equal(
			t, ownCipherText, path.BlindedHops[0].CipherText,
		)
	})

	t.Run("next node by channel", func(t *testing.T) {
		t.Parallel()

		path := BuildBlindedPath(t, []*sphinx.HopInfo{
			forwardHop(t, ourPub, toOtherByChannel, nil),
			finalHop(t, otherPub),
		}).Path

		got, err := peeler.PeelOwnHops(t.Context(), path)
		require.NoError(t, err)
		require.True(t, otherPub.IsEqual(got.IntroductionPoint))
		requireDecryptable(t, got, otherKey)
	})

	t.Run("two own hops", func(t *testing.T) {
		t.Parallel()

		path := BuildBlindedPath(t, []*sphinx.HopInfo{
			forwardHop(t, ourPub, toUs, nil),
			forwardHop(t, ourPub, toOther, nil),
			finalHop(t, otherPub),
		}).Path

		got, err := peeler.PeelOwnHops(t.Context(), path)
		require.NoError(t, err)
		require.True(t, otherPub.IsEqual(got.IntroductionPoint))
		require.Len(t, got.BlindedHops, 1)
		requireDecryptable(t, got, otherKey)
	})

	t.Run("blinding override", func(t *testing.T) {
		t.Parallel()

		// The own hop leads into a path that the other node built,
		// so the next blinding point is the override.
		otherPath := BuildBlindedPath(t, []*sphinx.HopInfo{
			finalHop(t, otherPub),
		})
		ownPath := BuildBlindedPath(t, []*sphinx.HopInfo{
			forwardHop(
				t, ourPub, toOther,
				otherPath.Path.BlindingPoint,
			),
		})
		path := ConcatBlindedPaths(t, ownPath, otherPath).Path

		got, err := peeler.PeelOwnHops(t.Context(), path)
		require.NoError(t, err)
		require.True(
			t, otherPath.Path.BlindingPoint.IsEqual(
				got.BlindingPoint,
			),
		)
		requireDecryptable(t, got, otherKey)
	})

	t.Run("other introduction node", func(t *testing.T) {
		t.Parallel()

		path := BuildBlindedPath(t, []*sphinx.HopInfo{
			finalHop(t, otherPub),
		}).Path

		got, err := peeler.PeelOwnHops(t.Context(), path)
		require.NoError(t, err)
		require.Same(t, path, got)
	})

	t.Run("path ends at this node", func(t *testing.T) {
		t.Parallel()

		path := BuildBlindedPath(t, []*sphinx.HopInfo{
			finalHop(t, ourPub),
		}).Path

		_, err := peeler.PeelOwnHops(t.Context(), path)
		require.ErrorIs(t, err, ErrPathEndsAtSelf)
	})
}
