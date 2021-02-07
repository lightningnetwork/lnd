package contractcourt

import (
	"bytes"
	"io"
	"io/ioutil"
	"testing"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	testInitialBlockHeight = 100
	testHtlcExpiry         = 150
)

var (
	testResPreimage         = lntypes.Preimage{1, 2, 3}
	testResHash             = testResPreimage.Hash()
	testResCircuitKey       = channeldb.CircuitKey{}
	testOnionBlob           = []byte{4, 5, 6}
	testAcceptHeight  int32 = 1234
	testHtlcAmount          = 2300
)

// TestHtlcIncomingResolverFwdPreimageKnown tests resolution of a forwarded htlc
// for which the preimage is already known initially.
func TestHtlcIncomingResolverFwdPreimageKnown(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

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
	defer timeout(t)()

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
	defer timeout(t)()

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
	defer timeout(t)()

	ctx := newIncomingResolverTestContext(t, true)
	ctx.witnessBeacon.lookupPreimage[testResHash] = testResPreimage
	ctx.resolver.htlcExpiry = 90
	ctx.resolve()
	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverExitSettle tests resolution of an exit hop htlc for
// which the invoice has already been settled when the resolver starts.
func TestHtlcIncomingResolverExitSettle(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

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

	if !bytes.Equal(
		ctx.onionProcessor.offeredOnionBlob, testOnionBlob,
	) {
		t.Fatal("unexpected onion blob")
	}
}

// TestHtlcIncomingResolverExitCancel tests resolution of an exit hop htlc for
// an invoice that is already canceled when the resolver starts.
func TestHtlcIncomingResolverExitCancel(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

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
	defer timeout(t)()

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
	defer timeout(t)()

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
	defer timeout(t)()

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
	isExit bool
	hop.Iterator
}

func (h *mockHopIterator) HopPayload() (*hop.Payload, error) {
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
	}), nil
}

type mockOnionProcessor struct {
	isExit           bool
	offeredOnionBlob []byte
}

func (o *mockOnionProcessor) ReconstructHopIterator(r io.Reader, rHash []byte) (
	hop.Iterator, error) {

	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	o.offeredOnionBlob = data

	return &mockHopIterator{isExit: o.isExit}, nil
}

type incomingResolverTestContext struct {
	registry       *mockRegistry
	witnessBeacon  *mockWitnessBeacon
	resolver       *htlcIncomingContestResolver
	notifier       *mock.ChainNotifier
	onionProcessor *mockOnionProcessor
	resolveErr     chan error
	nextResolver   ContractResolver
	t              *testing.T
}

func newIncomingResolverTestContext(t *testing.T, isExit bool) *incomingResolverTestContext {
	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		SpendChan: make(chan *chainntnfs.SpendDetail),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	witnessBeacon := newMockWitnessBeacon()
	registry := &mockRegistry{
		notifyChan: make(chan notifyExitHopData, 1),
	}

	onionProcessor := &mockOnionProcessor{isExit: isExit}

	checkPointChan := make(chan struct{}, 1)

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier:       notifier,
			PreimageDB:     witnessBeacon,
			Registry:       registry,
			OnionProcessor: onionProcessor,
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
	resolver := &htlcIncomingContestResolver{
		htlcSuccessResolver: &htlcSuccessResolver{
			contractResolverKit: *newContractResolverKit(cfg),
			htlcResolution:      lnwallet.IncomingHtlcResolution{},
			htlc: channeldb.HTLC{
				Amt:       lnwire.MilliSatoshi(testHtlcAmount),
				RHash:     testResHash,
				OnionBlob: testOnionBlob,
			},
		},
		htlcExpiry: testHtlcExpiry,
	}

	return &incomingResolverTestContext{
		registry:       registry,
		witnessBeacon:  witnessBeacon,
		resolver:       resolver,
		notifier:       notifier,
		onionProcessor: onionProcessor,
		t:              t,
	}
}

func (i *incomingResolverTestContext) resolve() {
	// Start resolver.
	i.resolveErr = make(chan error, 1)
	go func() {
		var err error
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
