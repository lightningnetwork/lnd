package contractcourt

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

type mockAuxChannelLifecycle struct {
	watchOwner func(context.Context, *chanstate.OpenChannel) (
		ChainWatchOwner, error)
	prepare  func(context.Context, wire.OutPoint) error
	finalize func(context.Context, wire.OutPoint) error
}

// ChainWatchOwner returns the configured owner or lnd by default.
func (m *mockAuxChannelLifecycle) ChainWatchOwner(ctx context.Context,
	channel *chanstate.OpenChannel) (ChainWatchOwner, error) {

	if m.watchOwner == nil {
		return ChainWatchOwnerLnd, nil
	}

	return m.watchOwner(ctx, channel)
}

// PrepareCommitmentPublish runs the configured publication barrier.
func (m *mockAuxChannelLifecycle) PrepareCommitmentPublish(
	ctx context.Context, point wire.OutPoint) error {

	if m.prepare == nil {
		return nil
	}

	return m.prepare(ctx, point)
}

// WaitForChannelFinalization runs the configured terminal barrier.
func (m *mockAuxChannelLifecycle) WaitForChannelFinalization(
	ctx context.Context, point wire.OutPoint) error {

	if m.finalize == nil {
		return nil
	}

	return m.finalize(ctx, point)
}

// TestChainArbitratorRepulishCloses tests that the chain arbitrator will
// republish closing transactions for channels marked CommitementBroadcast or
// CoopBroadcast in the database at startup.
func TestChainArbitratorRepublishCloses(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	// Create 10 test channels and sync them to the database.
	const numChans = 10
	var channels []*chanstate.OpenChannel
	for i := 0; i < numChans; i++ {
		lChannel, _, err := lnwallet.CreateTestChannels(
			t, channeldb.SingleFunderTweaklessBit,
		)
		if err != nil {
			t.Fatal(err)
		}

		channel := lChannel.State()

		// We manually set the db here to make sure all channels are
		// synced to the same db.
		channel.Db = db.ChannelStateDB()

		addr := &net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 18556,
		}
		if err := channel.SyncPending(addr, 101); err != nil {
			t.Fatal(err)
		}

		channels = append(channels, channel)
	}

	// Mark half of the channels as commitment broadcasted.
	for i := 0; i < numChans/2; i++ {
		closeTx := channels[i].FundingTxn.Copy()
		closeTx.TxIn[0].PreviousOutPoint = channels[i].FundingOutpoint
		err := channels[i].MarkCommitmentBroadcasted(
			closeTx, lntypes.Local,
		)
		if err != nil {
			t.Fatal(err)
		}

		err = channels[i].MarkCoopBroadcasted(closeTx, lntypes.Local)
		if err != nil {
			t.Fatal(err)
		}
	}

	// We keep track of the transactions published by the ChainArbitrator
	// at startup.
	published := make(map[chainhash.Hash]int)

	chainArbCfg := ChainArbitratorConfig{
		ChainIO: &mock.ChainIO{},
		Notifier: &mock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		PublishTx: func(tx *wire.MsgTx, _ string) error {
			published[tx.TxHash()]++
			return nil
		},
		Clock:  clock.NewDefaultClock(),
		Budget: *DefaultBudgetConfig(),
	}
	chainArb := NewChainArbitrator(
		chainArbCfg, db,
	)

	beat := newBeatFromHeight(0)
	if err := chainArb.Start(beat); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, chainArb.Stop())
	})

	// Half of the channels should have had their closing tx re-published.
	if len(published) != numChans/2 {
		t.Fatalf("expected %d re-published transactions, got %d",
			numChans/2, len(published))
	}

	// And make sure the published transactions are correct, and unique.
	for i := 0; i < numChans/2; i++ {
		closeTx := channels[i].FundingTxn.Copy()
		closeTx.TxIn[0].PreviousOutPoint = channels[i].FundingOutpoint

		count, ok := published[closeTx.TxHash()]
		if !ok {
			t.Fatalf("closing tx not re-published")
		}

		// We expect one coop close and one force close.
		if count != 2 {
			t.Fatalf("expected 2 closing txns, only got %d", count)
		}

		delete(published, closeTx.TxHash())
	}

	if len(published) != 0 {
		t.Fatalf("unexpected tx published")
	}
}

// TestChainArbitratorFiltersOpenChannels verifies an embedding runtime can
// keep selected open channels out of chain observation at startup and through
// the explicit channel admission path.
func TestChainArbitratorFiltersOpenChannels(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	channels := make([]*chanstate.OpenChannel, 0, 2)
	for i := 0; i < 2; i++ {
		lightningChannel, _, err := lnwallet.CreateTestChannels(
			t, channeldb.SingleFunderTweaklessBit,
		)
		require.NoError(t, err)

		channel := lightningChannel.State()
		channel.Db = db.ChannelStateDB()
		require.NoError(t, channel.SyncPending(&net.TCPAddr{
			IP: net.ParseIP("127.0.0.1"), Port: 18556 + i,
		}, 101))
		channels = append(channels, channel)
	}

	admitted := channels[0].FundingOutpoint
	chainArb := NewChainArbitrator(ChainArbitratorConfig{
		ChainIO: &mock.ChainIO{},
		Notifier: &mock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		PublishTx: func(*wire.MsgTx, string) error { return nil },
		Clock:     clock.NewDefaultClock(),
		Budget:    *DefaultBudgetConfig(),
		AuxChannelLifecycle: fn.Some[AuxChannelLifecycle](
			&mockAuxChannelLifecycle{
				watchOwner: func(_ context.Context,
					channel *chanstate.OpenChannel) (
					ChainWatchOwner, error) {

					if channel.FundingOutpoint == admitted {
						return ChainWatchOwnerLnd, nil
					}

					return ChainWatchOwnerAux, nil
				},
			},
		),
	}, db)
	require.NoError(t, chainArb.Start(newBeatFromHeight(0)))
	t.Cleanup(func() {
		require.NoError(t, chainArb.Stop())
	})

	require.Contains(t, chainArb.activeChannels, admitted)
	require.NotContains(
		t, chainArb.activeChannels, channels[1].FundingOutpoint,
	)
	require.Error(t, chainArb.WatchNewChannel(channels[1]))
}

// TestChainArbitratorRejectsUnknownWatchOwner verifies an auxiliary lifecycle
// cannot accidentally disable lnd's watcher with an invalid owner value.
func TestChainArbitratorRejectsUnknownWatchOwner(t *testing.T) {
	t.Parallel()

	chainArb := NewChainArbitrator(ChainArbitratorConfig{
		AuxChannelLifecycle: fn.Some[AuxChannelLifecycle](
			&mockAuxChannelLifecycle{
				watchOwner: func(context.Context,
					*chanstate.OpenChannel) (
					ChainWatchOwner, error) {

					return ChainWatchOwner(99), nil
				},
			},
		),
	}, nil)

	_, err := chainArb.shouldWatchChannel(&chanstate.OpenChannel{})
	require.ErrorContains(t, err, "unknown chain watch owner")
}

// TestResolveContract tests that if we have an active channel being watched by
// the chain arb, then a call to ResolveContract will mark the channel as fully
// closed in the database, and also clean up all arbitrator state.
func TestResolveContract(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	// With the DB created, we'll make a new channel, and mark it as
	// pending open within the database.
	newChannel, _, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err, "unable to make new test channel")
	channel := newChannel.State()
	channel.Db = db.ChannelStateDB()
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}
	if err := channel.SyncPending(addr, 101); err != nil {
		t.Fatalf("unable to write channel to db: %v", err)
	}

	// With the channel inserted into the database, we'll now create a new
	// chain arbitrator that should pick up these new channels and launch
	// resolver for them.
	chainArbCfg := ChainArbitratorConfig{
		ChainIO: &mock.ChainIO{},
		Notifier: &mock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		PublishTx: func(tx *wire.MsgTx, _ string) error {
			return nil
		},
		Clock:  clock.NewDefaultClock(),
		Budget: *DefaultBudgetConfig(),
		QueryIncomingCircuit: func(
			circuit models.CircuitKey) *models.CircuitKey {

			return nil
		},
	}
	chainArb := NewChainArbitrator(
		chainArbCfg, db,
	)
	beat := newBeatFromHeight(0)
	if err := chainArb.Start(beat); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, chainArb.Stop())
	})

	channelArb := chainArb.activeChannels[channel.FundingOutpoint]

	// While the resolver are active, we'll now remove the channel from the
	// database (mark is as closed).
	err = db.ChannelStateDB().AbandonChannel(&channel.FundingOutpoint, 4)
	require.NoError(t, err, "unable to remove channel")

	// With the channel removed, we'll now manually call ResolveContract.
	// This stimulates needing to remove a channel from the chain arb due
	// to any possible external consistency issues.
	err = chainArb.ResolveContract(channel.FundingOutpoint)
	require.NoError(t, err, "unable to resolve contract")

	// The shouldn't be an active chain watcher or channel arb for this
	// channel.
	if len(chainArb.activeChannels) != 0 {
		t.Fatalf("expected zero active channels, instead have %v",
			len(chainArb.activeChannels))
	}
	if len(chainArb.activeWatchers) != 0 {
		t.Fatalf("expected zero active watchers, instead have %v",
			len(chainArb.activeWatchers))
	}

	// At this point, the channel's arbitrator log should also be empty as
	// well.
	_, err = channelArb.log.FetchContractResolutions()
	if err != errScopeBucketNoExist {
		t.Fatalf("channel arb log state should have been "+
			"removed: %v", err)
	}

	// If we attempt to call this method again, then we should get a nil
	// error, as there is no more state to be cleaned up.
	err = chainArb.ResolveContract(channel.FundingOutpoint)
	require.NoError(t, err, "second resolve call shouldn't fail")
}

// TestChainArbitratorFullyResolvedBarrier verifies an embedding runtime can
// durably gate lnd's terminal cleanup without losing the resolution signal.
func TestChainArbitratorFullyResolvedBarrier(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	lightningChannel, _, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)
	channel := lightningChannel.State()
	channel.Db = db.ChannelStateDB()
	require.NoError(t, channel.SyncPending(&net.TCPAddr{
		IP: net.ParseIP("127.0.0.1"), Port: 18556,
	}, 101))

	barrierEntered := make(chan struct{}, 1)
	releaseBarrier := make(chan struct{})
	var notified atomic.Bool
	chainArb := NewChainArbitrator(ChainArbitratorConfig{
		ChainIO: &mock.ChainIO{},
		Notifier: &mock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		PublishTx: func(*wire.MsgTx, string) error { return nil },
		Clock:     clock.NewDefaultClock(),
		Budget:    *DefaultBudgetConfig(),
		AuxChannelLifecycle: fn.Some[AuxChannelLifecycle](
			&mockAuxChannelLifecycle{
				finalize: func(ctx context.Context,
					_ wire.OutPoint) error {

					barrierEntered <- struct{}{}
					select {
					case <-releaseBarrier:
						return nil

					case <-ctx.Done():
						return ctx.Err()
					}
				},
			},
		),
		NotifyFullyResolvedChannel: func(wire.OutPoint) {
			notified.Store(true)
		},
	}, db)
	require.NoError(t, chainArb.Start(newBeatFromHeight(0)))
	t.Cleanup(func() {
		require.NoError(t, chainArb.Stop())
	})

	require.NoError(t, db.ChannelStateDB().AbandonChannel(
		&channel.FundingOutpoint, 4,
	))
	chainArb.resolvedChan <- channel.FundingOutpoint

	select {
	case <-barrierEntered:
	case <-time.After(time.Second):
		t.Fatal("auxiliary lifecycle was not called")
	}
	require.False(t, notified.Load())
	chainArb.Lock()
	_, active := chainArb.activeChannels[channel.FundingOutpoint]
	chainArb.Unlock()
	require.True(t, active)

	close(releaseBarrier)
	require.Eventually(t, func() bool {
		chainArb.Lock()
		_, active := chainArb.activeChannels[channel.FundingOutpoint]
		chainArb.Unlock()

		return notified.Load() && !active
	}, time.Second, 10*time.Millisecond)
}

// TestChainArbitratorFullyResolvedBarriersAreIndependent verifies a failed
// durable callback for one channel does not stall unrelated channel cleanup.
func TestChainArbitratorFullyResolvedBarriersAreIndependent(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	lightningChannel, _, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)
	readyChannel := lightningChannel.State()
	readyChannel.Db = db.ChannelStateDB()
	require.NoError(t, readyChannel.SyncPending(&net.TCPAddr{
		IP: net.ParseIP("127.0.0.1"), Port: 18557,
	}, 102))
	readyPoint := readyChannel.FundingOutpoint
	require.NoError(t, db.ChannelStateDB().AbandonChannel(
		&readyPoint, 4,
	))
	blockedPoint := readyPoint
	blockedPoint.Index++
	blocked := make(chan struct{}, 1)
	notified := make(chan wire.OutPoint, 1)
	chainArb := NewChainArbitrator(ChainArbitratorConfig{
		AuxChannelLifecycle: fn.Some[AuxChannelLifecycle](
			&mockAuxChannelLifecycle{
				finalize: func(ctx context.Context,
					point wire.OutPoint) error {

					if point != blockedPoint {
						return nil
					}

					select {
					case blocked <- struct{}{}:
					default:
					}

					<-ctx.Done()

					return ctx.Err()
				},
			},
		),
		NotifyFullyResolvedChannel: func(point wire.OutPoint) {
			notified <- point
		},
	}, db)
	chainArb.wg.Add(1)
	go func() {
		defer chainArb.wg.Done()
		chainArb.resolveContracts()
	}()
	t.Cleanup(func() {
		require.NoError(t, chainArb.Stop())
	})

	chainArb.resolvedChan <- blockedPoint
	require.Eventually(t, func() bool {
		select {
		case <-blocked:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	chainArb.resolvedChan <- readyPoint

	select {
	case point := <-notified:
		require.Equal(t, readyPoint, point)

	case <-time.After(time.Second):
		t.Fatal("independent channel resolution was blocked")
	}
	require.Eventually(t, func() bool {
		closed, err := db.ChannelStateDB().FetchClosedChannel(
			&readyPoint,
		)

		return err == nil && !closed.IsPending
	}, time.Second, 10*time.Millisecond)
}

// TestShouldSuppressClosedChannelNotify pins down the gate that prevents
// MarkChannelClosed from firing a duplicate NotifyClosedChannel after the
// chain watcher has already emitted a preliminary CLOSED_CHANNEL via the
// early-dispatch path. Only the cooperative-close path can be suppressed;
// every other CloseType (force, breach, abandon) must always notify here
// regardless of the early-dispatched flag. The fast path (numConfs==1)
// never sets the early-dispatched flag, so cooperative closes on that path
// also fall through to NotifyClosedChannel.
func TestShouldSuppressClosedChannelNotify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		closeType       channeldb.ClosureType
		earlyDispatched bool
		wantSuppress    bool
	}{
		{
			name:            "coop close with early dispatch",
			closeType:       channeldb.CooperativeClose,
			earlyDispatched: true,
			wantSuppress:    true,
		},
		{
			name: "coop close without early dispatch " +
				"(fast path or no watcher)",
			closeType:       channeldb.CooperativeClose,
			earlyDispatched: false,
			wantSuppress:    false,
		},
		{
			name:            "local force close",
			closeType:       channeldb.LocalForceClose,
			earlyDispatched: true,
			wantSuppress:    false,
		},
		{
			name:            "remote force close",
			closeType:       channeldb.RemoteForceClose,
			earlyDispatched: true,
			wantSuppress:    false,
		},
		{
			name:            "breach close",
			closeType:       channeldb.BreachClose,
			earlyDispatched: true,
			wantSuppress:    false,
		},
		{
			name:            "abandoned close",
			closeType:       channeldb.Abandoned,
			earlyDispatched: true,
			wantSuppress:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldSuppressClosedChannelNotify(
				tc.closeType, tc.earlyDispatched,
			)
			require.Equal(t, tc.wantSuppress, got)
		})
	}
}
