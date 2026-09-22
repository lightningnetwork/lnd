// Package timeout provides a fire-and-forget timer scheduling actor built on
// the lnd actor package.
//
// Other actors send the timeout Actor a ScheduleTimeoutRequest (a one-shot
// timer) or a ScheduleRecurringTickRequest (a fixed-delay ticker), each keyed
// by an ID and carrying a callback ref. When the timer fires, the actor
// delivers an ExpiredMsg or TickFiredMsg to that callback. A
// CancelTimeoutRequest with the same ID stops either kind, and scheduling an
// ID that is already live replaces the existing entry. MapTimeoutExpired and
// MapTickFired adapt a caller's own actor ref so the notification arrives as
// one of the caller's message types.
//
// All actor state is owned by the Receive goroutine. Clock callbacks never
// touch it directly; they hand an internal fire message, stamped with a
// generation number, back into the actor's own mailbox, and stale fires that
// raced a cancel or reschedule are dropped by the generation check.
//
// Two invariants make the actor safe to share between many requesters:
//
//   - Receive never blocks on a callback. Delivery uses TryTell. A requester
//     whose mailbox is full keeps its entry, and the same fire is re-armed on
//     a backoff that doubles from one second up to thirty seconds. Only
//     actor.ErrActorTerminated is treated as permanent. Because Receive never
//     blocks, the actor's own mailbox always drains, which is what makes it
//     safe for a requester to issue a blocking Tell into this actor from
//     inside its own receive loop.
//
//   - Clock goroutines never block on the actor's own mailbox. A fire that
//     finds the mailbox full re-arms itself after a short delay instead of
//     parking a goroutine, so at most one fire is outstanding per entry no
//     matter how far behind the actor runs.
//
// The actor takes its notion of time from a Clock. RealClock delegates to the
// time package, which also makes the actor deterministic inside a
// testing/synctest bubble. Tests that want to drive Receive synchronously can
// supply their own Clock through NewActorWithClock.
//
// See README.md in this directory for a longer discussion and a usage
// example.
package timeout
