package invoices

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

type mockPayload struct {
	mpp           *record.MPP
	amp           *record.AMP
	customRecords record.CustomSet
}

func (p *mockPayload) MultiPath() *record.MPP {
	return p.mpp
}

func (p *mockPayload) AMPRecord() *record.AMP {
	return p.amp
}

func (p *mockPayload) CustomRecords() record.CustomSet {
	// This function should always return a map instance, but for mock
	// configuration we do accept nil.
	if p.customRecords == nil {
		return make(record.CustomSet)
	}

	return p.customRecords
}

const (
	testHtlcExpiry = uint32(5)

	testInvoiceCltvDelta = uint32(4)

	testFinalCltvRejectDelta = int32(4)

	testCurrentHeight = int32(1)
)

var (
	testTimeout = 5 * time.Second

	testTime = time.Date(2018, time.February, 2, 14, 0, 0, 0, time.UTC)

	testInvoicePreimage = lntypes.Preimage{1}

	testInvoicePaymentHash = testInvoicePreimage.Hash()

	testPrivKeyBytes, _ = hex.DecodeString(
		"e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")

	testPrivKey, _ = btcec.PrivKeyFromBytes(
		btcec.S256(), testPrivKeyBytes)

	testInvoiceDescription = "coffee"

	testInvoiceAmount = lnwire.MilliSatoshi(100000)

	testNetParams = &chaincfg.MainNetParams

	testMessageSigner = zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			hash := chainhash.HashB(msg)
			sig, err := btcec.SignCompact(
				btcec.S256(), testPrivKey, hash, true,
			)
			if err != nil {
				return nil, fmt.Errorf("can't sign the message: %v", err)
			}
			return sig, nil
		},
	}

	testFeatures = lnwire.NewFeatureVector(
		nil, lnwire.Features,
	)

	testPayload = &mockPayload{}

	testInvoiceCreationDate = testTime
)

var (
	testInvoiceAmt = lnwire.MilliSatoshi(100000)
	testInvoice    = &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
			PaymentPreimage: &testInvoicePreimage,
			Value:           testInvoiceAmt,
			Expiry:          time.Hour,
			Features:        testFeatures,
		},
		CreationDate: testInvoiceCreationDate,
	}

	testPayAddrReqInvoice = &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
			PaymentPreimage: &testInvoicePreimage,
			Value:           testInvoiceAmt,
			Expiry:          time.Hour,
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadOptional,
					lnwire.PaymentAddrRequired,
				),
				lnwire.Features,
			),
		},
		CreationDate: testInvoiceCreationDate,
	}

	testPayAddrOptionalInvoice = &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
			PaymentPreimage: &testInvoicePreimage,
			Value:           testInvoiceAmt,
			Expiry:          time.Hour,
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadOptional,
					lnwire.PaymentAddrOptional,
				),
				lnwire.Features,
			),
		},
		CreationDate: testInvoiceCreationDate,
	}

	testHodlInvoice = &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
			Value:    testInvoiceAmt,
			Expiry:   time.Hour,
			Features: testFeatures,
		},
		CreationDate: testInvoiceCreationDate,
		HodlInvoice:  true,
	}
)

func newTestChannelDB(clock clock.Clock) (*channeldb.DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(
		tempDirName, channeldb.OptionClock(clock),
	)
	if err != nil {
		os.RemoveAll(tempDirName)
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}

type testContext struct {
	cdb      *channeldb.DB
	registry *InvoiceRegistry
	notifier *mockChainNotifier
	clock    *clock.TestClock

	cleanup func()
	t       *testing.T
}

func newTestContext(t *testing.T) *testContext {
	clock := clock.NewTestClock(testTime)

	cdb, cleanup, err := newTestChannelDB(clock)
	if err != nil {
		t.Fatal(err)
	}

	notifier := newMockNotifier()

	expiryWatcher := NewInvoiceExpiryWatcher(
		clock, 0, uint32(testCurrentHeight), nil, notifier,
	)

	// Instantiate and start the invoice ctx.registry.
	cfg := RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		HtlcHoldDuration:     30 * time.Second,
		Clock:                clock,
	}
	registry := NewRegistry(cdb, expiryWatcher, &cfg)

	err = registry.Start()
	if err != nil {
		cleanup()
		t.Fatal(err)
	}

	ctx := testContext{
		cdb:      cdb,
		registry: registry,
		notifier: notifier,
		clock:    clock,
		t:        t,
		cleanup: func() {
			if err = registry.Stop(); err != nil {
				t.Fatalf("failed to stop invoice registry: %v", err)
			}
			cleanup()
		},
	}

	return &ctx
}

func getCircuitKey(htlcID uint64) channeldb.CircuitKey {
	return channeldb.CircuitKey{
		ChanID: lnwire.ShortChannelID{
			BlockHeight: 1, TxIndex: 2, TxPosition: 3,
		},
		HtlcID: htlcID,
	}
}

func newTestInvoice(t *testing.T, preimage lntypes.Preimage,
	timestamp time.Time, expiry time.Duration) *channeldb.Invoice {

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
	if err != nil {
		t.Fatalf("Error while creating new invoice: %v", err)
	}

	paymentRequest, err := rawInvoice.Encode(testMessageSigner)

	if err != nil {
		t.Fatalf("Error while encoding payment request: %v", err)
	}

	return &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
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

// timeout implements a test level timeout.
func timeout() func() {
	done := make(chan struct{})

	go func() {
		select {
		case <-time.After(5 * time.Second):
			err := pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
			if err != nil {
				panic(fmt.Sprintf("error writing to std out after timeout: %v", err))
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
	expiredInvoices map[lntypes.Hash]*channeldb.Invoice
	pendingInvoices map[lntypes.Hash]*channeldb.Invoice
}

// generateInvoiceExpiryTestData generates the specified number of fake expired
// and pending invoices anchored to the passed now timestamp.
func generateInvoiceExpiryTestData(
	t *testing.T, now time.Time,
	offset, numExpired, numPending int) invoiceExpiryTestData {

	var testData invoiceExpiryTestData

	testData.expiredInvoices = make(map[lntypes.Hash]*channeldb.Invoice)
	testData.pendingInvoices = make(map[lntypes.Hash]*channeldb.Invoice)

	expiredCreationDate := now.Add(-24 * time.Hour)

	for i := 1; i <= numExpired; i++ {
		var preimage lntypes.Preimage
		binary.BigEndian.PutUint32(preimage[:4], uint32(offset+i))
		expiry := time.Duration((i+offset)%24) * time.Hour
		invoice := newTestInvoice(t, preimage, expiredCreationDate, expiry)
		testData.expiredInvoices[preimage.Hash()] = invoice
	}

	for i := 1; i <= numPending; i++ {
		var preimage lntypes.Preimage
		binary.BigEndian.PutUint32(preimage[4:], uint32(offset+i))
		expiry := time.Duration((i+offset)%24) * time.Hour
		invoice := newTestInvoice(t, preimage, now, expiry)
		testData.pendingInvoices[preimage.Hash()] = invoice
	}

	return testData
}

// checkSettleResolution asserts the resolution is a settle with the correct
// preimage. If successful, the HtlcSettleResolution is returned in case further
// checks are desired.
func checkSettleResolution(t *testing.T, res HtlcResolution,
	expPreimage lntypes.Preimage) *HtlcSettleResolution {

	t.Helper()

	settleResolution, ok := res.(*HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, expPreimage, settleResolution.Preimage)

	return settleResolution
}

// checkFailResolution asserts the resolution is a fail with the correct reason.
// If successful, the HtlcFailResolutionis returned in case further checks are
// desired.
func checkFailResolution(t *testing.T, res HtlcResolution,
	expOutcome FailResolutionResult) *HtlcFailResolution {

	t.Helper()
	failResolution, ok := res.(*HtlcFailResolution)
	require.True(t, ok)
	require.Equal(t, expOutcome, failResolution.Outcome)

	return failResolution
}

type hodlExpiryTest struct {
	hash         lntypes.Hash
	state        channeldb.ContractState
	stateLock    sync.Mutex
	mockNotifier *mockChainNotifier
	mockClock    *clock.TestClock
	cancelChan   chan lntypes.Hash
	watcher      *InvoiceExpiryWatcher
}

func (h *hodlExpiryTest) setState(state channeldb.ContractState) {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	h.state = state
}

func (h *hodlExpiryTest) announceBlock(t *testing.T, height uint32) {
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

// setupHodlExpiry creates a hodl invoice in our expiry watcher and runs an
// arbitrary update function which advances the invoices's state.
func setupHodlExpiry(t *testing.T, creationDate time.Time,
	expiry time.Duration, heightDelta uint32,
	startState channeldb.ContractState,
	startHtlcs []*channeldb.InvoiceHTLC) *hodlExpiryTest {

	mockNotifier := newMockNotifier()
	mockClock := clock.NewTestClock(testTime)

	test := &hodlExpiryTest{
		state: startState,
		watcher: NewInvoiceExpiryWatcher(
			mockClock, heightDelta, uint32(testCurrentHeight), nil,
			mockNotifier,
		),
		cancelChan:   make(chan lntypes.Hash),
		mockNotifier: mockNotifier,
		mockClock:    mockClock,
	}

	// Use an unbuffered channel to block on cancel calls so that the test
	// does not exit before we've processed all the invoices we expect.
	cancelImpl := func(paymentHash lntypes.Hash, force bool) error {
		test.stateLock.Lock()
		currentState := test.state
		test.stateLock.Unlock()

		if currentState != channeldb.ContractOpen && !force {
			return nil
		}

		select {
		case test.cancelChan <- paymentHash:
		case <-time.After(testTimeout):
		}

		return nil
	}

	require.NoError(t, test.watcher.Start(cancelImpl))

	// We set preimage and hash so that we can use our existing test
	// helpers. In practice we would only have the hash, but this does not
	// affect what we're testing at all.
	preimage := lntypes.Preimage{1}
	test.hash = preimage.Hash()

	invoice := newTestInvoice(t, preimage, creationDate, expiry)
	invoice.State = startState
	invoice.HodlInvoice = true
	invoice.Htlcs = make(map[channeldb.CircuitKey]*channeldb.InvoiceHTLC)

	// If we have any htlcs, add them with unique circult keys.
	for i, htlc := range startHtlcs {
		key := channeldb.CircuitKey{
			HtlcID: uint64(i),
		}

		invoice.Htlcs[key] = htlc
	}

	// Create an expiry entry for our invoice in its starting state. This
	// mimics adding invoices to the watcher on start.
	entry := makeInvoiceExpiry(test.hash, invoice)
	test.watcher.AddInvoices(entry)

	return test
}
