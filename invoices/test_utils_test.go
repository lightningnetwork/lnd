package invoices

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/zpay32"
)

type mockPayload struct {
	mpp *record.MPP
}

func (p *mockPayload) MultiPath() *record.MPP {
	return p.mpp
}

func (p *mockPayload) CustomRecords() record.CustomSet {
	return make(record.CustomSet)
}

var (
	testTimeout = 5 * time.Second

	testTime = time.Date(2018, time.February, 2, 14, 0, 0, 0, time.UTC)

	testInvoicePreimage = lntypes.Preimage{1}

	testInvoicePaymentHash = testInvoicePreimage.Hash()

	testHtlcExpiry = uint32(5)

	testInvoiceCltvDelta = uint32(4)

	testFinalCltvRejectDelta = int32(4)

	testCurrentHeight = int32(1)

	testPrivKeyBytes, _ = hex.DecodeString(
		"e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")

	testPrivKey, testPubKey = btcec.PrivKeyFromBytes(
		btcec.S256(), testPrivKeyBytes)

	testInvoiceDescription = "coffee"

	testInvoiceAmount = lnwire.MilliSatoshi(100000)

	testNetParams = &chaincfg.MainNetParams

	testMessageSigner = zpay32.MessageSigner{
		SignCompact: func(hash []byte) ([]byte, error) {
			sig, err := btcec.SignCompact(btcec.S256(), testPrivKey, hash, true)
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
)

var (
	testInvoiceAmt = lnwire.MilliSatoshi(100000)
	testInvoice    = &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
			PaymentPreimage: testInvoicePreimage,
			Value:           testInvoiceAmt,
			Features:        testFeatures,
		},
	}

	testHodlInvoice = &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
			PaymentPreimage: channeldb.UnknownPreimage,
			Value:           testInvoiceAmt,
			Features:        testFeatures,
		},
	}
)

func newTestChannelDB() (*channeldb.DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
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
	registry *InvoiceRegistry
	clock    *testClock

	cleanup func()
	t       *testing.T
}

func newTestContext(t *testing.T) *testContext {
	clock := newTestClock(testTime)

	cdb, cleanup, err := newTestChannelDB()
	if err != nil {
		t.Fatal(err)
	}
	cdb.Now = clock.now

	// Instantiate and start the invoice ctx.registry.
	cfg := RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		HtlcHoldDuration:     30 * time.Second,
		Now:                  clock.now,
		TickAfter:            clock.tickAfter,
	}
	registry := NewRegistry(cdb, &cfg)

	err = registry.Start()
	if err != nil {
		cleanup()
		t.Fatal(err)
	}

	ctx := testContext{
		registry: registry,
		clock:    clock,
		t:        t,
		cleanup: func() {
			registry.Stop()
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

func newTestInvoice(t *testing.T,
	timestamp time.Time, expiry time.Duration) *channeldb.Invoice {

	if expiry == 0 {
		expiry = time.Hour
	}

	rawInvoice, err := zpay32.NewInvoice(
		testNetParams,
		testInvoicePaymentHash,
		timestamp,
		zpay32.Amount(testInvoiceAmount),
		zpay32.Description(testInvoiceDescription),
		zpay32.Expiry(expiry))

	if err != nil {
		t.Fatalf("Error while creating new invoice: %v", err)
	}

	paymentRequest, err := rawInvoice.Encode(testMessageSigner)

	if err != nil {
		t.Fatalf("Error while encoding payment request: %v", err)
	}

	return &channeldb.Invoice{
		Terms: channeldb.ContractTerm{
			PaymentPreimage: testInvoicePreimage,
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
