package contractcourt

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	testInitialBlockHeight = 100
	testHtlcExpiry         = 150
)

var (
	testResPreimage         = lntypes.Preimage{1, 2, 3}
	testResHash             = testResPreimage.Hash()
	testResCircuitKey       = models.CircuitKey{}
	testAcceptHeight  int32 = 1234
	testHtlcAmount          = 2300
)

// TestHtlcIncomingResolverFwdPreimageKnown tests resolution of a forwarded htlc
// for which the preimage is already known initially.
func TestHtlcIncomingResolverFwdPreimageKnown(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, false)
	ctx.witnessBeacon.lookupPreimage[testResHash] = testResPreimage
	ctx.resolve()
	ctx.waitForResult(true)
}

// TestHtlcIncomingResolverFwdContestedSuccess tests resolution of a forwarded
// htlc for which the preimage becomes known after the resolver has been
// started.
func TestHtlcIncomingResolverFwdContestedSuccess(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, false)
	ctx.resolve()

	// Simulate a new block coming in. HTLC is not yet expired.
	ctx.notifyBlockbeat(testInitialBlockHeight + 1)

	ctx.witnessBeacon.preImageUpdates <- testResPreimage
	ctx.waitForResult(true)
}

// TestHtlcIncomingResolverFwdContestedTimeout tests resolution of a forwarded
// htlc that times out after the resolver has been started.
func TestHtlcIncomingResolverFwdContestedTimeout(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, false)

	// Replace our checkpoint with one which will push reports into a
	// channel for us to consume. We replace this function on the resolver
	// itself because it is created by the test context.
	reportChan := make(chan *channeldb.ResolverReport, 1)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	ctx.resolve()

	// Simulate a new block coming in. HTLC expires.
	ctx.notifyBlockbeat(testHtlcExpiry)

	// Assert that we have a failure resolution because our invoice was
	// cancelled.
	assertResolverReport(t, reportChan, &channeldb.ResolverReport{
		Amount:          lnwire.MilliSatoshi(testHtlcAmount).ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
	})

	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverFwdExpiredPreimageKnown tests resolution of an
// expired forwarded HTLC whose preimage is already known when resolution
// starts.
func TestHtlcIncomingResolverFwdExpiredPreimageKnown(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, false)
	ctx.witnessBeacon.lookupPreimage[testResHash] = testResPreimage
	ctx.resolver.htlcExpiry = 90
	ctx.resolve()
	ctx.waitForResult(true)
}

// TestHtlcIncomingResolverLaunchAfterExpiry tests which preimage sources may
// start the success path once the HTLC has expired.
func TestHtlcIncomingResolverLaunchAfterExpiry(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		isExit       bool
		setup        func(*incomingResolverTestContext)
		expectLaunch bool
	}{
		{
			name:   "known preimage",
			isExit: false,
			setup: func(ctx *incomingResolverTestContext) {
				ctx.witnessBeacon.lookupPreimage[testResHash] =
					testResPreimage
			},
			expectLaunch: true,
		},
		{
			name:   "settlement replay",
			isExit: true,
			setup: func(ctx *incomingResolverTestContext) {
				ctx.registry.notifyResolution =
					invoices.NewSettleResolution(
						testResPreimage,
						testResCircuitKey,
						testAcceptHeight,
						invoices.ResultReplayToSettled,
					)
			},
			expectLaunch: true,
		},
		{
			name:   "fresh settlement",
			isExit: true,
			setup: func(ctx *incomingResolverTestContext) {
				ctx.registry.notifyResolution =
					invoices.NewSettleResolution(
						testResPreimage,
						testResCircuitKey,
						testAcceptHeight,
						invoices.ResultSettled,
					)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer timeout()()

			ctx := newIncomingResolverTestContext(t, tc.isExit)
			ctx.resolver.htlcExpiry = testInitialBlockHeight
			tc.setup(ctx)

			require.NoError(t, ctx.resolver.Launch())
			if tc.isExit {
				require.Len(t, ctx.registry.immediateNotify, 1)
				notify := ctx.registry.immediateNotify[0]
				require.Equal(
					t, int32(testInitialBlockHeight),
					notify.currentHeight,
				)
			}
			require.Equal(
				t, tc.expectLaunch, ctx.resolver.isLaunched(),
			)
			if tc.expectLaunch {
				require.Equal(
					t, [32]byte(testResPreimage),
					ctx.resolver.htlcResolution.Preimage,
				)
			} else {
				require.Zero(
					t, ctx.resolver.htlcResolution.Preimage,
				)
			}
		})
	}
}

// TestHtlcIncomingResolverLaunchUsesZeroHeightBeforeExpiry asserts that the
// on-chain lookup bypasses the registry's admission-time CLTV safety margin.
func TestHtlcIncomingResolverLaunchUsesZeroHeightBeforeExpiry(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, true)
	ctx.chainIO.setBestHeight(testHtlcExpiry - 1)
	ctx.registry.notifyResolution = invoices.NewSettleResolution(
		testResPreimage, testResCircuitKey, testAcceptHeight,
		invoices.ResultSettled,
	)

	require.NoError(t, ctx.resolver.Launch())
	require.True(t, ctx.resolver.isLaunched())
	require.Len(t, ctx.registry.immediateNotify, 1)
	require.Equal(
		t, int32(0),
		ctx.registry.immediateNotify[0].currentHeight,
	)
}

// TestHtlcIncomingResolverLaunchContinuesAfterLookupExpiry tests that a fresh
// settlement accepted before expiry still launches if the tip advances while
// the registry call returns.
func TestHtlcIncomingResolverLaunchContinuesAfterLookupExpiry(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, true)
	ctx.chainIO.setBestHeight(testHtlcExpiry - 1)
	ctx.registry.notifyResolution = invoices.NewSettleResolution(
		testResPreimage, testResCircuitKey, testAcceptHeight,
		invoices.ResultSettled,
	)
	ctx.registry.notifyHook = func() {
		ctx.chainIO.setBestHeight(testHtlcExpiry)
	}

	require.NoError(t, ctx.resolver.Launch())
	require.True(t, ctx.resolver.isLaunched())
	require.Len(t, ctx.registry.immediateNotify, 1)
	require.Equal(
		t, int32(0), ctx.registry.immediateNotify[0].currentHeight,
	)
	require.Equal(
		t, [32]byte(testResPreimage),
		ctx.resolver.htlcResolution.Preimage,
	)
}

// TestHtlcIncomingResolverExitSettle tests resolution of an exit hop htlc for
// which the invoice has already been settled when the resolver starts.
func TestHtlcIncomingResolverExitSettle(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, true)
	ctx.registry.notifyResolution = invoices.NewSettleResolution(
		testResPreimage, testResCircuitKey, testAcceptHeight,
		invoices.ResultReplayToSettled,
	)

	ctx.resolve()

	data := <-ctx.registry.notifyChan
	if data.expiry != testHtlcExpiry {
		t.Fatal("incorrect expiry")
	}
	if data.currentHeight != testInitialBlockHeight {
		t.Fatal("incorrect block height")
	}

	ctx.waitForResult(true)

	expetedOnion := lnmock.MockOnion()
	if !bytes.Equal(
		ctx.onionProcessor.offeredOnionBlob, expetedOnion[:],
	) {

		t.Fatal("unexpected onion blob")
	}
}

// TestHtlcIncomingResolverExitCancel tests resolution of an exit hop htlc for
// an invoice that is already canceled when the resolver starts.
func TestHtlcIncomingResolverExitCancel(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, true)
	ctx.registry.notifyResolution = invoices.NewFailResolution(
		testResCircuitKey, testAcceptHeight,
		invoices.ResultInvoiceAlreadyCanceled,
	)

	ctx.resolve()
	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverExitSettleHodl tests resolution of an exit hop htlc
// for a hodl invoice that is settled after the resolver has started.
func TestHtlcIncomingResolverExitSettleHodl(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, true)
	ctx.resolve()

	notifyData := <-ctx.registry.notifyChan
	notifyData.hodlChan <- invoices.NewSettleResolution(
		testResPreimage, testResCircuitKey, testAcceptHeight,
		invoices.ResultSettled,
	)

	ctx.waitForResult(true)
}

// TestHtlcIncomingResolverExitTimeoutHodl tests resolution of an exit hop htlc
// for a hodl invoice that times out.
func TestHtlcIncomingResolverExitTimeoutHodl(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, true)

	// Replace our checkpoint with one which will push reports into a
	// channel for us to consume. We replace this function on the resolver
	// itself because it is created by the test context.
	reportChan := make(chan *channeldb.ResolverReport, 1)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	ctx.resolve()
	ctx.notifyBlockbeat(testHtlcExpiry)

	// Assert that we have a failure resolution because our invoice was
	// cancelled.
	assertResolverReport(t, reportChan, &channeldb.ResolverReport{
		Amount:          lnwire.MilliSatoshi(testHtlcAmount).ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
	})

	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverExitCancelHodl tests resolution of an exit hop htlc
// for a hodl invoice that is canceled after the resolver has started.
func TestHtlcIncomingResolverExitCancelHodl(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, true)

	// Replace our checkpoint with one which will push reports into a
	// channel for us to consume. We replace this function on the resolver
	// itself because it is created by the test context.
	reportChan := make(chan *channeldb.ResolverReport)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	ctx.resolve()
	notifyData := <-ctx.registry.notifyChan
	notifyData.hodlChan <- invoices.NewFailResolution(
		testResCircuitKey, testAcceptHeight, invoices.ResultCanceled,
	)

	// Assert that we have a failure resolution because our invoice was
	// cancelled.
	assertResolverReport(t, reportChan, &channeldb.ResolverReport{
		Amount:          lnwire.MilliSatoshi(testHtlcAmount).ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeAbandoned,
	})

	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverInvalidFinalHtlc asserts that an exit-hop HTLC with
// final-hop details outside the expected range resolves without querying the
// invoice registry for a preimage.
func TestHtlcIncomingResolverInvalidFinalHtlc(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		cachePreimage bool
		mutate        func(*incomingResolverTestContext)
	}{{
		name: "expiry too far",
		mutate: func(ctx *incomingResolverTestContext) {
			ctx.resolver.htlcExpiry = testInitialBlockHeight +
				invoices.MaxFinalCltvDelta + 1
		},
	}, {
		name:          "cached preimage expiry too far",
		cachePreimage: true,
		mutate: func(ctx *incomingResolverTestContext) {
			ctx.resolver.htlcExpiry = testInitialBlockHeight +
				invoices.MaxFinalCltvDelta + 1
		},
	}, {
		name: "amount too low",
		mutate: func(ctx *incomingResolverTestContext) {
			ctx.onionProcessor.forwardAmount = testHtlcAmount + 1
		},
	}, {
		name: "final cltv too low",
		mutate: func(ctx *incomingResolverTestContext) {
			ctx.onionProcessor.outgoingCltv = testHtlcExpiry + 1
		},
	}}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			defer timeout()()

			ctx := newIncomingResolverTestContext(t, true)
			if testCase.cachePreimage {
				ctx.witnessBeacon.lookupPreimage[testResHash] =
					testResPreimage
			}

			testCase.mutate(ctx)
			resolution := invoices.NewSettleResolution(
				testResPreimage, testResCircuitKey,
				testAcceptHeight, invoices.ResultSettled,
			)
			ctx.registry.notifyResolution = resolution

			ctx.resolve()
			ctx.waitForResult(false)

			require.EqualValues(
				t, 0, ctx.registry.notifyCalls.Load(),
			)
		})
	}
}

// TestHtlcIncomingResolverCustomHtlc asserts that a custom HTLC bypasses the
// standard final-hop amount check in contract court, matching the link flow.
func TestHtlcIncomingResolverCustomHtlc(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newIncomingResolverTestContext(t, true)
	ctx.resolver.CustomHtlcChecker = fn.Some[CustomHtlcChecker](
		mockCustomHtlcChecker{},
	)
	ctx.onionProcessor.forwardAmount = testHtlcAmount + 1
	ctx.registry.notifyResolution = invoices.NewSettleResolution(
		testResPreimage, testResCircuitKey, testAcceptHeight,
		invoices.ResultSettled,
	)

	ctx.resolve()
	ctx.waitForResult(true)

	require.NotZero(t, ctx.registry.notifyCalls.Load())
}

type mockHopIterator struct {
	isExit        bool
	forwardAmount int
	outgoingCltv  uint32
	hop.Iterator
}

func (h *mockHopIterator) HopPayload() (*hop.Payload, hop.RouteRole, error) {
	var nextAddress [8]byte
	if !h.isExit {
		nextAddress = [8]byte{0x01}
	}

	forwardAmount := h.forwardAmount
	if forwardAmount == 0 {
		forwardAmount = 100
	}

	outgoingCltv := h.outgoingCltv
	if outgoingCltv == 0 {
		outgoingCltv = 40
	}

	return hop.NewLegacyPayload(&sphinx.HopData{
		Realm:         [1]byte{},
		NextAddress:   nextAddress,
		ForwardAmount: uint64(forwardAmount),
		OutgoingCltv:  outgoingCltv,
		ExtraBytes:    [12]byte{},
	}), hop.RouteRoleCleartext, nil
}

func (h *mockHopIterator) EncodeNextHop(w io.Writer) error {
	return nil
}

type mockOnionProcessor struct {
	isExit           bool
	forwardAmount    int
	outgoingCltv     uint32
	offeredOnionBlob []byte
}

func (o *mockOnionProcessor) ReconstructHopIterator(r io.Reader, rHash []byte,
	_ hop.ReconstructBlindingInfo) (hop.Iterator, error) {

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	o.offeredOnionBlob = data

	return &mockHopIterator{
		isExit:        o.isExit,
		forwardAmount: o.forwardAmount,
		outgoingCltv:  o.outgoingCltv,
	}, nil
}

type mockCustomHtlcChecker struct{}

func (m mockCustomHtlcChecker) IsCustomHTLC(lnwire.CustomRecords) bool {
	return true
}

type incomingResolverChainIO struct {
	*mock.ChainIO
	mu sync.RWMutex
}

func (i *incomingResolverChainIO) setBestHeight(height int32) {
	i.mu.Lock()
	i.BestHeight = height
	i.mu.Unlock()
}

func (i *incomingResolverChainIO) GetBestBlock() (*chainhash.Hash, int32,
	error) {

	i.mu.RLock()
	defer i.mu.RUnlock()

	return i.ChainIO.GetBestBlock()
}

type incomingResolverTestContext struct {
	registry               *mockRegistry
	witnessBeacon          *mockWitnessBeacon
	resolver               *htlcIncomingContestResolver
	notifier               *mock.ChainNotifier
	chainIO                *incomingResolverChainIO
	blockbeatSub           *incomingResolverBlockbeatSub
	blockbeatSubChan       chan *incomingResolverBlockbeatSub
	onionProcessor         *mockOnionProcessor
	resolveErr             chan error
	nextResolver           ContractResolver
	finalHtlcOutcomeStored bool
	t                      *testing.T
}

func newIncomingResolverTestContext(t *testing.T, isExit bool) *incomingResolverTestContext {
	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		SpendChan: make(chan *chainntnfs.SpendDetail),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	witnessBeacon := newMockWitnessBeacon()
	chainIO := &incomingResolverChainIO{
		ChainIO: &mock.ChainIO{BestHeight: testInitialBlockHeight},
	}
	registry := &mockRegistry{
		notifyChan: make(chan notifyExitHopData, 1),
	}

	onionProcessor := &mockOnionProcessor{isExit: isExit}

	checkPointChan := make(chan struct{}, 1)

	c := &incomingResolverTestContext{
		registry:         registry,
		witnessBeacon:    witnessBeacon,
		notifier:         notifier,
		chainIO:          chainIO,
		blockbeatSubChan: make(chan *incomingResolverBlockbeatSub, 1),
		onionProcessor:   onionProcessor,
		t:                t,
	}

	htlcNotifier := &mockHTLCNotifier{}

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier:       notifier,
			PreimageDB:     witnessBeacon,
			Registry:       registry,
			OnionProcessor: onionProcessor,
			PutFinalHtlcOutcome: func(chanId lnwire.ShortChannelID,
				htlcId uint64, settled bool) error {

				c.finalHtlcOutcomeStored = true

				return nil
			},
			HtlcNotifier: htlcNotifier,
			Budget:       *DefaultBudgetConfig(),
			QueryIncomingCircuit: func(
				circuit models.CircuitKey) *models.CircuitKey {

				return nil
			},
			Sweeper: newMockSweeper(),
			ChainIO: chainIO,
		},
		PutResolverReport: func(_ kvdb.RwTx,
			_ *channeldb.ResolverReport) error {

			return nil
		},
		subscribeBlockbeats: c.subscribeBeats,
	}

	cfg := ResolverConfig{
		ChannelArbitratorConfig: chainCfg,
		Checkpoint: func(_ ContractResolver,
			_ ...*channeldb.ResolverReport) error {

			checkPointChan <- struct{}{}
			return nil
		},
	}

	res := lnwallet.IncomingHtlcResolution{
		SweepSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{},
		},
	}

	c.resolver = &htlcIncomingContestResolver{
		htlcSuccessResolver: &htlcSuccessResolver{
			contractResolverKit: *newContractResolverKit(cfg),
			htlcResolution:      res,
			htlc: channeldb.HTLC{
				Amt:       lnwire.MilliSatoshi(testHtlcAmount),
				RHash:     testResHash,
				OnionBlob: lnmock.MockOnion(),
			},
		},
		htlcExpiry: testHtlcExpiry,
	}
	c.resolver.initLogger("htlcIncomingContestResolver")

	return c
}

type incomingResolverBlockbeatSub struct {
	sub    *blockbeatSubscription
	cancel func()
}

func (i *incomingResolverTestContext) resolve() {
	require.NoError(i.t, i.resolver.Launch())

	i.resolveErr = make(chan error, 1)
	go func() {
		nextResolver, err := i.resolver.Resolve()
		i.nextResolver = nextResolver
		i.resolveErr <- err
	}()
}

func (i *incomingResolverTestContext) subscribeBeats() (
	*blockbeatSubscription, func()) {

	sub := &blockbeatSubscription{
		blockbeatChan: make(chan blockbeatUpdate),
		quit:          make(chan struct{}),
	}
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			close(sub.quit)
		})
	}

	i.blockbeatSubChan <- &incomingResolverBlockbeatSub{
		sub:    sub,
		cancel: cancel,
	}

	return sub, cancel
}

func (i *incomingResolverTestContext) activeSub() *blockbeatSubscription {
	if i.blockbeatSub != nil {
		return i.blockbeatSub.sub
	}

	select {
	case i.blockbeatSub = <-i.blockbeatSubChan:
		return i.blockbeatSub.sub
	case <-time.After(time.Second):
		i.t.Fatal("timeout waiting for blockbeat subscription")
		return nil
	}
}

func (i *incomingResolverTestContext) cancelActiveSub() {
	i.blockbeatSub.cancel()
	i.blockbeatSub = nil
}

func (i *incomingResolverTestContext) notifyBlockbeat(height int32) {
	sub := i.activeSub()
	update := blockbeatUpdate{
		beat:    newBeatFromHeight(height),
		errChan: make(chan error, 1),
	}

	select {
	case sub.blockbeatChan <- update:
	case <-time.After(time.Second):
		i.t.Fatal("timeout sending blockbeat")
	}

	select {
	case err := <-update.errChan:
		require.NoError(i.t, err)
	case <-time.After(time.Second):
		i.t.Fatal("timeout waiting for blockbeat ack")
	}
}

func (i *incomingResolverTestContext) waitForResult(expectSuccessRes bool) {
	i.t.Helper()

	err := <-i.resolveErr
	if err != nil {
		i.t.Fatal(err)
	}

	if !expectSuccessRes {
		if i.nextResolver != nil {
			i.t.Fatal("expected no next resolver")
		}

		require.True(i.t, i.finalHtlcOutcomeStored,
			"expected final htlc outcome to be stored")

		return
	}

	successResolver, ok := i.nextResolver.(*htlcSuccessResolver)
	if !ok {
		i.t.Fatal("expected htlcSuccessResolver")
	}

	if successResolver.htlcResolution.Preimage != testResPreimage {
		i.t.Fatal("invalid preimage")
	}

	successTx := successResolver.htlcResolution.SignedSuccessTx
	if successTx != nil &&
		!bytes.Equal(successTx.TxIn[0].Witness[3], testResPreimage[:]) {

		i.t.Fatal("invalid preimage")
	}
}
