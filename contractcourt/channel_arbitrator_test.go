package contractcourt

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

type mockArbitratorLog struct {
	state           ArbitratorState
	newStates       chan ArbitratorState
	failLog         bool
	failFetch       error
	failCommit      bool
	failCommitState ArbitratorState
	resolutions     *ContractResolutions
	chainActions    ChainActionMap
	resolvers       map[ContractResolver]struct{}

	sync.Mutex
}

// A compile time check to ensure mockArbitratorLog meets the ArbitratorLog
// interface.
var _ ArbitratorLog = (*mockArbitratorLog)(nil)

func (b *mockArbitratorLog) CurrentState() (ArbitratorState, error) {
	return b.state, nil
}

func (b *mockArbitratorLog) CommitState(s ArbitratorState) error {
	if b.failCommit && s == b.failCommitState {
		return fmt.Errorf("intentional commit error at state %v",
			b.failCommitState)
	}
	b.state = s
	b.newStates <- s
	return nil
}

func (b *mockArbitratorLog) FetchUnresolvedContracts() ([]ContractResolver,
	error) {

	b.Lock()
	v := make([]ContractResolver, len(b.resolvers))
	idx := 0
	for resolver := range b.resolvers {
		v[idx] = resolver
		idx++
	}
	b.Unlock()

	return v, nil
}

func (b *mockArbitratorLog) InsertUnresolvedContracts(
	resolvers ...ContractResolver) error {

	b.Lock()
	for _, resolver := range resolvers {
		b.resolvers[resolver] = struct{}{}
	}
	b.Unlock()
	return nil
}

func (b *mockArbitratorLog) SwapContract(oldContract,
	newContract ContractResolver) error {

	b.Lock()
	delete(b.resolvers, oldContract)
	b.resolvers[newContract] = struct{}{}
	b.Unlock()

	return nil
}

func (b *mockArbitratorLog) ResolveContract(res ContractResolver) error {
	b.Lock()
	delete(b.resolvers, res)
	b.Unlock()

	return nil
}

func (b *mockArbitratorLog) LogContractResolutions(c *ContractResolutions) error {
	if b.failLog {
		return fmt.Errorf("intentional log failure")
	}
	b.resolutions = c
	return nil
}

func (b *mockArbitratorLog) FetchContractResolutions() (*ContractResolutions, error) {
	if b.failFetch != nil {
		return nil, b.failFetch
	}

	return b.resolutions, nil
}

func (b *mockArbitratorLog) LogChainActions(actions ChainActionMap) error {
	b.chainActions = actions
	return nil
}

func (b *mockArbitratorLog) FetchChainActions() (ChainActionMap, error) {
	actionsMap := b.chainActions
	return actionsMap, nil
}

func (b *mockArbitratorLog) WipeHistory() error {
	return nil
}

type mockChainIO struct{}

func (*mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return nil, 0, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint, _ []byte,
	heightHint uint32) (*wire.TxOut, error) {
	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, nil
}

func createTestChannelArbitrator(log ArbitratorLog) (*ChannelArbitrator,
	chan struct{}, error) {
	blockEpoch := &chainntnfs.BlockEpochEvent{
		Cancel: func() {},
	}

	chanPoint := wire.OutPoint{}
	shortChanID := lnwire.ShortChannelID{}
	chanEvents := &ChainEventSubscription{
		RemoteUnilateralClosure: make(chan *lnwallet.UnilateralCloseSummary, 1),
		LocalUnilateralClosure:  make(chan *LocalUnilateralCloseInfo, 1),
		CooperativeClosure:      make(chan *CooperativeCloseInfo, 1),
		ContractBreach:          make(chan *lnwallet.BreachRetribution, 1),
	}

	chainIO := &mockChainIO{}
	chainArbCfg := ChainArbitratorConfig{
		ChainIO: chainIO,
		PublishTx: func(*wire.MsgTx) error {
			return nil
		},
		DeliverResolutionMsg: func(...ResolutionMsg) error {
			return nil
		},
		BroadcastDelta: 5,
		Notifier: &mockNotifier{
			epochChan: make(chan *chainntnfs.BlockEpoch),
			spendChan: make(chan *chainntnfs.SpendDetail),
			confChan:  make(chan *chainntnfs.TxConfirmation),
		},
		IncubateOutputs: func(wire.OutPoint, *lnwallet.CommitOutputResolution,
			*lnwallet.OutgoingHtlcResolution,
			*lnwallet.IncomingHtlcResolution, uint32) error {
			return nil
		},
	}

	// We'll use the resolvedChan to synchronize on call to
	// MarkChannelResolved.
	resolvedChan := make(chan struct{}, 1)

	// Next we'll create the matching configuration struct that contains
	// all interfaces and methods the arbitrator needs to do its job.
	arbCfg := ChannelArbitratorConfig{
		ChanPoint:   chanPoint,
		ShortChanID: shortChanID,
		BlockEpochs: blockEpoch,
		MarkChannelResolved: func() error {
			resolvedChan <- struct{}{}
			return nil
		},
		ForceCloseChan: func() (*lnwallet.LocalForceCloseSummary, error) {
			summary := &lnwallet.LocalForceCloseSummary{
				CloseTx:         &wire.MsgTx{},
				HtlcResolutions: &lnwallet.HtlcResolutions{},
			}
			return summary, nil
		},
		MarkCommitmentBroadcasted: func() error {
			return nil
		},
		MarkChannelClosed: func(*channeldb.ChannelCloseSummary) error {
			return nil
		},
		IsPendingClose:        false,
		ChainArbitratorConfig: chainArbCfg,
		ChainEvents:           chanEvents,
	}

	return NewChannelArbitrator(arbCfg, nil, log), resolvedChan, nil
}

// assertState checks that the ChannelArbitrator is in the state we expect it
// to be.
func assertState(t *testing.T, c *ChannelArbitrator, expected ArbitratorState) {
	if c.state != expected {
		t.Fatalf("expected state %v, was %v", expected, c.state)
	}
}

// TestChannelArbitratorCooperativeClose tests that the ChannelArbitertor
// correctly marks the channel resolved in case a cooperative close is
// confirmed.
func TestChannelArbitratorCooperativeClose(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// We set up a channel to detect when MarkChannelClosed is called.
	closeInfos := make(chan *channeldb.ChannelCloseSummary)
	chanArb.cfg.MarkChannelClosed = func(
		closeInfo *channeldb.ChannelCloseSummary) error {
		closeInfos <- closeInfo
		return nil
	}

	// Cooperative close should do trigger a MarkChannelClosed +
	// MarkChannelResolved.
	closeInfo := &CooperativeCloseInfo{
		&channeldb.ChannelCloseSummary{},
	}
	chanArb.cfg.ChainEvents.CooperativeClosure <- closeInfo

	select {
	case c := <-closeInfos:
		if c.CloseType != channeldb.CooperativeClose {
			t.Fatalf("expected cooperative close, got %v", c.CloseType)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for channel close")
	}

	// It should mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

func assertStateTransitions(t *testing.T, newStates <-chan ArbitratorState,
	expectedStates ...ArbitratorState) {
	t.Helper()

	for _, exp := range expectedStates {
		var state ArbitratorState
		select {
		case state = <-newStates:
		case <-time.After(5 * time.Second):
			t.Fatalf("new state not received")
		}

		if state != exp {
			t.Fatalf("expected new state %v, got %v", exp, state)
		}
	}
}

// TestChannelArbitratorRemoteForceClose checks that the ChannelArbitrator goes
// through the expected states if a remote force close is observed in the
// chain.
func TestChannelArbitratorRemoteForceClose(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// Send a remote force close event.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}

	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// It should transition StateDefault -> StateContractClosed ->
	// StateFullyResolved.
	assertStateTransitions(
		t, log.newStates, StateContractClosed, StateFullyResolved,
	)

	// It should alos mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorLocalForceClose tests that the ChannelArbitrator goes
// through the expected states in case we request it to force close the channel,
// and the local force close event is observed in chain.
func TestChannelArbitratorLocalForceClose(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// We create a channel we can use to pause the ChannelArbitrator at the
	// point where it broadcasts the close tx, and check its state.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx) error {
		// When the force close tx is being broadcasted, check that the
		// state is correct at that point.
		select {
		case stateChan <- chanArb.state:
		case <-chanArb.quit:
			return fmt.Errorf("exiting")
		}
		return nil
	}

	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	// With the channel found, and the request crafted, we'll send over a
	// force close request to the arbitrator that watches this channel.
	chanArb.forceCloseReqs <- &forceCloseReq{
		errResp: errChan,
		closeTx: respChan,
	}

	// It should transition to StateBroadcastCommit.
	assertStateTransitions(t, log.newStates, StateBroadcastCommit)

	// When it is broadcasting the force close, its state should be
	// StateBroadcastCommit.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("did not get state update")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	assertStateTransitions(t, log.newStates, StateCommitmentBroadcasted)

	select {
	case <-respChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	// After broadcasting the close tx, it should be in state
	// StateCommitmentBroadcasted.
	assertState(t, chanArb, StateCommitmentBroadcasted)

	// Now notify about the local force close getting confirmed.
	chanArb.cfg.ChainEvents.LocalUnilateralClosure <- &LocalUnilateralCloseInfo{
		&chainntnfs.SpendDetail{},
		&lnwallet.LocalForceCloseSummary{
			CloseTx:         &wire.MsgTx{},
			HtlcResolutions: &lnwallet.HtlcResolutions{},
		},
		&channeldb.ChannelCloseSummary{},
	}

	// It should transition StateContractClosed -> StateFullyResolved.
	assertStateTransitions(t, log.newStates, StateContractClosed,
		StateFullyResolved)

	// It should also mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorLocalForceClosePendingHtlc tests that the
// ChannelArbitrator goes through the expected states in case we request it to
// force close a channel that still has an HTLC pending.
func TestChannelArbitratorLocalForceClosePendingHtlc(t *testing.T) {
	arbLog := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
		resolvers: make(map[ContractResolver]struct{}),
	}

	chanArb, resolved, err := createTestChannelArbitrator(arbLog)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	incubateChan := make(chan struct{})
	chanArb.cfg.IncubateOutputs = func(_ wire.OutPoint,
		_ *lnwallet.CommitOutputResolution,
		_ *lnwallet.OutgoingHtlcResolution,
		_ *lnwallet.IncomingHtlcResolution, _ uint32) error {

		incubateChan <- struct{}{}

		return nil
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// Create htlcUpdates channel.
	htlcUpdates := make(chan []channeldb.HTLC)

	signals := &ContractSignals{
		HtlcUpdates: htlcUpdates,
		ShortChanID: lnwire.ShortChannelID{},
	}
	chanArb.UpdateContractSignals(signals)

	// Add HTLC to channel arbitrator.
	htlc := channeldb.HTLC{
		Incoming:  false,
		Amt:       10000,
		HtlcIndex: 0,
	}

	htlcUpdates <- []channeldb.HTLC{
		htlc,
	}

	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	// With the channel found, and the request crafted, we'll send over a
	// force close request to the arbitrator that watches this channel.
	chanArb.forceCloseReqs <- &forceCloseReq{
		errResp: errChan,
		closeTx: respChan,
	}

	// The force close request should trigger broadcast of the commitment
	// transaction.
	assertStateTransitions(t, arbLog.newStates,
		StateBroadcastCommit, StateCommitmentBroadcasted)
	select {
	case <-respChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	// Now notify about the local force close getting confirmed.
	closeTx := &wire.MsgTx{}

	htlcOp := wire.OutPoint{
		Hash:  closeTx.TxHash(),
		Index: 0,
	}

	// Set up the outgoing resolution. Populate SignedTimeoutTx because
	// our commitment transaction got confirmed.
	outgoingRes := lnwallet.OutgoingHtlcResolution{
		Expiry: 10,
		SweepSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{},
		},
		SignedTimeoutTx: &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{
					PreviousOutPoint: htlcOp,
					Witness:          [][]byte{{}},
				},
			},
			TxOut: []*wire.TxOut{
				{},
			},
		},
	}

	chanArb.cfg.ChainEvents.LocalUnilateralClosure <- &LocalUnilateralCloseInfo{
		&chainntnfs.SpendDetail{},
		&lnwallet.LocalForceCloseSummary{
			CloseTx: closeTx,
			HtlcResolutions: &lnwallet.HtlcResolutions{
				OutgoingHTLCs: []lnwallet.OutgoingHtlcResolution{
					outgoingRes,
				},
			},
		},
		&channeldb.ChannelCloseSummary{},
	}

	assertStateTransitions(t, arbLog.newStates, StateContractClosed,
		StateWaitingFullResolution)

	// htlcOutgoingContestResolver is now active and waiting for the HTLC to
	// expire. It should not yet have passed it on for incubation.
	select {
	case <-incubateChan:
		t.Fatalf("contract should not be incubated yet")
	default:
	}

	// Send a notification that the expiry height has been reached.
	notifier := chanArb.cfg.Notifier.(*mockNotifier)
	notifier.epochChan <- &chainntnfs.BlockEpoch{Height: 10}

	// htlcOutgoingContestResolver is now transforming into a
	// htlcTimeoutResolver and should send the contract off for incubation.
	select {
	case <-incubateChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	// Notify resolver that output of the commitment has been spent.
	notifier.confChan <- &chainntnfs.TxConfirmation{}

	// As this is our own commitment transaction, the HTLC will go through
	// to the second level. Channel arbitrator should still not be marked as
	// resolved.
	select {
	case <-resolved:
		t.Fatalf("channel resolved prematurely")
	default:
	}

	// Notify resolver that the second level transaction is spent.
	notifier.spendChan <- &chainntnfs.SpendDetail{}

	// At this point channel should be marked as resolved.
	assertStateTransitions(t, arbLog.newStates, StateFullyResolved)

	select {
	case <-resolved:
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorLocalForceCloseRemoteConfiremd tests that the
// ChannelArbitrator behaves as expected in the case where we request a local
// force close, but a remote commitment ends up being confirmed in chain.
func TestChannelArbitratorLocalForceCloseRemoteConfirmed(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// Create a channel we can use to assert the state when it publishes
	// the close tx.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx) error {
		// When the force close tx is being broadcasted, check that the
		// state is correct at that point.
		select {
		case stateChan <- chanArb.state:
		case <-chanArb.quit:
			return fmt.Errorf("exiting")
		}
		return nil
	}

	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	// With the channel found, and the request crafted, we'll send over a
	// force close request to the arbitrator that watches this channel.
	chanArb.forceCloseReqs <- &forceCloseReq{
		errResp: errChan,
		closeTx: respChan,
	}

	// It should transition to StateBroadcastCommit.
	assertStateTransitions(t, log.newStates, StateBroadcastCommit)

	// We expect it to be in state StateBroadcastCommit when publishing
	// the force close.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no state update received")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	assertStateTransitions(t, log.newStates, StateCommitmentBroadcasted)

	// Wait for a response to the force close.
	select {
	case <-respChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	// The state should be StateCommitmentBroadcasted.
	assertState(t, chanArb, StateCommitmentBroadcasted)

	// Now notify about the _REMOTE_ commitment getting confirmed.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}
	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// It should transition StateContractClosed -> StateFullyResolved.
	assertStateTransitions(t, log.newStates, StateContractClosed,
		StateFullyResolved)

	// It should resolve.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(15 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorLocalForceCloseDoubleSpend tests that the
// ChannelArbitrator behaves as expected in the case where we request a local
// force close, but we fail broadcasting our commitment because a remote
// commitment has already been published.
func TestChannelArbitratorLocalForceDoubleSpend(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// Return ErrDoubleSpend when attempting to publish the tx.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx) error {
		// When the force close tx is being broadcasted, check that the
		// state is correct at that point.
		select {
		case stateChan <- chanArb.state:
		case <-chanArb.quit:
			return fmt.Errorf("exiting")
		}
		return lnwallet.ErrDoubleSpend
	}

	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	// With the channel found, and the request crafted, we'll send over a
	// force close request to the arbitrator that watches this channel.
	chanArb.forceCloseReqs <- &forceCloseReq{
		errResp: errChan,
		closeTx: respChan,
	}

	// It should transition to StateBroadcastCommit.
	assertStateTransitions(t, log.newStates, StateBroadcastCommit)

	// We expect it to be in state StateBroadcastCommit when publishing
	// the force close.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no state update received")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	assertStateTransitions(t, log.newStates, StateCommitmentBroadcasted)

	// Wait for a response to the force close.
	select {
	case <-respChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no response received")
	}

	// The state should be StateCommitmentBroadcasted.
	assertState(t, chanArb, StateCommitmentBroadcasted)

	// Now notify about the _REMOTE_ commitment getting confirmed.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}
	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// It should transition StateContractClosed -> StateFullyResolved.
	assertStateTransitions(t, log.newStates, StateContractClosed,
		StateFullyResolved)

	// It should resolve.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(15 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorPersistence tests that the ChannelArbitrator is able to
// keep advancing the state machine from various states after restart.
func TestChannelArbitratorPersistence(t *testing.T) {
	// Start out with a log that will fail writing the set of resolutions.
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
		failLog:   true,
	}

	chanArb, resolved, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}

	// It should start in StateDefault.
	assertState(t, chanArb, StateDefault)

	// Send a remote force close event.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}

	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// Since writing the resolutions fail, the arbitrator should not
	// advance to the next state.
	time.Sleep(100 * time.Millisecond)
	if log.state != StateDefault {
		t.Fatalf("expected to stay in StateDefault")
	}
	chanArb.Stop()

	// Create a new arbitrator with the same log.
	chanArb, resolved, err = createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}

	// Again, it should start up in the default state.
	assertState(t, chanArb, StateDefault)

	// Now we make the log succeed writing the resolutions, but fail when
	// attempting to close the channel.
	log.failLog = false
	chanArb.cfg.MarkChannelClosed = func(*channeldb.ChannelCloseSummary) error {
		return fmt.Errorf("intentional close error")
	}

	// Send a new remote force close event.
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// Since closing the channel failed, the arbitrator should stay in the
	// default state.
	time.Sleep(100 * time.Millisecond)
	if log.state != StateDefault {
		t.Fatalf("expected to stay in StateDefault")
	}
	chanArb.Stop()

	// Create yet another arbitrator with the same log.
	chanArb, resolved, err = createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}

	// Starts out in StateDefault.
	assertState(t, chanArb, StateDefault)

	// Now make fetching the resolutions fail.
	log.failFetch = fmt.Errorf("intentional fetch failure")
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose

	// Since logging the resolutions and closing the channel now succeeds,
	// it should advance to StateContractClosed.
	assertStateTransitions(
		t, log.newStates, StateContractClosed,
	)

	// It should not advance further, however, as fetching resolutions
	// failed.
	time.Sleep(100 * time.Millisecond)
	if log.state != StateContractClosed {
		t.Fatalf("expected to stay in StateContractClosed")
	}
	chanArb.Stop()

	// Create a new arbitrator, and now make fetching resolutions succeed.
	log.failFetch = nil
	chanArb, resolved, err = createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// Finally it should advance to StateFullyResolved.
	assertStateTransitions(
		t, log.newStates, StateFullyResolved,
	)

	// It should also mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorCommitFailure tests that the channel arbitrator is able
// to recover from a failed CommitState call at restart.
func TestChannelArbitratorCommitFailure(t *testing.T) {

	testCases := []struct {

		// closeType is the type of channel close we want ot test.
		closeType channeldb.ClosureType

		// sendEvent is a function that will send the event
		// corresponding to this test's closeType to the passed
		// ChannelArbitrator.
		sendEvent func(chanArb *ChannelArbitrator)

		// expectedStates is the states we expect the state machine to
		// go through after a restart and successful log commit.
		expectedStates []ArbitratorState
	}{
		{
			closeType: channeldb.CooperativeClose,
			sendEvent: func(chanArb *ChannelArbitrator) {
				closeInfo := &CooperativeCloseInfo{
					&channeldb.ChannelCloseSummary{},
				}
				chanArb.cfg.ChainEvents.CooperativeClosure <- closeInfo
			},
			expectedStates: []ArbitratorState{StateFullyResolved},
		},
		{
			closeType: channeldb.RemoteForceClose,
			sendEvent: func(chanArb *ChannelArbitrator) {
				commitSpend := &chainntnfs.SpendDetail{
					SpenderTxHash: &chainhash.Hash{},
				}

				uniClose := &lnwallet.UnilateralCloseSummary{
					SpendDetail:     commitSpend,
					HtlcResolutions: &lnwallet.HtlcResolutions{},
				}
				chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- uniClose
			},
			expectedStates: []ArbitratorState{StateContractClosed, StateFullyResolved},
		},
		{
			closeType: channeldb.LocalForceClose,
			sendEvent: func(chanArb *ChannelArbitrator) {
				chanArb.cfg.ChainEvents.LocalUnilateralClosure <- &LocalUnilateralCloseInfo{
					&chainntnfs.SpendDetail{},
					&lnwallet.LocalForceCloseSummary{
						CloseTx:         &wire.MsgTx{},
						HtlcResolutions: &lnwallet.HtlcResolutions{},
					},
					&channeldb.ChannelCloseSummary{},
				}
			},
			expectedStates: []ArbitratorState{StateContractClosed, StateFullyResolved},
		},
	}

	for _, test := range testCases {
		log := &mockArbitratorLog{
			state:      StateDefault,
			newStates:  make(chan ArbitratorState, 5),
			failCommit: true,

			// Set the log to fail on the first expected state
			// after state machine progress for this test case.
			failCommitState: test.expectedStates[0],
		}

		chanArb, resolved, err := createTestChannelArbitrator(log)
		if err != nil {
			t.Fatalf("unable to create ChannelArbitrator: %v", err)
		}

		if err := chanArb.Start(); err != nil {
			t.Fatalf("unable to start ChannelArbitrator: %v", err)
		}

		// It should start in StateDefault.
		assertState(t, chanArb, StateDefault)

		closed := make(chan struct{})
		chanArb.cfg.MarkChannelClosed = func(
			*channeldb.ChannelCloseSummary) error {
			close(closed)
			return nil
		}

		// Send the test event to trigger the state machine.
		test.sendEvent(chanArb)

		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatalf("channel was not marked closed")
		}

		// Since the channel was marked closed in the database, but the
		// commit to the next state failed, the state should still be
		// StateDefault.
		time.Sleep(100 * time.Millisecond)
		if log.state != StateDefault {
			t.Fatalf("expected to stay in StateDefault, instead "+
				"has %v", log.state)
		}
		chanArb.Stop()

		// Start the arbitrator again, with IsPendingClose reporting
		// the channel closed in the database.
		chanArb, resolved, err = createTestChannelArbitrator(log)
		if err != nil {
			t.Fatalf("unable to create ChannelArbitrator: %v", err)
		}

		log.failCommit = false

		chanArb.cfg.IsPendingClose = true
		chanArb.cfg.ClosingHeight = 100
		chanArb.cfg.CloseType = test.closeType

		if err := chanArb.Start(); err != nil {
			t.Fatalf("unable to start ChannelArbitrator: %v", err)
		}

		// Since the channel is marked closed in the database, it
		// should advance to the expected states.
		assertStateTransitions(
			t, log.newStates, test.expectedStates...,
		)

		// It should also mark the channel as resolved.
		select {
		case <-resolved:
			// Expected.
		case <-time.After(5 * time.Second):
			t.Fatalf("contract was not resolved")
		}
	}
}

// TestChannelArbitratorEmptyResolutions makes sure that a channel that is
// pending close in the database, but haven't had any resolutions logged will
// not be marked resolved. This situation must be handled to avoid closing
// channels from earlier versions of the ChannelArbitrator, which didn't have a
// proper handoff from the ChainWatcher, and we could risk ending up in a state
// where the channel was closed in the DB, but the resolutions weren't properly
// written.
func TestChannelArbitratorEmptyResolutions(t *testing.T) {
	// Start out with a log that will fail writing the set of resolutions.
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
		failFetch: errNoResolutions,
	}

	chanArb, _, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}

	chanArb.cfg.IsPendingClose = true
	chanArb.cfg.ClosingHeight = 100
	chanArb.cfg.CloseType = channeldb.RemoteForceClose

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}

	// It should not advance its state beyond StateContractClosed, since
	// fetching resolutions fails.
	assertStateTransitions(
		t, log.newStates, StateContractClosed,
	)

	// It should not advance further, however, as fetching resolutions
	// failed.
	time.Sleep(100 * time.Millisecond)
	if log.state != StateContractClosed {
		t.Fatalf("expected to stay in StateContractClosed")
	}
	chanArb.Stop()
}

// TestChannelArbitratorAlreadyForceClosed ensures that we cannot force close a
// channel that is already in the process of doing so.
func TestChannelArbitratorAlreadyForceClosed(t *testing.T) {
	t.Parallel()

	// We'll create the arbitrator and its backing log to signal that it's
	// already in the process of being force closed.
	log := &mockArbitratorLog{
		state: StateCommitmentBroadcasted,
	}
	chanArb, _, err := createTestChannelArbitrator(log)
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}
	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// Then, we'll create a request to signal a force close request to the
	// channel arbitrator.
	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	select {
	case chanArb.forceCloseReqs <- &forceCloseReq{
		closeTx: respChan,
		errResp: errChan,
	}:
	case <-chanArb.quit:
	}

	// Finally, we should ensure that we are not able to do so by seeing the
	// expected errAlreadyForceClosed error.
	select {
	case err = <-errChan:
		if err != errAlreadyForceClosed {
			t.Fatalf("expected errAlreadyForceClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected to receive error response")
	}
}
