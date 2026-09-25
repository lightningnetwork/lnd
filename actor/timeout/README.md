# timeout

The `timeout` package is a generic, fire-and-forget timer scheduling actor
built on the `github.com/lightningnetwork/lnd/actor` package. It schedules
one-shot timeouts and recurring ticks, and delivers `ExpiredMsg` or
`TickFiredMsg` to a callback ref that the requester supplies when the timer
fires.

It exists so that actors do not each have to own a `time.Timer`, a goroutine
that waits on it, and the bookkeeping that turns a fire back into a message
for the actor. A requester sends a schedule request, and later receives a
message in its own mailbox like any other.

## Key Types

- `Actor`: the actor behavior. It holds the `oneshots` and `recurring` entry
  maps, and all state mutation happens on the single goroutine that runs
  `Receive`. Clock callbacks never touch actor state directly. They hand an
  internal fire message (`internalTimerFired` or `internalTickFired`)
  carrying a generation number back into the actor's own mailbox, so a stale
  fire that raced a cancel or reschedule is dropped when it arrives.
- `Config` and `NewActorWithConfig`: the optional clock and logger.
  `NewActor` and `NewActorWithClock` are shorthands that leave the logger
  unset, in which case the package logger installed with `UseLogger` is used.
- `Clock`: an interface (`Now`, `AfterFunc`) over the time source.
  `RealClock` is the production implementation. Tests can inject a fake
  clock via `NewActorWithClock`, or use `RealClock` inside a
  `testing/synctest` bubble.
- `ScheduleTimeoutRequest`, `ScheduleRecurringTickRequest`, and
  `CancelTimeoutRequest`: the messages that schedule and cancel timers.
  One-shot and recurring timers share the same `ID` namespace.
- `MapTimeoutExpired` and `MapTickFired`: wrap a target ref so that
  `ExpiredMsg` or `TickFiredMsg` arrives as one of the target actor's own
  message types.

## Invariants

- **One-shot and recurring timers share one ID namespace.** Scheduling
  either kind with an ID that is already live cancels the prior entry,
  regardless of its kind. `CancelTimeoutRequest` works on either.
- **Recurring intervals must be strictly positive.** A zero or negative
  `ScheduleRecurringTickRequest.Interval` is rejected before any state is
  touched, since an immediately re-arming timer would starve the mailbox.
- **Recurring ticks are fixed-delay.** The next fire is scheduled at
  handler-finish plus the interval, unlike `time.Ticker`, which is
  fixed-rate and drops ticks when the consumer is slow.
- **`Receive` never blocks on a callback.** A single timeout actor is
  typically shared by many requesters, so one backlogged requester that
  parked its goroutine would freeze every other timer it owns. Delivery uses
  `TryTell`. A failed delivery keeps the entry, which is the only copy of the
  reminder, and re-arms the same fire on a backoff that doubles from 1s and
  flattens out at 30s. A recurring entry whose interval is shorter than the
  1s base starts its doubling from its own interval instead, so one hiccup
  does not stretch a 100ms ticker to a full second. Only the starting point
  moves: the 30s ceiling applies to recurring entries too, so a consumer
  that stays unreachable backs off well past its own cadence. Callers must
  therefore tolerate a late callback.
- **Every requester relies on the previous invariant.** A requester may
  issue a blocking `Tell` into this actor's mailbox from inside its own
  receive loop. That is safe only because this actor's `Receive` never
  blocks, so its mailbox always drains. A blocking call anywhere inside
  `Receive` would allow a cycle: the requester parked on the timeout
  mailbox, and the timeout actor parked on the requester.
- **Only `actor.ErrActorTerminated` is terminal.** The actor package reports
  a closed mailbox as `ErrActorTerminated` as well. Everything else is
  retried: `ErrMailboxFull`, a `Router` target reporting
  `ErrNoActorsAvailable` while its actor is being replaced, and a context
  deadline surfaced by a custom mailbox whose enqueue is slow. Treating any
  of these as permanent would silently discard a timer that nothing
  re-derives.
- **Clock goroutines never block on the actor's own mailbox.** A fire that
  finds no room re-arms itself after `selfSignalRetry` (50ms) instead of
  parking, so at most one fire is outstanding per entry no matter how far
  behind the actor runs.
- **`Start(ref)` must be called before any request is delivered.**
  `actor.RegisterWithSystem` starts the receive loop immediately, so wire the
  self-ref before handing the returned ref to anyone. Calling `Receive`
  directly without a mailbox in front breaks the self-tell model.

## Gotchas

- **Prefer a dedicated instance for slow callbacks.** If a callback's
  mailbox makes delivery expensive (for example, one backed by a database
  write), handing it to a shared instance puts every other requester's timer
  behind that latency, and behind its retry backoff when the store is slow.
  Register a separate timeout actor for such callers.
- **Recurring ticks carry the original `FiredAt` across a retry.** A tick
  delayed by backoff reports when its timer fired, not when delivery
  succeeded. A consumer that computes `deadline = FiredAt + interval` can
  therefore conclude that the deadline already passed the moment it receives
  a retried tick.

## Usage

```go
system := actor.NewActorSystem()

// Register the timeout actor and wire its self-ref before anyone else can
// send it a request.
behavior := timeout.NewActor()
key := actor.NewServiceKey[timeout.Msg, timeout.Resp]("timeout")
timeoutRef, err := actor.RegisterWithSystem(
	system, "timeout", key, behavior,
)
if err != nil {
	return err
}
behavior.Start(timeoutRef)

// Inside another actor: adapt its own ref so the expiry arrives as one of
// its own message types.
callbackRef := timeout.MapTimeoutExpired(
	selfRef,
	func(expired timeout.ExpiredMsg) MyActorMsg {
		return &SessionTimedOut{ID: expired.ID}
	},
)

timeoutRef.Tell(ctx, &timeout.ScheduleTimeoutRequest{
	ID:       timeout.ID("session-42"),
	Duration: 30 * time.Second,
	Callback: callbackRef,
})

// Later, if the session completes first.
timeoutRef.Tell(ctx, &timeout.CancelTimeoutRequest{
	ID: timeout.ID("session-42"),
})
```

`MapTickFired` works the same way for `ScheduleRecurringTickRequest`.

## Testing

The package's tests use two styles.

- A fake `Clock` plus a synchronous self-ref (`newTestActor`) drives
  `Receive` on the test goroutine. `fakeClock.Advance` fires due callbacks
  in time order, so every assertion is exact the moment `Advance` returns.
- The `synctest_test.go` tests register the real actor in an
  `actor.ActorSystem` with `RealClock` inside a `testing/synctest` bubble.
  `time.Sleep` advances the bubble's fake clock, and `synctest.Wait` blocks
  until every goroutine in the bubble is idle, which makes the full
  mailbox-and-timer pipeline deterministic.
