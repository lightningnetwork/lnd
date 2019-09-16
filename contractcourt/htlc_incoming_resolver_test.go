package contractcourt

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwallet"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	testInitialBlockHeight = 100
	testHtlcExpiry         = 150
)

var (
	testResPreimage   = lntypes.Preimage{1, 2, 3}
	testResHash       = testResPreimage.Hash()
	testResCircuitKey = channeldb.CircuitKey{}
)

// TestHtlcIncomingResolverFwdPreimageKnown tests resolution of a forwarded htlc
// for which the preimage is already known initially.
func TestHtlcIncomingResolverFwdPreimageKnown(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

	ctx := newIncomingResolverTestContext(t)
	ctx.registry.notifyErr = channeldb.ErrInvoiceNotFound
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

	ctx := newIncomingResolverTestContext(t)
	ctx.registry.notifyErr = channeldb.ErrInvoiceNotFound
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

	ctx := newIncomingResolverTestContext(t)
	ctx.registry.notifyErr = channeldb.ErrInvoiceNotFound
	ctx.resolve()

	// Simulate a new block coming in. HTLC expires.
	ctx.notifyEpoch(testHtlcExpiry)

	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverFwdTimeout tests resolution of a forwarded htlc that
// has already expired when the resolver starts.
func TestHtlcIncomingResolverFwdTimeout(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

	ctx := newIncomingResolverTestContext(t)

	ctx.registry.notifyErr = channeldb.ErrInvoiceNotFound
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

	ctx := newIncomingResolverTestContext(t)
	ctx.registry.notifyEvent = &invoices.HodlEvent{
		CircuitKey: testResCircuitKey,
		Preimage:   &testResPreimage,
	}
	ctx.resolve()

	data := <-ctx.registry.notifyChan
	if data.expiry != testHtlcExpiry {
		t.Fatal("incorrect expiry")
	}
	if data.currentHeight != testInitialBlockHeight {
		t.Fatal("incorrect block height")
	}

	ctx.waitForResult(true)
}

// TestHtlcIncomingResolverExitCancel tests resolution of an exit hop htlc for
// an invoice that is already canceled when the resolver starts.
func TestHtlcIncomingResolverExitCancel(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

	ctx := newIncomingResolverTestContext(t)
	ctx.registry.notifyEvent = &invoices.HodlEvent{
		CircuitKey: testResCircuitKey,
	}
	ctx.resolve()
	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverExitSettleHodl tests resolution of an exit hop htlc
// for a hodl invoice that is settled after the resolver has started.
func TestHtlcIncomingResolverExitSettleHodl(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

	ctx := newIncomingResolverTestContext(t)
	ctx.resolve()

	notifyData := <-ctx.registry.notifyChan
	notifyData.hodlChan <- invoices.HodlEvent{
		CircuitKey: testResCircuitKey,
		Preimage:   &testResPreimage,
	}

	ctx.waitForResult(true)
}

// TestHtlcIncomingResolverExitTimeoutHodl tests resolution of an exit hop htlc
// for a hodl invoice that times out.
func TestHtlcIncomingResolverExitTimeoutHodl(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

	ctx := newIncomingResolverTestContext(t)
	ctx.resolve()
	ctx.notifyEpoch(testHtlcExpiry)
	ctx.waitForResult(false)
}

// TestHtlcIncomingResolverExitCancelHodl tests resolution of an exit hop htlc
// for a hodl invoice that is canceled after the resolver has started.
func TestHtlcIncomingResolverExitCancelHodl(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

	ctx := newIncomingResolverTestContext(t)
	ctx.resolve()
	notifyData := <-ctx.registry.notifyChan
	notifyData.hodlChan <- invoices.HodlEvent{
		CircuitKey: testResCircuitKey,
	}
	ctx.waitForResult(false)
}

type incomingResolverTestContext struct {
	registry      *mockRegistry
	witnessBeacon *mockWitnessBeacon
	resolver      *htlcIncomingContestResolver
	notifier      *mockNotifier
	resolveErr    chan error
	nextResolver  ContractResolver
	t             *testing.T
}

func newIncomingResolverTestContext(t *testing.T) *incomingResolverTestContext {
	notifier := &mockNotifier{
		epochChan: make(chan *chainntnfs.BlockEpoch),
		spendChan: make(chan *chainntnfs.SpendDetail),
		confChan:  make(chan *chainntnfs.TxConfirmation),
	}
	witnessBeacon := newMockWitnessBeacon()
	registry := &mockRegistry{
		notifyChan: make(chan notifyExitHopData, 1),
	}

	checkPointChan := make(chan struct{}, 1)

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier:   notifier,
			PreimageDB: witnessBeacon,
			Registry:   registry,
		},
	}

	resolver := &htlcIncomingContestResolver{
		htlcSuccessResolver: htlcSuccessResolver{
			ResolverKit: ResolverKit{
				ChannelArbitratorConfig: chainCfg,
				Checkpoint: func(_ ContractResolver) error {
					checkPointChan <- struct{}{}
					return nil
				},
			},
			htlcResolution: lnwallet.IncomingHtlcResolution{},
			payHash:        testResHash,
		},
		htlcExpiry: testHtlcExpiry,
	}

	return &incomingResolverTestContext{
		registry:      registry,
		witnessBeacon: witnessBeacon,
		resolver:      resolver,
		notifier:      notifier,
		t:             t,
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
	i.notifier.epochChan <- &chainntnfs.BlockEpoch{
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
		if err != nil {
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
