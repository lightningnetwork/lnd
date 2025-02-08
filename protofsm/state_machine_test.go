package protofsm

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type dummyEvents interface {
	dummy()
}

type goToFin struct {
}

func (g *goToFin) dummy() {
}

type emitInternal struct {
}

func (e *emitInternal) dummy() {
}

type daemonEvents struct {
}

func (s *daemonEvents) dummy() {
}

type dummyEnv struct {
	mock.Mock
}

func (d *dummyEnv) Name() string {
	return "test"
}

type dummyStateStart struct {
	canSend *atomic.Bool
}

func (d *dummyStateStart) String() string {
	return "dummyStateStart"
}

var (
	hexDecode = func(keyStr string) []byte {
		keyBytes, _ := hex.DecodeString(keyStr)
		return keyBytes
	}
	pub1, _ = btcec.ParsePubKey(hexDecode(
		"02ec95e4e8ad994861b95fc5986eedaac24739e5ea3d0634db4c8ccd44cd" +
			"a126ea",
	))
	pub2, _ = btcec.ParsePubKey(hexDecode(
		"0356167ba3e54ac542e86e906d4186aba9ca0b9df45001c62b753d33fe06" +
			"f5b4e8",
	))
)

func (d *dummyStateStart) ProcessEvent(event dummyEvents, env *dummyEnv,
) (*StateTransition[dummyEvents, *dummyEnv], error) {

	switch event.(type) {
	case *goToFin:
		return &StateTransition[dummyEvents, *dummyEnv]{
			NextState: &dummyStateFin{},
		}, nil

	// This state will loop back upon itself, but will also emit an event
	// to head to the terminal state.
	case *emitInternal:
		return &StateTransition[dummyEvents, *dummyEnv]{
			NextState: &dummyStateStart{},
			NewEvents: fn.Some(EmittedEvent[dummyEvents]{
				InternalEvent: []dummyEvents{&goToFin{}},
			}),
		}, nil

	// This state will proceed to the terminal state, but will emit all the
	// possible daemon events.
	case *daemonEvents:
		// This send event can only succeed once the bool turns to
		// true. After that, then we'll expect another event to take us
		// to the final state.
		sendEvent := &SendMsgEvent[dummyEvents]{
			TargetPeer: *pub1,
			SendWhen: fn.Some(func() bool {
				return d.canSend.Load()
			}),
			PostSendEvent: fn.Some(dummyEvents(&goToFin{})),
		}

		// We'll also send out a normal send event that doesn't have
		// any preconditions.
		sendEvent2 := &SendMsgEvent[dummyEvents]{
			TargetPeer: *pub2,
		}

		return &StateTransition[dummyEvents, *dummyEnv]{
			// We'll state in this state until the send succeeds
			// based on our predicate. Then it'll transition to the
			// final state.
			NextState: &dummyStateStart{
				canSend: d.canSend,
			},
			NewEvents: fn.Some(EmittedEvent[dummyEvents]{
				ExternalEvents: DaemonEventSet{
					sendEvent, sendEvent2,
					&BroadcastTxn{
						Tx:    &wire.MsgTx{},
						Label: "test",
					},
				},
			}),
		}, nil
	}

	return nil, fmt.Errorf("unknown event: %T", event)
}

func (d *dummyStateStart) IsTerminal() bool {
	return false
}

type dummyStateFin struct {
}

func (d *dummyStateFin) String() string {
	return "dummyStateFin"
}

func (d *dummyStateFin) ProcessEvent(event dummyEvents, env *dummyEnv,
) (*StateTransition[dummyEvents, *dummyEnv], error) {

	return &StateTransition[dummyEvents, *dummyEnv]{
		NextState: &dummyStateFin{},
	}, nil
}

func (d *dummyStateFin) IsTerminal() bool {
	return true
}

func assertState[Event any, Env Environment](t *testing.T,
	m *StateMachine[Event, Env], expectedState State[Event, Env]) {

	state, err := m.CurrentState()
	require.NoError(t, err)
	require.IsType(t, expectedState, state)
}

func assertStateTransitions[Event any, Env Environment](
	t *testing.T, stateSub StateSubscriber[Event, Env],
	expectedStates []State[Event, Env]) {

	for _, expectedState := range expectedStates {
		newState := <-stateSub.NewItemCreated.ChanOut()

		require.IsType(t, expectedState, newState)
	}
}

type dummyAdapters struct {
	mock.Mock

	confChan  chan *chainntnfs.TxConfirmation
	spendChan chan *chainntnfs.SpendDetail
}

func newDaemonAdapters() *dummyAdapters {
	return &dummyAdapters{
		confChan:  make(chan *chainntnfs.TxConfirmation, 1),
		spendChan: make(chan *chainntnfs.SpendDetail, 1),
	}
}

func (d *dummyAdapters) SendMessages(pub btcec.PublicKey,
	msgs []lnwire.Message) error {

	args := d.Called(pub, msgs)

	return args.Error(0)
}

func (d *dummyAdapters) BroadcastTransaction(tx *wire.MsgTx,
	label string) error {

	args := d.Called(tx, label)

	return args.Error(0)
}

func (d *dummyAdapters) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption,
) (*chainntnfs.ConfirmationEvent, error) {

	args := d.Called(txid, pkScript, numConfs)

	err := args.Error(0)

	return &chainntnfs.ConfirmationEvent{
		Confirmed: d.confChan,
	}, err
}

func (d *dummyAdapters) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	args := d.Called(outpoint, pkScript, heightHint)

	err := args.Error(0)

	return &chainntnfs.SpendEvent{
		Spend: d.spendChan,
	}, err
}

// TestStateMachineOnInitDaemonEvent tests that the state machine will properly
// execute any init-level daemon events passed into it.
func TestStateMachineOnInitDaemonEvent(t *testing.T) {
	ctx := context.Background()

	// First, we'll create our state machine given the env, and our
	// starting state.
	env := &dummyEnv{}
	startingState := &dummyStateStart{}

	adapters := newDaemonAdapters()

	// We'll make an init event that'll send to a peer, then transition us
	// to our terminal state.
	initEvent := &SendMsgEvent[dummyEvents]{
		TargetPeer:    *pub1,
		PostSendEvent: fn.Some(dummyEvents(&goToFin{})),
	}

	cfg := StateMachineCfg[dummyEvents, *dummyEnv]{
		Daemon:       adapters,
		InitialState: startingState,
		Env:          env,
		InitEvent:    fn.Some[DaemonEvent](initEvent),
	}
	stateMachine := NewStateMachine(cfg)

	// Before we start up the state machine, we'll assert that the send
	// message adapter is called on start up.
	adapters.On("SendMessages", *pub1, mock.Anything).Return(nil)

	// As we're triggering internal events, we'll also subscribe to the set
	// of new states so we can assert as we go.
	stateSub := stateMachine.RegisterStateEvents()
	defer stateMachine.RemoveStateSub(stateSub)

	stateMachine.Start(ctx)
	defer stateMachine.Stop()

	// Assert that we go from the starting state to the final state.  The
	// state machine should now also be on the final terminal state.
	expectedStates := []State[dummyEvents, *dummyEnv]{
		&dummyStateStart{}, &dummyStateFin{},
	}
	assertStateTransitions(t, stateSub, expectedStates)

	// We'll now assert that after the daemon was started, the send message
	// adapter was called above as specified in the init event.
	adapters.AssertExpectations(t)
	env.AssertExpectations(t)
}

// TestStateMachineInternalEvents tests that the state machine is able to add
// new internal events to the event queue for further processing during a state
// transition.
func TestStateMachineInternalEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// First, we'll create our state machine given the env, and our
	// starting state.
	env := &dummyEnv{}
	startingState := &dummyStateStart{}

	adapters := newDaemonAdapters()

	cfg := StateMachineCfg[dummyEvents, *dummyEnv]{
		Daemon:       adapters,
		InitialState: startingState,
		Env:          env,
		InitEvent:    fn.None[DaemonEvent](),
	}
	stateMachine := NewStateMachine(cfg)

	// As we're triggering internal events, we'll also subscribe to the set
	// of new states so we can assert as we go.
	stateSub := stateMachine.RegisterStateEvents()
	defer stateMachine.RemoveStateSub(stateSub)

	stateMachine.Start(ctx)
	defer stateMachine.Stop()

	// For this transition, we'll send in the emitInternal event, which'll
	// send us back to the starting event, but emit an internal event.
	stateMachine.SendEvent(ctx, &emitInternal{})

	// We'll now also assert the path we took to get here to ensure the
	// internal events were processed.
	expectedStates := []State[dummyEvents, *dummyEnv]{
		&dummyStateStart{}, &dummyStateStart{}, &dummyStateFin{},
	}
	assertStateTransitions(
		t, stateSub, expectedStates,
	)

	// We should ultimately end up in the terminal state.
	assertState[dummyEvents, *dummyEnv](t, &stateMachine, &dummyStateFin{})

	// Make sure all the env expectations were met.
	env.AssertExpectations(t)
}

// TestStateMachineDaemonEvents tests that the state machine is able to process
// daemon emitted as part of the state transition process.
func TestStateMachineDaemonEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// First, we'll create our state machine given the env, and our
	// starting state.
	env := &dummyEnv{}

	var boolTrigger atomic.Bool
	startingState := &dummyStateStart{
		canSend: &boolTrigger,
	}

	adapters := newDaemonAdapters()

	cfg := StateMachineCfg[dummyEvents, *dummyEnv]{
		Daemon:       adapters,
		InitialState: startingState,
		Env:          env,
		InitEvent:    fn.None[DaemonEvent](),
	}
	stateMachine := NewStateMachine(cfg)

	// As we're triggering internal events, we'll also subscribe to the set
	// of new states so we can assert as we go.
	stateSub := stateMachine.RegisterStateEvents()
	defer stateMachine.RemoveStateSub(stateSub)

	stateMachine.Start(ctx)
	defer stateMachine.Stop()

	// As soon as we send in the daemon event, we expect the
	// disable+broadcast events to be processed, as they are unconditional.
	adapters.On(
		"BroadcastTransaction", mock.Anything, mock.Anything,
	).Return(nil)
	adapters.On("SendMessages", *pub2, mock.Anything).Return(nil)

	// We'll start off by sending in the daemon event, which'll trigger the
	// state machine to execute the series of daemon events.
	stateMachine.SendEvent(ctx, &daemonEvents{})

	// We should transition back to the starting state now, after we
	// started from the very same state.
	expectedStates := []State[dummyEvents, *dummyEnv]{
		&dummyStateStart{}, &dummyStateStart{},
	}
	assertStateTransitions(t, stateSub, expectedStates)

	// At this point, we expect that the two methods above were called.
	adapters.AssertExpectations(t)

	// However, we don't expect the SendMessages for the first peer target
	// to be called yet, as the condition hasn't yet been met.
	adapters.AssertNotCalled(t, "SendMessages", *pub1)

	// We'll now flip the bool to true, which should cause the SendMessages
	// method to be called, and for us to transition to the final state.
	boolTrigger.Store(true)
	adapters.On("SendMessages", *pub1, mock.Anything).Return(nil)

	expectedStates = []State[dummyEvents, *dummyEnv]{&dummyStateFin{}}
	assertStateTransitions(t, stateSub, expectedStates)

	adapters.AssertExpectations(t)
	env.AssertExpectations(t)
}

type dummyMsgMapper struct {
	mock.Mock
}

func (d *dummyMsgMapper) MapMsg(wireMsg msgmux.PeerMsg) fn.Option[dummyEvents] {
	args := d.Called(wireMsg)

	//nolint:forcetypeassert
	return args.Get(0).(fn.Option[dummyEvents])
}

// TestStateMachineMsgMapper tests that given a message mapper, we can properly
// send in wire messages get mapped to FSM events.
func TestStateMachineMsgMapper(t *testing.T) {
	ctx := context.Background()

	// First, we'll create our state machine given the env, and our
	// starting state.
	env := &dummyEnv{}
	startingState := &dummyStateStart{}
	adapters := newDaemonAdapters()

	// We'll also provide a message mapper that only knows how to map a
	// single wire message (error).
	dummyMapper := &dummyMsgMapper{}

	// The only thing we know how to map is the error message, which'll
	// terminate the state machine.
	wireError := msgmux.PeerMsg{
		Message: &lnwire.Error{},
	}
	initMsg := msgmux.PeerMsg{
		Message: &lnwire.Init{},
	}

	dummyMapper.On("MapMsg", wireError).Return(
		fn.Some(dummyEvents(&goToFin{})),
	)
	dummyMapper.On("MapMsg", initMsg).Return(fn.None[dummyEvents]())

	cfg := StateMachineCfg[dummyEvents, *dummyEnv]{
		Daemon:       adapters,
		InitialState: startingState,
		Env:          env,
		MsgMapper:    fn.Some[MsgMapper[dummyEvents]](dummyMapper),
	}
	stateMachine := NewStateMachine(cfg)

	// As we're triggering internal events, we'll also subscribe to the set
	// of new states so we can assert as we go.
	//
	// We register before calling Start to ensure we don't miss any events.
	stateSub := stateMachine.RegisterStateEvents()
	defer stateMachine.RemoveStateSub(stateSub)

	stateMachine.Start(ctx)
	defer stateMachine.Stop()

	// First, we'll verify that the CanHandle method works as expected.
	require.True(t, stateMachine.CanHandle(wireError))
	require.False(t, stateMachine.CanHandle(msgmux.PeerMsg{
		Message: &lnwire.Init{},
	}))

	// Next, we'll attempt to send the wire message into the state machine.
	// We should transition to the final state.
	require.True(t, stateMachine.SendMessage(ctx, wireError))

	// We should transition to the final state.
	expectedStates := []State[dummyEvents, *dummyEnv]{
		&dummyStateStart{}, &dummyStateFin{},
	}
	assertStateTransitions(t, stateSub, expectedStates)

	dummyMapper.AssertExpectations(t)
	adapters.AssertExpectations(t)
	env.AssertExpectations(t)
}
