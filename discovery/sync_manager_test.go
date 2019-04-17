package discovery

import (
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lntest"
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
		ChanSeries:                newMockChannelGraphTimeSeries(hID),
		RotateTicker:              ticker.NewForce(DefaultSyncerRotationInterval),
		HistoricalSyncTicker:      ticker.NewForce(DefaultHistoricalSyncInterval),
		ActiveSyncerTimeoutTicker: ticker.NewForce(DefaultActiveSyncerTimeout),
		NumActiveSyncers:          numActiveSyncers,
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

		// The first syncer registered always attempts a historical
		// sync.
		if i == 0 {
			assertTransitionToChansSynced(t, syncMgr, peer, true)
		}

		assertPassiveSyncerTransition(t, syncMgr, peer)
		assertSyncerStatus(t, syncMgr, peer, chansSynced, ActiveSync)
	}

	for i := 0; i < numSyncers-numActiveSyncers; i++ {
		peer := randPeer(t, syncMgr.quit)
		syncMgr.InitSyncState(peer)
		assertSyncerStatus(t, syncMgr, peer, chansSynced, PassiveSync)
	}
}

// TestSyncManagerNewActiveSyncerAfterDisconnect ensures that we can regain an
// active syncer after losing one due to the peer disconnecting.
func TestSyncManagerNewActiveSyncerAfterDisconnect(t *testing.T) {
	t.Parallel()

	// We'll create our test sync manager to only have one active syncer.
	syncMgr := newTestSyncManager(1)
	syncMgr.Start()
	defer syncMgr.Stop()

	// peer1 will represent an active syncer that performs a historical
	// sync since it is the first registered peer with the SyncManager.
	peer1 := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(peer1)
	assertTransitionToChansSynced(t, syncMgr, peer1, true)
	assertPassiveSyncerTransition(t, syncMgr, peer1)

	// It will then be torn down to simulate a disconnection. Since there
	// are no other candidate syncers available, the active syncer won't be
	// replaced.
	syncMgr.PruneSyncState(peer1.PubKey())

	// Then, we'll start our active syncer again, but this time we'll also
	// have a passive syncer available to replace the active syncer after
	// the peer disconnects.
	syncMgr.InitSyncState(peer1)
	assertPassiveSyncerTransition(t, syncMgr, peer1)

	// Create our second peer, which should be initialized as a passive
	// syncer.
	peer2 := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(peer2)
	assertSyncerStatus(t, syncMgr, peer2, chansSynced, PassiveSync)

	// Disconnect our active syncer, which should trigger the SyncManager to
	// replace it with our passive syncer.
	syncMgr.PruneSyncState(peer1.PubKey())
	assertPassiveSyncerTransition(t, syncMgr, peer2)
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
	assertTransitionToChansSynced(t, syncMgr, activeSyncPeer, true)
	assertPassiveSyncerTransition(t, syncMgr, activeSyncPeer)

	// We'll send a tick to force a rotation. Since there aren't any
	// candidates, none of the active syncers will be rotated.
	syncMgr.cfg.RotateTicker.(*ticker.Force).Force <- time.Time{}
	assertNoMsgSent(t, activeSyncPeer)
	assertSyncerStatus(t, syncMgr, activeSyncPeer, chansSynced, ActiveSync)

	// We'll then go ahead and add a passive syncer.
	passiveSyncPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(passiveSyncPeer)
	assertSyncerStatus(t, syncMgr, passiveSyncPeer, chansSynced, PassiveSync)

	// We'll force another rotation - this time, since we have a passive
	// syncer available, they should be rotated.
	syncMgr.cfg.RotateTicker.(*ticker.Force).Force <- time.Time{}

	// The transition from an active syncer to a passive syncer causes the
	// peer to send out a new GossipTimestampRange in the past so that they
	// don't receive new graph updates.
	assertActiveSyncerTransition(t, syncMgr, activeSyncPeer)

	// The transition from a passive syncer to an active syncer causes the
	// peer to send a new GossipTimestampRange with the current timestamp to
	// signal that they would like to receive new graph updates from their
	// peers. This will also cause the gossip syncer to redo its state
	// machine, starting from its initial syncingChans state. We'll then
	// need to transition it to its final chansSynced state to ensure the
	// next syncer is properly started in the round-robin.
	assertPassiveSyncerTransition(t, syncMgr, passiveSyncPeer)
}

// TestSyncManagerHistoricalSync ensures that we only attempt a single
// historical sync during the SyncManager's startup, and that we can routinely
// force historical syncs whenever the HistoricalSyncTicker fires.
func TestSyncManagerHistoricalSync(t *testing.T) {
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

// TestSyncManagerRoundRobinQueue ensures that any subsequent active syncers can
// only be started after the previous one has completed its state machine.
func TestSyncManagerRoundRobinQueue(t *testing.T) {
	t.Parallel()

	const numActiveSyncers = 3

	// We'll start by creating our sync manager with support for three
	// active syncers.
	syncMgr := newTestSyncManager(numActiveSyncers)
	syncMgr.Start()
	defer syncMgr.Stop()

	peers := make([]*mockPeer, 0, numActiveSyncers)

	// The first syncer registered always attempts a historical sync.
	firstPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(firstPeer)
	peers = append(peers, firstPeer)
	assertTransitionToChansSynced(t, syncMgr, firstPeer, true)

	// After completing the historical sync, a sync transition to ActiveSync
	// should happen. It should transition immediately since it has no
	// dependents.
	assertActiveGossipTimestampRange(t, firstPeer)

	// We'll create the remaining numActiveSyncers. These will be queued in
	// the round robin since the first syncer has yet to reach chansSynced.
	queuedPeers := make([]*mockPeer, 0, numActiveSyncers-1)
	for i := 0; i < numActiveSyncers-1; i++ {
		peer := randPeer(t, syncMgr.quit)
		syncMgr.InitSyncState(peer)
		peers = append(peers, peer)
		queuedPeers = append(queuedPeers, peer)
	}

	// Ensure they cannot transition without sending a GossipTimestampRange
	// message first.
	for _, peer := range queuedPeers {
		assertNoMsgSent(t, peer)
	}

	// Transition the first syncer to chansSynced, which should allow the
	// second to transition next.
	assertTransitionToChansSynced(t, syncMgr, firstPeer, false)

	// assertSyncerTransitioned ensures the target peer's syncer is the only
	// that has transitioned.
	assertSyncerTransitioned := func(target *mockPeer) {
		t.Helper()

		for _, peer := range peers {
			if peer.PubKey() != target.PubKey() {
				assertNoMsgSent(t, peer)
				continue
			}

			assertActiveGossipTimestampRange(t, target)
		}
	}

	// For each queued syncer, we'll ensure they have transitioned to an
	// ActiveSync type and reached their final chansSynced state to allow
	// the next one to transition.
	for _, peer := range queuedPeers {
		assertSyncerTransitioned(peer)
		assertTransitionToChansSynced(t, syncMgr, peer, false)
	}
}

// TestSyncManagerRoundRobinTimeout ensures that if we timeout while waiting for
// an active syncer to reach its final chansSynced state, then we will go on to
// start the next.
func TestSyncManagerRoundRobinTimeout(t *testing.T) {
	t.Parallel()

	// Create our sync manager with support for two active syncers.
	syncMgr := newTestSyncManager(2)
	syncMgr.Start()
	defer syncMgr.Stop()

	// peer1 will be the first peer we start, which will time out and cause
	// peer2 to start.
	peer1 := randPeer(t, syncMgr.quit)
	peer2 := randPeer(t, syncMgr.quit)

	// The first syncer registered always attempts a historical sync.
	syncMgr.InitSyncState(peer1)
	assertTransitionToChansSynced(t, syncMgr, peer1, true)

	// We assume the syncer for peer1 has transitioned once we see it send a
	// lnwire.GossipTimestampRange message.
	assertActiveGossipTimestampRange(t, peer1)

	// We'll then create the syncer for peer2. This should cause it to be
	// queued so that it starts once the syncer for peer1 is done.
	syncMgr.InitSyncState(peer2)
	assertNoMsgSent(t, peer2)

	// Send a force tick to pretend the sync manager has timed out waiting
	// for peer1's syncer to reach chansSynced.
	syncMgr.cfg.ActiveSyncerTimeoutTicker.(*ticker.Force).Force <- time.Time{}

	// Finally, ensure that the syncer for peer2 has transitioned.
	assertActiveGossipTimestampRange(t, peer2)
}

// TestSyncManagerRoundRobinStaleSyncer ensures that any stale active syncers we
// are currently waiting for or are queued up to start are properly removed and
// stopped.
func TestSyncManagerRoundRobinStaleSyncer(t *testing.T) {
	t.Parallel()

	const numActiveSyncers = 4

	// We'll create and start our sync manager with some active syncers.
	syncMgr := newTestSyncManager(numActiveSyncers)
	syncMgr.Start()
	defer syncMgr.Stop()

	peers := make([]*mockPeer, 0, numActiveSyncers)

	// The first syncer registered always attempts a historical sync.
	firstPeer := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(firstPeer)
	peers = append(peers, firstPeer)
	assertTransitionToChansSynced(t, syncMgr, firstPeer, true)

	// After completing the historical sync, a sync transition to ActiveSync
	// should happen. It should transition immediately since it has no
	// dependents.
	assertActiveGossipTimestampRange(t, firstPeer)
	assertMsgSent(t, firstPeer, &lnwire.QueryChannelRange{
		FirstBlockHeight: startHeight,
		NumBlocks:        math.MaxUint32 - startHeight,
	})

	// We'll create the remaining numActiveSyncers. These will be queued in
	// the round robin since the first syncer has yet to reach chansSynced.
	queuedPeers := make([]*mockPeer, 0, numActiveSyncers-1)
	for i := 0; i < numActiveSyncers-1; i++ {
		peer := randPeer(t, syncMgr.quit)
		syncMgr.InitSyncState(peer)
		peers = append(peers, peer)
		queuedPeers = append(queuedPeers, peer)
	}

	// Ensure they cannot transition without sending a GossipTimestampRange
	// message first.
	for _, peer := range queuedPeers {
		assertNoMsgSent(t, peer)
	}

	// assertSyncerTransitioned ensures the target peer's syncer is the only
	// that has transitioned.
	assertSyncerTransitioned := func(target *mockPeer) {
		t.Helper()

		for _, peer := range peers {
			if peer.PubKey() != target.PubKey() {
				assertNoMsgSent(t, peer)
				continue
			}

			assertPassiveSyncerTransition(t, syncMgr, target)
		}
	}

	// We'll then remove the syncers in the middle to cover the case where
	// they are queued up in the sync manager's pending list.
	for i, peer := range peers {
		if i == 0 || i == len(peers)-1 {
			continue
		}

		syncMgr.PruneSyncState(peer.PubKey())
	}

	// We'll then remove the syncer we are currently waiting for. This
	// should prompt the last syncer to start since it is the only one left
	// pending. We'll do this in a goroutine since the peer behind the new
	// active syncer will need to send out its new GossipTimestampRange.
	go syncMgr.PruneSyncState(peers[0].PubKey())
	assertSyncerTransitioned(peers[len(peers)-1])
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
	case <-time.After(time.Second):
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

// assertSyncerStatus asserts that the gossip syncer for the given peer matches
// the expected sync state and type.
func assertSyncerStatus(t *testing.T, syncMgr *SyncManager, peer *mockPeer,
	syncState syncerState, syncType SyncerType) {

	t.Helper()

	s, ok := syncMgr.GossipSyncer(peer.PubKey())
	if !ok {
		t.Fatalf("gossip syncer for peer %x not found", peer.PubKey())
	}

	// We'll check the status of our syncer within a WaitPredicate as some
	// sync transitions might cause this to be racy.
	err := lntest.WaitNoError(func() error {
		state := s.syncState()
		if s.syncState() != syncState {
			return fmt.Errorf("expected syncState %v for peer "+
				"%x, got %v", syncState, peer.PubKey(), state)
		}

		typ := s.SyncType()
		if s.SyncType() != syncType {
			return fmt.Errorf("expected syncType %v for peer "+
				"%x, got %v", syncType, peer.PubKey(), typ)
		}

		return nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

// assertTransitionToChansSynced asserts the transition of an ActiveSync
// GossipSyncer to its final chansSynced state.
func assertTransitionToChansSynced(t *testing.T, syncMgr *SyncManager,
	peer *mockPeer, historicalSync bool) {

	t.Helper()

	s, ok := syncMgr.GossipSyncer(peer.PubKey())
	if !ok {
		t.Fatalf("gossip syncer for peer %x not found", peer.PubKey())
	}

	firstBlockHeight := uint32(startHeight)
	if historicalSync {
		firstBlockHeight = 0
	}
	assertMsgSent(t, peer, &lnwire.QueryChannelRange{
		FirstBlockHeight: firstBlockHeight,
		NumBlocks:        math.MaxUint32 - firstBlockHeight,
	})

	s.ProcessQueryMsg(&lnwire.ReplyChannelRange{Complete: 1}, nil)

	chanSeries := syncMgr.cfg.ChanSeries.(*mockChannelGraphTimeSeries)

	select {
	case <-chanSeries.filterReq:
		chanSeries.filterResp <- nil
	case <-time.After(2 * time.Second):
		t.Fatal("expected to receive FilterKnownChanIDs request")
	}

	err := lntest.WaitNoError(func() error {
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
func assertPassiveSyncerTransition(t *testing.T, syncMgr *SyncManager,
	peer *mockPeer) {

	t.Helper()

	assertActiveGossipTimestampRange(t, peer)
	assertTransitionToChansSynced(t, syncMgr, peer, false)
}

// assertActiveSyncerTransition asserts that a gossip syncer goes through all of
// its expected steps when transitioning from active to passive.
func assertActiveSyncerTransition(t *testing.T, syncMgr *SyncManager,
	peer *mockPeer) {

	t.Helper()

	assertMsgSent(t, peer, &lnwire.GossipTimestampRange{
		FirstTimestamp: uint32(zeroTimestamp.Unix()),
		TimestampRange: 0,
	})
	assertSyncerStatus(t, syncMgr, peer, chansSynced, PassiveSync)
}
