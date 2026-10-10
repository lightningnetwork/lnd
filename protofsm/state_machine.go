package protofsm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/msgmux"
)

const (
	// DefaultStateQueryTimeout bounds how long CurrentState waits for the
	// answer once the machine has accepted the query.
	DefaultStateQueryTimeout = time.Second
)

var (
	// ErrStateMachineShutdown occurs when trying to feed an event to a
	// StateMachine that has been asked to Stop.
	ErrStateMachineShutdown = fmt.Errorf("StateMachine is shutting down")
)

// EmittedEvent is the set of events a state transition can emit. Internal
// events are routed back into the state machine before any new external event
// is processed, and outbox events are handed to whoever drives the machine,
// once all internal events have been processed.
type EmittedEvent[Event any, Out any] struct {
	// InternalEvent is an optional set of internal events that are routed
	// back to the next state. This lets a state trigger one or many state
	// transitions without a new external event.
	InternalEvent []Event

	// Outbox is an optional set of side effects the transition requests,
	// such as sending a message or arming a timer. Outbox events are
	// accumulated across every transition triggered by one external event,
	// in the order they were emitted, and are only dispatched once the new
	// state has been committed.
	Outbox []Out
}

// StateTransition is a state transition type. It denotes the next state to go
// to, and also the set of events to emit.
type StateTransition[Event any, Out any, Env Environment] struct {
	// NextState is the next state to transition to.
	NextState State[Event, Out, Env]

	// NewEvents is the set of events to emit.
	NewEvents fn.Option[EmittedEvent[Event, Out]]
}

// Environment is an abstract interface that represents the environment that
// the state machine will execute using. It carries the read-only context a
// state needs to compute its transitions, such as configuration or local
// lookups.
type Environment interface {
	// Name returns the name of the environment. This is used to uniquely
	// identify the environment of related state machines in logs.
	Name() string
}

// State defines an abstract state, namely its state transition function that
// takes as input an event and an environment, and returns a state transition
// (next state, and set of events to emit). A state can also be terminal, in
// which case the machine no longer expects any events.
//
// A transition function should be a pure function of the state, the event and
// the environment: all side effects are expressed as outbox events, so the
// transition can be tested without goroutines, and the new state is always
// committed before any of its side effects run.
type State[Event any, Out any, Env Environment] interface {
	// ProcessEvent takes an event and an environment, and returns a new
	// state transition. This will be iteratively called until no further
	// internal events are emitted.
	ProcessEvent(ctx context.Context, event Event, env Env) (
		*StateTransition[Event, Out, Env], error)

	// IsTerminal returns true if this state is terminal, and false
	// otherwise.
	IsTerminal() bool

	// String returns a human readable string that represents the state.
	String() string
}

// TransitionObserver is called once for every state transition applied by
// ApplyEventsObserved, after the transition has been computed and before the
// next internal event is processed. It receives the event, the states before
// and after, and the outbox events that this single transition emitted.
type TransitionObserver[Event any, Out any, Env Environment] func(
	event Event, from, to State[Event, Out, Env], outbox []Out)

// ApplyEvents applies an event to the given state, then keeps applying any
// internal events the transitions emit until none remain. It returns the final
// state and every outbox event emitted along the way, in emission order.
//
// ApplyEvents performs no I/O and spawns no goroutines, so an actor that owns
// its state can call it directly from its Receive method: the actor commits
// the returned state, then dispatches the outbox. If any transition returns an
// error, the error is returned along with the last state successfully reached,
// and the outbox is discarded.
func ApplyEvents[Event any, Out any, Env Environment](ctx context.Context,
	state State[Event, Out, Env], event Event,
	env Env) (State[Event, Out, Env], []Out, error) {

	return ApplyEventsObserved(ctx, state, event, env, nil)
}

// ApplyEventsObserved is ApplyEvents with an optional observer that is called
// for every transition, which callers can use for logging or to notify
// subscribers of intermediate states.
func ApplyEventsObserved[Event any, Out any, Env Environment](
	ctx context.Context, state State[Event, Out, Env], event Event,
	env Env, observe TransitionObserver[Event, Out, Env]) (
	State[Event, Out, Env], []Out, error) {

	var (
		queue  = []Event{event}
		outbox []Out
	)

	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]

		transition, err := state.ProcessEvent(ctx, next, env)
		if err != nil {
			return state, nil, err
		}

		var emitted []Out
		transition.NewEvents.WhenSome(func(e EmittedEvent[Event, Out]) {
			queue = append(queue, e.InternalEvent...)
			emitted = e.Outbox
		})
		outbox = append(outbox, emitted...)

		if observe != nil {
			observe(next, state, transition.NextState, emitted)
		}

		state = transition.NextState
	}

	return state, outbox, nil
}

// EventSink is handed to an OutboxHandler so it can feed events back into the
// state machine, and run goroutines that are bound to the machine's lifetime.
type EventSink[Event any] interface {
	// SendEvent sends a new event to the state machine.
	SendEvent(ctx context.Context, event Event)

	// Go runs f in a goroutine that is stopped along with the state
	// machine. It returns false if the machine is already shutting down.
	Go(ctx context.Context, f func(ctx context.Context)) bool
}

// OutboxHandler executes the outbox events emitted by a StateMachine.
type OutboxHandler[Event any, Out any] interface {
	// HandleOutbox executes a single outbox event. An error is treated
	// like a failed transition: it is reported, and the state machine is
	// stopped.
	HandleOutbox(ctx context.Context, sink EventSink[Event], out Out) error
}

// stateQuery is used by outside callers to query the internal state of the
// state machine.
type stateQuery[Event any, Out any, Env Environment] struct {
	// CurrentState is a channel that will be sent the current state of the
	// state machine.
	CurrentState chan State[Event, Out, Env]
}

// syncEventRequest is used to send an event to the state machine
// synchronously, waiting for the event processing to complete and returning
// the accumulated outbox events.
type syncEventRequest[Event any, Out any] struct {
	// event is the event to process.
	event Event

	// promise is used to signal completion and return the accumulated
	// outbox events or an error.
	promise actor.Promise[[]Out]
}

// StateMachine drives a State to completion from its own goroutine. Events are
// fed in with SendEvent or AskEvent, each is applied with ApplyEvents, and the
// resulting outbox is dispatched to the configured OutboxHandler once the new
// state has been committed.
//
// Components that are themselves actors do not need a StateMachine: they can
// hold their current state and call ApplyEvents from their Receive method,
// which avoids a second goroutine per machine.
type StateMachine[Event any, Out any, Env Environment] struct {
	cfg StateMachineCfg[Event, Out, Env]

	log btclog.Logger

	// events is the channel that will be used to send new events to the
	// FSM.
	events chan Event

	// syncEvents is the channel that will be used to send synchronous
	// event requests to the FSM, returning the accumulated outbox events.
	syncEvents chan syncEventRequest[Event, Out]

	// newStateEvents is an EventDistributor that will be used to notify
	// any relevant callers of new state transitions that occur.
	newStateEvents *fn.EventDistributor[State[Event, Out, Env]]

	// stateQuery is a channel that will be used by outside callers to
	// query the internal state machine state.
	stateQuery chan stateQuery[Event, Out, Env]

	gm   fn.GoroutineManager
	quit chan struct{}

	// startOnce and stopOnce are used to ensure that the state machine is
	// only started and stopped once.
	startOnce sync.Once
	stopOnce  sync.Once

	// running is a flag that indicates if the state machine is currently
	// running.
	running atomic.Bool
}

// ErrorReporter is an interface that's used to report errors that occur during
// state machine execution.
type ErrorReporter interface {
	// ReportError is a method that's used to report an error that occurred
	// during state machine execution.
	ReportError(err error)
}

// StateMachineCfg is a configuration struct that's used to create a new state
// machine.
type StateMachineCfg[Event any, Out any, Env Environment] struct {
	// ErrorReporter is used to report errors that occur during state
	// transitions.
	ErrorReporter ErrorReporter

	// OutboxHandler is an optional handler that executes the outbox events
	// emitted by each processed event. If it is not set, outbox events are
	// only returned to AskEvent callers, and dropped for SendEvent.
	OutboxHandler fn.Option[OutboxHandler[Event, Out]]

	// InitialState is the initial state of the state machine.
	InitialState State[Event, Out, Env]

	// Env is the environment that the state machine will use to execute.
	Env Env

	// InitEvent is an optional outbox event that is dispatched to the
	// OutboxHandler as if it was emitted at the onset of the state
	// machine. This can be used to set up tracking state such as a txid
	// confirmation event.
	InitEvent fn.Option[Out]

	// MsgMapper is an optional message mapper that can be used to map
	// normal wire messages into FSM events.
	MsgMapper fn.Option[MsgMapper[Event]]
}

// NewStateMachine creates a new state machine given its configuration.
func NewStateMachine[Event any, Out any, Env Environment](
	cfg StateMachineCfg[Event, Out, Env]) StateMachine[Event, Out, Env] {

	return StateMachine[Event, Out, Env]{
		cfg: cfg,
		log: log.WithPrefix(
			fmt.Sprintf("FSM(%v):", cfg.Env.Name()),
		),
		events:     make(chan Event, 1),
		syncEvents: make(chan syncEventRequest[Event, Out], 1),
		stateQuery: make(chan stateQuery[Event, Out, Env]),
		gm:         *fn.NewGoroutineManager(),
		newStateEvents: fn.NewEventDistributor[State[
			Event, Out, Env,
		]](),
		quit: make(chan struct{}),
	}
}

// Start starts the state machine. This will spawn a goroutine that will drive
// the state machine to completion.
func (s *StateMachine[Event, Out, Env]) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		_ = s.gm.Go(ctx, func(ctx context.Context) {
			s.driveMachine(ctx)
		})

		s.running.Store(true)
	})
}

// Stop stops the state machine. This will block until the state machine has
// reached a stopping point.
func (s *StateMachine[Event, Out, Env]) Stop() {
	s.stopOnce.Do(func() {
		close(s.quit)
		s.gm.Stop()

		s.running.Store(false)
	})
}

// SendEvent sends a new event to the state machine.
func (s *StateMachine[Event, Out, Env]) SendEvent(ctx context.Context,
	event Event) {

	s.log.Debugf("Sending event %T", event)

	select {
	case s.events <- event:
	case <-ctx.Done():
		return
	case <-s.quit:
		return
	}
}

// AskEvent sends a new event to the state machine and returns a Future that
// is resolved once the event, and every internal event it triggers, has been
// processed. The Future carries the accumulated outbox events, or an error if
// processing failed. If an OutboxHandler is configured, the outbox has already
// been dispatched to it by the time the Future resolves.
func (s *StateMachine[Event, Out, Env]) AskEvent(ctx context.Context,
	event Event) actor.Future[[]Out] {

	s.log.Debugf("Asking event %T", event)

	promise := actor.NewPromise[[]Out]()

	req := syncEventRequest[Event, Out]{
		event:   event,
		promise: promise,
	}

	// Check for context cancellation or shutdown first to avoid races.
	select {
	case <-ctx.Done():
		promise.Complete(
			fn.Errf[[]Out]("context cancelled: %w", ctx.Err()),
		)

		return promise.Future()

	case <-s.quit:
		promise.Complete(fn.Err[[]Out](ErrStateMachineShutdown))

		return promise.Future()

	default:
	}

	select {
	// Successfully sent, the promise will be completed by driveMachine.
	case s.syncEvents <- req:

	case <-ctx.Done():
		promise.Complete(
			fn.Errf[[]Out]("context cancelled: %w", ctx.Err()),
		)

	case <-s.quit:
		promise.Complete(fn.Err[[]Out](ErrStateMachineShutdown))
	}

	return promise.Future()
}

// Receive processes an actor message by asking the state machine to process
// the wrapped event, and returns the accumulated outbox events.
//
// NOTE: This implements the actor.ActorBehavior interface.
func (s *StateMachine[Event, Out, Env]) Receive(ctx context.Context,
	e ActorMessage[Event]) fn.Result[[]Out] {

	return s.AskEvent(ctx, e.Event).Await(ctx)
}

// CanHandle returns true if the target message can be routed to the state
// machine.
func (s *StateMachine[Event, Out, Env]) CanHandle(msg msgmux.PeerMsg) bool {
	cfgMapper := s.cfg.MsgMapper
	return fn.MapOptionZ(cfgMapper, func(mapper MsgMapper[Event]) bool {
		return mapper.MapMsg(msg).IsSome()
	})
}

// Name returns the name of the state machine's environment.
func (s *StateMachine[Event, Out, Env]) Name() string {
	return s.cfg.Env.Name()
}

// SendMessage attempts to send a wire message to the state machine. If the
// message can be mapped using the default message mapper, then true is
// returned indicating that the message was processed. Otherwise, false is
// returned.
func (s *StateMachine[Event, Out, Env]) SendMessage(ctx context.Context,
	msg msgmux.PeerMsg) bool {

	// If we have no message mapper, then return false as we can't process
	// this message.
	if !s.cfg.MsgMapper.IsSome() {
		return false
	}

	s.log.DebugS(ctx, "Sending msg", "msg", lnutils.SpewLogClosure(msg))

	// Otherwise, try to map the message using the default message mapper.
	// If we can't extract an event, then we'll return false to indicate
	// that the message wasn't processed.
	var processed bool
	s.cfg.MsgMapper.WhenSome(func(mapper MsgMapper[Event]) {
		event := mapper.MapMsg(msg)

		event.WhenSome(func(event Event) {
			s.SendEvent(ctx, event)

			processed = true
		})
	})

	return processed
}

// CurrentState returns the current state of the state machine. It waits for
// the machine to finish any event it is processing, then waits up to
// DefaultStateQueryTimeout for the answer. Callers that need to bound the
// whole wait should use CurrentStateWithContext.
func (s *StateMachine[Event, Out, Env]) CurrentState() (State[Event, Out, Env],
	error) {

	query := stateQuery[Event, Out, Env]{
		CurrentState: make(chan State[Event, Out, Env], 1),
	}

	if !fn.SendOrQuit(s.stateQuery, query, s.quit) {
		return nil, ErrStateMachineShutdown
	}

	return fn.RecvOrTimeout(query.CurrentState, DefaultStateQueryTimeout)
}

// CurrentStateWithContext returns the current state of the state machine,
// giving up when ctx is done.
func (s *StateMachine[Event, Out, Env]) CurrentStateWithContext(
	ctx context.Context) (State[Event, Out, Env], error) {

	query := stateQuery[Event, Out, Env]{
		CurrentState: make(chan State[Event, Out, Env], 1),
	}

	select {
	case s.stateQuery <- query:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.quit:
		return nil, ErrStateMachineShutdown
	}

	select {
	case state := <-query.CurrentState:
		return state, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.quit:
		return nil, ErrStateMachineShutdown
	}
}

// StateSubscriber represents an active subscription to be notified of new
// state transitions.
type StateSubscriber[Event any, Out any, Env Environment] *fn.EventReceiver[
	State[Event, Out, Env]]

// RegisterStateEvents registers a new event listener that will be notified of
// new state transitions.
func (s *StateMachine[Event, Out, Env]) RegisterStateEvents() StateSubscriber[
	Event, Out, Env] {

	subscriber := fn.NewEventReceiver[State[Event, Out, Env]](10)

	s.newStateEvents.RegisterSubscriber(subscriber)

	return subscriber
}

// RemoveStateSub removes the target state subscriber from the set of active
// subscribers.
func (s *StateMachine[Event, Out, Env]) RemoveStateSub(sub StateSubscriber[
	Event, Out, Env]) {

	_ = s.newStateEvents.RemoveSubscriber(sub)
}

// IsRunning returns true if the state machine is currently running.
func (s *StateMachine[Event, Out, Env]) IsRunning() bool {
	return s.running.Load()
}

// Go runs f in a goroutine bound to the state machine's lifetime.
//
// NOTE: This implements the EventSink interface.
func (s *StateMachine[Event, Out, Env]) Go(ctx context.Context,
	f func(ctx context.Context)) bool {

	return s.gm.Go(ctx, f)
}

// dispatchOutbox hands each outbox event to the configured OutboxHandler, if
// any, in emission order.
func (s *StateMachine[Event, Out, Env]) dispatchOutbox(ctx context.Context,
	outbox []Out) error {

	handler := func(h OutboxHandler[Event, Out]) error {
		for _, out := range outbox {
			if err := h.HandleOutbox(ctx, s, out); err != nil {
				return err
			}
		}

		return nil
	}

	return fn.MapOptionZ(s.cfg.OutboxHandler, handler)
}

// processEvent applies an event, commits the new state, then dispatches the
// outbox. Subscribers are notified of every state the event passed through
// only once the outbox has been dispatched, so a subscriber that observes a
// state can rely on that state's side effects having been issued. It returns
// the new state and the outbox.
func (s *StateMachine[Event, Out, Env]) processEvent(ctx context.Context,
	currentState State[Event, Out, Env], event Event) (
	State[Event, Out, Env], []Out, error) {

	var visited []State[Event, Out, Env]
	observe := func(event Event, from, to State[Event, Out, Env], _ []Out) {
		s.log.DebugS(ctx, "Processed event",
			"event", lnutils.SpewLogClosure(event))

		s.log.InfoS(ctx, "State transition",
			btclog.Fmt("from_state", "%v", from),
			btclog.Fmt("to_state", "%v", to))

		visited = append(visited, to)
	}

	newState, outbox, err := ApplyEventsObserved(
		ctx, currentState, event, s.cfg.Env, observe,
	)
	if err != nil {
		return currentState, nil, err
	}

	if err := s.dispatchOutbox(ctx, outbox); err != nil {
		return newState, nil, err
	}

	for _, state := range visited {
		s.newStateEvents.NotifySubscribers(state)
	}

	return newState, outbox, nil
}

// driveMachine is the main event loop of the state machine. It accepts any new
// incoming events, and then drives the state machine forward until it reaches
// a terminal state.
func (s *StateMachine[Event, Out, Env]) driveMachine(ctx context.Context) {
	s.log.DebugS(ctx, "Starting state machine")

	currentState := s.cfg.InitialState

	// Before we start, if we have an init event specified, then we'll
	// dispatch that now.
	err := fn.MapOptionZ(s.cfg.InitEvent, func(out Out) error {
		return s.dispatchOutbox(ctx, []Out{out})
	})
	if err != nil {
		s.log.ErrorS(ctx, "Unable to execute init event", err)
		return
	}

	// We just started driving the state machine, so we'll notify our
	// subscribers of this starting state.
	s.newStateEvents.NotifySubscribers(currentState)

	for {
		select {
		// We have a new external event, so we'll drive the state
		// machine forward until we run out of internal events.
		case newEvent := <-s.events:
			newState, _, err := s.processEvent(
				ctx, currentState, newEvent,
			)
			if err != nil {
				s.cfg.ErrorReporter.ReportError(err)

				s.log.ErrorS(ctx, "Unable to apply event", err)

				// An error occurred, so we'll tear down the
				// entire state machine as we can't proceed.
				go s.Stop()

				return
			}

			currentState = newState

		// We have a synchronous event request that expects the
		// accumulated outbox events to be returned via the promise.
		case syncReq := <-s.syncEvents:
			newState, outbox, err := s.processEvent(
				ctx, currentState, syncReq.event,
			)
			if err != nil {
				s.cfg.ErrorReporter.ReportError(err)

				s.log.ErrorS(ctx, "Unable to apply sync event",
					err)

				syncReq.promise.Complete(fn.Err[[]Out](err))

				// An error occurred, so we'll tear down the
				// entire state machine as we can't proceed.
				go s.Stop()

				return
			}

			currentState = newState

			syncReq.promise.Complete(fn.Ok(outbox))

		// An outside caller is querying our state, so we'll return the
		// latest state.
		case stateQuery := <-s.stateQuery:
			if !fn.SendOrQuit(
				stateQuery.CurrentState, currentState, s.quit,
			) {

				return
			}

		case <-s.gm.Done():
			return
		}
	}
}
