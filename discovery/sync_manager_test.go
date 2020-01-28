package discovery

import (
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
)

// randPeer creates a random peer.
func randPeer(t *testing.T, quit chan struct{}) *mockPeer {
	t.Helper()

	return &mockPeer{
		pk:       randPubKey(t),
		sentMsgs: make(chan lnwire.Message),
		quit:     quit,
	}
}

// newTestSyncManager creates a new test SyncManager using mock implementations
// of its dependencies.
func newTestSyncManager(numActiveSyncers int) *SyncManager {
	hID := lnwire.ShortChannelID{BlockHeight: latestKnownHeight}
	return newSyncManager(&SyncManagerCfg{
		ChanSeries:           newMockChannelGraphTimeSeries(hID),
		RotateTicker:         ticker.NewForce(DefaultSyncerRotationInterval),
		HistoricalSyncTicker: ticker.NewForce(DefaultHistoricalSyncInterval),
		NumActiveSyncers:     numActiveSyncers,
	})
}

// TestSyncManagerNumActiveSyncers ensures that we are unable to have more than
// NumActiveSyncers active syncers.
func TestSyncManagerNumActiveSyncers(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test sync manager which will hold up to
	// 3 active syncers.
	const numActiveSyncers = 3
	const numSyncers = numActiveSyncers + 1

	syncMgr := newTestSyncManager(numActiveSyncers)
	syncMgr.Start()
	defer syncMgr.Stop()

	// We'll go ahead and create our syncers. We'll gather the ones which
	// should be active and passive to check them later on.
	for i := 0; i < numActiveSyncers; i++ {
		peer := randPeer(t, syncMgr.quit)
		syncMgr.InitSyncState(peer)
		s := assertSyncerExistence(t, syncMgr, peer)

		// The first syncer registered always attempts a historical
		// sync.
		if i == 0 {
			assertTransitionToChansSynced(t, s, peer)
		}
		assertActiveGossipTimestampRange(t, peer)
		assertSyncerStatus(t, s, chansSynced, ActiveSync)
	}

	for i := 0; i < numSyncers-numActiveSyncers; i++ {
		peer := randPeer(t, syncMgr.quit)
		syncMgr.InitSyncState(peer)
		s := assertSyncerExistence(t, syncMgr, peer)
		assertSyncerStatus(t, s, chansSynced, PassiveSync)
	}
}

// TestSyncManagerNewActiveSyncerAfterDisconnect ensures that we can regain an
// active syncer after losing one due to the peer disconnecting.
func TestSyncManagerNewActiveSyncerAfterDisconnect(t *testing.T) {
	t.Parallel()

	// We'll create our test sync manager to have two active syncers.
	syncMgr := newTestSyncManager(2)
	syncMgr.Start()
	defer syncMgr.Stop()

	// The first will be an active syncer that performs a historical sync
	// since it is the first one registered with the SyncManager.
	historicalSyncPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(historicalSyncPeer)
	historicalSyncer := assertSyncerExistence(t, syncMgr, historicalSyncPeer)
	assertTransitionToChansSynced(t, historicalSyncer, historicalSyncPeer)
	assertActiveGossipTimestampRange(t, historicalSyncPeer)
	assertSyncerStatus(t, historicalSyncer, chansSynced, ActiveSync)

	// Then, we'll create the second active syncer, which is the one we'll
	// disconnect.
	activeSyncPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(activeSyncPeer)
	activeSyncer := assertSyncerExistence(t, syncMgr, activeSyncPeer)
	assertActiveGossipTimestampRange(t, activeSyncPeer)
	assertSyncerStatus(t, activeSyncer, chansSynced, ActiveSync)

	// It will then be torn down to simulate a disconnection. Since there
	// are no other candidate syncers available, the active syncer won't be
	// replaced.
	syncMgr.PruneSyncState(activeSyncPeer.PubKey())

	// Then, we'll start our active syncer again, but this time we'll also
	// have a passive syncer available to replace the active syncer after
	// the peer disconnects.
	syncMgr.InitSyncState(activeSyncPeer)
	activeSyncer = assertSyncerExistence(t, syncMgr, activeSyncPeer)
	assertActiveGossipTimestampRange(t, activeSyncPeer)
	assertSyncerStatus(t, activeSyncer, chansSynced, ActiveSync)

	// Create our second peer, which should be initialized as a passive
	// syncer.
	newActiveSyncPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(newActiveSyncPeer)
	newActiveSyncer := assertSyncerExistence(t, syncMgr, newActiveSyncPeer)
	assertSyncerStatus(t, newActiveSyncer, chansSynced, PassiveSync)

	// Disconnect our active syncer, which should trigger the SyncManager to
	// replace it with our passive syncer.
	go syncMgr.PruneSyncState(activeSyncPeer.PubKey())
	assertPassiveSyncerTransition(t, newActiveSyncer, newActiveSyncPeer)
}

// TestSyncManagerRotateActiveSyncerCandidate tests that we can successfully
// rotate our active syncers after a certain interval.
func TestSyncManagerRotateActiveSyncerCandidate(t *testing.T) {
	t.Parallel()

	// We'll create our sync manager with three active syncers.
	syncMgr := newTestSyncManager(1)
	syncMgr.Start()
	defer syncMgr.Stop()

	// The first syncer registered always performs a historical sync.
	activeSyncPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(activeSyncPeer)
	activeSyncer := assertSyncerExistence(t, syncMgr, activeSyncPeer)
	assertTransitionToChansSynced(t, activeSyncer, activeSyncPeer)
	assertActiveGossipTimestampRange(t, activeSyncPeer)
	assertSyncerStatus(t, activeSyncer, chansSynced, ActiveSync)

	// We'll send a tick to force a rotation. Since there aren't any
	// candidates, none of the active syncers will be rotated.
	syncMgr.cfg.RotateTicker.(*ticker.Force).Force <- time.Time{}
	assertNoMsgSent(t, activeSyncPeer)
	assertSyncerStatus(t, activeSyncer, chansSynced, ActiveSync)

	// We'll then go ahead and add a passive syncer.
	passiveSyncPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(passiveSyncPeer)
	passiveSyncer := assertSyncerExistence(t, syncMgr, passiveSyncPeer)
	assertSyncerStatus(t, passiveSyncer, chansSynced, PassiveSync)

	// We'll force another rotation - this time, since we have a passive
	// syncer available, they should be rotated.
	syncMgr.cfg.RotateTicker.(*ticker.Force).Force <- time.Time{}

	// The transition from an active syncer to a passive syncer causes the
	// peer to send out a new GossipTimestampRange in the past so that they
	// don't receive new graph updates.
	assertActiveSyncerTransition(t, activeSyncer, activeSyncPeer)

	// The transition from a passive syncer to an active syncer causes the
	// peer to send a new GossipTimestampRange with the current timestamp to
	// signal that they would like to receive new graph updates from their
	// peers. This will also cause the gossip syncer to redo its state
	// machine, starting from its initial syncingChans state. We'll then
	// need to transition it to its final chansSynced state to ensure the
	// next syncer is properly started in the round-robin.
	assertPassiveSyncerTransition(t, passiveSyncer, passiveSyncPeer)
}

// TestSyncManagerInitialHistoricalSync ensures that we only attempt a single
// historical sync during the SyncManager's startup. If the peer corresponding
// to the initial historical syncer disconnects, we should attempt to find a
// replacement.
func TestSyncManagerInitialHistoricalSync(t *testing.T) {
	t.Parallel()

	syncMgr := newTestSyncManager(0)

	// The graph should not be considered as synced since the sync manager
	// has yet to start.
	if syncMgr.IsGraphSynced() {
		t.Fatal("expected graph to not be considered as synced")
	}

	syncMgr.Start()
	defer syncMgr.Stop()

	// We should expect to see a QueryChannelRange message with a
	// FirstBlockHeight of the genesis block, signaling that an initial
	// historical sync is being attempted.
	peer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(peer)
	assertMsgSent(t, peer, &lnwire.QueryChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        math.MaxUint32,
	})

	// The graph should not be considered as synced since the initial
	// historical sync has not finished.
	if syncMgr.IsGraphSynced() {
		t.Fatal("expected graph to not be considered as synced")
	}

	// If an additional peer connects, then another historical sync should
	// not be attempted.
	finalHistoricalPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(finalHistoricalPeer)
	finalHistoricalSyncer := assertSyncerExistence(t, syncMgr, finalHistoricalPeer)
	assertNoMsgSent(t, finalHistoricalPeer)

	// If we disconnect the peer performing the initial historical sync, a
	// new one should be chosen.
	syncMgr.PruneSyncState(peer.PubKey())

	// Complete the initial historical sync by transitionining the syncer to
	// its final chansSynced state. The graph should be considered as synced
	// after the fact.
	assertTransitionToChansSynced(t, finalHistoricalSyncer, finalHistoricalPeer)
	if !syncMgr.IsGraphSynced() {
		t.Fatal("expected graph to be considered as synced")
	}

	// Once the initial historical sync has succeeded, another one should
	// not be attempted by disconnecting the peer who performed it.
	extraPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(extraPeer)
	assertNoMsgSent(t, extraPeer)
	syncMgr.PruneSyncState(finalHistoricalPeer.PubKey())
	assertNoMsgSent(t, extraPeer)
}

// TestSyncManagerHistoricalSyncOnReconnect tests that the sync manager will
// re-trigger a historical sync when a new peer connects after a historical
// sync has completed, but we have lost all peers.
func TestSyncManagerHistoricalSyncOnReconnect(t *testing.T) {
	t.Parallel()

	syncMgr := newTestSyncManager(2)
	syncMgr.Start()
	defer syncMgr.Stop()

	// We should expect to see a QueryChannelRange message with a
	// FirstBlockHeight of the genesis block, signaling that an initial
	// historical sync is being attempted.
	peer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(peer)
	s := assertSyncerExistence(t, syncMgr, peer)
	assertTransitionToChansSynced(t, s, peer)
	assertActiveGossipTimestampRange(t, peer)
	assertSyncerStatus(t, s, chansSynced, ActiveSync)

	// Now that the historical sync is completed, we prune the syncer,
	// simulating all peers having disconnected.
	syncMgr.PruneSyncState(peer.PubKey())

	// If a new peer now connects, then another historical sync should
	// be attempted. This is to ensure we get an up-to-date graph if we
	// haven't had any peers for a time.
	nextPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(nextPeer)
	s1 := assertSyncerExistence(t, syncMgr, nextPeer)
	assertTransitionToChansSynced(t, s1, nextPeer)
	assertActiveGossipTimestampRange(t, nextPeer)
	assertSyncerStatus(t, s1, chansSynced, ActiveSync)
}

// TestSyncManagerForceHistoricalSync ensures that we can perform routine
// historical syncs whenever the HistoricalSyncTicker fires.
func TestSyncManagerForceHistoricalSync(t *testing.T) {
	t.Parallel()

	syncMgr := newTestSyncManager(0)
	syncMgr.Start()
	defer syncMgr.Stop()

	// We should expect to see a QueryChannelRange message with a
	// FirstBlockHeight of the genesis block, signaling that a historical
	// sync is being attempted.
	peer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(peer)
	assertMsgSent(t, peer, &lnwire.QueryChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        math.MaxUint32,
	})

	// If an additional peer connects, then a historical sync should not be
	// attempted again.
	extraPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(extraPeer)
	assertNoMsgSent(t, extraPeer)

	// Then, we'll send a tick to force a historical sync. This should
	// trigger the extra peer to also perform a historical sync since the
	// first peer is not eligible due to not being in a chansSynced state.
	syncMgr.cfg.HistoricalSyncTicker.(*ticker.Force).Force <- time.Time{}
	assertMsgSent(t, extraPeer, &lnwire.QueryChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        math.MaxUint32,
	})
}

// TestSyncManagerGraphSyncedAfterHistoricalSyncReplacement ensures that the
// sync manager properly marks the graph as synced given that our initial
// historical sync has stalled, but a replacement has fully completed.
func TestSyncManagerGraphSyncedAfterHistoricalSyncReplacement(t *testing.T) {
	t.Parallel()

	syncMgr := newTestSyncManager(0)
	syncMgr.Start()
	defer syncMgr.Stop()

	// We should expect to see a QueryChannelRange message with a
	// FirstBlockHeight of the genesis block, signaling that an initial
	// historical sync is being attempted.
	peer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(peer)
	assertMsgSent(t, peer, &lnwire.QueryChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        math.MaxUint32,
	})

	// The graph should not be considered as synced since the initial
	// historical sync has not finished.
	if syncMgr.IsGraphSynced() {
		t.Fatal("expected graph to not be considered as synced")
	}

	// If an additional peer connects, then another historical sync should
	// not be attempted.
	finalHistoricalPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(finalHistoricalPeer)
	finalHistoricalSyncer := assertSyncerExistence(t, syncMgr, finalHistoricalPeer)
	assertNoMsgSent(t, finalHistoricalPeer)

	// To simulate that our initial historical sync has stalled, we'll force
	// a historical sync with the new peer to ensure it is replaced.
	syncMgr.cfg.HistoricalSyncTicker.(*ticker.Force).Force <- time.Time{}

	// The graph should still not be considered as synced since the
	// replacement historical sync has not finished.
	if syncMgr.IsGraphSynced() {
		t.Fatal("expected graph to not be considered as synced")
	}

	// Complete the replacement historical sync by transitioning the syncer
	// to its final chansSynced state. The graph should be considered as
	// synced after the fact.
	assertTransitionToChansSynced(t, finalHistoricalSyncer, finalHistoricalPeer)
	if !syncMgr.IsGraphSynced() {
		t.Fatal("expected graph to be considered as synced")
	}
}

// TestSyncManagerWaitUntilInitialHistoricalSync ensures that no GossipSyncers
// are initialized as ActiveSync until the initial historical sync has been
// completed. Once it does, the pending GossipSyncers should be transitioned to
// ActiveSync.
func TestSyncManagerWaitUntilInitialHistoricalSync(t *testing.T) {
	t.Parallel()

	const numActiveSyncers = 2

	// We'll start by creating our test sync manager which will hold up to
	// 2 active syncers.
	syncMgr := newTestSyncManager(numActiveSyncers)
	syncMgr.Start()
	defer syncMgr.Stop()

	// We'll go ahead and create our syncers.
	peers := make([]*mockPeer, 0, numActiveSyncers)
	syncers := make([]*GossipSyncer, 0, numActiveSyncers)
	for i := 0; i < numActiveSyncers; i++ {
		peer := randPeer(t, syncMgr.quit)
		peers = append(peers, peer)

		syncMgr.InitSyncState(peer)
		s := assertSyncerExistence(t, syncMgr, peer)
		syncers = append(syncers, s)

		// The first one always attempts a historical sync. We won't
		// transition it to chansSynced to ensure the remaining syncers
		// aren't started as active.
		if i == 0 {
			assertSyncerStatus(t, s, syncingChans, PassiveSync)
			continue
		}

		// The rest should remain in a passive and chansSynced state,
		// and they should be queued to transition to active once the
		// initial historical sync is completed.
		assertNoMsgSent(t, peer)
		assertSyncerStatus(t, s, chansSynced, PassiveSync)
	}

	// To ensure we don't transition any pending active syncers that have
	// previously disconnected, we'll disconnect the last one.
	stalePeer := peers[numActiveSyncers-1]
	syncMgr.PruneSyncState(stalePeer.PubKey())

	// Then, we'll complete the initial historical sync by transitioning the
	// historical syncer to its final chansSynced state. This should trigger
	// all of the pending active syncers to transition, except for the one
	// we disconnected.
	assertTransitionToChansSynced(t, syncers[0], peers[0])
	for i, s := range syncers {
		if i == numActiveSyncers-1 {
			assertNoMsgSent(t, peers[i])
			continue
		}
		assertPassiveSyncerTransition(t, s, peers[i])
	}
}

// assertNoMsgSent is a helper function that ensures a peer hasn't sent any
// messages.
func assertNoMsgSent(t *testing.T, peer *mockPeer) {
	t.Helper()

	select {
	case msg := <-peer.sentMsgs:
		t.Fatalf("peer %x sent unexpected message %v", peer.PubKey(),
			spew.Sdump(msg))
	case <-time.After(time.Second):
	}
}

// assertMsgSent asserts that the peer has sent the given message.
func assertMsgSent(t *testing.T, peer *mockPeer, msg lnwire.Message) {
	t.Helper()

	var msgSent lnwire.Message
	select {
	case msgSent = <-peer.sentMsgs:
	case <-time.After(time.Second):
		t.Fatalf("expected peer %x to send %T message", peer.PubKey(),
			msg)
	}

	if !reflect.DeepEqual(msgSent, msg) {
		t.Fatalf("expected peer %x to send message: %v\ngot: %v",
			peer.PubKey(), spew.Sdump(msg), spew.Sdump(msgSent))
	}
}

// assertActiveGossipTimestampRange is a helper function that ensures a peer has
// sent a lnwire.GossipTimestampRange message indicating that it would like to
// receive new graph updates.
func assertActiveGossipTimestampRange(t *testing.T, peer *mockPeer) {
	t.Helper()

	var msgSent lnwire.Message
	select {
	case msgSent = <-peer.sentMsgs:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected peer %x to send lnwire.GossipTimestampRange "+
			"message", peer.PubKey())
	}

	msg, ok := msgSent.(*lnwire.GossipTimestampRange)
	if !ok {
		t.Fatalf("expected peer %x to send %T message", peer.PubKey(),
			msg)
	}
	if msg.FirstTimestamp == 0 {
		t.Fatalf("expected *lnwire.GossipTimestampRange message with " +
			"non-zero FirstTimestamp")
	}
	if msg.TimestampRange == 0 {
		t.Fatalf("expected *lnwire.GossipTimestampRange message with " +
			"non-zero TimestampRange")
	}
}

// assertSyncerExistence asserts that a GossipSyncer exists for the given peer.
func assertSyncerExistence(t *testing.T, syncMgr *SyncManager,
	peer *mockPeer) *GossipSyncer {

	t.Helper()

	s, ok := syncMgr.GossipSyncer(peer.PubKey())
	if !ok {
		t.Fatalf("gossip syncer for peer %x not found", peer.PubKey())
	}

	return s
}

// assertSyncerStatus asserts that the gossip syncer for the given peer matches
// the expected sync state and type.
func assertSyncerStatus(t *testing.T, s *GossipSyncer, syncState syncerState,
	syncType SyncerType) {

	t.Helper()

	// We'll check the status of our syncer within a WaitPredicate as some
	// sync transitions might cause this to be racy.
	err := wait.NoError(func() error {
		state := s.syncState()
		if s.syncState() != syncState {
			return fmt.Errorf("expected syncState %v for peer "+
				"%x, got %v", syncState, s.cfg.peerPub, state)
		}

		typ := s.SyncType()
		if s.SyncType() != syncType {
			return fmt.Errorf("expected syncType %v for peer "+
				"%x, got %v", syncType, s.cfg.peerPub, typ)
		}

		return nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

// assertTransitionToChansSynced asserts the transition of an ActiveSync
// GossipSyncer to its final chansSynced state.
func assertTransitionToChansSynced(t *testing.T, s *GossipSyncer, peer *mockPeer) {
	t.Helper()

	query := &lnwire.QueryChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        math.MaxUint32,
	}
	assertMsgSent(t, peer, query)

	s.ProcessQueryMsg(&lnwire.ReplyChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        math.MaxUint32,
		Complete:         1,
	}, nil)

	chanSeries := s.cfg.channelSeries.(*mockChannelGraphTimeSeries)

	select {
	case <-chanSeries.filterReq:
		chanSeries.filterResp <- nil
	case <-time.After(2 * time.Second):
		t.Fatal("expected to receive FilterKnownChanIDs request")
	}

	err := wait.NoError(func() error {
		state := syncerState(atomic.LoadUint32(&s.state))
		if state != chansSynced {
			return fmt.Errorf("expected syncerState %v, got %v",
				chansSynced, state)
		}

		return nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

// assertPassiveSyncerTransition asserts that a gossip syncer goes through all
// of its expected steps when transitioning from passive to active.
func assertPassiveSyncerTransition(t *testing.T, s *GossipSyncer, peer *mockPeer) {

	t.Helper()

	assertActiveGossipTimestampRange(t, peer)
	assertSyncerStatus(t, s, chansSynced, ActiveSync)
}

// assertActiveSyncerTransition asserts that a gossip syncer goes through all of
// its expected steps when transitioning from active to passive.
func assertActiveSyncerTransition(t *testing.T, s *GossipSyncer, peer *mockPeer) {
	t.Helper()

	assertMsgSent(t, peer, &lnwire.GossipTimestampRange{
		FirstTimestamp: uint32(zeroTimestamp.Unix()),
		TimestampRange: 0,
	})
	assertSyncerStatus(t, s, chansSynced, PassiveSync)
}
