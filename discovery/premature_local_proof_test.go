package discovery

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

// TestPrematureLocalProofKept checks that our own half of a channel
// announcement proof isn't lost when the gossiper accepts it while it's still
// premature. The funding manager never sends that half again.
func TestPrematureLocalProofKept(t *testing.T) {
	t.Parallel()

	// "control" loses nothing: the cached half is replayed once it
	// matures, and the channel is announced.
	for _, loss := range []string{"control", "restart", "eviction"} {
		t.Run(loss, func(t *testing.T) {
			t.Parallel()
			testPrematureLocalProof(t, loss)
		})
	}
}

func testPrematureLocalProof(t *testing.T, loss string) {
	ctx := t.Context()

	const startHeight = 100
	tCtx, err := createTestCtx(t, startHeight, false)
	require.NoError(t, err, "can't create context")

	// The channel's SCID is one block ahead of the gossiper's best height,
	// so our half is premature.
	batch, err := tCtx.createLocalAnnouncements(startHeight + 1)
	require.NoError(t, err, "can't generate announcements")

	require.NoError(t, mustProcess(
		t, tCtx.gossiper.ProcessLocalAnnouncement(batch.chanAnn),
	))

	// The funding manager hands the gossiper our half, and the gossiper
	// reports success.
	require.NoError(t, mustProcess(
		t, tCtx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn),
	))

	gossiper, notifier := tCtx.gossiper, tCtx.notifier
	switch loss {
	// Restart the gossiper with the same graph and stores.
	case "restart":
		require.NoError(t, tCtx.gossiper.Stop())

		notifier = newMockNotifier()
		cfg := *tCtx.gossiper.cfg
		cfg.Notifier = notifier
		cfg.RetransmitTicker = ticker.NewForce(retransmitDelay)
		cfg.RotateTicker = ticker.NewForce(
			DefaultSyncerRotationInterval,
		)
		cfg.HistoricalSyncTicker = ticker.NewForce(
			DefaultHistoricalSyncInterval,
		)
		gossiper = New(cfg, &keychain.KeyDescriptor{
			PubKey:     tCtx.gossiper.selfKey,
			KeyLocator: tCtx.gossiper.selfKeyLoc,
		})
		require.NoError(t, gossiper.Start())
		t.Cleanup(func() {
			require.NoError(t, gossiper.Stop())
		})
		gossiper.syncMgr.markGraphSynced()

	// An unrelated peer sends unsigned announcement_signatures for a
	// far-future block until our half is evicted from the cache.
	case "eviction":
		attackerKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		attacker := &mockPeer{
			attackerKey.PubKey(), nil, nil, atomic.Bool{},
		}
		junk := &lnwire.AnnounceSignatures1{
			ShortChannelID: lnwire.ShortChannelID{
				BlockHeight: 1_000_000,
			},
		}
		for range maxFutureMessages {
			require.NoError(t, mustProcess(
				t, gossiper.ProcessRemoteAnnouncement(
					ctx, junk, attacker,
				),
			))
		}
	}

	// The proof matures, and the remote half arrives.
	notifier.notifyBlock(chainhash.Hash{}, startHeight+1)

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err)
	remotePeer := &mockPeer{remoteKey, nil, nil, atomic.Bool{}}
	require.NoError(t, mustProcess(t, gossiper.ProcessRemoteAnnouncement(
		ctx, batch.remoteProofAnn, remotePeer,
	)))

	// With both halves, the channel is announced.
	timeout := time.After(2 * time.Second)
	for {
		select {
		case msg := <-tCtx.broadcastedMessage:
			_, ok := msg.msg.(*lnwire.ChannelAnnouncement1)
			if ok {
				return
			}

		case <-timeout:
			t.Fatal("channel not announced: our premature half " +
				"was lost")
		}
	}
}

// TestLocalProofSkipsFutureCache asserts that a premature local half-proof is
// processed immediately rather than being stored in the evictable, in-memory
// futureMsgs cache.
func TestLocalProofSkipsFutureCache(t *testing.T) {
	t.Parallel()

	const startHeight = 100
	tCtx, err := createTestCtx(t, startHeight, false)
	require.NoError(t, err)

	batch, err := tCtx.createLocalAnnouncements(startHeight + 1)
	require.NoError(t, err)

	require.NoError(t, mustProcess(
		t, tCtx.gossiper.ProcessLocalAnnouncement(batch.chanAnn),
	))
	require.NoError(t, mustProcess(
		t, tCtx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn),
	))

	// The local half must not be sitting in the future message cache: it is
	// durably stored by the normal proof pipeline instead.
	require.Zero(t, tCtx.gossiper.futureMsgs.Len())
}
