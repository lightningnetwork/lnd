package contractcourt

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	lnmock "github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// testReporter is a minimal interface for test reporting that is satisfied
// by both *testing.T and *rapid.T, allowing the harness to work with
// property-based tests.
type testReporter interface {
	Helper()
	Fatalf(format string, args ...any)
}

// chainWatcherTestHarness provides a test harness for chain watcher tests
// with utilities for simulating spends, confirmations, and reorganizations.
type chainWatcherTestHarness struct {
	t testReporter

	// aliceChannel and bobChannel are the test channels.
	aliceChannel *lnwallet.LightningChannel
	bobChannel   *lnwallet.LightningChannel

	// chainWatcher is the chain watcher under test.
	chainWatcher *chainWatcher

	// notifier is the mock chain notifier.
	notifier *mockChainNotifier

	// chanEvents is the channel event subscription.
	chanEvents *ChainEventSubscription

	// currentHeight tracks the current block height.
	currentHeight int32

	// blockbeatProcessed is a channel that signals when a blockbeat has
	// been processed.
	blockbeatProcessed chan struct{}
}

// mockChainNotifier extends the standard mock with additional channels for
// testing cooperative close reorgs.
type mockChainNotifier struct {
	*lnmock.ChainNotifier

	// confEvents tracks active confirmation event subscriptions.
	confEvents []*mockConfirmationEvent

	// confRegistered is a channel that signals when a new confirmation
	// event has been registered.
	confRegistered chan struct{}

	// spendEvents tracks active spend event subscriptions.
	spendEvents []*chainntnfs.SpendEvent

	// spendRegistered is a channel that signals when a new spend
	// event has been registered.
	spendRegistered chan struct{}
}

// mockConfirmationEvent represents a mock confirmation event subscription.
type mockConfirmationEvent struct {
	txid          chainhash.Hash
	numConfs      uint32
	confirmedChan chan *chainntnfs.TxConfirmation
	negConfChan   chan int32
	cancelled     bool
}

// RegisterSpendNtfn creates a new mock spend event.
func (m *mockChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	// The base mock already has SpendChan, use that.
	spendEvent := &chainntnfs.SpendEvent{
		Spend: m.SpendChan,
		Cancel: func() {
			// No-op for now.
		},
	}

	m.spendEvents = append(m.spendEvents, spendEvent)

	// Signal that a new spend event has been registered.
	select {
	case m.spendRegistered <- struct{}{}:
	default:
	}

	return spendEvent, nil
}

// RegisterConfirmationsNtfn creates a new mock confirmation event.
func (m *mockChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption,
) (*chainntnfs.ConfirmationEvent, error) {

	mockEvent := &mockConfirmationEvent{
		txid:          *txid,
		numConfs:      numConfs,
		confirmedChan: make(chan *chainntnfs.TxConfirmation, 1),
		negConfChan:   make(chan int32, 1),
	}

	m.confEvents = append(m.confEvents, mockEvent)

	// Signal that a new confirmation event has been registered.
	select {
	case m.confRegistered <- struct{}{}:
	default:
	}

	return &chainntnfs.ConfirmationEvent{
		Confirmed:    mockEvent.confirmedChan,
		NegativeConf: mockEvent.negConfChan,
		Cancel: func() {
			mockEvent.cancelled = true
		},
	}, nil
}

// harnessOpt is a functional option for configuring the test harness.
type harnessOpt func(*harnessConfig)

// harnessConfig holds configuration for the test harness.
type harnessConfig struct {
	requiredConfs fn.Option[uint32]
}

// withRequiredConfs sets the number of confirmations required for channel
// closes.
func withRequiredConfs(confs uint32) harnessOpt {
	return func(cfg *harnessConfig) {
		cfg.requiredConfs = fn.Some(confs)
	}
}

// newChainWatcherTestHarness creates a new test harness for chain watcher
// tests.
func newChainWatcherTestHarness(t *testing.T,
	opts ...harnessOpt) *chainWatcherTestHarness {

	return newChainWatcherTestHarnessFromReporter(t, t, opts...)
}

// newChainWatcherTestHarnessFromReporter creates a test harness that works
// with both *testing.T and *rapid.T. The t parameter is used for
// operations that specifically require *testing.T (like CreateTestChannels),
// while reporter is used for all test reporting (Helper, Fatalf).
func newChainWatcherTestHarnessFromReporter(t *testing.T,
	reporter testReporter, opts ...harnessOpt) *chainWatcherTestHarness {

	reporter.Helper()

	// Apply options.
	cfg := &harnessConfig{
		requiredConfs: fn.None[uint32](),
	}
	for _, opt := range opts {
		opt(cfg)
	}

	// Create test channels.
	aliceChannel, bobChannel, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	if err != nil {
		reporter.Fatalf("unable to create test channels: %v", err)
	}

	// Create mock notifier.
	baseNotifier := &lnmock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail, 1),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation, 1),
	}

	notifier := &mockChainNotifier{
		ChainNotifier:   baseNotifier,
		confEvents:      make([]*mockConfirmationEvent, 0),
		confRegistered:  make(chan struct{}, 10),
		spendEvents:     make([]*chainntnfs.SpendEvent, 0),
		spendRegistered: make(chan struct{}, 10),
	}

	// Create chain watcher.
	chainWatcher, err := newChainWatcher(chainWatcherConfig{
		chanState:           aliceChannel.State(),
		notifier:            notifier,
		signer:              aliceChannel.Signer,
		extractStateNumHint: lnwallet.GetStateNumHint,
		chanCloseConfs:      cfg.requiredConfs,
		contractBreach: func(
			retInfo *lnwallet.BreachRetribution,
		) error {
			// In tests, we just need to accept the breach
			// notification.
			return nil
		},
	})
	if err != nil {
		reporter.Fatalf("unable to create chain watcher: %v", err)
	}

	// Start chain watcher (this will register for spend notification).
	err = chainWatcher.Start()
	if err != nil {
		reporter.Fatalf("unable to start chain watcher: %v", err)
	}

	// Subscribe to channel events.
	chanEvents := chainWatcher.SubscribeChannelEvents()

	harness := &chainWatcherTestHarness{
		t:                  reporter,
		aliceChannel:       aliceChannel,
		bobChannel:         bobChannel,
		chainWatcher:       chainWatcher,
		notifier:           notifier,
		chanEvents:         chanEvents,
		currentHeight:      100,
		blockbeatProcessed: make(chan struct{}),
	}

	// Wait for the initial spend registration that happens in Start().
	harness.waitForSpendRegistration()

	// Verify BlockbeatChan is initialized.
	if chainWatcher.BlockbeatChan == nil {
		reporter.Fatalf("BlockbeatChan is nil after initialization")
	}

	// Register cleanup. We use the t for Cleanup since rapid.T
	// may not have this method in the same way.
	t.Cleanup(func() {
		_ = chainWatcher.Stop()
	})

	return harness
}

// createCoopCloseTx creates a cooperative close transaction with the given
// output value. The transaction will have the proper sequence number to
// indicate it's a cooperative close.
func (h *chainWatcherTestHarness) createCoopCloseTx(
	outputValue int64) *wire.MsgTx {

	fundingOutpoint := h.aliceChannel.State().FundingOutpoint

	return &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: fundingOutpoint,
			Sequence:         wire.MaxTxInSequenceNum,
		}},
		TxOut: []*wire.TxOut{{
			Value: outputValue,
			// Unique script.
			PkScript: []byte{byte(outputValue % 255)},
		}},
	}
}

// createRemoteForceCloseTx creates a remote force close transaction.
// From Alice's perspective, this is Bob's local commitment transaction.
func (h *chainWatcherTestHarness) createRemoteForceCloseTx() *wire.MsgTx {
	return h.bobChannel.State().LocalCommitment.CommitTx
}

// createLocalForceCloseTx creates a local force close transaction.
// This is Alice's local commitment transaction.
func (h *chainWatcherTestHarness) createLocalForceCloseTx() *wire.MsgTx {
	return h.aliceChannel.State().LocalCommitment.CommitTx
}

// createBreachCloseTx creates a breach (revoked commitment) transaction.
// We advance the channel state, save the commitment, then advance again
// to revoke it. Returns the revoked commitment tx.
func (h *chainWatcherTestHarness) createBreachCloseTx() *wire.MsgTx {
	h.t.Helper()

	// To create a revoked commitment, we need to advance the channel state
	// at least once. We'll use the test utils helper to add an HTLC and
	// force a state transition.

	// Get the current commitment before we advance (this will be revoked).
	revokedCommit := h.bobChannel.State().LocalCommitment.CommitTx

	// Add a fake HTLC to advance state.
	htlcAmount := lnwire.NewMSatFromSatoshis(10000)
	paymentHash := [32]byte{4, 5, 6}
	htlc := &lnwire.UpdateAddHTLC{
		ID:          0,
		Amount:      htlcAmount,
		Expiry:      uint32(h.currentHeight + 100),
		PaymentHash: paymentHash,
	}

	// Add HTLC to both channels.
	if _, err := h.aliceChannel.AddHTLC(htlc, nil); err != nil {
		h.t.Fatalf("unable to add HTLC to alice: %v", err)
	}
	if _, err := h.bobChannel.ReceiveHTLC(htlc); err != nil {
		h.t.Fatalf("unable to add HTLC to bob: %v", err)
	}

	// Force state transition using the helper.
	err := lnwallet.ForceStateTransition(h.aliceChannel, h.bobChannel)
	if err != nil {
		h.t.Fatalf("unable to force state transition: %v", err)
	}

	// Return the revoked commitment (Bob's previous local commitment).
	return revokedCommit
}

// sendSpend sends a spend notification for the given transaction.
func (h *chainWatcherTestHarness) sendSpend(tx *wire.MsgTx) {
	h.t.Helper()

	txHash := tx.TxHash()
	spend := &chainntnfs.SpendDetail{
		SpenderTxHash:  &txHash,
		SpendingTx:     tx,
		SpendingHeight: h.currentHeight,
	}

	select {
	case h.notifier.SpendChan <- spend:
	case <-time.After(time.Second):
		h.t.Fatalf("unable to send spend notification")
	}
}

// confirmTx sends a confirmation notification for the given transaction.
func (h *chainWatcherTestHarness) confirmTx(tx *wire.MsgTx, height int32) {
	h.t.Helper()

	// Find the confirmation event for this transaction.
	txHash := tx.TxHash()
	var confEvent *mockConfirmationEvent
	for _, event := range h.notifier.confEvents {
		if event.txid == txHash && !event.cancelled {
			confEvent = event
			break
		}
	}

	if confEvent == nil {
		h.t.Fatalf("no confirmation event registered for tx %v", txHash)
	}

	// Send confirmation.
	select {
	case confEvent.confirmedChan <- &chainntnfs.TxConfirmation{
		Tx:          tx,
		BlockHeight: uint32(height),
	}:
	case <-time.After(time.Second):
		h.t.Fatalf("unable to send confirmation")
	}
}

// triggerReorg sends a negative confirmation (reorg) notification for the
// given transaction with the specified reorg depth.
func (h *chainWatcherTestHarness) triggerReorg(tx *wire.MsgTx,
	reorgDepth int32) {

	h.t.Helper()

	// Find the confirmation event for this transaction.
	txHash := tx.TxHash()
	var confEvent *mockConfirmationEvent
	for _, event := range h.notifier.confEvents {
		if event.txid == txHash && !event.cancelled {
			confEvent = event
			break
		}
	}

	if confEvent == nil {
		// The chain watcher might not have registered for
		// confirmations yet.
		return
	}

	// Send negative confirmation.
	select {
	case confEvent.negConfChan <- reorgDepth:
	case <-time.After(time.Second):
		h.t.Fatalf("unable to send negative confirmation")
	}
}

// mineBlocks advances the current block height.
func (h *chainWatcherTestHarness) mineBlocks(n int32) {
	h.currentHeight += n
}

// waitForCoopClose waits for a cooperative close event and returns it.
func (h *chainWatcherTestHarness) waitForCoopClose(
	timeout time.Duration) *CooperativeCloseInfo {

	h.t.Helper()

	select {
	case coopClose := <-h.chanEvents.CooperativeClosure:
		return coopClose
	case <-time.After(timeout):
		h.t.Fatalf("didn't receive cooperative close event")
		return nil
	}
}

// waitForConfRegistration waits for the chain watcher to register for
// confirmation notifications.
func (h *chainWatcherTestHarness) waitForConfRegistration() {
	h.t.Helper()

	select {
	case <-h.notifier.confRegistered:
		// Registration complete.
	case <-time.After(2 * time.Second):
		// Not necessarily a failure - some tests don't register.
	}
}

// waitForSpendRegistration waits for the chain watcher to register for
// spend notifications.
func (h *chainWatcherTestHarness) waitForSpendRegistration() {
	h.t.Helper()

	select {
	case <-h.notifier.spendRegistered:
		// Registration complete.
	case <-time.After(2 * time.Second):
		// Not necessarily a failure - some tests don't register.
	}
}

// assertCoopCloseTx asserts that the given cooperative close info matches
// the expected transaction.
func (h *chainWatcherTestHarness) assertCoopCloseTx(
	closeInfo *CooperativeCloseInfo, expectedTx *wire.MsgTx) {

	h.t.Helper()

	expectedHash := expectedTx.TxHash()
	if closeInfo.ClosingTXID != expectedHash {
		h.t.Fatalf("wrong tx confirmed: expected %v, got %v",
			expectedHash, closeInfo.ClosingTXID)
	}
}

// assertNoCoopClose asserts that no cooperative close event is received
// within the given timeout.
func (h *chainWatcherTestHarness) assertNoCoopClose(timeout time.Duration) {
	h.t.Helper()

	select {
	case <-h.chanEvents.CooperativeClosure:
		h.t.Fatalf("unexpected cooperative close event")
	case <-time.After(timeout):
		// Expected timeout.
	}
}

// runCoopCloseFlow runs a complete cooperative close flow including spend,
// optional reorg, and confirmation. This helper coordinates the timing
// between the different events.
func (h *chainWatcherTestHarness) runCoopCloseFlow(
	tx *wire.MsgTx, shouldReorg bool, reorgDepth int32,
	altTx *wire.MsgTx) *CooperativeCloseInfo {

	h.t.Helper()

	// Send initial spend notification. The closeObserver's state machine
	// will detect this and register for confirmations.
	h.sendSpend(tx)

	// Wait for the chain watcher to register for confirmations.
	h.waitForConfRegistration()

	if shouldReorg {
		// Trigger reorg which resets the state machine.
		h.triggerReorg(tx, reorgDepth)

		// If we have an alternative transaction, send it.
		if altTx != nil {
			// After reorg, the chain watcher should re-register for
			// ANY spend of the funding output.
			h.waitForSpendRegistration()

			// Send alternative spend.
			h.sendSpend(altTx)

			// Wait for it to register for confirmations.
			h.waitForConfRegistration()

			// Confirm alternative transaction to unblock.
			h.mineBlocks(1)
			h.confirmTx(altTx, h.currentHeight)
		}
	} else {
		// Normal confirmation flow - confirm to unblock
		// waitForCoopCloseConfirmation.
		h.mineBlocks(1)
		h.confirmTx(tx, h.currentHeight)
	}

	// Wait for cooperative close event.
	return h.waitForCoopClose(5 * time.Second)
}

// runMultipleReorgFlow simulates multiple consecutive reorganizations with
// different transactions confirming after each reorg.
func (h *chainWatcherTestHarness) runMultipleReorgFlow(txs []*wire.MsgTx,
	reorgDepths []int32) *CooperativeCloseInfo {

	h.t.Helper()

	if len(txs) < 2 {
		h.t.Fatalf("need at least 2 transactions for reorg flow")
	}
	if len(reorgDepths) != len(txs)-1 {
		h.t.Fatalf("reorg depths must be one less than transactions")
	}

	// Send initial spend.
	h.sendSpend(txs[0])

	// Process each reorg.
	for i, depth := range reorgDepths {
		// Wait for confirmation registration.
		h.waitForConfRegistration()

		// Trigger reorg for current transaction.
		h.triggerReorg(txs[i], depth)

		// Wait for re-registration for spend.
		h.waitForSpendRegistration()

		// Send next transaction.
		h.sendSpend(txs[i+1])
	}

	// Wait for final confirmation registration.
	h.waitForConfRegistration()

	// Confirm the final transaction.
	finalTx := txs[len(txs)-1]
	h.mineBlocks(1)
	h.confirmTx(finalTx, h.currentHeight)

	// Wait for cooperative close event.
	return h.waitForCoopClose(10 * time.Second)
}

// waitForRemoteUnilateralClose waits for a remote unilateral close event.
func (h *chainWatcherTestHarness) waitForRemoteUnilateralClose(
	timeout time.Duration) *RemoteUnilateralCloseInfo {

	h.t.Helper()

	select {
	case remoteClose := <-h.chanEvents.RemoteUnilateralClosure:
		return remoteClose
	case <-time.After(timeout):
		h.t.Fatalf("didn't receive remote unilateral close event")
		return nil
	}
}

// waitForLocalUnilateralClose waits for a local unilateral close event.
func (h *chainWatcherTestHarness) waitForLocalUnilateralClose(
	timeout time.Duration) *LocalUnilateralCloseInfo {

	h.t.Helper()

	select {
	case localClose := <-h.chanEvents.LocalUnilateralClosure:
		return localClose
	case <-time.After(timeout):
		h.t.Fatalf("didn't receive local unilateral close event")
		return nil
	}
}

// waitForBreach waits for a breach (contract breach) event.
func (h *chainWatcherTestHarness) waitForBreach(
	timeout time.Duration) *BreachCloseInfo {

	h.t.Helper()

	select {
	case breach := <-h.chanEvents.ContractBreach:
		return breach
	case <-time.After(timeout):
		h.t.Fatalf("didn't receive contract breach event")
		return nil
	}
}

// assertRemoteUnilateralCloseTx asserts that the given remote unilateral close
// info matches the expected transaction.
func (h *chainWatcherTestHarness) assertRemoteUnilateralCloseTx(
	closeInfo *RemoteUnilateralCloseInfo, expectedTx *wire.MsgTx) {

	h.t.Helper()

	expectedHash := expectedTx.TxHash()
	actualHash := closeInfo.UnilateralCloseSummary.SpendDetail.SpenderTxHash
	if *actualHash != expectedHash {
		h.t.Fatalf("wrong tx confirmed: expected %v, got %v",
			expectedHash, *actualHash)
	}
}

// assertLocalUnilateralCloseTx asserts that the given local unilateral close
// info matches the expected transaction.
func (h *chainWatcherTestHarness) assertLocalUnilateralCloseTx(
	closeInfo *LocalUnilateralCloseInfo, expectedTx *wire.MsgTx) {

	h.t.Helper()

	expectedHash := expectedTx.TxHash()
	actualHash := closeInfo.LocalForceCloseSummary.CloseTx.TxHash()
	if actualHash != expectedHash {
		h.t.Fatalf("wrong tx confirmed: expected %v, got %v",
			expectedHash, actualHash)
	}
}

// assertBreachTx asserts that the given breach info matches the expected
// transaction.
func (h *chainWatcherTestHarness) assertBreachTx(
	breachInfo *BreachCloseInfo, expectedTx *wire.MsgTx) {

	h.t.Helper()

	expectedHash := expectedTx.TxHash()
	if breachInfo.CommitHash != expectedHash {
		h.t.Fatalf("wrong tx confirmed: expected %v, got %v",
			expectedHash, breachInfo.CommitHash)
	}
}
