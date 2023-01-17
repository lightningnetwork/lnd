package invoices

import (
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// invoiceExpiryWatcherTest holds a test fixture and implements checks
// for InvoiceExpiryWatcher tests.
type invoiceExpiryWatcherTest struct {
	t                *testing.T
	wg               sync.WaitGroup
	watcher          *InvoiceExpiryWatcher
	testData         invoiceExpiryTestData
	canceledInvoices []lntypes.Hash
}

// newInvoiceExpiryWatcherTest creates a new InvoiceExpiryWatcher test fixture
// and sets up the test environment.
func newInvoiceExpiryWatcherTest(t *testing.T, now time.Time,
	numExpiredInvoices, numPendingInvoices int) *invoiceExpiryWatcherTest {

	mockNotifier := newMockNotifier()
	test := &invoiceExpiryWatcherTest{
		watcher: NewInvoiceExpiryWatcher(
			clock.NewTestClock(testTime), 0,
			uint32(testCurrentHeight), nil, mockNotifier,
		),
		testData: generateInvoiceExpiryTestData(
			t, now, 0, numExpiredInvoices, numPendingInvoices,
		),
	}

	test.wg.Add(numExpiredInvoices)

	err := test.watcher.Start(func(paymentHash lntypes.Hash,
		force bool) error {

		test.canceledInvoices = append(
			test.canceledInvoices, paymentHash,
		)
		test.wg.Done()
		return nil
	})

	require.NoError(t, err, "cannot start InvoiceExpiryWatcher")

	return test
}

func (t *invoiceExpiryWatcherTest) waitForFinish(timeout time.Duration) {
	done := make(chan struct{})

	// Wait for all cancels.
	go func() {
		t.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		t.t.Fatalf("test timeout")
	}
}

func (t *invoiceExpiryWatcherTest) checkExpectations() {
	// Check that invoices that got canceled during the test are the ones
	// that expired.
	if len(t.canceledInvoices) != len(t.testData.expiredInvoices) {
		t.t.Fatalf("expected %v cancellations, got %v",
			len(t.testData.expiredInvoices),
			len(t.canceledInvoices))
	}

	for i := range t.canceledInvoices {
		if _, ok := t.testData.expiredInvoices[t.canceledInvoices[i]]; !ok {
			t.t.Fatalf("wrong invoice canceled")
		}
	}
}

// Tests that InvoiceExpiryWatcher can be started and stopped.
func TestInvoiceExpiryWatcherStartStop(t *testing.T) {
	watcher := NewInvoiceExpiryWatcher(
		clock.NewTestClock(testTime), 0, uint32(testCurrentHeight), nil,
		newMockNotifier(),
	)
	cancel := func(lntypes.Hash, bool) error {
		t.Fatalf("unexpected call")
		return nil
	}

	if err := watcher.Start(cancel); err != nil {
		t.Fatalf("unexpected error upon start: %v", err)
	}

	if err := watcher.Start(cancel); err == nil {
		t.Fatalf("expected error upon second start")
	}

	watcher.Stop()

	if err := watcher.Start(cancel); err != nil {
		t.Fatalf("unexpected error upon start: %v", err)
	}
}

// Tests that no invoices will expire from an empty InvoiceExpiryWatcher.
func TestInvoiceExpiryWithNoInvoices(t *testing.T) {
	t.Parallel()

	test := newInvoiceExpiryWatcherTest(t, testTime, 0, 0)

	test.waitForFinish(testTimeout)
	test.watcher.Stop()
	test.checkExpectations()
}

// Tests that if all add invoices are expired, then all invoices
// will be canceled.
func TestInvoiceExpiryWithOnlyExpiredInvoices(t *testing.T) {
	t.Parallel()

	test := newInvoiceExpiryWatcherTest(t, testTime, 0, 5)

	for paymentHash, invoice := range test.testData.pendingInvoices {
		test.watcher.AddInvoices(makeInvoiceExpiry(paymentHash, invoice))
	}

	test.waitForFinish(testTimeout)
	test.watcher.Stop()
	test.checkExpectations()
}

// Tests that if some invoices are expired, then those invoices
// will be canceled.
func TestInvoiceExpiryWithPendingAndExpiredInvoices(t *testing.T) {
	t.Parallel()

	test := newInvoiceExpiryWatcherTest(t, testTime, 5, 5)

	for paymentHash, invoice := range test.testData.expiredInvoices {
		test.watcher.AddInvoices(makeInvoiceExpiry(paymentHash, invoice))
	}

	for paymentHash, invoice := range test.testData.pendingInvoices {
		test.watcher.AddInvoices(makeInvoiceExpiry(paymentHash, invoice))
	}

	test.waitForFinish(testTimeout)
	test.watcher.Stop()
	test.checkExpectations()
}

// Tests adding multiple invoices at once.
func TestInvoiceExpiryWhenAddingMultipleInvoices(t *testing.T) {
	t.Parallel()

	test := newInvoiceExpiryWatcherTest(t, testTime, 5, 5)
	var invoices []invoiceExpiry

	for hash, invoice := range test.testData.expiredInvoices {
		invoices = append(invoices, makeInvoiceExpiry(hash, invoice))
	}

	for hash, invoice := range test.testData.pendingInvoices {
		invoices = append(invoices, makeInvoiceExpiry(hash, invoice))
	}

	test.watcher.AddInvoices(invoices...)
	test.waitForFinish(testTimeout)
	test.watcher.Stop()
	test.checkExpectations()
}

// TestExpiredHodlInv tests expiration of an already-expired hodl invoice
// which has no htlcs.
func TestExpiredHodlInv(t *testing.T) {
	t.Parallel()

	creationDate := testTime.Add(time.Hour * -24)
	expiry := time.Hour

	test := setupHodlExpiry(
		t, creationDate, expiry, 0, ContractOpen, nil,
	)

	test.assertCanceled(t, test.hash)
	test.watcher.Stop()
}

// TestAcceptedHodlNotExpired tests that hodl invoices which are in an accepted
// state are not expired once their time-based expiry elapses, using a regular
// invoice that expires at the same time as a control to ensure that invoices
// with that timestamp would otherwise be expired.
func TestAcceptedHodlNotExpired(t *testing.T) {
	t.Parallel()

	creationDate := testTime
	expiry := time.Hour

	test := setupHodlExpiry(
		t, creationDate, expiry, 0, ContractAccepted, nil,
	)
	defer test.watcher.Stop()

	// Add another invoice that will expire at our expiry time as a control
	// value.
	tsExpires := &invoiceExpiryTs{
		PaymentHash: lntypes.Hash{1, 2, 3},
		Expiry:      creationDate.Add(expiry),
		Keysend:     true,
	}
	test.watcher.AddInvoices(tsExpires)

	test.mockClock.SetTime(creationDate.Add(expiry + 1))

	// Assert that only the ts expiry invoice is expired.
	test.assertCanceled(t, tsExpires.PaymentHash)
}

// TestHeightAlreadyExpired tests the case where we add an invoice with htlcs
// that have already expired to the expiry watcher.
func TestHeightAlreadyExpired(t *testing.T) {
	t.Parallel()

	expiredHtlc := []*InvoiceHTLC{
		{
			State:  HtlcStateAccepted,
			Expiry: uint32(testCurrentHeight),
		},
	}

	test := setupHodlExpiry(
		t, testTime, time.Hour, 0, ContractAccepted,
		expiredHtlc,
	)
	defer test.watcher.Stop()

	test.assertCanceled(t, test.hash)
}

// TestExpiryHeightArrives tests the case where we add a hodl invoice to the
// expiry watcher when it has no htlcs, htlcs are added and then they finally
// expire. We use a non-zero delta for this test to check that we expire with
// sufficient buffer.
func TestExpiryHeightArrives(t *testing.T) {
	var (
		creationDate        = testTime
		expiry              = time.Hour * 2
		delta        uint32 = 1
	)

	// Start out with a hodl invoice that is open, and has no htlcs.
	test := setupHodlExpiry(
		t, creationDate, expiry, delta, ContractOpen, nil,
	)
	defer test.watcher.Stop()

	htlc1 := uint32(testCurrentHeight + 10)
	expiry1 := makeHeightExpiry(test.hash, htlc1)

	// Add htlcs to our invoice and progress its state to accepted.
	test.watcher.AddInvoices(expiry1)
	test.setState(ContractAccepted)

	// Progress time so that our expiry has elapsed. We no longer expect
	// this invoice to be canceled because it has been accepted.
	test.mockClock.SetTime(creationDate.Add(expiry))

	// Tick our mock block subscription with the next block, we don't
	// expect anything to happen.
	currentHeight := uint32(testCurrentHeight + 1)
	test.announceBlock(t, currentHeight)

	// Now, we add another htlc to the invoice. This one has a lower expiry
	// height than our current ones.
	htlc2 := currentHeight + 5
	expiry2 := makeHeightExpiry(test.hash, htlc2)
	test.watcher.AddInvoices(expiry2)

	// Announce our lowest htlc expiry block minus our delta, the invoice
	// should be expired now.
	test.announceBlock(t, htlc2-delta)
	test.assertCanceled(t, test.hash)
}
