package contractcourt

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
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
	"github.com/lightningnetwork/lnd/record"
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
	ctx.notifyEpoch(testInitialBlockHeight + 1)

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

	// Simulate a new block coming in. HTLC expires.
	ctx.notifyEpoch(testHtlcExpiry)

	// Assert that we have a failure resolution because our invoice was
	// cancelled.
	assertResolverReport(t, reportChan, &channeldb.ResolverReport{
		Amount:          lnwire.MilliSatoshi(testHtlcAmount).ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeIncomingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
	})

	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverFwdTimeout tests resolution of a forwarded htlc that
// has already expired when the resolver starts.
func TestHtlcIncomingResolverFwdTimeout(t *testing.T) {
	t.Parallel()
	defer timeout()()

	// Arrange: start an already expired forwarded HTLC without a known
	// preimage.
	ctx := newIncomingResolverTestContext(t, false)
	ctx.resolver.htlcExpiry = 90

	// Act: resolve the expired HTLC.
	ctx.resolve()

	// Assert: no success resolver is returned and the HTLC is failed.
	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverLaunchUsesKnownPreimageAfterExpiry asserts that
// Launch still offers the HTLC to the sweeper when the preimage was already
// learned before restart, even if the HTLC is now expired.
func TestHtlcIncomingResolverLaunchUsesKnownPreimageAfterExpiry(t *testing.T) {
	t.Parallel()
	defer timeout()()

	tests := []struct {
		name   string
		isExit bool
		setup  func(*incomingResolverTestContext)
		assert func(*testing.T, *incomingResolverTestContext)
	}{
		{
			name:   "preimage db",
			isExit: false,
			setup: func(ctx *incomingResolverTestContext) {
				ctx.witnessBeacon.lookupPreimage[testResHash] =
					testResPreimage
			},
			assert: func(t *testing.T,
				ctx *incomingResolverTestContext) {

				require.Empty(t, ctx.registry.immediateNotify)
			},
		},
		{
			name:   "settled invoice lookup",
			isExit: true,
			setup: func(ctx *incomingResolverTestContext) {
				ctx.registry.lookupInvoiceSet = true
				ctx.registry.lookupInvoice =
					settledTestInvoice()
			},
			assert: func(t *testing.T,
				ctx *incomingResolverTestContext) {

				require.Equal(t, 1, ctx.registry.lookupCount)
				require.Empty(t, ctx.registry.immediateNotify)
			},
		},
		{
			name:   "settled AMP invoice lookup",
			isExit: true,
			setup: func(ctx *incomingResolverTestContext) {
				ctx.onionProcessor.payload = ampTestPayload()
				ctx.registry.lookupInvoiceSet = true
				ctx.registry.lookupInvoice =
					settledAMPTestInvoice()
			},
			assert: func(t *testing.T,
				ctx *incomingResolverTestContext) {

				require.Equal(t, 1, ctx.registry.lookupCount)
				require.Empty(t, ctx.registry.immediateNotify)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange: restart at the HTLC expiry with the preimage
			// already available from this test's source.
			ctx := newIncomingResolverTestContext(t, test.isExit)
			ctx.resolver.htlcExpiry = testInitialBlockHeight
			test.setup(ctx)

			// Act: launch the resolver, then enter the production
			// Resolve path at the HTLC expiry height.
			require.NoError(t, ctx.resolver.Launch())
			resolveResultChan := make(chan resolveResult, 1)
			go func() {
				nextResolver, err := ctx.resolver.Resolve()
				resolveResultChan <- resolveResult{
					nextResolver: nextResolver,
					err:          err,
				}
			}()
			ctx.notifyEpoch(testInitialBlockHeight)

			// Assert: the known preimage still starts a sweep after
			// expiry. Resolve also returns the launched success
			// resolver instead of timing out.
			require.True(t, ctx.resolver.isLaunched())
			preimage := lntypes.Preimage(
				ctx.resolver.htlcResolution.Preimage,
			)
			require.Equal(
				t, testResPreimage, preimage,
			)

			sweeper, ok := ctx.resolver.Sweeper.(*mockSweeper)
			require.True(t, ok)
			select {
			case <-sweeper.sweptInputs:
			case <-time.After(time.Second):
				t.Fatal("expected known preimage sweep")
			}

			select {
			case result := <-resolveResultChan:
				require.NoError(t, result.err)
				require.Same(
					t, ctx.resolver.htlcSuccessResolver,
					result.nextResolver,
				)
			case <-time.After(time.Second):
				t.Fatal("expected launched success resolver")
			}

			test.assert(t, ctx)
		})
	}
}

// TestHtlcIncomingResolverLaunchIgnoresOtherSettledCircuitAfterExpiry asserts
// that Launch only consumes settled invoices for this exact circuit.
func TestHtlcIncomingResolverLaunchIgnoresOtherSettledCircuitAfterExpiry(
	t *testing.T) {

	t.Parallel()
	defer timeout()()

	// Arrange: set up an expired exit-hop resolver where another HTLC on
	// the same invoice has already settled.
	ctx := newIncomingResolverTestContext(t, true)
	ctx.resolver.htlcExpiry = testInitialBlockHeight
	ctx.registry.lookupInvoiceSet = true
	ctx.registry.lookupInvoice = settledOtherCircuitTestInvoice()

	// Act: launch runs the immediate lookup, then Resolve runs at expiry.
	require.NoError(t, ctx.resolver.Launch())
	ctx.resolveErr = make(chan error, 1)
	go func() {
		var err error
		ctx.nextResolver, err = ctx.resolver.Resolve()
		ctx.resolveErr <- err
	}()
	ctx.notifyEpoch(testInitialBlockHeight)

	// Assert: the other settled HTLC was not consumed by Launch and the
	// expired HTLC fails rather than returning a success resolver.
	require.Equal(t, 1, ctx.registry.lookupCount)
	require.Empty(t, ctx.registry.immediateNotify)
	require.False(t, ctx.resolver.isLaunched())
	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverLaunchUsesSideEffectFreeLookup asserts that Launch
// uses a side-effect-free invoice-registry lookup.
func TestHtlcIncomingResolverLaunchUsesSideEffectFreeLookup(t *testing.T) {
	t.Parallel()
	defer timeout()()

	// Arrange: set up an expired exit-hop resolver with a settled invoice
	// that Launch can consume immediately.
	ctx := newIncomingResolverTestContext(t, true)
	ctx.resolver.htlcExpiry = testInitialBlockHeight
	ctx.chainIO.BestHeight = testInitialBlockHeight + 1
	ctx.registry.lookupInvoiceSet = true
	ctx.registry.lookupInvoice = settledTestInvoice()

	// Act: launch performs the immediate invoice lookup.
	require.NoError(t, ctx.resolver.Launch())

	// Assert: the immediate lookup did not use the mutating registry path.
	require.Equal(t, 1, ctx.registry.lookupCount)
	require.Empty(t, ctx.registry.immediateNotify)
	require.True(t, ctx.resolver.isLaunched())
}

// TestHtlcIncomingResolverLaunchIgnoresOpenInvoiceAfterExpiry asserts that an
// immediate launch-time invoice lookup does not bypass expiry checks by
// consuming an open invoice.
func TestHtlcIncomingResolverLaunchIgnoresOpenInvoiceAfterExpiry(t *testing.T) {
	t.Parallel()
	defer timeout()()

	// Arrange: set up an expired exit-hop resolver where the invoice lookup
	// finds an open invoice with a known preimage.
	ctx := newIncomingResolverTestContext(t, true)
	ctx.resolver.htlcExpiry = testInitialBlockHeight
	ctx.registry.lookupInvoiceSet = true
	ctx.registry.lookupInvoice = invoices.Invoice{
		State: invoices.ContractOpen,
		Terms: invoices.ContractTerm{
			PaymentPreimage: &testResPreimage,
		},
	}

	// Act: launch runs the immediate lookup, then Resolve runs at expiry.
	require.NoError(t, ctx.resolver.Launch())
	ctx.resolveErr = make(chan error, 1)
	go func() {
		var err error
		ctx.nextResolver, err = ctx.resolver.Resolve()
		ctx.resolveErr <- err
	}()
	ctx.notifyEpoch(testInitialBlockHeight)

	// Assert: the open invoice was not consumed by Launch and the expired
	// HTLC fails rather than returning a success resolver.
	require.Equal(t, 1, ctx.registry.lookupCount)
	require.Empty(t, ctx.registry.immediateNotify)
	require.False(t, ctx.resolver.isLaunched())
	ctx.waitForResult(false)
}

// settledTestInvoice returns a settled invoice with the test preimage.
func settledTestInvoice() invoices.Invoice {
	return invoices.Invoice{
		State: invoices.ContractSettled,
		Terms: invoices.ContractTerm{
			PaymentPreimage: &testResPreimage,
		},
		Htlcs: map[models.CircuitKey]*invoices.InvoiceHTLC{
			testResCircuitKey: {
				State: invoices.HtlcStateSettled,
			},
		},
	}
}

// settledOtherCircuitTestInvoice returns a settled invoice whose settled HTLC
// is not the circuit used by the resolver.
func settledOtherCircuitTestInvoice() invoices.Invoice {
	invoice := settledTestInvoice()
	invoice.Htlcs = map[models.CircuitKey]*invoices.InvoiceHTLC{
		{HtlcID: 99}: {
			State: invoices.HtlcStateSettled,
		},
	}

	return invoice
}

// settledAMPTestInvoice returns a settled AMP invoice with the test preimage on
// the exact HTLC rather than the invoice terms.
func settledAMPTestInvoice() invoices.Invoice {
	return invoices.Invoice{
		State: invoices.ContractSettled,
		Htlcs: map[models.CircuitKey]*invoices.InvoiceHTLC{
			testResCircuitKey: {
				State: invoices.HtlcStateSettled,
				AMP: &invoices.InvoiceHtlcAMPData{
					Preimage: &testResPreimage,
				},
			},
		},
	}
}

// ampTestPayload returns an exit-hop AMP payload.
func ampTestPayload() *hop.Payload {
	return &hop.Payload{
		FwdInfo: hop.ForwardingInfo{
			NextHop: hop.Exit,
		},
		MPP: record.NewMPP(
			lnwire.MilliSatoshi(testHtlcAmount), [32]byte{1},
		),
		AMP: record.NewAMP([32]byte{2}, [32]byte{3}, 4),
	}
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
	ctx.notifyEpoch(testHtlcExpiry)

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

type mockHopIterator struct {
	isExit  bool
	payload *hop.Payload
	hop.Iterator
}

func (h *mockHopIterator) HopPayload() (*hop.Payload, hop.RouteRole, error) {
	if h.payload != nil {
		return h.payload, hop.RouteRoleCleartext, nil
	}

	var nextAddress [8]byte
	if !h.isExit {
		nextAddress = [8]byte{0x01}
	}

	return hop.NewLegacyPayload(&sphinx.HopData{
		Realm:         [1]byte{},
		NextAddress:   nextAddress,
		ForwardAmount: 100,
		OutgoingCltv:  40,
		ExtraBytes:    [12]byte{},
	}), hop.RouteRoleCleartext, nil
}

func (h *mockHopIterator) EncodeNextHop(w io.Writer) error {
	return nil
}

type mockOnionProcessor struct {
	isExit           bool
	payload          *hop.Payload
	offeredOnionBlob []byte
}

func (o *mockOnionProcessor) ReconstructHopIterator(r io.Reader, rHash []byte,
	_ hop.ReconstructBlindingInfo) (hop.Iterator, error) {

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	o.offeredOnionBlob = data

	return &mockHopIterator{isExit: o.isExit, payload: o.payload}, nil
}

type incomingResolverTestContext struct {
	registry               *mockRegistry
	witnessBeacon          *mockWitnessBeacon
	resolver               *htlcIncomingContestResolver
	notifier               *mock.ChainNotifier
	chainIO                *mock.ChainIO
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
	chainIO := &mock.ChainIO{
		BestHeight: testInitialBlockHeight,
	}
	registry := &mockRegistry{
		notifyChan: make(chan notifyExitHopData, 1),
	}

	onionProcessor := &mockOnionProcessor{isExit: isExit}

	checkPointChan := make(chan struct{}, 1)

	c := &incomingResolverTestContext{
		registry:       registry,
		witnessBeacon:  witnessBeacon,
		notifier:       notifier,
		chainIO:        chainIO,
		onionProcessor: onionProcessor,
		t:              t,
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

func (i *incomingResolverTestContext) resolve() {
	// Start resolver.
	i.resolveErr = make(chan error, 1)
	go func() {
		var err error

		err = i.resolver.Launch()
		require.NoError(i.t, err)

		i.nextResolver, err = i.resolver.Resolve()
		i.resolveErr <- err
	}()

	// Notify initial block height.
	i.notifyEpoch(testInitialBlockHeight)
}

func (i *incomingResolverTestContext) notifyEpoch(height int32) {
	i.notifier.EpochChan <- &chainntnfs.BlockEpoch{
		Height: height,
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
