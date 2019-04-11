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
		go syncMgr.InitSyncState(peer)

		// The first syncer registered always attempts a historical
		// sync.
		assertActiveGossipTimestampRange(t, peer)
		if i == 0 {
			assertTransitionToChansSynced(t, syncMgr, peer)
		}
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
	go syncMgr.InitSyncState(peer1)
	assertActiveGossipTimestampRange(t, peer1)
	assertTransitionToChansSynced(t, syncMgr, peer1)
	assertSyncerStatus(t, syncMgr, peer1, chansSynced, ActiveSync)

	// It will then be torn down to simulate a disconnection. Since there
	// are no other candidate syncers available, the active syncer won't be
	// replaced.
	syncMgr.PruneSyncState(peer1.PubKey())

	// Then, we'll start our active syncer again, but this time we'll also
	// have a passive syncer available to replace the active syncer after
	// the peer disconnects.
	go syncMgr.InitSyncState(peer1)
	assertActiveGossipTimestampRange(t, peer1)
	assertSyncerStatus(t, syncMgr, peer1, chansSynced, ActiveSync)

	// Create our second peer, which should be initialized as a passive
	// syncer.
	peer2 := randPeer(t, syncMgr.quit)
	syncMgr.InitSyncState(peer2)
	assertSyncerStatus(t, syncMgr, peer2, chansSynced, PassiveSync)

	// Disconnect our active syncer, which should trigger the SyncManager to
	// replace it with our passive syncer.
	go syncMgr.PruneSyncState(peer1.PubKey())
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
	go syncMgr.InitSyncState(activeSyncPeer)
	assertActiveGossipTimestampRange(t, activeSyncPeer)
	assertTransitionToChansSynced(t, syncMgr, activeSyncPeer)
	assertSyncerStatus(t, syncMgr, activeSyncPeer, chansSynced, ActiveSync)

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
	peer *mockPeer) {

	t.Helper()

	s, ok := syncMgr.GossipSyncer(peer.PubKey())
	if !ok {
		t.Fatalf("gossip syncer for peer %x not found", peer.PubKey())
	}

	assertMsgSent(t, peer, &lnwire.QueryChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        math.MaxUint32,
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
	assertSyncerStatus(t, syncMgr, peer, chansSynced, ActiveSync)
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
