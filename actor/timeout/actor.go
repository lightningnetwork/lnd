package timeout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// retryBaseDelay is how long the actor waits before re-attempting a
	// callback delivery that found the requester's mailbox full.
	retryBaseDelay = time.Second

	// retryMaxDelay caps the backoff so a requester that recovers after a
	// long stall still gets its reminder reasonably promptly.
	retryMaxDelay = 30 * time.Second

	// selfSignalRetry is how long a clock goroutine waits before
	// re-offering a timer fire that the actor's own mailbox had no room
	// for. It is short because an internal fire is the actor's own work,
	// and the mailbox only stays full for as long as the actor is behind
	// on its queue.
	selfSignalRetry = 50 * time.Millisecond
)

// terminalDelivery reports whether a failed delivery is permanent. Only a
// stopped actor qualifies (the actor package also reports a closed mailbox as
// ErrActorTerminated), because that is an end state no amount of waiting
// reverses. Everything else is treated as passing and retried: a full mailbox,
// a custom mailbox surfacing a slow enqueue as a deadline, and a router that
// momentarily has no actor registered are all conditions the requester
// recovers from. Guessing the other way round means silently discarding a
// reminder nobody will re-derive.
func terminalDelivery(err error) bool {
	return errors.Is(err, actor.ErrActorTerminated)
}

// backoffDelay returns the delay before attempt n of a delivery that keeps
// failing, doubling from base and flattening out at retryMaxDelay. The
// doubling runs as a loop that returns at the cap, so no attempt count can
// overflow the duration.
func backoffDelay(base time.Duration, attempt int) time.Duration {
	// A non-positive base would make the loop below spin forever, and a
	// zero-th attempt has no doubling to do.
	if base <= 0 {
		base = retryBaseDelay
	}
	if attempt < 1 {
		return base
	}

	delay := base
	for range attempt - 1 {
		delay *= 2
		if delay >= retryMaxDelay {
			return retryMaxDelay
		}
	}

	return delay
}

// retryDelay returns how long to wait before one-shot delivery attempt n.
func retryDelay(attempt int) time.Duration {
	return backoffDelay(retryBaseDelay, attempt)
}

// tickRetryDelay returns how long to wait before recurring delivery attempt
// n. A ticker faster than the standard base starts its doubling from its own
// interval instead, so a single transient failure does not stretch a 100ms
// ticker to a full second; anything slower than the base keeps it.
//
// Only the starting point moves. The doubling and the retryMaxDelay ceiling
// apply to a recurring entry exactly as they do to a one-shot, so a consumer
// that stays unreachable ends up retrying well past its own cadence. That is
// deliberate rather than an oversight: an attempt against a callback backed by
// a persistent mailbox may be a bounded database write, and re-offering a
// 100ms tick every 100ms forever would hammer the store it is already
// struggling with.
func tickRetryDelay(interval time.Duration, attempt int) time.Duration {
	return backoffDelay(min(interval, retryBaseDelay), attempt)
}

// oneshotEntry tracks a one-shot timeout scheduled via
// ScheduleTimeoutRequest. The generation counter lets a stale fire that
// raced with a Cancel/reschedule identify itself and exit cleanly when
// it lands inside Receive.
type oneshotEntry struct {
	timer    Stoppable
	gen      uint64
	callback actor.TellOnlyRef[*ExpiredMsg]

	// attempts counts how many times delivery to callback has been tried
	// and found its mailbox full. It only drives the backoff: the entry
	// itself is the single copy of the reminder and survives every retry,
	// so a slow requester costs one entry, not one per attempt.
	attempts int
}

// recurringEntry tracks a recurring tick scheduled via
// ScheduleRecurringTickRequest. interval is preserved so each
// internalTickFired handler can re-arm without re-deriving it from the
// original request.
type recurringEntry struct {
	timer    Stoppable
	gen      uint64
	interval time.Duration
	callback actor.TellOnlyRef[*TickFiredMsg]

	// attempts counts consecutive failed deliveries of pending, resetting
	// to zero as soon as a tick lands.
	attempts int

	// pending holds a tick the callback could not accept yet. Holding the
	// message rather than rebuilding it means a retried tick still reports
	// the instant its timer fired, not the instant delivery finally
	// succeeded.
	pending *TickFiredMsg
}

// Actor is the timeout scheduling actor. It manages one-shot timers and
// recurring tickers, sending notifications when they fire.
//
// All state lives only inside the actor's Receive goroutine. Clock
// callbacks TryTell self with an internalTimerFired / internalTickFired
// message and the real state mutation happens when that message reaches
// Receive. There is no cross-goroutine map access, so no mutex.
//
// Recurring ticks are implemented as a chain of one-shot timers: each
// fire delivers TickFiredMsg to the user callback and re-arms via
// AfterFunc. This gives "fixed-delay" semantics (the next tick is
// scheduled at handler-finish + interval) and avoids the burst-of-ticks
// catch-up behaviour of time.Ticker when a consumer is slow.
type Actor struct {
	// clock is the time source. RealClock in production; a fake clock
	// in tests.
	clock Clock

	// log is the logger injected via Config.Log. When it is None the
	// actor falls back to the package-level logger set by UseLogger.
	log fn.Option[btclog.Logger]

	// self is the actor's reference to its own mailbox. Set by Start
	// after the actor system has registered the behavior. Clock
	// callbacks run in separate goroutines, so Start publishes the ref
	// atomically before any callback reads it.
	self atomic.Value

	// nextGen issues a fresh generation number on every Schedule call.
	// A stale internalTimerFired/internalTickFired arriving after a
	// Cancel or reschedule will see its gen no longer match the live
	// entry and silently drop.
	nextGen uint64

	// oneshots maps timeout IDs to their active one-shot entries.
	oneshots map[ID]*oneshotEntry

	// recurring maps tick IDs to their active recurring entries.
	recurring map[ID]*recurringEntry
}

// Config carries the timeout actor's optional dependencies.
type Config struct {
	// Clock is the time source. Defaults to RealClock; tests pass a fake
	// clock to drive scheduling deterministically.
	Clock fn.Option[Clock]

	// Log is the logger the actor writes to. When unset, the actor uses
	// the package-level logger installed via UseLogger, which is disabled
	// by default. Setting it lets separate instances log under separate
	// prefixes.
	Log fn.Option[btclog.Logger]
}

// NewActorWithConfig creates a timeout actor from an explicit config. This is
// the only constructor that can inject a per-instance logger.
func NewActorWithConfig(cfg Config) *Actor {
	return &Actor{
		clock:     cfg.Clock.UnwrapOr(RealClock{}),
		log:       cfg.Log,
		oneshots:  make(map[ID]*oneshotEntry),
		recurring: make(map[ID]*recurringEntry),
	}
}

// NewActor creates a new timeout actor backed by the real wall clock.
// The returned actor must be wired to its mailbox via Start before any
// schedule request is delivered.
func NewActor() *Actor {
	return NewActorWithConfig(Config{})
}

// NewActorWithClock creates a new timeout actor that uses the supplied
// clock. Tests use this constructor with a fake clock to drive
// scheduling deterministically.
func NewActorWithClock(clock Clock) *Actor {
	return NewActorWithConfig(Config{
		Clock: fn.Some(clock),
	})
}

// Start attaches the actor's self-reference. Production callers obtain
// this ref from actor.RegisterWithSystem and pass it here so that clock
// callbacks can hand internal fire messages back into the actor's own
// mailbox. Tests that drive the actor directly (without an ActorSystem)
// inject a synchronous self-ref via newSyncSelfRef.
//
// RegisterWithSystem starts the actor's receive loop immediately, so the
// caller must call Start before handing the ref to anyone who may send a
// ScheduleTimeoutRequest or ScheduleRecurringTickRequest. Start is meant to
// be called once. The ref is published through an atomic.Value, so a second
// call carrying the same concrete type replaces the first, while one carrying
// a different concrete type panics.
func (a *Actor) Start(self actor.TellOnlyRef[Msg]) {
	a.self.Store(self)
}

// loadSelf returns the actor self-reference published by Start. The false case
// is only expected in direct tests or misuse that bypasses actor registration.
func (a *Actor) loadSelf() (actor.TellOnlyRef[Msg], bool) {
	self, ok := a.self.Load().(actor.TellOnlyRef[Msg])

	return self, ok
}

// signalSelf hands an internal fire message to the actor's own mailbox.
// It runs on a clock goroutine, so it must not touch actor state
// directly: the state mutation happens when Receive picks the message
// up.
//
// The send never blocks. A blocking one would leave a goroutine parked
// per timer fire for as long as the actor stayed backed up, which is a
// slow leak triggered by exactly the situation the actor is trying to
// work through. Instead a full mailbox re-arms the fire, so pending
// fires coalesce into one outstanding timer rather than a growing pile
// of parked senders.
func (a *Actor) signalSelf(msg Msg) {
	// Fires that arrive before Start wired the self-ref have nowhere to
	// go. That only happens in direct tests or a miswired caller, so say
	// so rather than dropping the timer in silence.
	self, ok := a.loadSelf()
	if !ok {
		bgCtx := context.Background()

		a.logger().DebugS(
			bgCtx,
			"Dropping timer fire, actor self-ref not set",
			slog.String("msg_type", msg.MessageType()),
		)

		return
	}

	err := self.TryTell(context.Background(), msg)
	if err == nil {
		return
	}

	// The actor is stopped or its mailbox closed. The fire has nowhere to
	// go and no amount of waiting will change that, so let the chain end
	// here.
	if terminalDelivery(err) {
		return
	}

	// The retry handle is deliberately not recorded on the entry: we are
	// off the receive goroutine, where touching actor state would race.
	// A Cancel landing in the meantime is still honoured, because the
	// generation check drops the fire when it finally arrives.
	a.clock.AfterFunc(selfSignalRetry, func() {
		a.signalSelf(msg)
	})
}

// Receive processes incoming messages.
func (a *Actor) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	switch m := msg.(type) {
	case *ScheduleTimeoutRequest:
		return a.handleSchedule(ctx, m)

	case *ScheduleRecurringTickRequest:
		return a.handleScheduleRecurring(ctx, m)

	case *CancelTimeoutRequest:
		return a.handleCancel(ctx, m)

	case *internalTimerFired:
		return a.handleTimerFired(ctx, m)

	case *internalTickFired:
		return a.handleTickFired(ctx, m)

	default:
		return fn.Err[Resp](fmt.Errorf("unknown message type: %T", msg))
	}
}

// handleSchedule schedules a new one-shot timeout. If an entry (one-shot
// or recurring) already exists for this ID, it is cancelled first.
func (a *Actor) handleSchedule(_ context.Context,
	req *ScheduleTimeoutRequest) fn.Result[Resp] {

	a.cancelExisting(req.ID)

	a.nextGen++
	gen := a.nextGen
	id := req.ID

	// AfterFunc fires from the clock's own goroutine; it must not touch
	// any actor state directly. Instead it signals an internalTimerFired
	// message back into self, where Receive will deliver the
	// user-visible ExpiredMsg single-threadedly.
	timer := a.clock.AfterFunc(req.Duration, func() {
		a.signalSelf(&internalTimerFired{
			ID:  id,
			Gen: gen,
		})
	})

	a.oneshots[id] = &oneshotEntry{
		timer:    timer,
		gen:      gen,
		callback: req.Callback,
	}

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// handleScheduleRecurring schedules a recurring tick. If an entry
// (one-shot or recurring) already exists for this ID, it is cancelled
// first. A re-arming chain of AfterFunc one-shots drives the cadence;
// Tick delivery and re-arm both happen inside Receive when
// internalTickFired lands.
//
// Interval must be strictly positive. A zero or negative interval would
// schedule an immediate fire whose handler re-arms with the same value,
// trapping the actor in an unbounded fire/re-arm loop that starves
// every other message in the mailbox. We reject the request before
// touching state so a malformed request cannot disturb existing
// entries.
func (a *Actor) handleScheduleRecurring(_ context.Context,
	req *ScheduleRecurringTickRequest) fn.Result[Resp] {

	if req.Interval <= 0 {
		return fn.Err[Resp](
			fmt.Errorf("recurring tick interval must be "+
				"positive, got %s", req.Interval),
		)
	}

	a.cancelExisting(req.ID)

	a.nextGen++
	gen := a.nextGen

	a.recurring[req.ID] = &recurringEntry{
		gen:      gen,
		interval: req.Interval,
		callback: req.Callback,
	}

	a.armRecurring(req.ID, gen, req.Interval)

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// handleCancel cancels a pending one-shot timeout or recurring tick. If
// no entry exists for this ID, this is a no-op.
func (a *Actor) handleCancel(_ context.Context,
	req *CancelTimeoutRequest) fn.Result[Resp] {

	a.cancelExisting(req.ID)

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// handleTimerFired delivers a one-shot expiry to its callback. Stale
// fires (cancelled or rescheduled before the timer ran) are dropped via
// the generation check.
//
// Delivery is non-blocking on purpose. A single timeout actor is typically
// shared by many requesters, so parking this goroutine on one backlogged
// requester would freeze every other timer it owns. A requester that cannot
// take the message right now has its expiry re-armed on a backoff instead.
func (a *Actor) handleTimerFired(ctx context.Context,
	m *internalTimerFired) fn.Result[Resp] {

	entry, ok := a.oneshots[m.ID]
	if !ok || entry.gen != m.Gen {
		return fn.Ok[Resp](&AckResponse{
			Success: true,
		})
	}

	err := entry.callback.TryTell(ctx, &ExpiredMsg{
		ID: m.ID,
	})

	switch {
	// The reminder landed, so the entry has done its job.
	case err == nil:
		delete(a.oneshots, m.ID)

	// The requester is gone for good, so there is nobody left to remind
	// and no backoff that would change that.
	case terminalDelivery(err):
		delete(a.oneshots, m.ID)

		a.logger().WarnS(ctx, "Dropping expired timeout, callback "+
			"unreachable", err,
			slog.String("timeout_id", string(m.ID)),
			slog.String("callback", entry.callback.ID()),
		)

	// Anything else is a passing failure. Keep the entry, which is the
	// only copy of this reminder, and re-arm the same fire later.
	default:
		a.retryOneshot(ctx, m.ID, entry, err)
	}

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// retryOneshot re-arms an expiry whose callback could not take it. The
// entry keeps its generation so the re-armed fire still matches on
// arrival, and the new timer handle is recorded on the entry so a
// Cancel or reschedule can stop the retry chain.
func (a *Actor) retryOneshot(ctx context.Context, id ID, entry *oneshotEntry,
	cause error) {

	entry.attempts++
	delay := retryDelay(entry.attempts)

	a.logger().WarnS(ctx, "Timeout callback delivery failed, retrying",
		cause,
		slog.String("timeout_id", string(id)),
		slog.String("callback", entry.callback.ID()),
		slog.Int("attempt", entry.attempts),
		slog.Duration("retry_in", delay),
	)

	gen := entry.gen

	entry.timer = a.clock.AfterFunc(delay, func() {
		a.signalSelf(&internalTimerFired{
			ID:  id,
			Gen: gen,
		})
	})
}

// handleTickFired delivers a recurring-tick fire to its callback and
// re-arms the next one-shot. Stale fires (cancelled or replaced before
// the timer ran) are dropped via the generation check.
//
// As with one-shot expiries, delivery never blocks. A tick the consumer
// cannot take is held on the entry and retried on a backoff, which also
// stretches the cadence for as long as the consumer stays behind. That
// is the intended backpressure: a slow consumer slows its own ticker
// rather than every other requester's timers.
func (a *Actor) handleTickFired(ctx context.Context,
	m *internalTickFired) fn.Result[Resp] {

	entry, ok := a.recurring[m.ID]
	if !ok || entry.gen != m.Gen {
		return fn.Ok[Resp](&AckResponse{
			Success: true,
		})
	}

	// A retry re-delivers the tick that could not land earlier, so the
	// consumer still sees the instant its timer fired.
	tick := entry.pending
	if tick == nil {
		tick = &TickFiredMsg{
			ID:      m.ID,
			FiredAt: m.FiredAt,
		}
	}

	err := entry.callback.TryTell(ctx, tick)

	switch {
	// The tick landed, so the cadence resumes from a clean slate.
	case err == nil:
		entry.pending = nil
		entry.attempts = 0

		a.armRecurring(m.ID, entry.gen, entry.interval)

	// A ticker with no reachable consumer is pure overhead, so stop the
	// chain instead of re-arming into a dead actor forever.
	case terminalDelivery(err):
		delete(a.recurring, m.ID)

		a.logger().WarnS(ctx, "Stopping recurring tick, callback "+
			"unreachable", err,
			slog.String("tick_id", string(m.ID)),
			slog.String("callback", entry.callback.ID()),
		)

	// The consumer could not take this tick. Hold it and come back for
	// it rather than dropping it on the floor.
	default:
		entry.pending = tick
		entry.attempts++
		delay := tickRetryDelay(entry.interval, entry.attempts)

		a.logger().WarnS(ctx, "Tick callback delivery failed, "+
			"retrying", err,
			slog.String("tick_id", string(m.ID)),
			slog.String("callback", entry.callback.ID()),
			slog.Int("attempt", entry.attempts),
			slog.Duration("retry_in", delay),
		)

		a.armRecurring(m.ID, entry.gen, delay)
	}

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// armRecurring schedules the next AfterFunc that will fire an
// internalTickFired for (id, gen). The newly created timer handle is
// recorded on the live entry so a subsequent Cancel can stop the
// pending fire. If the entry has been replaced (gen mismatch), the
// timer is stopped, and any fire already in flight is filtered by the gen
// check on arrival.
func (a *Actor) armRecurring(id ID, gen uint64, d time.Duration) {
	timer := a.clock.AfterFunc(d, func() {
		a.signalSelf(&internalTickFired{
			ID:      id,
			Gen:     gen,
			FiredAt: a.clock.Now(),
		})
	})

	if entry, ok := a.recurring[id]; ok && entry.gen == gen {
		entry.timer = timer
	} else {
		// Entry was replaced or cancelled between AfterFunc
		// returning and this assignment, so stop the dangling
		// timer rather than letting it fire into a dropped gen.
		timer.Stop()
	}
}

// cancelExisting removes any one-shot or recurring entry for id. It runs
// inside Receive so no synchronization is needed for the map ops; the
// timer.Stop() calls are safe across goroutines on their own. Any
// AfterFunc that was already in flight when Stop landed will deliver
// its internal message anyway, but the gen check in
// handleTimerFired/handleTickFired drops it cleanly.
func (a *Actor) cancelExisting(id ID) {
	if entry, ok := a.oneshots[id]; ok {
		entry.timer.Stop()
		delete(a.oneshots, id)
	}

	if entry, ok := a.recurring[id]; ok {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		delete(a.recurring, id)
	}
}
