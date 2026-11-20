package invoices_test

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type mockPayload struct {
	mpp           *record.MPP
	amp           *record.AMP
	customRecords record.CustomSet
	metadata      []byte
	pathID        *chainhash.Hash
	totalAmtMsat  lnwire.MilliSatoshi
}

func (p *mockPayload) MultiPath() *record.MPP {
	return p.mpp
}

func (p *mockPayload) AMPRecord() *record.AMP {
	return p.amp
}

func (p *mockPayload) PathID() *chainhash.Hash {
	return p.pathID
}

func (p *mockPayload) TotalAmtMsat() lnwire.MilliSatoshi {
	return p.totalAmtMsat
}

func (p *mockPayload) CustomRecords() record.CustomSet {
	// This function should always return a map instance, but for mock
	// configuration we do accept nil.
	if p.customRecords == nil {
		return make(record.CustomSet)
	}

	return p.customRecords
}

func (p *mockPayload) Metadata() []byte {
	return p.metadata
}

type mockChainNotifier struct {
	chainntnfs.ChainNotifier

	blockChan chan *chainntnfs.BlockEpoch
}

func newMockNotifier() *mockChainNotifier {
	return &mockChainNotifier{
		blockChan: make(chan *chainntnfs.BlockEpoch),
	}
}

// RegisterBlockEpochNtfn mocks a block epoch notification, using the mock's
// block channel to deliver blocks to the client.
func (m *mockChainNotifier) RegisterBlockEpochNtfn(*chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: m.blockChan,
		Cancel: func() {},
	}, nil
}

const (
	testHtlcExpiry = uint32(5)

	testInvoiceCltvDelta = uint32(4)

	testFinalCltvRejectDelta = int32(4)

	testCurrentHeight = int32(1)
)

var (
	testTimeout = 5 * time.Second

	testTimeoutLong = time.Minute

	testTime = time.Date(2018, time.February, 2, 14, 0, 0, 0, time.UTC)

	testInvoicePreimage = lntypes.Preimage{1}

	testInvoicePaymentHash = testInvoicePreimage.Hash()

	testPrivKeyBytes, _ = hex.DecodeString(
		"e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2d" +
			"b734",
	)

	testPrivKey, _ = btcec.PrivKeyFromBytes(testPrivKeyBytes)

	testInvoiceDescription = "coffee"

	testInvoiceAmount = lnwire.MilliSatoshi(100000)

	testNetParams = &chaincfg.MainNetParams

	testMessageSigner = zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			hash := chainhash.HashB(msg)

			return ecdsa.SignCompact(testPrivKey, hash, true), nil
		},
	}

	testFeatures = lnwire.NewFeatureVector(
		nil, lnwire.Features,
	)

	testPayload = &mockPayload{}

	testInvoiceCreationDate = testTime
)

type testContext struct {
	idb        invpkg.InvoiceDB
	registry   *invpkg.InvoiceRegistry
	notifier   *mockChainNotifier
	clock      *clock.TestClock
	finalHtlcs *mockFinalHtlcSource

	t *testing.T
}

// mockFinalHtlcSource supplies arranged recovery evidence and a test-owned
// live channel without implementing a second final-outcome store.
type mockFinalHtlcSource struct {
	mock.Mock

	outcomes chan invpkg.FinalHtlcOutcome
}

// newMockFinalHtlcSource arranges one subscription and optional empty lookups
// for ordinary fixture use. Regression tests replace those optional calls in
// Arrange when they need specific recovery evidence or ordering.
func newMockFinalHtlcSource(t *testing.T) *mockFinalHtlcSource {
	t.Helper()

	source := &mockFinalHtlcSource{
		outcomes: make(chan invpkg.FinalHtlcOutcome),
	}
	source.On("SubscribeFinalHtlcs").Return(
		(<-chan invpkg.FinalHtlcOutcome)(source.outcomes),
		func() {}, nil,
	).Once()
	source.On("LookupFinalHtlcs", mock.Anything).Return(nil, nil).Maybe()
	source.On("InferFinalHtlcs", mock.Anything).Return(nil, nil).Maybe()

	// Fixtures register Stop later, so LIFO cleanup stops all consumers
	// before this single expectation verification reads the mock.
	t.Cleanup(func() {
		source.AssertExpectations(t)
	})

	return source
}

// SubscribeFinalHtlcs returns the arranged test-owned event channel and its
// cancellation callback so subscription ownership matches the real source.
func (m *mockFinalHtlcSource) SubscribeFinalHtlcs() (
	<-chan invpkg.FinalHtlcOutcome, func(), error) {

	args := m.Called()
	outcomes, _ := args.Get(0).(<-chan invpkg.FinalHtlcOutcome)
	cancel, _ := args.Get(1).(func())

	return outcomes, cancel, args.Error(2)
}

// LookupFinalHtlcs returns only the durable evidence arranged for these keys;
// nil represents a source that has not recorded any of their outcomes.
func (m *mockFinalHtlcSource) LookupFinalHtlcs(keys []invpkg.CircuitKey) (
	map[invpkg.CircuitKey]invpkg.FinalHtlcOutcome, error) {

	args := m.Called(keys)
	res, _ := args.Get(0).(map[invpkg.CircuitKey]invpkg.FinalHtlcOutcome)

	return res, args.Error(1)
}

// InferFinalHtlcs returns arranged channel evidence independently of recorded
// outcomes so tests can control the real reconciliation boundary.
func (m *mockFinalHtlcSource) InferFinalHtlcs(keys []invpkg.CircuitKey) (
	map[invpkg.CircuitKey]invpkg.FinalHtlcOutcome, error) {

	args := m.Called(keys)
	res, _ := args.Get(0).(map[invpkg.CircuitKey]invpkg.FinalHtlcOutcome)

	return res, args.Error(1)
}

var _ invpkg.FinalHtlcSource = (*mockFinalHtlcSource)(nil)

func defaultRegistryConfig() invpkg.RegistryConfig {
	return invpkg.RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		HtlcHoldDuration:     30 * time.Second,
		HtlcInterceptor:      &invpkg.MockHtlcModifier{},
	}
}

// newTestContext owns a registry backed by the selected real database and a
// controlled finality source, stopping consumers before verifying the mock.
func newTestContext(t *testing.T,
	registryCfg *invpkg.RegistryConfig,
	makeDB func(t *testing.T) (invpkg.InvoiceDB,
		*clock.TestClock)) *testContext {

	t.Helper()

	idb, clock := makeDB(t)
	notifier := newMockNotifier()

	expiryWatcher := invpkg.NewInvoiceExpiryWatcher(
		clock, 0, uint32(testCurrentHeight), nil, notifier,
	)

	cfg := defaultRegistryConfig()
	if registryCfg != nil {
		cfg = *registryCfg
	}
	cfg.Clock = clock

	finalHtlcs, ok := cfg.FinalHtlcSource.(*mockFinalHtlcSource)
	if !ok {
		finalHtlcs = newMockFinalHtlcSource(t)
		cfg.FinalHtlcSource = finalHtlcs
	}

	// Instantiate and start the invoice ctx.registry.
	registry := invpkg.NewRegistry(idb, expiryWatcher, &cfg)

	require.NoError(t, registry.Start())
	t.Cleanup(func() {
		require.NoError(t, registry.Stop())
	})

	ctx := testContext{
		idb:        idb,
		registry:   registry,
		notifier:   notifier,
		clock:      clock,
		finalHtlcs: finalHtlcs,
		t:          t,
	}

	return &ctx
}

// finalizeHtlc delivers the final outcome of an htlc to the registry, like
// the link or the contract court would, and waits until the registry recorded
// it on the invoice identified by ref.
func (c *testContext) finalizeHtlc(ref invpkg.InvoiceRef,
	key invpkg.CircuitKey, settled bool) {

	c.t.Helper()

	outcome := invpkg.FinalHtlcOutcome{
		Key:      key,
		Settled:  settled,
		Offchain: true,
	}

	select {
	case c.finalHtlcs.outcomes <- outcome:
	case <-time.After(testTimeout):
		c.t.Fatalf("registry didn't receive final outcome of %v", key)
	}

	c.waitHtlcFinal(ref, key)
}

// waitHtlcFinal waits until the htlc on the invoice identified by ref is no
// longer pending settlement.
func (c *testContext) waitHtlcFinal(ref invpkg.InvoiceRef,
	key invpkg.CircuitKey) {

	c.t.Helper()

	isFinal := func() bool {
		inv, err := c.registry.LookupInvoiceByRef(
			c.t.Context(), ref,
		)
		if err != nil {
			return false
		}

		htlc, ok := inv.Htlcs[key]

		return ok && htlc.State != invpkg.HtlcStatePendingSettle
	}

	// The outcome is usually recorded right away, so avoid waiting for the
	// first tick.
	if isFinal() {
		return
	}

	require.Eventually(c.t, isFinal, testTimeout, 5*time.Millisecond)
}

func getCircuitKey(htlcID uint64) invpkg.CircuitKey {
	return invpkg.CircuitKey{
		ChanID: lnwire.ShortChannelID{
			BlockHeight: 1, TxIndex: 2, TxPosition: 3,
		},
		HtlcID: htlcID,
	}
}

// newInvoice returns an invoice that can be used for testing, using the
// constant values defined above (deep copied if necessary).
//
// Note that this invoice *does not* have a payment address set. It will
// create a regular invoice with a preimage is hodl is false, and a hodl
// invoice with no preimage otherwise.
func newInvoice(t *testing.T, hodl bool, ampInvoice bool) *invpkg.Invoice {
	invoice := &invpkg.Invoice{
		Terms: invpkg.ContractTerm{
			Value:    testInvoiceAmount,
			Expiry:   time.Hour,
			Features: testFeatures.Clone(),
		},
		CreationDate: testInvoiceCreationDate,
	}

	// This makes the invoice an AMP invoice. We do not support AMP hodl
	// invoices.
	if ampInvoice {
		ampFeature := lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.PaymentAddrOptional,
			lnwire.AMPRequired,
		)

		ampFeatures := lnwire.NewFeatureVector(
			ampFeature, lnwire.Features,
		)
		invoice.Terms.Features = ampFeatures

		return invoice
	}

	// If creating a hodl invoice, we don't include a preimage.
	if hodl {
		invoice.HodlInvoice = true
		return invoice
	}

	preimage, err := lntypes.MakePreimage(
		testInvoicePreimage[:],
	)
	require.NoError(t, err)
	invoice.Terms.PaymentPreimage = &preimage

	return invoice
}

// timeout implements a test level timeout.
func timeout() func() {
	done := make(chan struct{})

	go func() {
		select {
		// Use a longer timeout to accommodate slow Postgres database
		// setup and migrations when running tests in parallel.
		case <-time.After(testTimeoutLong):
			err := pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
			if err != nil {
				panic(fmt.Sprintf("error writing to std out "+
					"after timeout: %v", err))
			}
			panic("timeout")
		case <-done:
		}
	}()

	return func() {
		close(done)
	}
}

// invoiceExpiryTestData simply holds generated expired and pending invoices.
type invoiceExpiryTestData struct {
	expiredInvoices map[lntypes.Hash]*invpkg.Invoice
	pendingInvoices map[lntypes.Hash]*invpkg.Invoice
}

// generateInvoiceExpiryTestData generates the specified number of fake expired
// and pending invoices anchored to the passed now timestamp.
func generateInvoiceExpiryTestData(
	t *testing.T, now time.Time,
	offset, numExpired, numPending int) invoiceExpiryTestData {

	var testData invoiceExpiryTestData

	testData.expiredInvoices = make(map[lntypes.Hash]*invpkg.Invoice)
	testData.pendingInvoices = make(map[lntypes.Hash]*invpkg.Invoice)

	expiredCreationDate := now.Add(-24 * time.Hour)

	for i := 1; i <= numExpired; i++ {
		var preimage lntypes.Preimage
		binary.BigEndian.PutUint32(preimage[:4], uint32(offset+i))
		expiry := time.Duration((i+offset)%24) * time.Hour
		invoice := newInvoiceExpiryTestInvoice(
			t, preimage, expiredCreationDate, expiry,
		)
		testData.expiredInvoices[preimage.Hash()] = invoice
	}

	for i := 1; i <= numPending; i++ {
		var preimage lntypes.Preimage
		binary.BigEndian.PutUint32(preimage[4:], uint32(offset+i))
		expiry := time.Duration((i+offset)%24) * time.Hour
		invoice := newInvoiceExpiryTestInvoice(t, preimage, now, expiry)
		testData.pendingInvoices[preimage.Hash()] = invoice
	}

	return testData
}

// newInvoiceExpiryTestInvoice creates a test invoice with a randomly generated
// payment address and custom preimage and expiry details. It should be used in
// the case where tests require custom invoice expiry and unique payment
// hashes.
func newInvoiceExpiryTestInvoice(t *testing.T, preimage lntypes.Preimage,
	timestamp time.Time, expiry time.Duration) *invpkg.Invoice {

	if expiry == 0 {
		expiry = time.Hour
	}

	var payAddr [32]byte
	if _, err := rand.Read(payAddr[:]); err != nil {
		t.Fatalf("unable to generate payment addr: %v", err)
	}

	rawInvoice, err := zpay32.NewInvoice(
		testNetParams,
		preimage.Hash(),
		timestamp,
		zpay32.Amount(testInvoiceAmount),
		zpay32.Description(testInvoiceDescription),
		zpay32.Expiry(expiry),
		zpay32.PaymentAddr(payAddr),
	)
	require.NoError(t, err, "Error while creating new invoice")

	paymentRequest, err := rawInvoice.Encode(testMessageSigner)

	require.NoError(t, err, "Error while encoding payment request")

	return &invpkg.Invoice{
		Terms: invpkg.ContractTerm{
			PaymentPreimage: &preimage,
			PaymentAddr:     payAddr,
			Value:           testInvoiceAmount,
			Expiry:          expiry,
			Features:        testFeatures,
		},
		PaymentRequest: []byte(paymentRequest),
		CreationDate:   timestamp,
	}
}

// checkSettleResolution asserts the resolution is a settle with the correct
// preimage. If successful, the HtlcSettleResolution is returned in case further
// checks are desired.
func checkSettleResolution(t *testing.T, res invpkg.HtlcResolution,
	expPreimage lntypes.Preimage) *invpkg.HtlcSettleResolution {

	t.Helper()

	settleResolution, ok := res.(*invpkg.HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, expPreimage, settleResolution.Preimage)

	return settleResolution
}

// checkFailResolution asserts the resolution is a fail with the correct reason.
// If successful, the HtlcFailResolution is returned in case further checks are
// desired.
func checkFailResolution(t *testing.T, res invpkg.HtlcResolution,
	expOutcome invpkg.FailResolutionResult) *invpkg.HtlcFailResolution {

	t.Helper()
	failResolution, ok := res.(*invpkg.HtlcFailResolution)
	require.True(t, ok)
	require.Equal(t, expOutcome, failResolution.Outcome)

	return failResolution
}

type hodlExpiryTest struct {
	hash         lntypes.Hash
	state        invpkg.ContractState
	stateLock    sync.Mutex
	mockNotifier *mockChainNotifier
	mockClock    *clock.TestClock
	cancelChan   chan lntypes.Hash
	watcher      *invpkg.InvoiceExpiryWatcher
}

func (h *hodlExpiryTest) announceBlock(t *testing.T, height uint32) {
	t.Helper()

	select {
	case h.mockNotifier.blockChan <- &chainntnfs.BlockEpoch{
		Height: int32(height),
	}:

	case <-time.After(testTimeout):
		t.Fatalf("block %v not consumed", height)
	}
}

func (h *hodlExpiryTest) assertCanceled(t *testing.T, expected lntypes.Hash) {
	select {
	case actual := <-h.cancelChan:
		require.Equal(t, expected, actual)

	case <-time.After(testTimeout):
		t.Fatalf("invoice: %v not canceled", h.hash)
	}
}
