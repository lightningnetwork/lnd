package contractcourt

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	// stateTimeout is the timeout we allow when waiting for state
	// transitions.
	stateTimeout = time.Second * 15
)

type mockArbitratorLog struct {
	state           ArbitratorState
	newStates       chan ArbitratorState
	failLog         bool
	failFetch       error
	failCommit      bool
	failCommitState ArbitratorState
	resolutions     *ContractResolutions
	resolvers       map[ContractResolver]struct{}

	commitSet *CommitSet

	sync.Mutex
}

// A compile time check to ensure mockArbitratorLog meets the ArbitratorLog
// interface.
var _ ArbitratorLog = (*mockArbitratorLog)(nil)

func (b *mockArbitratorLog) CurrentState(kvdb.RTx) (ArbitratorState, error) {
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

func (b *mockArbitratorLog) InsertUnresolvedContracts(_ []*channeldb.ResolverReport,
	resolvers ...ContractResolver) error {

	b.Lock()
	for _, resolver := range resolvers {
		resKey := resolver.ResolverKey()
		if resKey == nil {
			continue
		}

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

func (b *mockArbitratorLog) FetchChainActions() (ChainActionMap, error) {
	return nil, nil
}

func (b *mockArbitratorLog) InsertConfirmedCommitSet(c *CommitSet) error {
	b.commitSet = c
	return nil
}

func (b *mockArbitratorLog) FetchConfirmedCommitSet(kvdb.RTx) (*CommitSet, error) {
	return b.commitSet, nil
}

func (b *mockArbitratorLog) WipeHistory() error {
	return nil
}

// testArbLog is a wrapper around an existing (ideally fully concrete
// ArbitratorLog) that lets us intercept certain calls like transitioning to a
// new state.
type testArbLog struct {
	ArbitratorLog

	newStates chan ArbitratorState
}

func (t *testArbLog) CommitState(s ArbitratorState) error {
	if err := t.ArbitratorLog.CommitState(s); err != nil {
		return err
	}

	t.newStates <- s

	return nil
}

type mockChainIO struct{}

var _ lnwallet.BlockChainIO = (*mockChainIO)(nil)

func (*mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return nil, 0, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint, _ []byte,
	heightHint uint32, _ <-chan struct{}) (*wire.TxOut, error) {
	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, nil
}

func (*mockChainIO) GetBlockHeader(*chainhash.Hash) (*wire.BlockHeader, error) {
	return nil, nil
}

type chanArbTestCtx struct {
	t *testing.T

	chanArb *ChannelArbitrator

	cleanUp func()

	resolvedChan chan struct{}

	incubationRequests chan struct{}

	resolutions chan []ResolutionMsg

	log ArbitratorLog

	sweeper *mockSweeper

	breachSubscribed     chan struct{}
	breachResolutionChan chan struct{}

	finalHtlcs map[uint64]bool
}

func (c *chanArbTestCtx) CleanUp() {
	if err := c.chanArb.Stop(); err != nil {
		c.t.Fatalf("unable to stop chan arb: %v", err)
	}

	if c.cleanUp != nil {
		c.cleanUp()
	}
}

// receiveBlockbeat mocks the behavior of a blockbeat being sent by the
// BlockbeatDispatcher, which essentially mocks the method `ProcessBlock`.
func (c *chanArbTestCtx) receiveBlockbeat(height int) {
	go func() {
		beat := newBeatFromHeight(int32(height))
		c.chanArb.BlockbeatChan <- beat
	}()
}

// AssertStateTransitions asserts that the state machine steps through the
// passed states in order.
func (c *chanArbTestCtx) AssertStateTransitions(expectedStates ...ArbitratorState) {
	c.t.Helper()

	var newStatesChan chan ArbitratorState
	switch log := c.log.(type) {
	case *mockArbitratorLog:
		newStatesChan = log.newStates

	case *testArbLog:
		newStatesChan = log.newStates

	default:
		c.t.Fatalf("unable to assert state transitions with %T", log)
	}

	for _, exp := range expectedStates {
		var state ArbitratorState
		select {
		case state = <-newStatesChan:
		case <-time.After(defaultTimeout):
			c.t.Fatalf("new state not received")
		}

		if state != exp {
			c.t.Fatalf("expected new state %v, got %v", exp, state)
		}
	}
}

// AssertState checks that the ChannelArbitrator is in the state we expect it
// to be.
func (c *chanArbTestCtx) AssertState(expected ArbitratorState) {
	if c.chanArb.state != expected {
		c.t.Fatalf("expected state %v, was %v", expected, c.chanArb.state)
	}
}

// Restart simulates a clean restart of the channel arbitrator, forcing it to
// walk through it's recovery logic. If this function returns nil, then a
// restart was successful. Note that the restart process keeps the log in
// place, in order to simulate proper persistence of the log. The caller can
// optionally provide a restart closure which will be executed before the
// resolver is started again, but after it is created.
func (c *chanArbTestCtx) Restart(restartClosure func(*chanArbTestCtx)) (*chanArbTestCtx, error) {
	if err := c.chanArb.Stop(); err != nil {
		return nil, err
	}

	newCtx, err := createTestChannelArbitrator(c.t, c.log)
	if err != nil {
		return nil, err
	}

	if restartClosure != nil {
		restartClosure(newCtx)
	}

	beat := newBeatFromHeight(0)
	if err := newCtx.chanArb.Start(nil, beat); err != nil {
		return nil, err
	}

	return newCtx, nil
}

// testChanArbOpts is a struct that contains options that can be used to
// initialize the channel arbitrator test context.
type testChanArbOpts struct {
	forceCloseErr error
	arbCfg        *ChannelArbitratorConfig
}

// testChanArbOption applies custom settings to a channel arbitrator config for
// testing purposes.
type testChanArbOption func(cfg *testChanArbOpts)

// remoteInitiatorOption sets the MarkChannelClosed function in the Channel
// Arbitrator's config.
func withMarkClosed(markClosed func(*channeldb.ChannelCloseSummary,
	...channeldb.ChannelStatus) error) testChanArbOption {

	return func(cfg *testChanArbOpts) {
		cfg.arbCfg.MarkChannelClosed = markClosed
	}
}

// withForceCloseErr is used to specify an error that should be returned when
// the channel arb tries to force close a channel.
func withForceCloseErr(err error) testChanArbOption {
	return func(opts *testChanArbOpts) {
		opts.forceCloseErr = err
	}
}

// createTestChannelArbitrator returns a channel arbitrator test context which
// contains a channel arbitrator with default values. These values can be
// changed by providing options which overwrite the default config.
func createTestChannelArbitrator(t *testing.T, log ArbitratorLog,
	opts ...testChanArbOption) (*chanArbTestCtx, error) {

	chanArbCtx := &chanArbTestCtx{
		breachSubscribed: make(chan struct{}),
		finalHtlcs:       make(map[uint64]bool),
	}

	chanPoint := wire.OutPoint{}
	shortChanID := lnwire.ShortChannelID{}
	chanEvents := &ChainEventSubscription{
		RemoteUnilateralClosure: make(chan *RemoteUnilateralCloseInfo, 1),
		LocalUnilateralClosure:  make(chan *LocalUnilateralCloseInfo, 1),
		CooperativeClosure:      make(chan *CooperativeCloseInfo, 1),
		ContractBreach:          make(chan *BreachCloseInfo, 1),
	}

	resolutionChan := make(chan []ResolutionMsg, 1)
	incubateChan := make(chan struct{})

	chainIO := &mockChainIO{}
	mockSweeper := newMockSweeper()
	chainArbCfg := ChainArbitratorConfig{
		ChainIO: chainIO,
		PublishTx: func(*wire.MsgTx, string) error {
			return nil
		},
		DeliverResolutionMsg: func(msgs ...ResolutionMsg) error {
			resolutionChan <- msgs
			return nil
		},
		OutgoingBroadcastDelta: 5,
		IncomingBroadcastDelta: 5,
		Notifier: &mock.ChainNotifier{
			EpochChan: make(chan *chainntnfs.BlockEpoch),
			SpendChan: make(chan *chainntnfs.SpendDetail),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		IncubateOutputs: func(wire.OutPoint,
			fn.Option[lnwallet.OutgoingHtlcResolution],
			fn.Option[lnwallet.IncomingHtlcResolution],
			uint32, fn.Option[int32]) error {

			incubateChan <- struct{}{}
			return nil
		},
		OnionProcessor: &mockOnionProcessor{},
		IsForwardedHTLC: func(chanID lnwire.ShortChannelID,
			htlcIndex uint64) bool {

			return true
		},
		SubscribeBreachComplete: func(op *wire.OutPoint,
			c chan struct{}) (bool, error) {

			chanArbCtx.breachResolutionChan = c
			chanArbCtx.breachSubscribed <- struct{}{}
			return false, nil
		},
		Clock:        clock.NewDefaultClock(),
		Sweeper:      mockSweeper,
		HtlcNotifier: &mockHTLCNotifier{},
		PutFinalHtlcOutcome: func(chanId lnwire.ShortChannelID,
			htlcId uint64, settled bool) error {

			chanArbCtx.finalHtlcs[htlcId] = settled

			return nil
		},
		Budget:     *DefaultBudgetConfig(),
		PreimageDB: newMockWitnessBeacon(),
		Registry:   &mockRegistry{},
		QueryIncomingCircuit: func(
			circuit models.CircuitKey) *models.CircuitKey {

			return nil
		},
	}

	// We'll use the resolvedChan to synchronize on call to
	// NotifyChannelResolved.
	resolvedChan := make(chan struct{}, 1)

	// Next we'll create the matching configuration struct that contains
	// all interfaces and methods the arbitrator needs to do its job.
	arbCfg := &ChannelArbitratorConfig{
		ChanPoint:   chanPoint,
		ShortChanID: shortChanID,
		NotifyChannelResolved: func() {
			resolvedChan <- struct{}{}
		},
		MarkCommitmentBroadcasted: func(_ *wire.MsgTx,
			_ lntypes.ChannelParty) error {

			return nil
		},
		MarkChannelClosed: func(*channeldb.ChannelCloseSummary,
			...channeldb.ChannelStatus) error {

			return nil
		},
		IsPendingClose:        false,
		ChainArbitratorConfig: chainArbCfg,
		ChainEvents:           chanEvents,
		PutResolverReport: func(_ kvdb.RwTx,
			_ *channeldb.ResolverReport) error {

			return nil
		},
		FetchHistoricalChannel: func() (*channeldb.OpenChannel, error) {
			return &channeldb.OpenChannel{}, nil
		},
		FindOutgoingHTLCDeadline: func(
			htlc channeldb.HTLC) fn.Option[int32] {

			return fn.None[int32]()
		},
	}

	testOpts := &testChanArbOpts{
		arbCfg: arbCfg,
	}

	// Apply all custom options to the config struct.
	for _, option := range opts {
		option(testOpts)
	}

	arbCfg.Channel = &mockChannel{
		forceCloseErr: testOpts.forceCloseErr,
	}

	var cleanUp func()
	if log == nil {
		dbPath := filepath.Join(t.TempDir(), "testdb")
		db, err := kvdb.Create(
			kvdb.BoltBackendName, dbPath, true,
			kvdb.DefaultDBTimeout, false,
		)
		if err != nil {
			return nil, err
		}

		backingLog, err := newBoltArbitratorLog(
			db, *arbCfg, chainhash.Hash{}, chanPoint,
		)
		if err != nil {
			return nil, err
		}
		cleanUp = func() {
			db.Close()
		}

		log = &testArbLog{
			ArbitratorLog: backingLog,
			newStates:     make(chan ArbitratorState),
		}
	}

	htlcSets := make(map[HtlcSetKey]htlcSet)

	chanArb := NewChannelArbitrator(*arbCfg, htlcSets, log)

	chanArbCtx.t = t
	chanArbCtx.chanArb = chanArb
	chanArbCtx.cleanUp = cleanUp
	chanArbCtx.resolvedChan = resolvedChan
	chanArbCtx.resolutions = resolutionChan
	chanArbCtx.log = log
	chanArbCtx.incubationRequests = incubateChan
	chanArbCtx.sweeper = mockSweeper

	return chanArbCtx, nil
}

// TestChannelArbitratorCooperativeClose tests that the ChannelArbitertor
// correctly marks the channel resolved in case a cooperative close is
// confirmed.
func TestChannelArbitratorCooperativeClose(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")

	beat := newBeatFromHeight(0)
	if err := chanArbCtx.chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	t.Cleanup(func() {
		require.NoError(t, chanArbCtx.chanArb.Stop())
	})

	// It should start out in the default state.
	chanArbCtx.AssertState(StateDefault)

	// We set up a channel to detect when MarkChannelClosed is called.
	closeInfos := make(chan *channeldb.ChannelCloseSummary)
	chanArbCtx.chanArb.cfg.MarkChannelClosed = func(
		closeInfo *channeldb.ChannelCloseSummary,
		statuses ...channeldb.ChannelStatus) error {

		closeInfos <- closeInfo
		return nil
	}

	// Cooperative close should do trigger a MarkChannelClosed +
	// NotifyChannelResolved.
	closeInfo := &CooperativeCloseInfo{
		&channeldb.ChannelCloseSummary{},
	}
	chanArbCtx.chanArb.cfg.ChainEvents.CooperativeClosure <- closeInfo

	select {
	case c := <-closeInfos:
		if c.CloseType != channeldb.CooperativeClose {
			t.Fatalf("expected cooperative close, got %v", c.CloseType)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("timeout waiting for channel close")
	}

	// It should mark the channel as resolved.
	select {
	case <-chanArbCtx.resolvedChan:
		// Expected.
	case <-time.After(defaultTimeout):
		t.Fatalf("contract was not resolved")
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

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb := chanArbCtx.chanArb

	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	chanArbCtx.AssertState(StateDefault)

	// Send a remote force close event.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}

	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- &RemoteUnilateralCloseInfo{
		UnilateralCloseSummary: uniClose,
		CommitSet: CommitSet{
			ConfCommitKey: fn.Some(RemoteHtlcSet),
			HtlcSets:      make(map[HtlcSetKey][]channeldb.HTLC),
		},
	}

	// It should transition StateDefault -> StateContractClosed ->
	// StateFullyResolved.
	chanArbCtx.AssertStateTransitions(
		StateContractClosed, StateFullyResolved,
	)

	// It should also mark the channel as resolved.
	select {
	case <-chanArbCtx.resolvedChan:
		// Expected.
	case <-time.After(defaultTimeout):
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

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb := chanArbCtx.chanArb

	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	chanArbCtx.AssertState(StateDefault)

	// We create a channel we can use to pause the ChannelArbitrator at the
	// point where it broadcasts the close tx, and check its state.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx, string) error {
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
	chanArbCtx.AssertStateTransitions(StateBroadcastCommit)

	// When it is broadcasting the force close, its state should be
	// StateBroadcastCommit.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(stateTimeout):
		t.Fatalf("did not get state update")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	chanArbCtx.AssertStateTransitions(StateCommitmentBroadcasted)

	select {
	case <-respChan:
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	// After broadcasting the close tx, it should be in state
	// StateCommitmentBroadcasted.
	chanArbCtx.AssertState(StateCommitmentBroadcasted)

	// Now notify about the local force close getting confirmed.
	//
	//nolint:ll
	chanArb.cfg.ChainEvents.LocalUnilateralClosure <- &LocalUnilateralCloseInfo{
		SpendDetail: &chainntnfs.SpendDetail{},
		LocalForceCloseSummary: &lnwallet.LocalForceCloseSummary{
			CloseTx: &wire.MsgTx{},
			ContractResolutions: fn.Some(lnwallet.ContractResolutions{
				HtlcResolutions: &lnwallet.HtlcResolutions{},
			}),
		},
		ChannelCloseSummary: &channeldb.ChannelCloseSummary{},
	}

	// It should transition StateContractClosed -> StateFullyResolved.
	chanArbCtx.AssertStateTransitions(StateContractClosed, StateFullyResolved)

	// It should also mark the channel as resolved.
	select {
	case <-chanArbCtx.resolvedChan:
		// Expected.
	case <-time.After(defaultTimeout):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorBreachClose tests that the ChannelArbitrator goes
// through the expected states in case we notice a breach in the chain, and
// is able to properly progress the breachResolver and anchorResolver to a
// successful resolution.
func TestChannelArbitratorBreachClose(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
		resolvers: make(map[ContractResolver]struct{}),
	}

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb := chanArbCtx.chanArb
	chanArb.cfg.PreimageDB = newMockWitnessBeacon()
	chanArb.cfg.Registry = &mockRegistry{}

	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	t.Cleanup(func() {
		require.NoError(t, chanArb.Stop())
	})

	// It should start out in the default state.
	chanArbCtx.AssertState(StateDefault)

	// We create two HTLCs, one incoming and one outgoing. We will later
	// assert that we only receive a ResolutionMsg for the outgoing HTLC.
	outgoingIdx := uint64(2)

	rHash1 := [lntypes.PreimageSize]byte{1, 2, 3}
	htlc1 := channeldb.HTLC{
		RHash:       rHash1,
		OutputIndex: 2,
		Incoming:    false,
		HtlcIndex:   outgoingIdx,
		LogIndex:    2,
	}

	rHash2 := [lntypes.PreimageSize]byte{2, 2, 2}
	htlc2 := channeldb.HTLC{
		RHash:       rHash2,
		OutputIndex: 3,
		Incoming:    true,
		HtlcIndex:   3,
		LogIndex:    3,
	}

	anchorRes := &lnwallet.AnchorResolution{
		AnchorSignDescriptor: input.SignDescriptor{
			Output: &wire.TxOut{Value: 1},
		},
	}

	// Create the BreachCloseInfo that the chain_watcher would normally
	// send to the channel_arbitrator.
	breachInfo := &BreachCloseInfo{
		BreachResolution: &BreachResolution{
			FundingOutPoint: wire.OutPoint{},
		},
		AnchorResolution: anchorRes,
		CommitSet: CommitSet{
			ConfCommitKey: fn.Some(RemoteHtlcSet),
			HtlcSets: map[HtlcSetKey][]channeldb.HTLC{
				RemoteHtlcSet: {htlc1, htlc2},
			},
		},
		CommitHash: chainhash.Hash{},
	}

	// Send a breach close event.
	chanArb.cfg.ChainEvents.ContractBreach <- breachInfo

	// It should transition StateDefault -> StateContractClosed.
	chanArbCtx.AssertStateTransitions(StateContractClosed)

	// We should receive one ResolutionMsg as there was only one outgoing
	// HTLC at the time of the breach.
	select {
	case res := <-chanArbCtx.resolutions:
		require.Equal(t, 1, len(res))
		require.Equal(t, outgoingIdx, res[0].HtlcIndex)
	case <-time.After(5 * time.Second):
		t.Fatal("expected to receive a resolution msg")
	}

	// We should now transition from StateContractClosed to
	// StateWaitingFullResolution.
	chanArbCtx.AssertStateTransitions(StateWaitingFullResolution)

	// One of the resolvers should be an anchor resolver and the other
	// should be a breach resolver.
	require.Equal(t, 2, len(chanArb.activeResolvers))

	var anchorExists, breachExists bool
	for _, resolver := range chanArb.activeResolvers {
		switch resolver.(type) {
		case *anchorResolver:
			anchorExists = true
		case *breachResolver:
			breachExists = true
		default:
			t.Fatalf("did not expect resolver %T", resolver)
		}
	}
	require.True(t, anchorExists && breachExists)

	// The anchor resolver is expected to re-offer the anchor input to the
	// sweeper.
	<-chanArbCtx.sweeper.sweptInputs

	// Wait for SubscribeBreachComplete to be called.
	<-chanArbCtx.breachSubscribed

	// We'll now close the breach channel so that the state transitions to
	// StateFullyResolved.
	close(chanArbCtx.breachResolutionChan)

	chanArbCtx.AssertStateTransitions(StateFullyResolved)

	// It should also mark the channel as resolved.
	select {
	case <-chanArbCtx.resolvedChan:
		// Expected.
	case <-time.After(defaultTimeout):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorLocalForceClosePendingHtlc tests that the
// ChannelArbitrator goes through the expected states in case we request it to
// force close a channel that still has an HTLC pending.
func TestChannelArbitratorLocalForceClosePendingHtlc(t *testing.T) {
	// We create a new test context for this channel arb, notice that we
	// pass in a nil ArbitratorLog which means that a default one backed by
	// a real DB will be created. We need this for our test as we want to
	// test proper restart recovery and resolver population.
	chanArbCtx, err := createTestChannelArbitrator(t, nil)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb := chanArbCtx.chanArb
	chanArb.cfg.PreimageDB = newMockWitnessBeacon()
	chanArb.cfg.Registry = &mockRegistry{}

	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	signals := &ContractSignals{
		ShortChanID: lnwire.ShortChannelID{},
	}
	chanArb.UpdateContractSignals(signals)

	// Add HTLC to channel arbitrator.
	htlcAmt := 10000
	htlc := channeldb.HTLC{
		Incoming:  false,
		Amt:       lnwire.MilliSatoshi(htlcAmt),
		HtlcIndex: 99,
	}

	outgoingDustHtlc := channeldb.HTLC{
		Incoming:    false,
		Amt:         100,
		HtlcIndex:   100,
		OutputIndex: -1,
	}

	incomingDustHtlc := channeldb.HTLC{
		Incoming:    true,
		Amt:         105,
		HtlcIndex:   101,
		OutputIndex: -1,
	}

	htlcSet := []channeldb.HTLC{
		htlc, outgoingDustHtlc, incomingDustHtlc,
	}

	newUpdate := &ContractUpdate{
		HtlcKey: LocalHtlcSet,
		Htlcs:   htlcSet,
	}
	chanArb.notifyContractUpdate(newUpdate)

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
	chanArbCtx.AssertStateTransitions(
		StateBroadcastCommit,
		StateCommitmentBroadcasted,
	)
	select {
	case <-respChan:
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	// We expect an immediate resolution message for the outgoing dust htlc.
	// It is not resolvable on-chain and it can be canceled back even before
	// the commitment transaction confirmed.
	select {
	case msgs := <-chanArbCtx.resolutions:
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, instead got %v",
				len(msgs))
		}

		if msgs[0].HtlcIndex != outgoingDustHtlc.HtlcIndex {
			t.Fatalf("wrong htlc index: expected %v, got %v",
				outgoingDustHtlc.HtlcIndex, msgs[0].HtlcIndex)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("resolution msgs not sent")
	}

	// Now notify about the local force close getting confirmed.
	closeTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{},
				Witness: [][]byte{
					{0x1},
					{0x2},
				},
			},
		},
	}
	closeTxid := closeTx.TxHash()

	htlcOp := wire.OutPoint{
		Hash:  closeTx.TxHash(),
		Index: 0,
	}

	// Set up the outgoing resolution. Populate SignedTimeoutTx because our
	// commitment transaction got confirmed.
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

	//nolint:ll
	chanArb.cfg.ChainEvents.LocalUnilateralClosure <- &LocalUnilateralCloseInfo{
		SpendDetail: &chainntnfs.SpendDetail{},
		LocalForceCloseSummary: &lnwallet.LocalForceCloseSummary{
			CloseTx: closeTx,
			ContractResolutions: fn.Some(lnwallet.ContractResolutions{
				HtlcResolutions: &lnwallet.HtlcResolutions{
					OutgoingHTLCs: []lnwallet.OutgoingHtlcResolution{
						outgoingRes,
					},
				},
			}),
		},
		ChannelCloseSummary: &channeldb.ChannelCloseSummary{},
		CommitSet: CommitSet{
			ConfCommitKey: fn.Some(LocalHtlcSet),
			HtlcSets: map[HtlcSetKey][]channeldb.HTLC{
				LocalHtlcSet: htlcSet,
			},
		},
	}

	chanArbCtx.AssertStateTransitions(
		StateContractClosed,
		StateWaitingFullResolution,
	)

	// We'll grab the old notifier here as our resolvers are still holding
	// a reference to this instance, and a new one will be created when we
	// restart the channel arb below.
	oldNotifier := chanArb.cfg.Notifier.(*mock.ChainNotifier)

	// At this point, in order to simulate a restart, we'll re-create the
	// channel arbitrator. We do this to ensure that all information
	// required to properly resolve this HTLC are populated.
	if err := chanArb.Stop(); err != nil {
		t.Fatalf("unable to stop chan arb: %v", err)
	}

	// Assert that a final resolution was stored for the incoming dust htlc.
	expectedFinalHtlcs := map[uint64]bool{
		incomingDustHtlc.HtlcIndex: false,
	}
	require.Equal(t, expectedFinalHtlcs, chanArbCtx.finalHtlcs)

	// We'll now re-create the resolver, notice that we use the existing
	// arbLog so it carries over the same on-disk state.
	chanArbCtxNew, err := chanArbCtx.Restart(nil)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb = chanArbCtxNew.chanArb
	defer chanArbCtxNew.CleanUp()

	// Post restart, it should be the case that our resolver was properly
	// supplemented, and we only have a single resolver in the final set.
	// The resolvers are added concurrently so we need to wait here.
	err = wait.NoError(func() error {
		chanArb.activeResolversLock.Lock()
		defer chanArb.activeResolversLock.Unlock()

		if len(chanArb.activeResolvers) != 1 {
			return fmt.Errorf("expected single resolver, instead "+
				"got: %v", len(chanArb.activeResolvers))
		}

		return nil
	}, defaultTimeout)
	require.NoError(t, err)

	// We'll now examine the in-memory state of the active resolvers to
	// ensure t hey were populated properly.
	resolver := chanArb.activeResolvers[0]
	outgoingResolver, ok := resolver.(*htlcOutgoingContestResolver)
	if !ok {
		t.Fatalf("expected outgoing contest resolver, got %vT",
			resolver)
	}

	// The resolver should have its htlc amt field populated as it.
	if int64(outgoingResolver.htlc.Amt) != int64(htlcAmt) {
		t.Fatalf("wrong htlc amount: expected %v, got %v,",
			htlcAmt, int64(outgoingResolver.htlc.Amt))
	}

	// htlcOutgoingContestResolver is now active and waiting for the HTLC to
	// expire. It should not yet have passed it on for incubation.
	select {
	case <-chanArbCtx.incubationRequests:
		t.Fatalf("contract should not be incubated yet")
	default:
	}

	// Send a notification that the expiry height has been reached.
	oldNotifier.EpochChan <- &chainntnfs.BlockEpoch{Height: 10}

	// htlcOutgoingContestResolver is now transforming into a
	// htlcTimeoutResolver and should send the contract off for incubation.
	select {
	case <-chanArbCtx.incubationRequests:
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	// Notify resolver that the HTLC output of the commitment has been
	// spent.
	oldNotifier.SpendChan <- &chainntnfs.SpendDetail{
		SpendingTx:    closeTx,
		SpentOutPoint: &wire.OutPoint{},
		SpenderTxHash: &closeTxid,
	}

	// Finally, we should also receive a resolution message instructing the
	// switch to cancel back the HTLC.
	select {
	case msgs := <-chanArbCtx.resolutions:
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, instead got %v", len(msgs))
		}

		if msgs[0].HtlcIndex != htlc.HtlcIndex {
			t.Fatalf("wrong htlc index: expected %v, got %v",
				htlc.HtlcIndex, msgs[0].HtlcIndex)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("resolution msgs not sent")
	}

	// As this is our own commitment transaction, the HTLC will go through
	// to the second level. Channel arbitrator should still not be marked
	// as resolved.
	select {
	case <-chanArbCtxNew.resolvedChan:
		t.Fatalf("channel resolved prematurely")
	default:
	}

	// Notify resolver that the output of the timeout tx has been spent.
	oldNotifier.SpendChan <- &chainntnfs.SpendDetail{
		SpendingTx:    closeTx,
		SpentOutPoint: &wire.OutPoint{},
		SpenderTxHash: &closeTxid,
	}

	// At this point channel should be marked as resolved.
	chanArbCtxNew.AssertStateTransitions(StateFullyResolved)
	select {
	case <-chanArbCtxNew.resolvedChan:
	case <-time.After(defaultTimeout):
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

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb := chanArbCtx.chanArb

	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	chanArbCtx.AssertState(StateDefault)

	// Create a channel we can use to assert the state when it publishes
	// the close tx.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx, string) error {
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
	chanArbCtx.AssertStateTransitions(StateBroadcastCommit)

	// We expect it to be in state StateBroadcastCommit when publishing
	// the force close.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(stateTimeout):
		t.Fatalf("no state update received")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	chanArbCtx.AssertStateTransitions(StateCommitmentBroadcasted)

	// Wait for a response to the force close.
	select {
	case <-respChan:
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	// The state should be StateCommitmentBroadcasted.
	chanArbCtx.AssertState(StateCommitmentBroadcasted)

	// Now notify about the _REMOTE_ commitment getting confirmed.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}
	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- &RemoteUnilateralCloseInfo{
		UnilateralCloseSummary: uniClose,
	}

	// It should transition StateContractClosed -> StateFullyResolved.
	chanArbCtx.AssertStateTransitions(StateContractClosed, StateFullyResolved)

	// It should resolve.
	select {
	case <-chanArbCtx.resolvedChan:
		// Expected.
	case <-time.After(stateTimeout):
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

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb := chanArbCtx.chanArb

	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	defer chanArb.Stop()

	// It should start out in the default state.
	chanArbCtx.AssertState(StateDefault)

	// Return ErrDoubleSpend when attempting to publish the tx.
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx, string) error {
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
	chanArbCtx.AssertStateTransitions(StateBroadcastCommit)

	// We expect it to be in state StateBroadcastCommit when publishing
	// the force close.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(stateTimeout):
		t.Fatalf("no state update received")
	}

	// After broadcasting, transition should be to
	// StateCommitmentBroadcasted.
	chanArbCtx.AssertStateTransitions(StateCommitmentBroadcasted)

	// Wait for a response to the force close.
	select {
	case <-respChan:
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("error force closing channel: %v", err)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	// The state should be StateCommitmentBroadcasted.
	chanArbCtx.AssertState(StateCommitmentBroadcasted)

	// Now notify about the _REMOTE_ commitment getting confirmed.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}
	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- &RemoteUnilateralCloseInfo{
		UnilateralCloseSummary: uniClose,
	}

	// It should transition StateContractClosed -> StateFullyResolved.
	chanArbCtx.AssertStateTransitions(StateContractClosed, StateFullyResolved)

	// It should resolve.
	select {
	case <-chanArbCtx.resolvedChan:
		// Expected.
	case <-time.After(stateTimeout):
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

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")

	chanArb := chanArbCtx.chanArb
	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}

	// It should start in StateDefault.
	chanArbCtx.AssertState(StateDefault)

	// Send a remote force close event.
	commitSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &chainhash.Hash{},
	}

	uniClose := &lnwallet.UnilateralCloseSummary{
		SpendDetail:     commitSpend,
		HtlcResolutions: &lnwallet.HtlcResolutions{},
	}
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- &RemoteUnilateralCloseInfo{
		UnilateralCloseSummary: uniClose,
	}

	// Since writing the resolutions fail, the arbitrator should not
	// advance to the next state.
	time.Sleep(100 * time.Millisecond)
	if log.state != StateDefault {
		t.Fatalf("expected to stay in StateDefault")
	}

	// Restart the channel arb, this'll use the same long and prior
	// context.
	chanArbCtx, err = chanArbCtx.Restart(nil)
	require.NoError(t, err, "unable to restart channel arb")
	chanArb = chanArbCtx.chanArb

	// Again, it should start up in the default state.
	chanArbCtx.AssertState(StateDefault)

	// Now we make the log succeed writing the resolutions, but fail when
	// attempting to close the channel.
	log.failLog = false
	chanArb.cfg.MarkChannelClosed = func(*channeldb.ChannelCloseSummary,
		...channeldb.ChannelStatus) error {

		return fmt.Errorf("intentional close error")
	}

	// Send a new remote force close event.
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- &RemoteUnilateralCloseInfo{
		UnilateralCloseSummary: uniClose,
	}

	// Since closing the channel failed, the arbitrator should stay in the
	// default state.
	time.Sleep(100 * time.Millisecond)
	if log.state != StateDefault {
		t.Fatalf("expected to stay in StateDefault")
	}

	// Restart once again to simulate yet another restart.
	chanArbCtx, err = chanArbCtx.Restart(nil)
	require.NoError(t, err, "unable to restart channel arb")
	chanArb = chanArbCtx.chanArb

	// Starts out in StateDefault.
	chanArbCtx.AssertState(StateDefault)

	// Now make fetching the resolutions fail.
	log.failFetch = fmt.Errorf("intentional fetch failure")
	chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- &RemoteUnilateralCloseInfo{
		UnilateralCloseSummary: uniClose,
	}

	// Since logging the resolutions and closing the channel now succeeds,
	// it should advance to StateContractClosed.
	chanArbCtx.AssertStateTransitions(StateContractClosed)

	// It should not advance further, however, as fetching resolutions
	// failed.
	time.Sleep(100 * time.Millisecond)
	if log.state != StateContractClosed {
		t.Fatalf("expected to stay in StateContractClosed")
	}
	chanArb.Stop()

	// Create a new arbitrator, and now make fetching resolutions succeed.
	log.failFetch = nil
	chanArbCtx, err = chanArbCtx.Restart(nil)
	require.NoError(t, err, "unable to restart channel arb")
	defer chanArbCtx.CleanUp()

	// Finally it should advance to StateFullyResolved.
	chanArbCtx.AssertStateTransitions(StateFullyResolved)

	// It should also mark the channel as resolved.
	select {
	case <-chanArbCtx.resolvedChan:
		// Expected.
	case <-time.After(defaultTimeout):
		t.Fatalf("contract was not resolved")
	}
}

// TestChannelArbitratorForceCloseBreachedChannel tests that the channel
// arbitrator is able to handle a channel in the process of being force closed
// is breached by the remote node. In these cases we expect the
// ChannelArbitrator to properly execute the breachResolver flow and then
// gracefully exit once the breachResolver receives the signal from what would
// normally be the BreachArbitrator.
func TestChannelArbitratorForceCloseBreachedChannel(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
		resolvers: make(map[ContractResolver]struct{}),
	}

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")

	chanArb := chanArbCtx.chanArb
	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}

	// It should start in StateDefault.
	chanArbCtx.AssertState(StateDefault)

	// We start by attempting a local force close. We'll return an
	// unexpected publication error, causing the state machine to halt.
	expErr := errors.New("intentional publication error")
	stateChan := make(chan ArbitratorState)
	chanArb.cfg.PublishTx = func(*wire.MsgTx, string) error {
		// When the force close tx is being broadcasted, check that the
		// state is correct at that point.
		select {
		case stateChan <- chanArb.state:
		case <-chanArb.quit:
			return fmt.Errorf("exiting")
		}
		return expErr
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
	chanArbCtx.AssertStateTransitions(StateBroadcastCommit)

	// We expect it to be in state StateBroadcastCommit when attempting
	// the force close.
	select {
	case state := <-stateChan:
		if state != StateBroadcastCommit {
			t.Fatalf("state during PublishTx was %v", state)
		}
	case <-time.After(stateTimeout):
		t.Fatalf("no state update received")
	}

	// Make sure we get the expected error.
	select {
	case err := <-errChan:
		if err != expErr {
			t.Fatalf("unexpected error force closing channel: %v",
				err)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("no response received")
	}

	// Before restarting, we'll need to modify the arbitrator log to have
	// a set of contract resolutions and a commit set.
	log.resolutions = &ContractResolutions{
		BreachResolution: &BreachResolution{
			FundingOutPoint: wire.OutPoint{},
		},
	}
	log.commitSet = &CommitSet{
		ConfCommitKey: fn.Some(RemoteHtlcSet),
		HtlcSets: map[HtlcSetKey][]channeldb.HTLC{
			RemoteHtlcSet: {},
		},
	}

	// We mimic that the channel is breached while the channel arbitrator
	// is down. This means that on restart it will be started with a
	// pending close channel, of type BreachClose.
	chanArbCtx, err = chanArbCtx.Restart(func(c *chanArbTestCtx) {
		c.chanArb.cfg.IsPendingClose = true
		c.chanArb.cfg.ClosingHeight = 100
		c.chanArb.cfg.CloseType = channeldb.BreachClose
	})
	require.NoError(t, err, "unable to create ChannelArbitrator")
	defer chanArbCtx.CleanUp()

	// We should transition to StateContractClosed.
	chanArbCtx.AssertStateTransitions(
		StateContractClosed, StateWaitingFullResolution,
	)

	// Wait for SubscribeBreachComplete to be called.
	<-chanArbCtx.breachSubscribed

	// We'll close the breachResolutionChan to cleanup the breachResolver
	// and make the state transition to StateFullyResolved.
	close(chanArbCtx.breachResolutionChan)

	chanArbCtx.AssertStateTransitions(StateFullyResolved)

	// It should also mark the channel as resolved.
	select {
	case <-chanArbCtx.resolvedChan:
		// Expected.
	case <-time.After(defaultTimeout):
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
				chanArb.cfg.ChainEvents.RemoteUnilateralClosure <- &RemoteUnilateralCloseInfo{
					UnilateralCloseSummary: uniClose,
				}
			},
			expectedStates: []ArbitratorState{StateContractClosed, StateFullyResolved},
		},
		{
			closeType: channeldb.LocalForceClose,
			//nolint:ll
			sendEvent: func(chanArb *ChannelArbitrator) {
				chanArb.cfg.ChainEvents.LocalUnilateralClosure <- &LocalUnilateralCloseInfo{
					SpendDetail: &chainntnfs.SpendDetail{},
					LocalForceCloseSummary: &lnwallet.LocalForceCloseSummary{
						CloseTx: &wire.MsgTx{},
						ContractResolutions: fn.Some(lnwallet.ContractResolutions{
							HtlcResolutions: &lnwallet.HtlcResolutions{},
						}),
					},
					ChannelCloseSummary: &channeldb.ChannelCloseSummary{},
				}
			},
			expectedStates: []ArbitratorState{StateContractClosed, StateFullyResolved},
		},
	}

	for _, test := range testCases {
		test := test

		log := &mockArbitratorLog{
			state:      StateDefault,
			newStates:  make(chan ArbitratorState, 5),
			failCommit: true,

			// Set the log to fail on the first expected state
			// after state machine progress for this test case.
			failCommitState: test.expectedStates[0],
		}

		chanArbCtx, err := createTestChannelArbitrator(t, log)
		if err != nil {
			t.Fatalf("unable to create ChannelArbitrator: %v", err)
		}

		chanArb := chanArbCtx.chanArb
		beat := newBeatFromHeight(0)
		if err := chanArb.Start(nil, beat); err != nil {
			t.Fatalf("unable to start ChannelArbitrator: %v", err)
		}

		// It should start in StateDefault.
		chanArbCtx.AssertState(StateDefault)

		closed := make(chan struct{})
		chanArb.cfg.MarkChannelClosed = func(
			*channeldb.ChannelCloseSummary,
			...channeldb.ChannelStatus) error {

			close(closed)
			return nil
		}

		// Send the test event to trigger the state machine.
		test.sendEvent(chanArb)

		select {
		case <-closed:
		case <-time.After(defaultTimeout):
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
		log.failCommit = false
		chanArbCtx, err = chanArbCtx.Restart(func(c *chanArbTestCtx) {
			c.chanArb.cfg.IsPendingClose = true
			c.chanArb.cfg.ClosingHeight = 100
			c.chanArb.cfg.CloseType = test.closeType
		})
		if err != nil {
			t.Fatalf("unable to create ChannelArbitrator: %v", err)
		}

		// Since the channel is marked closed in the database, it
		// should advance to the expected states.
		chanArbCtx.AssertStateTransitions(test.expectedStates...)

		// It should also mark the channel as resolved.
		select {
		case <-chanArbCtx.resolvedChan:
			// Expected.
		case <-time.After(defaultTimeout):
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

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")

	chanArb := chanArbCtx.chanArb
	chanArb.cfg.IsPendingClose = true
	chanArb.cfg.ClosingHeight = 100
	chanArb.cfg.CloseType = channeldb.RemoteForceClose

	beat := newBeatFromHeight(100)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}

	// It should not advance its state beyond StateContractClosed, since
	// fetching resolutions fails.
	chanArbCtx.AssertStateTransitions(StateContractClosed)

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
	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb := chanArbCtx.chanArb
	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
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

	// Finally, we should ensure that we are not able to do so by seeing
	// the expected errAlreadyForceClosed error.
	select {
	case err = <-errChan:
		if err != errAlreadyForceClosed {
			t.Fatalf("expected errAlreadyForceClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected to receive error response")
	}
}

// TestChannelArbitratorDanglingCommitForceClose tests that if there're HTLCs
// on the remote party's commitment, but not ours, and they're about to time
// out, then we'll go on chain so we can cancel back the HTLCs on the incoming
// commitment.
func TestChannelArbitratorDanglingCommitForceClose(t *testing.T) {
	t.Parallel()

	type testCase struct {
		htlcExpired       bool
		remotePendingHTLC bool
		confCommit        HtlcSetKey
	}
	var testCases []testCase

	testOptions := []bool{true, false}
	confOptions := []HtlcSetKey{
		LocalHtlcSet, RemoteHtlcSet, RemotePendingHtlcSet,
	}
	for _, htlcExpired := range testOptions {
		for _, remotePendingHTLC := range testOptions {
			for _, commitConf := range confOptions {
				switch {
				// If the HTLC is on the remote commitment, and
				// that one confirms, then there's no special
				// behavior, we should play all the HTLCs on
				// that remote commitment as normal.
				case !remotePendingHTLC && commitConf == RemoteHtlcSet:
					fallthrough

				// If the HTLC is on the remote pending, and
				// that confirms, then we don't have any
				// special actions.
				case remotePendingHTLC && commitConf == RemotePendingHtlcSet:
					continue
				}

				testCases = append(testCases, testCase{
					htlcExpired:       htlcExpired,
					remotePendingHTLC: remotePendingHTLC,
					confCommit:        commitConf,
				})
			}
		}
	}

	for _, testCase := range testCases {
		testCase := testCase
		testName := fmt.Sprintf("testCase: htlcExpired=%v,"+
			"remotePendingHTLC=%v,remotePendingCommitConf=%v",
			testCase.htlcExpired, testCase.remotePendingHTLC,
			testCase.confCommit)

		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			arbLog := &mockArbitratorLog{
				state:     StateDefault,
				newStates: make(chan ArbitratorState, 5),
				resolvers: make(map[ContractResolver]struct{}),
			}

			chanArbCtx, err := createTestChannelArbitrator(
				t, arbLog,
			)
			if err != nil {
				t.Fatalf("unable to create ChannelArbitrator: %v", err)
			}
			chanArb := chanArbCtx.chanArb
			beat := newBeatFromHeight(0)
			err = chanArb.Start(nil, beat)
			require.NoError(t, err)

			defer chanArb.Stop()

			// Now that our channel arb has started, we'll set up
			// its contract signals channel so we can send it
			// various HTLC updates for this test.
			signals := &ContractSignals{
				ShortChanID: lnwire.ShortChannelID{},
			}
			chanArb.UpdateContractSignals(signals)

			htlcKey := RemoteHtlcSet
			if testCase.remotePendingHTLC {
				htlcKey = RemotePendingHtlcSet
			}

			// Next, we'll send it a new HTLC that is set to expire
			// in 10 blocks, this HTLC will only appear on the
			// commitment transaction of the _remote_ party.
			htlcIndex := uint64(99)
			htlcExpiry := uint32(10)
			danglingHTLC := channeldb.HTLC{
				Incoming:      false,
				Amt:           10000,
				HtlcIndex:     htlcIndex,
				RefundTimeout: htlcExpiry,
			}
			newUpdate := &ContractUpdate{
				HtlcKey: htlcKey,
				Htlcs:   []channeldb.HTLC{danglingHTLC},
			}
			chanArb.notifyContractUpdate(newUpdate)

			// At this point, we now have a split commitment state
			// from the PoV of the channel arb. There's now an HTLC
			// that only exists on the commitment transaction of
			// the remote party.
			errChan := make(chan error, 1)
			respChan := make(chan *wire.MsgTx, 1)
			switch {
			// If we want an HTLC expiration trigger, then We'll
			// now mine a block (height 5), which is 5 blocks away
			// (our grace delta) from the expiry of that HTLC.
			case testCase.htlcExpired:
				beat := newBeatFromHeight(5)
				chanArbCtx.chanArb.BlockbeatChan <- beat

			// Otherwise, we'll just trigger a regular force close
			// request.
			case !testCase.htlcExpired:
				chanArb.forceCloseReqs <- &forceCloseReq{
					errResp: errChan,
					closeTx: respChan,
				}

			}

			// At this point, the resolver should now have
			// determined that it needs to go to chain in order to
			// block off the redemption path so it can cancel the
			// incoming HTLC.
			chanArbCtx.AssertStateTransitions(
				StateBroadcastCommit,
				StateCommitmentBroadcasted,
			)

			// Next we'll craft a fake commitment transaction to
			// send to signal that the channel has closed out on
			// chain.
			closeTx := &wire.MsgTx{
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: wire.OutPoint{},
						Witness: [][]byte{
							{0x9},
						},
					},
				},
			}

			// We'll now signal to the channel arb that the HTLC
			// has fully closed on chain. Our local commit set
			// shows now HTLC on our commitment, but one on the
			// remote commitment. This should result in the HTLC
			// being canalled back. Also note that there're no HTLC
			// resolutions sent since we have none on our
			// commitment transaction.
			//
			//nolint:ll
			uniCloseInfo := &LocalUnilateralCloseInfo{
				SpendDetail: &chainntnfs.SpendDetail{},
				LocalForceCloseSummary: &lnwallet.LocalForceCloseSummary{
					CloseTx: closeTx,
					ContractResolutions: fn.Some(lnwallet.ContractResolutions{
						HtlcResolutions: &lnwallet.HtlcResolutions{},
					}),
				},
				ChannelCloseSummary: &channeldb.ChannelCloseSummary{},
				CommitSet: CommitSet{
					ConfCommitKey: fn.Some(
						testCase.confCommit,
					),
					HtlcSets: make(
						map[HtlcSetKey][]channeldb.HTLC,
					),
				},
			}

			// If the HTLC was meant to expire, then we'll mark the
			// closing transaction at the proper expiry height
			// since our comparison "need to timeout" comparison is
			// based on the confirmation height.
			if testCase.htlcExpired {
				uniCloseInfo.SpendDetail.SpendingHeight = 5
			}

			// Depending on if we're testing the remote pending
			// commitment or not, we'll populate either a fake
			// dangling remote commitment, or a regular locked in
			// one.
			htlcs := []channeldb.HTLC{danglingHTLC}
			if testCase.remotePendingHTLC {
				uniCloseInfo.CommitSet.HtlcSets[RemotePendingHtlcSet] = htlcs
			} else {
				uniCloseInfo.CommitSet.HtlcSets[RemoteHtlcSet] = htlcs
			}

			chanArb.cfg.ChainEvents.LocalUnilateralClosure <- uniCloseInfo

			// The channel arb should now transition to waiting
			// until the HTLCs have been fully resolved.
			chanArbCtx.AssertStateTransitions(
				StateContractClosed,
				StateWaitingFullResolution,
			)

			// Now that we've sent this signal, we should have that
			// HTLC be canceled back immediately.
			select {
			case msgs := <-chanArbCtx.resolutions:
				if len(msgs) != 1 {
					t.Fatalf("expected 1 message, "+
						"instead got %v", len(msgs))
				}

				if msgs[0].HtlcIndex != htlcIndex {
					t.Fatalf("wrong htlc index: expected %v, got %v",
						htlcIndex, msgs[0].HtlcIndex)
				}
			case <-time.After(defaultTimeout):
				t.Fatalf("resolution msgs not sent")
			}

			// There's no contract to send a fully resolve message,
			// so instead, we'll mine another block which'll cause
			// it to re-examine its state and realize there're no
			// more HTLCs.
			chanArbCtx.receiveBlockbeat(6)
		})
	}
}

// TestChannelArbitratorPendingExpiredHTLC tests that if we have pending htlc
// that is expired we will only go to chain if we are running at least the
// time defined in PaymentsExpirationGracePeriod.
// During this time the remote party is expected to send his updates and cancel
// The htlc.
func TestChannelArbitratorPendingExpiredHTLC(t *testing.T) {
	t.Parallel()

	// We'll create the arbitrator and its backing log in a default state.
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
		resolvers: make(map[ContractResolver]struct{}),
	}
	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")
	chanArb := chanArbCtx.chanArb

	// We'll inject a test clock implementation so we can control the uptime.
	startTime := time.Date(2020, time.February, 3, 13, 0, 0, 0, time.UTC)
	testClock := clock.NewTestClock(startTime)
	chanArb.cfg.Clock = testClock

	// We also configure the grace period and the IsForwardedHTLC to identify
	// the htlc as our initiated payment.
	chanArb.cfg.PaymentsExpirationGracePeriod = time.Second * 15
	chanArb.cfg.IsForwardedHTLC = func(chanID lnwire.ShortChannelID,
		htlcIndex uint64) bool {

		return false
	}

	beat := newBeatFromHeight(0)
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	t.Cleanup(func() {
		require.NoError(t, chanArb.Stop())
	})

	// Now that our channel arb has started, we'll set up
	// its contract signals channel so we can send it
	// various HTLC updates for this test.
	signals := &ContractSignals{
		ShortChanID: lnwire.ShortChannelID{},
	}
	chanArb.UpdateContractSignals(signals)

	// Next, we'll send it a new HTLC that is set to expire
	// in 10 blocks.
	htlcIndex := uint64(99)
	htlcExpiry := uint32(10)
	pendingHTLC := channeldb.HTLC{
		Incoming:      false,
		Amt:           10000,
		HtlcIndex:     htlcIndex,
		RefundTimeout: htlcExpiry,
	}
	newUpdate := &ContractUpdate{
		HtlcKey: RemoteHtlcSet,
		Htlcs:   []channeldb.HTLC{pendingHTLC},
	}
	chanArb.notifyContractUpdate(newUpdate)

	// We will advance the uptime to 10 seconds which should be still within
	// the grace period and should not trigger going to chain.
	testClock.SetTime(startTime.Add(time.Second * 10))
	beat = newBeatFromHeight(5)
	chanArbCtx.chanArb.BlockbeatChan <- beat
	chanArbCtx.AssertState(StateDefault)

	// We will advance the uptime to 16 seconds which should trigger going
	// to chain.
	testClock.SetTime(startTime.Add(time.Second * 16))
	beat = newBeatFromHeight(6)
	chanArbCtx.chanArb.BlockbeatChan <- beat
	chanArbCtx.AssertStateTransitions(
		StateBroadcastCommit,
		StateCommitmentBroadcasted,
	)
}

// TestRemoteCloseInitiator tests the setting of close initiator statuses
// for remote force closes and breaches.
func TestRemoteCloseInitiator(t *testing.T) {
	// getCloseSummary returns a unilateral close summary for the channel
	// provided.
	getCloseSummary := func(channel *channeldb.OpenChannel) *RemoteUnilateralCloseInfo {
		return &RemoteUnilateralCloseInfo{
			UnilateralCloseSummary: &lnwallet.UnilateralCloseSummary{
				SpendDetail: &chainntnfs.SpendDetail{
					SpenderTxHash: &chainhash.Hash{},
					SpendingTx: &wire.MsgTx{
						TxIn:  []*wire.TxIn{},
						TxOut: []*wire.TxOut{},
					},
				},
				ChannelCloseSummary: channeldb.ChannelCloseSummary{
					ChanPoint:         channel.FundingOutpoint,
					RemotePub:         channel.IdentityPub,
					SettledBalance:    btcutil.Amount(500),
					TimeLockedBalance: btcutil.Amount(10000),
					IsPending:         false,
				},
				HtlcResolutions: &lnwallet.HtlcResolutions{},
			},
		}
	}

	tests := []struct {
		name string

		// notifyClose sends the appropriate chain event to indicate
		// that the channel has closed. The event subscription channel
		// is expected to be buffered, as is the default for test
		// channel arbitrators.
		notifyClose func(sub *ChainEventSubscription,
			channel *channeldb.OpenChannel)

		// expectedStates is the set of states we expect the arbitrator
		// to progress through.
		expectedStates []ArbitratorState
	}{
		{
			name: "force close",
			notifyClose: func(sub *ChainEventSubscription,
				channel *channeldb.OpenChannel) {

				s := getCloseSummary(channel)
				sub.RemoteUnilateralClosure <- s
			},
			expectedStates: []ArbitratorState{
				StateContractClosed, StateFullyResolved,
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// First, create alice's channel.
			alice, _, err := lnwallet.CreateTestChannels(
				t, channeldb.SingleFunderTweaklessBit,
			)
			if err != nil {
				t.Fatalf("unable to create test channels: %v",
					err)
			}

			// Create a mock log which will not block the test's
			// expected number of transitions transitions, and has
			// no commit resolutions so that the channel will
			// resolve immediately.
			log := &mockArbitratorLog{
				state: StateDefault,
				newStates: make(chan ArbitratorState,
					len(test.expectedStates)),
				resolutions: &ContractResolutions{
					CommitHash:       chainhash.Hash{},
					CommitResolution: nil,
				},
			}

			// Mock marking the channel as closed, we only care
			// about setting of channel status.
			mockMarkClosed := func(_ *channeldb.ChannelCloseSummary,
				statuses ...channeldb.ChannelStatus) error {

				for _, status := range statuses {
					err := alice.State().ApplyChanStatus(status)
					if err != nil {
						return err
					}
				}
				return nil
			}

			chanArbCtx, err := createTestChannelArbitrator(
				t, log, withMarkClosed(mockMarkClosed),
			)
			if err != nil {
				t.Fatalf("unable to create "+
					"ChannelArbitrator: %v", err)
			}
			chanArb := chanArbCtx.chanArb
			beat := newBeatFromHeight(0)
			if err := chanArb.Start(nil, beat); err != nil {
				t.Fatalf("unable to start "+
					"ChannelArbitrator: %v", err)
			}
			t.Cleanup(func() {
				require.NoError(t, chanArb.Stop())
			})

			// It should start out in the default state.
			chanArbCtx.AssertState(StateDefault)

			// Notify the close event.
			test.notifyClose(chanArb.cfg.ChainEvents, alice.State())

			// Check that the channel transitions as expected.
			chanArbCtx.AssertStateTransitions(
				test.expectedStates...,
			)

			// It should also mark the channel as resolved.
			select {
			case <-chanArbCtx.resolvedChan:
				// Expected.
			case <-time.After(defaultTimeout):
				t.Fatalf("contract was not resolved")
			}

			// Check that alice has the status we expect.
			if !alice.State().HasChanStatus(
				channeldb.ChanStatusRemoteCloseInitiator,
			) {

				t.Fatalf("expected remote close initiator, "+
					"got: %v", alice.State().ChanStatus())
			}
		})
	}
}

// TestFindCommitmentDeadlineAndValue tests the logic used to determine
// confirmation deadline and total time-sensitive value is implemented as
// expected.
func TestFindCommitmentDeadlineAndValue(t *testing.T) {
	// Create a testing channel arbitrator.
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}
	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")

	// Add a dummy payment hash to the preimage lookup.
	rHash := [lntypes.PreimageSize]byte{1, 2, 3}
	mockPreimageDB := newMockWitnessBeacon()
	mockPreimageDB.lookupPreimage[rHash] = rHash

	// Attach a mock PreimageDB and Registry to channel arbitrator.
	chanArb := chanArbCtx.chanArb
	chanArb.cfg.PreimageDB = mockPreimageDB
	chanArb.cfg.Registry = &mockRegistry{}

	htlcIndexBase := uint64(99)
	heightHint := uint32(1000)
	htlcExpiryBase := heightHint + uint32(10)

	htlcAmt := lnwire.MilliSatoshi(1_000_000)

	// Create four testing HTLCs.
	htlcDust := channeldb.HTLC{
		HtlcIndex:     htlcIndexBase + 1,
		RefundTimeout: htlcExpiryBase + 1,
		OutputIndex:   -1,
		Amt:           htlcAmt,
	}
	htlcSmallExipry := channeldb.HTLC{
		HtlcIndex:     htlcIndexBase + 2,
		RefundTimeout: htlcExpiryBase + 2,
		Amt:           htlcAmt,
	}

	htlcPreimage := channeldb.HTLC{
		HtlcIndex:     htlcIndexBase + 3,
		RefundTimeout: htlcExpiryBase + 3,
		RHash:         rHash,
		Amt:           htlcAmt,
	}
	htlcLargeExpiry := channeldb.HTLC{
		HtlcIndex:     htlcIndexBase + 4,
		RefundTimeout: htlcExpiryBase + 100,
		Amt:           htlcAmt,
	}
	htlcExpired := channeldb.HTLC{
		HtlcIndex:     htlcIndexBase + 5,
		RefundTimeout: heightHint,
		Amt:           htlcAmt,
	}

	makeHTLCSet := func(incoming, outgoing channeldb.HTLC) htlcSet {
		return htlcSet{
			incomingHTLCs: map[uint64]channeldb.HTLC{
				incoming.HtlcIndex: incoming,
			},
			outgoingHTLCs: map[uint64]channeldb.HTLC{
				outgoing.HtlcIndex: outgoing,
			},
		}
	}

	testCases := []struct {
		name                         string
		htlcs                        htlcSet
		err                          error
		deadline                     fn.Option[int32]
		mockFindOutgoingHTLCDeadline func()
		expectedBudget               btcutil.Amount
	}{
		{
			// When we have no HTLCs, the default value should be
			// used.
			name:                         "use default conf target",
			htlcs:                        htlcSet{},
			err:                          nil,
			deadline:                     fn.None[int32](),
			mockFindOutgoingHTLCDeadline: func() {},
			expectedBudget:               0,
		},
		{
			// When we have a preimage available in the local HTLC
			// set, its CLTV should be used. And the value left
			// should be the sum of the HTLCs minus their budgets,
			// which is exactly htlcAmt.
			name:  "use htlc with preimage available",
			htlcs: makeHTLCSet(htlcPreimage, htlcLargeExpiry),
			err:   nil,
			deadline: fn.Some(int32(
				(htlcPreimage.RefundTimeout - heightHint) / 2,
			)),
			mockFindOutgoingHTLCDeadline: func() {
				chanArb.cfg.FindOutgoingHTLCDeadline = func(
					htlc channeldb.HTLC) fn.Option[int32] {

					return fn.Some(int32(
						htlcLargeExpiry.RefundTimeout,
					))
				}
			},
			expectedBudget: htlcAmt.ToSatoshis(),
		},
		{
			// When the HTLC in the local set is not preimage
			// available, we should not use its CLTV even its value
			// is smaller. And the value left should be half of
			// htlcAmt.
			name:  "use htlc with no preimage available",
			htlcs: makeHTLCSet(htlcSmallExipry, htlcLargeExpiry),
			err:   nil,
			deadline: fn.Some(int32(
				(htlcLargeExpiry.RefundTimeout -
					heightHint) / 2,
			)),
			mockFindOutgoingHTLCDeadline: func() {
				chanArb.cfg.FindOutgoingHTLCDeadline = func(
					htlc channeldb.HTLC) fn.Option[int32] {

					return fn.Some(int32(
						htlcLargeExpiry.RefundTimeout,
					))
				}
			},
			expectedBudget: htlcAmt.ToSatoshis() / 2,
		},
		{
			// When we have dust HTLCs, their CLTVs should NOT be
			// used even the values are smaller. And the value left
			// should be half of htlcAmt.
			name:  "ignore dust HTLCs",
			htlcs: makeHTLCSet(htlcPreimage, htlcDust),
			err:   nil,
			deadline: fn.Some(int32(
				(htlcPreimage.RefundTimeout - heightHint) / 2,
			)),
			mockFindOutgoingHTLCDeadline: func() {
				chanArb.cfg.FindOutgoingHTLCDeadline = func(
					htlc channeldb.HTLC) fn.Option[int32] {

					return fn.Some(int32(
						htlcDust.RefundTimeout,
					))
				}
			},
			expectedBudget: htlcAmt.ToSatoshis() / 2,
		},
		{
			// When we've reached our deadline, use conf target of
			// 1 as our deadline. And the value left should be
			// htlcAmt.
			name:     "use conf target 1",
			htlcs:    makeHTLCSet(htlcPreimage, htlcExpired),
			err:      nil,
			deadline: fn.Some(int32(1)),
			mockFindOutgoingHTLCDeadline: func() {
				chanArb.cfg.FindOutgoingHTLCDeadline = func(
					htlc channeldb.HTLC) fn.Option[int32] {

					return fn.Some(int32(
						htlcExpired.RefundTimeout,
					))
				}
			},
			expectedBudget: htlcAmt.ToSatoshis(),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Mock the method `FindOutgoingHTLCDeadline`.
			tc.mockFindOutgoingHTLCDeadline()

			deadline, budget, err := chanArb.
				findCommitmentDeadlineAndValue(
					heightHint, tc.htlcs,
				)

			require.Equal(t, tc.err, err)
			require.Equal(t, tc.deadline, deadline)
			require.Equal(t, tc.expectedBudget, budget)
		})
	}
}

// TestSweepAnchors checks the sweep transactions are created using the
// expected deadlines for different anchor resolutions.
func TestSweepAnchors(t *testing.T) {
	// Create a testing channel arbitrator.
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}
	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")

	// Add a dummy payment hash to the preimage lookup.
	rHash := [lntypes.PreimageSize]byte{1, 2, 3}
	mockPreimageDB := newMockWitnessBeacon()
	mockPreimageDB.lookupPreimage[rHash] = rHash

	// Attach a mock PreimageDB and Registry to channel arbitrator.
	chanArb := chanArbCtx.chanArb
	chanArb.cfg.PreimageDB = mockPreimageDB
	chanArb.cfg.Registry = &mockRegistry{}

	// Set current block height.
	heightHint := uint32(1000)
	chanArbCtx.receiveBlockbeat(int(heightHint))

	htlcIndexBase := uint64(99)
	deadlineDelta := uint32(10)

	htlcAmt := lnwire.MilliSatoshi(1_000_000)

	// Create three testing HTLCs.
	htlcDust := channeldb.HTLC{
		HtlcIndex:     htlcIndexBase + 1,
		RefundTimeout: heightHint + 1,
		OutputIndex:   -1,
	}

	deadlinePreimageDelta := deadlineDelta + 2
	htlcWithPreimage := channeldb.HTLC{
		HtlcIndex:     htlcIndexBase + 2,
		RefundTimeout: heightHint + deadlinePreimageDelta,
		RHash:         rHash,
		Amt:           htlcAmt,
	}

	deadlineSmallDelta := deadlineDelta + 4
	htlcSmallExipry := channeldb.HTLC{
		HtlcIndex:     htlcIndexBase + 3,
		RefundTimeout: heightHint + deadlineSmallDelta,
		Amt:           htlcAmt,
	}

	// Setup our local HTLC set such that we will use the HTLC's CLTV from
	// the incoming HTLC set.
	expectedLocalDeadline := heightHint + deadlinePreimageDelta/2
	chanArb.activeHTLCs[LocalHtlcSet] = htlcSet{
		incomingHTLCs: map[uint64]channeldb.HTLC{
			htlcWithPreimage.HtlcIndex: htlcWithPreimage,
		},
		outgoingHTLCs: map[uint64]channeldb.HTLC{
			htlcDust.HtlcIndex: htlcDust,
		},
	}
	chanArb.unmergedSet[LocalHtlcSet] = htlcSet{
		incomingHTLCs: map[uint64]channeldb.HTLC{
			htlcWithPreimage.HtlcIndex: htlcWithPreimage,
		},
		outgoingHTLCs: map[uint64]channeldb.HTLC{
			htlcDust.HtlcIndex: htlcDust,
		},
	}

	// Setup our remote HTLC set such that no valid HTLCs can be used, thus
	// the anchor sweeping is skipped.
	chanArb.activeHTLCs[RemoteHtlcSet] = htlcSet{
		incomingHTLCs: map[uint64]channeldb.HTLC{
			htlcSmallExipry.HtlcIndex: htlcSmallExipry,
		},
		outgoingHTLCs: map[uint64]channeldb.HTLC{
			htlcDust.HtlcIndex: htlcDust,
		},
	}
	chanArb.unmergedSet[RemoteHtlcSet] = htlcSet{
		incomingHTLCs: map[uint64]channeldb.HTLC{
			htlcSmallExipry.HtlcIndex: htlcSmallExipry,
		},
		outgoingHTLCs: map[uint64]channeldb.HTLC{
			htlcDust.HtlcIndex: htlcDust,
		},
	}

	// Setup out pending remote HTLC set such that we will use the HTLC's
	// CLTV from the outgoing HTLC set.
	expectedPendingDeadline := heightHint + deadlineSmallDelta/2
	chanArb.activeHTLCs[RemotePendingHtlcSet] = htlcSet{
		incomingHTLCs: map[uint64]channeldb.HTLC{
			htlcDust.HtlcIndex: htlcDust,
		},
		outgoingHTLCs: map[uint64]channeldb.HTLC{
			htlcSmallExipry.HtlcIndex: htlcSmallExipry,
		},
	}
	chanArb.unmergedSet[RemotePendingHtlcSet] = htlcSet{
		incomingHTLCs: map[uint64]channeldb.HTLC{
			htlcDust.HtlcIndex: htlcDust,
		},
		outgoingHTLCs: map[uint64]channeldb.HTLC{
			htlcSmallExipry.HtlcIndex: htlcSmallExipry,
		},
	}

	// Mock FindOutgoingHTLCDeadline so the pending remote's outgoing HTLC
	// returns the small expiry value.
	chanArb.cfg.FindOutgoingHTLCDeadline = func(
		htlc channeldb.HTLC) fn.Option[int32] {

		if htlc.RHash != htlcSmallExipry.RHash {
			return fn.None[int32]()
		}

		return fn.Some(int32(htlcSmallExipry.RefundTimeout))
	}

	// Create AnchorResolutions.
	anchors := &lnwallet.AnchorResolutions{
		Local: &lnwallet.AnchorResolution{
			AnchorSignDescriptor: input.SignDescriptor{
				Output: &wire.TxOut{Value: 1},
			},
		},
		Remote: &lnwallet.AnchorResolution{
			AnchorSignDescriptor: input.SignDescriptor{
				Output: &wire.TxOut{Value: 1},
			},
		},
		RemotePending: &lnwallet.AnchorResolution{
			AnchorSignDescriptor: input.SignDescriptor{
				Output: &wire.TxOut{Value: 1},
			},
		},
	}

	// Sweep anchors and check there's no error.
	err = chanArb.sweepAnchors(anchors, heightHint)
	require.NoError(t, err)

	// Verify deadlines are used as expected.
	deadlines := chanArbCtx.sweeper.deadlines

	// We should see two `SweepInput` calls.
	require.Len(t, deadlines, 2)

	// Since there's no guarantee of the deadline orders, we sort it here
	// so they can be compared.
	sort.Ints(deadlines) // [12, 13]
	require.EqualValues(
		t, expectedLocalDeadline, deadlines[0],
		"local deadline not matched, want %v, got %v",
		expectedLocalDeadline, deadlines[0],
	)
	require.EqualValues(
		t, expectedPendingDeadline, deadlines[1],
		"pending remote deadline not matched, want %v, got %v",
		expectedPendingDeadline, deadlines[1],
	)
}

// TestSweepLocalAnchor checks the sweep params used for the local anchor will
// be updated optionally based on the pending remote commit.
func TestSweepLocalAnchor(t *testing.T) {
	// Create a testing channel arbitrator.
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}
	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")

	// Attach a mock PreimageDB and Registry to channel arbitrator.
	chanArb := chanArbCtx.chanArb
	mockPreimageDB := newMockWitnessBeacon()
	chanArb.cfg.PreimageDB = mockPreimageDB
	chanArb.cfg.Registry = &mockRegistry{}

	// Set current block height.
	heightHint := uint32(1000)
	chanArbCtx.receiveBlockbeat(int(heightHint))

	htlcIndex := uint64(99)
	deadlineDelta := uint32(10)

	htlcAmt := lnwire.MilliSatoshi(1_000_000)

	// Create one testing HTLC.
	deadlineSmallDelta := deadlineDelta + 4
	htlcSmallExipry := channeldb.HTLC{
		HtlcIndex:     htlcIndex,
		RefundTimeout: heightHint + deadlineSmallDelta,
		Amt:           htlcAmt,
	}

	// Setup our local HTLC set such that it doesn't have any HTLCs. We
	// expect an anchor sweeping request to be made using the params
	// created from sweeping the anchor from the pending remote commit.
	chanArb.activeHTLCs[LocalHtlcSet] = htlcSet{}

	// Setup our remote HTLC set such that no valid HTLCs can be used, thus
	// the anchor sweeping is skipped.
	chanArb.activeHTLCs[RemoteHtlcSet] = htlcSet{}

	// Setup out pending remote HTLC set such that we will use the HTLC's
	// CLTV from the outgoing HTLC set.
	// Only half of the deadline is used since the anchor cpfp sweep. The
	// other half of the deadline is used to sweep the HTLCs at stake.
	expectedPendingDeadline := heightHint + deadlineSmallDelta/2
	chanArb.activeHTLCs[RemotePendingHtlcSet] = htlcSet{
		outgoingHTLCs: map[uint64]channeldb.HTLC{
			htlcSmallExipry.HtlcIndex: htlcSmallExipry,
		},
	}

	// Mock FindOutgoingHTLCDeadline so the pending remote's outgoing HTLC
	// returns the small expiry value.
	chanArb.cfg.FindOutgoingHTLCDeadline = func(
		htlc channeldb.HTLC) fn.Option[int32] {

		if htlc.RHash != htlcSmallExipry.RHash {
			return fn.None[int32]()
		}

		return fn.Some(int32(htlcSmallExipry.RefundTimeout))
	}

	// Create AnchorResolutions.
	anchors := &lnwallet.AnchorResolutions{
		Local: &lnwallet.AnchorResolution{
			AnchorSignDescriptor: input.SignDescriptor{
				Output: &wire.TxOut{Value: 1},
			},
		},
		Remote: &lnwallet.AnchorResolution{
			AnchorSignDescriptor: input.SignDescriptor{
				Output: &wire.TxOut{Value: 1},
			},
		},
		RemotePending: &lnwallet.AnchorResolution{
			AnchorSignDescriptor: input.SignDescriptor{
				Output: &wire.TxOut{Value: 1},
			},
		},
	}

	// Sweep anchors and check there's no error.
	err = chanArb.sweepAnchors(anchors, heightHint)
	require.NoError(t, err)

	// Verify deadlines are used as expected.
	deadlines := chanArbCtx.sweeper.deadlines

	// We should see two `SweepInput` calls - one for sweeping the local
	// anchor, the other from the remote pending anchor.
	require.Len(t, deadlines, 2)

	// Both deadlines should be the same since the local anchor uses the
	// parameters from the pending remote commitment.
	require.EqualValues(
		t, expectedPendingDeadline, deadlines[0],
		"local deadline not matched, want %v, got %v",
		expectedPendingDeadline, deadlines[0],
	)
	require.EqualValues(
		t, expectedPendingDeadline, deadlines[1],
		"pending remote deadline not matched, want %v, got %v",
		expectedPendingDeadline, deadlines[1],
	)
}

// TestChannelArbitratorAnchors asserts that the commitment tx anchor is swept.
func TestChannelArbitratorAnchors(t *testing.T) {
	log := &mockArbitratorLog{
		state:     StateDefault,
		newStates: make(chan ArbitratorState, 5),
	}

	chanArbCtx, err := createTestChannelArbitrator(t, log)
	require.NoError(t, err, "unable to create ChannelArbitrator")

	// Replace our mocked put report function with one which will push
	// reports into a channel for us to consume. We update this function
	// because our resolver will be created from the existing chanArb cfg.
	reports := make(chan *channeldb.ResolverReport)
	chanArbCtx.chanArb.cfg.PutResolverReport = putResolverReportInChannel(
		reports,
	)

	// Add a dummy payment hash to the preimage lookup.
	rHash := [lntypes.PreimageSize]byte{1, 2, 3}
	mockPreimageDB := newMockWitnessBeacon()
	mockPreimageDB.lookupPreimage[rHash] = rHash

	// Attach a mock PreimageDB and Registry to channel arbitrator.
	chanArb := chanArbCtx.chanArb
	chanArb.cfg.PreimageDB = mockPreimageDB
	chanArb.cfg.Registry = &mockRegistry{}

	// Setup two pre-confirmation anchor resolutions on the mock channel.
	chanArb.cfg.Channel.(*mockChannel).anchorResolutions =
		&lnwallet.AnchorResolutions{
			Local: &lnwallet.AnchorResolution{
				AnchorSignDescriptor: input.SignDescriptor{
					Output: &wire.TxOut{Value: 1},
				},
			},
			Remote: &lnwallet.AnchorResolution{
				AnchorSignDescriptor: input.SignDescriptor{
					Output: &wire.TxOut{Value: 1},
				},
			},
		}

	heightHint := uint32(1000)
	beat := newBeatFromHeight(int32(heightHint))
	if err := chanArb.Start(nil, beat); err != nil {
		t.Fatalf("unable to start ChannelArbitrator: %v", err)
	}
	t.Cleanup(func() {
		require.NoError(t, chanArb.Stop())
	})

	signals := &ContractSignals{
		ShortChanID: lnwire.ShortChannelID{},
	}
	chanArb.UpdateContractSignals(signals)

	htlcAmt := lnwire.MilliSatoshi(1_000_000)

	// Create testing HTLCs.
	spendingHeight := uint32(beat.Height())
	deadlineDelta := uint32(100)

	deadlinePreimageDelta := deadlineDelta
	htlcWithPreimage := channeldb.HTLC{
		HtlcIndex: 99,
		// RefundTimeout is 101.
		RefundTimeout: spendingHeight + deadlinePreimageDelta,
		RHash:         rHash,
		Incoming:      true,
		Amt:           htlcAmt,
	}
	expectedDeadline := deadlineDelta/2 + spendingHeight

	deadlineHTLCdelta := deadlineDelta + 40
	htlc := channeldb.HTLC{
		HtlcIndex: 100,
		// RefundTimeout is 141.
		RefundTimeout: spendingHeight + deadlineHTLCdelta,
		Amt:           htlcAmt,
	}

	// We now send two HTLC updates, one for local HTLC set and the other
	// for remote HTLC set.
	newUpdate := &ContractUpdate{
		HtlcKey: LocalHtlcSet,
		// This will make the deadline of the local anchor resolution
		// to be htlcWithPreimage's CLTV since the incoming HTLC
		// (toLocalHTLCs) has a lower CLTV value and is preimage
		// available.
		Htlcs: []channeldb.HTLC{htlc, htlcWithPreimage},
	}
	chanArb.notifyContractUpdate(newUpdate)

	newUpdate = &ContractUpdate{
		HtlcKey: RemoteHtlcSet,
		// This will make the deadline of the remote anchor resolution
		// to be htlcWithPreimage's CLTV because the incoming HTLC
		// (toRemoteHTLCs) has a lower CLTV.
		Htlcs: []channeldb.HTLC{htlc, htlcWithPreimage},
	}
	chanArb.notifyContractUpdate(newUpdate)

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
	chanArbCtx.AssertStateTransitions(
		StateBroadcastCommit,
		StateCommitmentBroadcasted,
	)

	// With the commitment tx still unconfirmed, we expect sweep attempts
	// for all three versions of the commitment transaction.
	<-chanArbCtx.sweeper.sweptInputs
	<-chanArbCtx.sweeper.sweptInputs

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
	closeTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{},
				Witness: [][]byte{
					{0x1},
					{0x2},
				},
			},
		},
	}

	anchorResolution := &lnwallet.AnchorResolution{
		AnchorSignDescriptor: input.SignDescriptor{
			Output: &wire.TxOut{
				Value: 1,
			},
		},
	}

	//nolint:ll
	chanArb.cfg.ChainEvents.LocalUnilateralClosure <- &LocalUnilateralCloseInfo{
		SpendDetail: &chainntnfs.SpendDetail{
			SpendingHeight: int32(spendingHeight),
		},
		LocalForceCloseSummary: &lnwallet.LocalForceCloseSummary{
			CloseTx: closeTx,
			ContractResolutions: fn.Some(lnwallet.ContractResolutions{
				HtlcResolutions:  &lnwallet.HtlcResolutions{},
				AnchorResolution: anchorResolution,
			}),
		},
		ChannelCloseSummary: &channeldb.ChannelCloseSummary{},
		CommitSet: CommitSet{
			ConfCommitKey: fn.Some(LocalHtlcSet),
			HtlcSets:      map[HtlcSetKey][]channeldb.HTLC{},
		},
	}

	chanArbCtx.AssertStateTransitions(
		StateContractClosed,
		StateWaitingFullResolution,
	)

	// We expect to only have the anchor resolver active.
	if len(chanArb.activeResolvers) != 1 {
		t.Fatalf("expected single resolver, instead got: %v",
			len(chanArb.activeResolvers))
	}

	resolver := chanArb.activeResolvers[0]
	_, ok := resolver.(*anchorResolver)
	if !ok {
		t.Fatalf("expected anchor resolver, got %T", resolver)
	}

	// The anchor resolver is expected to re-offer the anchor input to the
	// sweeper.
	<-chanArbCtx.sweeper.sweptInputs

	// The mock sweeper immediately signals success for that input. This
	// should transition the channel to the resolved state.
	chanArbCtx.AssertStateTransitions(StateFullyResolved)
	select {
	case <-chanArbCtx.resolvedChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("contract was not resolved")
	}

	anchorAmt := btcutil.Amount(
		anchorResolution.AnchorSignDescriptor.Output.Value,
	)
	spendTx := chanArbCtx.sweeper.sweepTx.TxHash()
	expectedReport := &channeldb.ResolverReport{
		OutPoint:        anchorResolution.CommitAnchor,
		Amount:          anchorAmt,
		ResolverType:    channeldb.ResolverTypeAnchor,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &spendTx,
	}

	assertResolverReport(t, reports, expectedReport)

	// We expect two anchor inputs, the local and the remote to be swept.
	// Thus we should expect there are two deadlines used, both are equal
	// to htlcWithPreimage's CLTV.
	require.Equal(t, 2, len(chanArbCtx.sweeper.deadlines))
	require.EqualValues(t,
		expectedDeadline,
		chanArbCtx.sweeper.deadlines[0], "want %d, got %d",
		expectedDeadline, chanArbCtx.sweeper.deadlines[0],
	)
	require.EqualValues(t,
		expectedDeadline,
		chanArbCtx.sweeper.deadlines[1], "want %d, got %d",
		expectedDeadline, chanArbCtx.sweeper.deadlines[1],
	)
}

// TestChannelArbitratorStartForceCloseFail tests that when we run into the
// case where our commitment tx is rejected by our bitcoin backend, or we fail
// to force close, we still continue to startup the arbitrator for a
// specific set of errors.
func TestChannelArbitratorStartForceCloseFail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string

		// The specific error during broadcasting the transaction.
		broadcastErr error

		// forceCloseErr is the error returned when we try to force the
		// channel.
		forceCloseErr error

		// expected state when the startup of the arbitrator succeeds.
		expectedState ArbitratorState

		expectedStartup bool
	}{
		{
			name: "Commitment is rejected because of low mempool " +
				"fees",
			broadcastErr:    lnwallet.ErrMempoolFee,
			expectedState:   StateCommitmentBroadcasted,
			expectedStartup: true,
		},
		{
			// We map a rejected rbf transaction to ErrDoubleSpend
			// in lnd.
			name: "Commitment is rejected because of a " +
				"rbf transaction not succeeding",
			broadcastErr:    lnwallet.ErrDoubleSpend,
			expectedState:   StateCommitmentBroadcasted,
			expectedStartup: true,
		},
		{
			name: "Commitment is rejected with an " +
				"unmatched error",
			broadcastErr:  fmt.Errorf("Reject Commitment Tx"),
			expectedState: StateBroadcastCommit,
			// We should still be able to start up since we other
			// channels might be closing as well and we should
			// resolve the contracts.
			expectedStartup: true,
		},

		// We started after the DLP was triggered, and try to force
		// close. This is rejected as we can't force close with local
		// data loss. We should still be able to start up however.
		{
			name:            "ignore force close local data loss",
			forceCloseErr:   lnwallet.ErrForceCloseLocalDataLoss,
			expectedState:   StateBroadcastCommit,
			expectedStartup: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// We'll create the arbitrator and its backing log
			// to signal that it's already in the process of being
			// force closed.
			log := &mockArbitratorLog{
				newStates: make(chan ArbitratorState, 5),
				state:     StateBroadcastCommit,
			}

			var testOpts []testChanArbOption
			if test.forceCloseErr != nil {
				testOpts = append(
					testOpts,
					withForceCloseErr(test.forceCloseErr),
				)
			}

			chanArbCtx, err := createTestChannelArbitrator(
				t, log, testOpts...,
			)
			require.NoError(
				t, err, "unable to create ChannelArbitrator",
			)

			chanArb := chanArbCtx.chanArb

			// Customize the PublishTx function of the arbitrator.
			chanArb.cfg.PublishTx = func(*wire.MsgTx,
				string) error {

				return test.broadcastErr
			}

			beat := newBeatFromHeight(0)
			err = chanArb.Start(nil, beat)

			if !test.expectedStartup {
				require.ErrorIs(t, err, test.broadcastErr)
				return
			}

			require.NoError(t, err)

			t.Cleanup(func() {
				require.NoError(t, chanArb.Stop())
			})

			// In case the startup succeeds we check that the state
			// is as expected, we only check this if we didn't need
			// to advance from StateBroadcastCommit.
			if test.expectedState != StateBroadcastCommit {
				chanArbCtx.AssertStateTransitions(
					test.expectedState,
				)
			} else {
				// Otherwise, we expect the state to stay the
				// same.
				chanArbCtx.AssertState(test.expectedState)
			}
		})
	}
}

// putResolverReportInChannel returns a put report function which will pipe
// reports into the channel provided.
func putResolverReportInChannel(reports chan *channeldb.ResolverReport) func(
	_ kvdb.RwTx, report *channeldb.ResolverReport) error {

	return func(_ kvdb.RwTx, report *channeldb.ResolverReport) error {
		reports <- report
		return nil
	}
}

// assertResolverReport checks that  a set of reports only contains a single
// report, and that it is equal to the expected report passed in.
func assertResolverReport(t *testing.T, reports chan *channeldb.ResolverReport,
	expected *channeldb.ResolverReport) {

	select {
	case report := <-reports:
		if !reflect.DeepEqual(report, expected) {
			t.Fatalf("expected: %v, got: %v", spew.Sdump(expected),
				spew.Sdump(report))
		}

	case <-time.After(defaultTimeout):
		t.Fatalf("no reports present")
	}
}

type mockChannel struct {
	anchorResolutions *lnwallet.AnchorResolutions

	forceCloseErr error
}

func (m *mockChannel) NewAnchorResolutions() (*lnwallet.AnchorResolutions,
	error) {

	if m.anchorResolutions != nil {
		return m.anchorResolutions, nil
	}

	return &lnwallet.AnchorResolutions{}, nil
}

func (m *mockChannel) ForceCloseChan() (*wire.MsgTx, error) {
	if m.forceCloseErr != nil {
		return nil, m.forceCloseErr
	}

	return &wire.MsgTx{}, nil
}

func newBeatFromHeight(height int32) *chainio.Beat {
	epoch := chainntnfs.BlockEpoch{
		Height: height,
	}

	return chainio.NewBeat(epoch)
}
