package invoices

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

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
	testCurrentHeight = int32(1)
)

var (
	testTimeout = 5 * time.Second

	testTime = time.Date(2018, time.February, 2, 14, 0, 0, 0, time.UTC)

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
			sig := ecdsa.SignCompact(testPrivKey, hash, true)

			return sig, nil
		},
	}

	testFeatures = lnwire.NewFeatureVector(
		nil, lnwire.Features,
	)
)

func newTestInvoice(t *testing.T, preimage lntypes.Preimage,
	timestamp time.Time, expiry time.Duration) *Invoice {

	t.Helper()

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

	return &Invoice{
		Terms: ContractTerm{
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

// invoiceExpiryTestData simply holds generated expired and pending invoices.
type invoiceExpiryTestData struct {
	expiredInvoices map[lntypes.Hash]*Invoice
	pendingInvoices map[lntypes.Hash]*Invoice
}

// generateInvoiceExpiryTestData generates the specified number of fake expired
// and pending invoices anchored to the passed now timestamp.
func generateInvoiceExpiryTestData(
	t *testing.T, now time.Time,
	offset, numExpired, numPending int) invoiceExpiryTestData {

	t.Helper()

	var testData invoiceExpiryTestData

	testData.expiredInvoices = make(map[lntypes.Hash]*Invoice)
	testData.pendingInvoices = make(map[lntypes.Hash]*Invoice)

	expiredCreationDate := now.Add(-24 * time.Hour)

	for i := 1; i <= numExpired; i++ {
		var preimage lntypes.Preimage
		binary.BigEndian.PutUint32(preimage[:4], uint32(offset+i))
		duration := (i + offset) % 24
		expiry := time.Duration(duration) * time.Hour
		invoice := newTestInvoice(
			t, preimage, expiredCreationDate, expiry,
		)
		testData.expiredInvoices[preimage.Hash()] = invoice
	}

	for i := 1; i <= numPending; i++ {
		var preimage lntypes.Preimage
		binary.BigEndian.PutUint32(preimage[4:], uint32(offset+i))
		duration := (i + offset) % 24
		expiry := time.Duration(duration) * time.Hour
		invoice := newTestInvoice(t, preimage, now, expiry)
		testData.pendingInvoices[preimage.Hash()] = invoice
	}

	return testData
}

type hodlExpiryTest struct {
	hash         lntypes.Hash
	state        ContractState
	stateLock    sync.Mutex
	mockNotifier *mockChainNotifier
	mockClock    *clock.TestClock
	cancelChan   chan lntypes.Hash
	watcher      *InvoiceExpiryWatcher
}

func (h *hodlExpiryTest) setState(state ContractState) {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	h.state = state
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
	t.Helper()

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
	startState ContractState,
	startHtlcs []*InvoiceHTLC) *hodlExpiryTest {

	t.Helper()

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

		if currentState != ContractOpen && !force {
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
	invoice.Htlcs = make(map[CircuitKey]*InvoiceHTLC)

	// If we have any htlcs, add them with unique circult keys.
	for i, htlc := range startHtlcs {
		key := CircuitKey{
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
