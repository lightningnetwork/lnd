package funding

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// blockingAuxLeafStore pauses commitment restoration before the pending remote
// commitment is read from the database.
type blockingAuxLeafStore struct {
	lnwallet.MockAuxLeafStore
	once    sync.Once
	entered chan struct{}
	resume  chan struct{}
	err     error
}

func (s *blockingAuxLeafStore) FetchLeavesFromCommit(
	_ lnwallet.AuxChanState, _ channeldb.ChannelCommitment,
	_ lnwallet.CommitmentKeyRing,
	_ lntypes.ChannelParty) fn.Result[lnwallet.CommitDiffAuxResult] {

	s.once.Do(func() {
		close(s.entered)
		<-s.resume
	})
	if s.err != nil {
		return fn.Err[lnwallet.CommitDiffAuxResult](s.err)
	}

	return fn.Ok(lnwallet.CommitDiffAuxResult{})
}

// TestFundingManagerDiscoverySignal verifies that an early channel_ready cannot
// start a link while the funding goroutine is still restoring commitment state.
// Processing must resume after restoration returns, even if it fails.
func TestFundingManagerDiscoverySignal(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		zeroConf     bool
		restoreError bool
	}{
		{name: "confirmed"},
		{name: "zero conf", zeroConf: true},
		{name: "confirmed restore error", restoreError: true},
		{
			name:         "zero conf restore error",
			zeroConf:     true,
			restoreError: true,
		},
	}
	for _, testCase := range testCases {
		for _, restart := range []bool{false, true} {
			name := testCase.name
			if restart {
				name += " restart"
			}
			t.Run(name, func(t *testing.T) {
				testFundingDiscoverySignal(
					t, testCase.zeroConf,
					testCase.restoreError, restart,
				)
			})
		}
	}
}

func testFundingDiscoverySignal(t *testing.T, zeroConf, restoreError,
	restart bool) {

	const timeout = 5 * time.Second

	store := &blockingAuxLeafStore{
		entered: make(chan struct{}),
		resume:  make(chan struct{}),
	}
	if restoreError || restart {
		store.err = errors.New("restore failed")
	}

	alice, bob := setupFundingManagers(
		t, func(cfg *Config) {
			if !cfg.IDKey.IsEqual(bobPubKey) {
				return
			}

			cfg.AuxLeafStore = fn.Some[lnwallet.AuxLeafStore](store)
			if zeroConf {
				cfg.OpenChannelPredicate =
					&mockZeroConfAcceptor{}
			}
		},
	)
	t.Cleanup(func() {
		select {
		case <-store.resume:
		default:
			close(store.resume)
		}
		tearDownFundingManagers(t, alice, bob)
	})

	var chanType *lnwire.ChannelType
	if zeroConf {
		features := []lnwire.FeatureBit{
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
			lnwire.ExplicitChannelTypeOptional,
			lnwire.StaticRemoteKeyOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
		}
		alice.localFeatures = features
		alice.remoteFeatures = features
		bob.localFeatures = features
		bob.remoteFeatures = features
		channelType := lnwire.ChannelType(
			*lnwire.NewRawFeatureVector(
				lnwire.ZeroConfRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
		)
		chanType = &channelType
	}

	updates := make(chan *lnrpc.OpenStatusUpdate)
	chanPoint, fundingTx := openChannel(
		t, alice, bob, 500_000, 0, 1, updates,
		false, chanType,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*chanPoint)
	if restart {
		if !zeroConf {
			sendAndCheckFirstConfirmation(
				t, bob, chanID, fundingTx,
			)
		}
		select {
		case <-store.entered:
		case <-time.After(timeout):
			t.Fatal("initial commitment restoration did not start")
		}

		// Simulate a restart after MarkAsOpen but before receiving
		// channel_ready. Fail the initial restore so that the old
		// funding goroutine can exit without advancing the opening
		// state.
		channel, err := bob.fundingMgr.cfg.FindChannel(
			alicePubKey, chanID,
		)
		require.NoError(t, err)
		require.False(t, channel.IsPending)
		require.Nil(t, channel.RemoteNextRevocation)
		assertDatabaseState(t, bob, chanPoint, markedOpen)
		close(store.resume)
		require.NoError(t, bob.fundingMgr.Stop())

		store = &blockingAuxLeafStore{
			entered: make(chan struct{}),
			resume:  make(chan struct{}),
		}
		if restoreError {
			store.err = errors.New("restore failed")
		}
		cfg := *bob.fundingMgr.cfg
		cfg.AuxLeafStore = fn.Some[lnwallet.AuxLeafStore](store)
		manager, err := NewFundingManager(cfg)
		require.NoError(t, err)
		bob.fundingMgr = manager
		require.NoError(t, manager.Start())
	}

	signal, ok := bob.fundingMgr.localDiscoverySignals.
		Load(chanID)
	require.True(t, ok, "channel without channel_ready needs a barrier")

	// Queue Alice's channel_ready before Bob finishes
	// opening the channel.
	channel, err := alice.fundingMgr.cfg.FindChannel(
		bobPubKey, chanID,
	)
	require.NoError(t, err)
	lnChannel, err := lnwallet.NewLightningChannel(
		nil, channel, nil,
	)
	require.NoError(t, err)
	nextRevocation, err := lnChannel.NextRevocationKey()
	require.NoError(t, err)
	channelReady := lnwire.NewChannelReady(chanID, nextRevocation)
	if zeroConf {
		channelReady.AliasScid = &alias
	}
	bob.fundingMgr.ProcessFundingMsg(channelReady, alice)
	require.NoError(t, wait.NoError(func() error {
		_, ok := bob.fundingMgr.
			handleChannelReadyBarriers.Load(chanID)
		if !ok {
			return errors.New("channel_ready not queued")
		}
		return nil
	}, timeout))

	if !zeroConf && !restart {
		sendAndCheckFirstConfirmation(
			t, bob, chanID, fundingTx,
		)
	}

	select {
	case <-store.entered:
	case <-time.After(timeout):
		t.Fatal("commitment restoration did not start")
	}

	select {
	case <-signal:
		t.Fatal("discovery signal released before " +
			"commitment restoration finished")
	default:
	}
	select {
	case <-alice.newChannels:
		t.Fatal("link started during restoration")
	default:
	}

	close(store.resume)
	select {
	case <-signal:
	case <-time.After(timeout):
		t.Fatal("discovery signal was not released")
	}
	if !restoreError {
		assertFundingMsgSent(t, bob.msgChan, "ChannelReady")
	}
	select {
	case msg := <-alice.newChannels:
		close(msg.err)
	case <-time.After(timeout):
		t.Fatal("queued channel_ready was not processed")
	}
	require.NoError(t, wait.NoError(func() error {
		_, discovery := bob.fundingMgr.localDiscoverySignals.Load(
			chanID,
		)
		_, handling := bob.fundingMgr.handleChannelReadyBarriers.Load(
			chanID,
		)
		if discovery || handling {
			return errors.New("channel_ready barriers not removed")
		}
		return nil
	}, timeout))
	channel, err = bob.fundingMgr.cfg.FindChannel(alicePubKey, chanID)
	require.NoError(t, err)
	require.NotNil(t, channel.RemoteNextRevocation)
}
