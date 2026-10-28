package discovery

import (
	"context"
	"iter"
	"testing"
	"testing/synctest"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/actor"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// emptySeries is a ChannelGraphTimeSeries for a node with an empty graph.
type emptySeries struct{}

func (emptySeries) HighestChanID(context.Context,
	chainhash.Hash) (*lnwire.ShortChannelID, error) {

	return &lnwire.ShortChannelID{}, nil
}

func (emptySeries) UpdatesInHorizon(context.Context, time.Time,
	time.Time) iter.Seq2[lnwire.Message, error] {

	return func(func(lnwire.Message, error) bool) {}
}

func (emptySeries) FilterKnownChanIDs(_ chainhash.Hash,
	superSet []graphdb.ChannelUpdateInfo,
	_ func(graphdb.ChannelUpdateInfo) bool) ([]lnwire.ShortChannelID,
	error) {

	scids := make([]lnwire.ShortChannelID, len(superSet))
	for i, info := range superSet {
		scids[i] = info.ShortChannelID
	}

	return scids, nil
}

func (emptySeries) FilterChannelRange(chainhash.Hash, uint32, uint32,
	bool) ([]graphdb.BlockChannelRange, error) {

	return nil, nil
}

func (emptySeries) FetchChanAnns(chainhash.Hash,
	[]lnwire.ShortChannelID) ([]lnwire.Message, error) {

	return nil, nil
}

func (emptySeries) FetchChanUpdates(chainhash.Hash,
	lnwire.ShortChannelID) ([]*lnwire.ChannelUpdate1, error) {

	return nil, nil
}

// TestSyncManagerV2Adapter drives the actor based syncer through the
// interface the gossiper uses: a peer connects, the initial historical sync
// runs over the wire, the graph is marked synced, and the peer is promoted
// to an active syncer.
func TestSyncManagerV2Adapter(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		chain := chainhash.Hash{3}

		system := actor.NewActorSystem()
		t.Cleanup(func() { require.NoError(t, system.Shutdown()) })

		mgr, err := newSyncManagerV2(syncManagerV2Cfg{
			chainHash:              chain,
			chanSeries:             emptySeries{},
			bestHeight:             func() uint32 { return 1_000 },
			numActiveSyncers:       1,
			historicalSyncInterval: time.Hour,
			rotateInterval:         20 * time.Minute,
			system:                 system,
		})
		require.NoError(t, err)
		mgr.Start()
		t.Cleanup(mgr.Stop)

		priv, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		peer := &mockPeer{
			pk:       priv.PubKey(),
			sentMsgs: make(chan lnwire.Message, 16),
			quit:     make(chan struct{}),
		}
		pub := route.Vertex(peer.PubKey())

		require.NoError(t, mgr.InitSyncState(peer))
		synctest.Wait()

		// The first peer runs the initial historical sync, which asks
		// for the whole chain.
		query, ok := (<-peer.sentMsgs).(*lnwire.QueryChannelRange)
		require.True(t, ok)
		require.Equal(t, uint32(0), query.FirstBlockHeight)
		require.False(t, mgr.IsGraphSynced())

		// The peer answers with a single, empty, final reply.
		err = mgr.deliverQueryMsg(ctx, peer, &lnwire.ReplyChannelRange{
			ChainHash:        chain,
			FirstBlockHeight: 0,
			NumBlocks:        query.NumBlocks,
			Complete:         1,
			EncodingType:     lnwire.EncodingSortedPlain,
		})
		require.NoError(t, err)
		synctest.Wait()

		// The sync completed, so the graph is synced and the peer
		// fills the single active slot, which sends it a filter
		// asking for all new gossip.
		require.True(t, mgr.IsGraphSynced())

		syncType, ok := mgr.SyncTypeOf(pub)
		require.True(t, ok)
		require.Equal(t, ActiveSync, syncType)

		filter, ok := (<-peer.sentMsgs).(*lnwire.GossipTimestampRange)
		require.True(t, ok)
		require.NotZero(t, filter.TimestampRange)

		// Live gossip is offered to the peer's responder.
		covered := mgr.forwardBatch(ctx, nil)
		require.Contains(t, covered, pub)

		// A message from a peer we don't know is refused as before.
		other, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		stranger := &mockPeer{pk: other.PubKey()}
		err = mgr.deliverQueryMsg(ctx, stranger, &lnwire.QueryChannelRange{
			ChainHash: chain,
		})
		require.ErrorIs(t, err, ErrGossipSyncerNotFound)

		// After the peer disconnects, it has no sync type.
		mgr.PruneSyncState(pub)
		synctest.Wait()
		_, ok = mgr.SyncTypeOf(pub)
		require.False(t, ok)
	})
}

// TestSyncManagerV2ForwardCopiesSenders asserts that live gossip handed to
// the actor based syncer is forwarded even though the gossiper, as it does
// right after forwardBatch returns, adds every covered peer to the senders
// of each message. The responders filter asynchronously, so they must not
// see that later change.
func TestSyncManagerV2ForwardCopiesSenders(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		chain := chainhash.Hash{3}

		system := actor.NewActorSystem()
		t.Cleanup(func() { require.NoError(t, system.Shutdown()) })

		// With no active syncers, connecting a peer starts no
		// historical sync, so the only traffic is what we send.
		mgr, err := newSyncManagerV2(syncManagerV2Cfg{
			chainHash:              chain,
			chanSeries:             emptySeries{},
			bestHeight:             func() uint32 { return 1_000 },
			historicalSyncInterval: time.Hour,
			rotateInterval:         20 * time.Minute,
			system:                 system,
		})
		require.NoError(t, err)
		mgr.Start()
		t.Cleanup(mgr.Stop)

		priv, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		peer := &mockPeer{
			pk:       priv.PubKey(),
			sentMsgs: make(chan lnwire.Message, 16),
			quit:     make(chan struct{}),
		}
		pub := route.Vertex(peer.PubKey())
		require.NoError(t, mgr.InitSyncState(peer))

		// The peer asks for all gossip from now on.
		now := uint32(time.Now().Unix())
		err = mgr.deliverQueryMsg(ctx, peer, &lnwire.GossipTimestampRange{
			ChainHash:      chain,
			FirstTimestamp: now,
			TimestampRange: ^uint32(0),
		})
		require.NoError(t, err)
		synctest.Wait()

		// A node announcement from another peer arrives, and the
		// gossiper forwards it, then marks every covered peer as a
		// sender so it doesn't broadcast to them too.
		ann := &lnwire.NodeAnnouncement1{Timestamp: now + 1}
		batch := []msgWithSenders{{
			msg:     ann,
			senders: map[route.Vertex]struct{}{{9}: {}},
		}}
		covered := mgr.forwardBatch(ctx, batch)
		batch[0].mergeSyncerMap(covered)
		synctest.Wait()

		select {
		case msg := <-peer.sentMsgs:
			require.Equal(t, ann, msg)
		default:
			t.Fatal("node announcement was not forwarded")
		}
		require.Contains(t, covered, pub)
	})
}
