package contractcourt

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

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

func createTestChannelArbitrator() (*ChannelArbitrator, chan struct{}, func(), error) {
	blockEpoch := &chainntnfs.BlockEpochEvent{
		Cancel: func() {},
	}

	chanPoint := wire.OutPoint{}
	shortChanID := lnwire.ShortChannelID{}
	chanEvents := &ChainEventSubscription{
		RemoteUnilateralClosure: make(chan *lnwallet.UnilateralCloseSummary, 1),
		LocalUnilateralClosure:  make(chan *LocalUnilateralCloseInfo, 1),
		CooperativeClosure:      make(chan struct{}, 1),
		ContractBreach:          make(chan *lnwallet.BreachRetribution, 1),
	}

	chainIO := &mockChainIO{}
	chainArbCfg := ChainArbitratorConfig{
		ChainIO: chainIO,
		PublishTx: func(*wire.MsgTx) error {
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

		ChainArbitratorConfig: chainArbCfg,
		ChainEvents:           chanEvents,
	}
	testLog, cleanUp, err := newTestBoltArbLog(
		testChainHash, testChanPoint1,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to create test log: %v",
			err)
	}

	return NewChannelArbitrator(arbCfg, nil, testLog),
		resolvedChan, cleanUp, nil
}

// assertState checks that the ChannelArbitrator is in the state we expect it
// to be.
func assertState(t *testing.T, c *ChannelArbitrator, expected ArbitratorState) {
	if c.state != expected {
		t.Fatalf("expected state %v, was %v", expected, c.state)
	}
}

// TestChannelArbitratorCooperativeClose tests that the ChannelArbitertor
// correctly does nothing in case a cooperative close is confirmed.
func TestChannelArbitratorCooperativeClose(t *testing.T) {
	chanArb, _, cleanUp, err := createTestChannelArbitrator()
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}
	defer cleanUp()

	if err := chanArb.Start(); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	assertState(t, chanArb, StateDefault)

	// Cooperative close should do nothing.
	// TODO: this will change?
	chanArb.cfg.ChainEvents.CooperativeClosure <- struct{}{}
	assertState(t, chanArb, StateDefault)
}

// TestChannelArbitratorRemoteForceClose checks that the ChannelArbitrotor goes
// through the expected states if a remote force close is observed in the
// chain.
func TestChannelArbitratorRemoteForceClose(t *testing.T) {
	chanArb, resolved, cleanUp, err := createTestChannelArbitrator()
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}
	defer cleanUp()

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

	// It should mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}

	// TODO: intermediate states.
	// We expect the ChannelArbitrator to end up in the the resolved state.
	assertState(t, chanArb, StateFullyResolved)
}

// TestChannelArbitratorLocalForceClose tests that the ChannelArbitrator goes
// through the expected states in case we request it to force close the channel,
// and the local force close event is observed in chain.
func TestChannelArbitratorLocalForceClose(t *testing.T) {
	chanArb, resolved, cleanUp, err := createTestChannelArbitrator()
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}
	defer cleanUp()

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

	select {
	case <-respChan:
	case err := <-errChan:
		t.Fatalf("error force closing channel: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatalf("did not receive reponse")
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
	}
	// It should mark the channel as resolved.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}

	// And end up in the StateFullyResolved state.
	// TODO: intermediate states as well.
	assertState(t, chanArb, StateFullyResolved)
}

// TestChannelArbitratorLocalForceCloseRemoteConfiremd tests that the
// ChannelArbitrator behaves as expected in the case where we request a local
// force close, but a remote commitment ends up being confirmed in chain.
func TestChannelArbitratorLocalForceCloseRemoteConfirmed(t *testing.T) {
	chanArb, resolved, cleanUp, err := createTestChannelArbitrator()
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}
	defer cleanUp()

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

	// Wait for a response to the force close.
	select {
	case <-respChan:
	case err := <-errChan:
		t.Fatalf("error force closing channel: %v", err)
	case <-time.After(15 * time.Second):
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

	// It should resolve.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(15 * time.Second):
		t.Fatalf("contract was not resolved")
	}

	// And we expect it to end up in StateFullyResolved.
	// TODO: intermediate states as well.
	assertState(t, chanArb, StateFullyResolved)
}

// TestChannelArbitratorLocalForceCloseDoubleSpend tests that the
// ChannelArbitrator behaves as expected in the case where we request a local
// force close, but we fail broadcasting our commitment because a remote
// commitment has already been published.
func TestChannelArbitratorLocalForceDoubleSpend(t *testing.T) {
	chanArb, resolved, cleanUp, err := createTestChannelArbitrator()
	if err != nil {
		t.Fatalf("unable to create ChannelArbitrator: %v", err)
	}
	defer cleanUp()

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

	// Wait for a response to the force close.
	select {
	case <-respChan:
	case err := <-errChan:
		t.Fatalf("error force closing channel: %v", err)
	case <-time.After(15 * time.Second):
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

	// It should resolve.
	select {
	case <-resolved:
		// Expected.
	case <-time.After(15 * time.Second):
		t.Fatalf("contract was not resolved")
	}

	// And we expect it to end up in StateFullyResolved.
	// TODO: intermediate states as well.
	assertState(t, chanArb, StateFullyResolved)
}
