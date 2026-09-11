package contractcourt

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// closedResolverSpendEvent creates a registration that terminates the wait
// path immediately and reports whether resolver cleanup invoked Cancel.
func closedResolverSpendEvent(cancelled chan struct{}) *chainntnfs.SpendEvent {
	spend := make(chan *chainntnfs.SpendDetail)
	close(spend)

	return &chainntnfs.SpendEvent{
		Spend: spend,
		Reorg: make(chan struct{}),
		Done:  make(chan struct{}),
		Cancel: func() {
			// Signal cleanup without timing sleeps or a mock
			// assertion unrelated to ChainNotifier.
			close(cancelled)
		},
	}
}

// resolverSpendCancelled reports cancellation without blocking a failing
// regression test when the resolver forgets to release its notification.
func resolverSpendCancelled(cancelled <-chan struct{}) bool {
	// Cleanup runs synchronously through defer, so an open channel at this
	// point is direct evidence that the registration leaked.
	select {
	case _, open := <-cancelled:
		return !open

	default:
		return false
	}
}

// TestResolverActionablePreimageSpendDepth verifies a contest resolver learns
// a durable preimage at one confirmation even when terminal cleanup uses a
// deeper channel policy.
func TestResolverActionablePreimageSpendDepth(t *testing.T) {
	t.Parallel()

	// Arrange a remote-commit output whose contest resolver carries a
	// three-block terminal policy. The mock.Mock-backed notifier expects a
	// one-block registration because the revealed preimage remains useful
	// after a reorg and must be learned before the HTLC expiry transition.
	const (
		broadcastHeight = uint32(21)
		terminalDepth   = uint32(3)
	)
	outpoint := wire.OutPoint{Index: 7}
	pkScript := []byte{0x51}
	cancelled := make(chan struct{})
	spendEvent := closedResolverSpendEvent(cancelled)
	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On(
		"RegisterSpendNtfn", &outpoint, pkScript, broadcastHeight,
		mock.MatchedBy(func(opts []chainntnfs.SpendOption) bool {
			// Parse the option in this test so the Arrange phase
			// exposes the one-confirmation contract under test.
			parsed, err := chainntnfs.ParseSpendOptions(opts...)
			return err == nil && parsed.NumConfs == 1
		}),
	).Return(spendEvent, nil).Once()
	resolver := newOutgoingContestResolver(
		lnwallet.OutgoingHtlcResolution{
			ClaimOutpoint: outpoint,
			SweepSignDesc: input.SignDescriptor{
				Output: &wire.TxOut{PkScript: pkScript},
			},
		},
		broadcastHeight, channeldb.HTLC{},
		channeldb.SingleFunderTweaklessBit,
		ResolverConfig{
			ChannelArbitratorConfig: ChannelArbitratorConfig{
				SpendConfDepth: terminalDepth,
				ChainArbitratorConfig: ChainArbitratorConfig{
					Notifier: notifier,
				},
			},
		},
	)

	// Act by running Resolve until the controlled closed spend channel
	// returns its shutdown sentinel. Deferred cancellation must complete
	// before Resolve returns to this test goroutine.
	nextResolver, err := resolver.Resolve()
	wasCancelled := resolverSpendCancelled(cancelled)

	// Assert the resolver did not transform after the controlled wait
	// failure. It must release the one-confirmation preimage registration,
	// then satisfy the exact option-bearing mock without redundant
	// call-count assertions.
	require.ErrorIs(t, err, errResolverShuttingDown)
	require.Nil(t, nextResolver)
	require.True(t, wasCancelled)
	notifier.AssertExpectations(t)
}

// TestResolverPreparatorySpendDepth verifies the synchronous lookup used to
// prepare an already-incubating success output remains at one confirmation,
// independent of the channel's terminal depth.
func TestResolverPreparatorySpendDepth(t *testing.T) {
	t.Parallel()

	// Arrange a success resolver with a terminal policy of six but a closed
	// registration for its already-confirmed second-level transaction. The
	// mock.Mock-backed notifier must observe the compatibility depth once.
	const (
		broadcastHeight = uint32(31)
		terminalDepth   = uint32(6)
	)
	firstStageOutpoint := wire.OutPoint{Index: 8}
	pkScript := []byte{0x51}
	cancelled := make(chan struct{})
	spendEvent := closedResolverSpendEvent(cancelled)
	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On(
		"RegisterSpendNtfn", &firstStageOutpoint, pkScript,
		broadcastHeight,
		mock.MatchedBy(func(opts []chainntnfs.SpendOption) bool {
			// Parse the option in this test so the Arrange phase
			// exposes the preparatory one-confirmation policy.
			parsed, err := chainntnfs.ParseSpendOptions(opts...)
			return err == nil && parsed.NumConfs == 1
		}),
	).Return(spendEvent, nil).Once()
	resolver := newSuccessResolver(
		lnwallet.IncomingHtlcResolution{
			SignedSuccessTx: &wire.MsgTx{
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: firstStageOutpoint,
				}},
				TxOut: []*wire.TxOut{{Value: 1}},
			},
			SignDetails: &input.SignDetails{
				SignDesc: input.SignDescriptor{
					Output: &wire.TxOut{PkScript: pkScript},
				},
			},
		},
		broadcastHeight, channeldb.HTLC{},
		channeldb.SingleFunderTweaklessBit,
		ResolverConfig{
			ChannelArbitratorConfig: ChannelArbitratorConfig{
				SpendConfDepth: terminalDepth,
				ChainArbitratorConfig: ChainArbitratorConfig{
					Notifier: notifier,
				},
			},
		},
	)

	// Act by invoking only the synchronous preparation method. The closed
	// channel stops before sweep construction, isolating its registration
	// policy and deferred cleanup from unrelated sweeper behavior.
	err := resolver.sweepSuccessTxOutput()
	wasCancelled := resolverSpendCancelled(cancelled)

	// Assert the preparatory path returned the controlled shutdown error,
	// canceled its registration, and requested one confirmation despite the
	// resolver carrying a deeper terminal policy.
	require.ErrorIs(t, err, errResolverShuttingDown)
	require.True(t, wasCancelled)
	notifier.AssertExpectations(t)
}

// TestTimeoutResolverMinedPreimageDepth verifies a mempool-capable resolver
// learns an already-mined preimage before its terminal confirmation depth.
func TestTimeoutResolverMinedPreimageDepth(t *testing.T) {
	t.Parallel()

	// Arrange one remote-commit output, a pending mempool subscription, and
	// two block clients. The depth-one client carries a preimage missed by
	// the mempool, while the terminal client stays pending.
	const (
		broadcastHeight = uint32(41)
		terminalDepth   = uint32(3)
	)
	outpoint := wire.OutPoint{Index: 10}
	pkScript := []byte{0x51}
	earlyCancelled := make(chan struct{})
	matureCancelled := make(chan struct{})
	earlyEvent := chainntnfs.NewSpendEvent(func() {
		close(earlyCancelled)
	})
	matureEvent := chainntnfs.NewSpendEvent(func() {
		close(matureCancelled)
	})
	preimageSpend := &chainntnfs.SpendDetail{
		SpendingTx: &wire.MsgTx{TxIn: []*wire.TxIn{{
			Witness: wire.TxWitness{
				{1}, {1}, {1}, make([]byte, 32), {1},
			},
		}}},
		SpenderInputIndex: 0,
	}
	earlyEvent.Spend <- preimageSpend
	mempoolSource := chainntnfs.NewMempoolNotifier()
	mempoolEvent := mempoolSource.SubscribeInput(outpoint)
	mempool := chainntnfs.NewMockMempoolWatcher()
	mempool.On("SubscribeMempoolSpent", outpoint).Return(
		mempoolEvent, nil,
	).Once()
	mempool.On("CancelMempoolSpendEvent", mempoolEvent).Return().Once()
	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On(
		"RegisterSpendNtfn", &outpoint, pkScript, broadcastHeight,
		mock.MatchedBy(func(opts []chainntnfs.SpendOption) bool {
			// Parse the early option here so the test visibly
			// proves actionable preimages are requested at depth
			// one.
			parsed, err := chainntnfs.ParseSpendOptions(opts...)
			return err == nil && parsed.NumConfs == 1
		}),
	).Return(earlyEvent, nil).Once()
	notifier.On(
		"RegisterSpendNtfn", &outpoint, pkScript, broadcastHeight,
		mock.MatchedBy(func(opts []chainntnfs.SpendOption) bool {
			// Parse the terminal option here so the deeper cleanup
			// depth remains visible beside the early registration.
			parsed, err := chainntnfs.ParseSpendOptions(opts...)
			return err == nil && parsed.NumConfs == terminalDepth
		}),
	).Return(matureEvent, nil).Once()
	resolver := &htlcTimeoutResolver{
		contractResolverKit: *newContractResolverKit(ResolverConfig{
			ChannelArbitratorConfig: ChannelArbitratorConfig{
				ChainArbitratorConfig: ChainArbitratorConfig{
					Notifier: notifier,
					Mempool:  mempool,
				},
			},
		}),
		htlcResolution: lnwallet.OutgoingHtlcResolution{
			ClaimOutpoint: outpoint,
			SweepSignDesc: input.SignDescriptor{
				Output: &wire.TxOut{PkScript: pkScript},
			},
		},
		broadcastHeight: broadcastHeight,
	}

	// Act after the transaction has bypassed the pending mempool channel.
	// The early block client must still return its remote-commit preimage
	// without waiting for terminal-depth delivery.
	spend, err := resolver.waitForPreimageOrMatureSpend(
		&outpoint, pkScript, terminalDepth,
	)
	earlyWasCancelled := resolverSpendCancelled(earlyCancelled)
	matureWasCancelled := resolverSpendCancelled(matureCancelled)

	// Assert the early preimage won unchanged, every subscription
	// was released synchronously, and all exact mock calls were consumed.
	require.NoError(t, err)
	require.Same(t, preimageSpend, spend)
	require.True(t, earlyWasCancelled)
	require.True(t, matureWasCancelled)
	notifier.AssertExpectations(t)
	mempool.AssertExpectations(t)
}
