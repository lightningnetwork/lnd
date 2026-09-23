# TLA+ specs of the gossip syncer

This directory holds two specs of `discovery/gossipsync` written in
[TLA+](https://lamport.azurewebsites.net/tla/tla.html), checked with TLC, the
TLA+ model checker. `SyncerPairing.tla` is the peer syncer against a peer and
a link, and `ManagerLiveness.tla` is the manager against its syncers and
timers. They state the same contract as the P models in
[`../pmodel/`](../pmodel/README.md) and the requirements in
[`../SPEC.md`](../SPEC.md). What they add is exhaustiveness: TLC visits every
state the spec can reach at a small scope, where the P checker samples
schedules. That settles two things the P models could only sample, the
liveness of the manager under an explicit fairness assumption and the
pairing rules of the syncer under every timing, and it turned up one gap in
`SPEC.md` along the way.

The rest of this document is written for a Go and Lightning developer who
hasn't used TLA+ before. It explains what TLA+ and PlusCal are, walks through
one excerpt of each spec and one counterexample, then lists what each spec
checks, the results, what's new relative to the P models, and what isn't
covered. How to run it is at the end.

## TLA+, PlusCal, and how they differ from P

A TLA+ spec describes a system as a set of variables and two formulas. `Init`
says which states the system may start in, and `Next` says which steps it may
take: a step is a pair of states, written with unprimed variables for the
state before and primed ones (`x'`) for the state after. A behavior is any
sequence of states that starts in `Init` and in which every step satisfies
`Next`. Nothing else is built in. There are no threads, messages or clocks,
only whatever the variables model. The formula

```tla
Spec == Init /\ [][Next]_vars /\ Fairness
```

reads "start in `Init`, always take a `Next` step or leave `vars` unchanged,
and meet `Fairness`". TLC takes a spec plus a `.cfg` file that gives each
constant a value, computes every initial state, and from each one explores
every possible next step, breadth first, remembering each distinct state it
has seen. When no new states turn up, it has visited every reachable state.
Each invariant is checked in every one of them, and a liveness property is
checked against the whole graph of states.

PlusCal is a language for writing algorithms that looks like C or Go, with
processes, `while` loops, `if` and assignments. It lives inside a comment in
a `.tla` file, and `pcal.trans` translates it into ordinary TLA+ (an `Init`
and a `Next`) that it writes below the comment. The checker never sees
PlusCal, only the translation. We wrote the syncer in PlusCal, since it
really is two processes handing each other events, and the manager in plain
TLA+, for reasons explained below.

Coming from P, three differences matter.

**Exhaustive, not sampled.** `p check` runs a test case for a number of
schedules, each a random sequence of choices, and a clean run means none of
the schedules it tried broke a monitor. TLC explores every choice at every
state. A clean run means no reachable state breaks an invariant, for the
constants in the `.cfg`. The price is that the constants must be small:
`SyncerPairing` runs with four blocks and three attempts, and
`ManagerLiveness` with two peers and three sessions. The bet, the "small
scope hypothesis", is that most design bugs show up at a small scope, which
is also where every bug the P models found showed up.

**States, not schedules.** P explores executions: two schedules that reach
the same state by different paths are two schedules. TLC explores states, so
it visits a state once however many paths lead to it, and a loop in the
system is a cycle in the state graph rather than an execution that never
ends. That is why TLC can check "eventually" properties on infinite
behaviors, where P approximates them with hot monitor states and a step
bound. It is also why the specs work to keep states canonical: a field that
nobody reads but that differs between two otherwise equal states would double
the state count.

**Fairness is a formula.** A P liveness test only holds under assumptions the
test drivers build in, for example a syncer that fails at most once. In
TLA+ the assumption is part of the spec, written with `WF` (weak fairness)
and `SF` (strong fairness) on named actions, and a config that drops one
conjunct shows whether that conjunct was needed. The manager excerpt below
shows how.

## A walkthrough of the syncer spec

`SyncerPairing` has two processes. The syncer handles one event at a time,
and the world, which is the peer, the link, the timeout actor and the
manager, makes every nondeterministic choice. The two meet in the variable
`ev`: the world puts an event there and waits, and the syncer handles it and
clears it, the way an actor takes one message from its mailbox and carries
out its outbox before taking the next.

Timing is the interesting part. The contract never talks about durations,
only about whether a timer can fire while the peer still has messages in
flight. Both timers are inactivity timers with the same timeout, so that can
only happen if the peer paused longer than the timeout before its next
message. We call that a long pause, count them per stream in `pauses`, and
bound them with the constant `MaxPauses`. This is the world's timer branch:

```tla
TimerMayFire ==
    IF link = <<>> THEN TRUE
    ELSE \/ FixedDrainDeadline /\ timer.drain
         \/ pauses[Head(link).q] < MaxPauses
```

```
} or {
    \* The armed timer fires, if the timing profile allows it. A
    \* fire with messages still on the link is a long pause.
    await timer.armed /\ TimerMayFire;
    if (link /= <<>>) {
        pauses[Head(link).q] := pauses[Head(link).q] + 1;
    };
    ev := [NoEvent EXCEPT !.kind = "timer", !.seq = timer.seq];
    timer.armed := FALSE;
}
```

The `either { ... } or { ... }` around this branch offers the world one choice
per branch, and TLC takes every branch that is enabled, so from every state
it tries delivering the next message, stalling the stream, firing the timer,
and the rest. `await` is what enables the branch: the timer can fire only if
it is armed and the timing profile allows it. With the link empty the peer
has sent all it will, so the timer may always fire. Otherwise a fire is a
long pause before the message at the head of the link, and the peer may take
only `MaxPauses` of them per stream. `MaxPauses = 1` is assumption A-DRAIN
from `SPEC.md`, and `MaxPauses = 2` is the peer of open question Q9.

`[NoEvent EXCEPT !.kind = "timer", !.seq = timer.seq]` is TLA+'s way to
update a record: a copy of `NoEvent` with two fields replaced. The `IF` in
`TimerMayFire` is deliberate. Inside an action TLC evaluates every disjunct
of a `\/` as its own branch, so writing the operator as a plain disjunction
would apply `Head` to an empty link.

`pcal.trans` turns the branch into this disjunct of the world's action:

```tla
\/ /\ timer.armed /\ TimerMayFire
   /\ IF link /= <<>>
         THEN /\ pauses' = [pauses EXCEPT ![Head(link).q] = pauses[Head(link).q] + 1]
         ELSE /\ TRUE
              /\ UNCHANGED pauses
   /\ ev' = [NoEvent EXCEPT !.kind = "timer", !.seq = timer.seq]
   /\ timer' = [timer EXCEPT !.armed = FALSE]
   /\ UNCHANGED <<link, lateFired, started, dups, corrupts>>
```

Each assignment became an equation on a primed variable, and every variable
the branch doesn't touch is listed in `UNCHANGED`, since a TLA+ action says
nothing about a variable it doesn't mention, and TLC would reject a step that
leaves one undetermined.

The properties are written after the translation. Every event goes through
`ev` before the syncer handles it, so a property of how the syncer handles an
event can be stated about the state in which the event is pending. This is
the pairing rule of GSS-012:

```tla
Accepts(s, m) ==
    \/ s.st = "AwaitingRange" /\ ~m.isEnd /\ Valid(s, m)
    \/ s.st = "QueryingSCIDs" /\ m.isEnd

NoCrossAttemptCredit ==
    Pending("msg") /\ Accepts(sy, ev.msg) => ev.msg.q = sy.qid
```

It says that whenever the syncer is about to fold a message into its attempt,
the message answers the attempt's own query. `q` is a ghost tag: every
message carries the ID of the query it answers, which the wire doesn't, and
which only the property reads.

## A walkthrough of the manager spec

`ManagerLiveness` is plain TLA+. In the Go code, every input to the manager
is handled and then settled in one transition, and settle makes choices of
its own. In PlusCal that would be several labeled steps, and TLC would
explore states halfway through a transition that nobody can observe. So each
input is a pure operator from a manager state to the set of states the
contract allows next, much like a protofsm `ProcessEvent` that returns every
allowed outcome at once, and one action applies the input and then settle:

```tla
HistTick ==
    /\ \E m1 \in OnTick(mgr) : \E m \in Settle(m1) : Apply(m)
    /\ ledger' = [p \in Peers |-> Older(ledger[p])]
    /\ UNCHANGED <<quota, pinned, sync, msgs>>
```

`\E m1 \in OnTick(mgr)` is the nondeterministic pick: TLC tries every peer
the tick may start. Two abstractions keep the state space finite without
losing anything the contract reads. Failure epochs are stored as an age
(`"now"`, `"last"` or `"none"`), since only the current and last epochs are
ever compared. Attempt IDs are reused, a new attempt taking the smallest ID
that nothing still refers to, since the manager only ever compares IDs for
equality. The invariant `IdsSuffice` checks that the reused IDs never run
out.

The headline of the manager spec is its fairness condition. `[][Next]_vars`
alone allows behaviors that no real node has, for example one in which the
manager never handles an outcome, or in which time stops. `Fairness` rules
them out:

```tla
TakeFairness == \A s \in SessionIds : WF_vars(Take(s))
DrainFairness == \A s \in SessionIds : WF_vars(FinishDrain(s))
DeliveryFairness ==
    \A s \in SessionIds : WF_vars(\E o \in msgs : o.s = s /\ Handle(o))
TickFairness == WF_vars(HistTick)
CompletionFairness == \A s \in SessionIds : SF_vars(Complete(s))
```

`WF_vars(A)`, weak fairness, says that if `A` stays enabled from some point
on, an `A` step eventually happens: a syncer that was sent a start
eventually takes it, a drain eventually ends, an outcome is eventually
handled, and historical ticks keep coming. `SF_vars(A)`, strong fairness,
says that if `A` is enabled again and again, even with gaps, an `A` step
eventually happens. `Complete(s)` is enabled only while session `s` is
working on an attempt, and a failure disables it until the next attempt, so
it is never enabled continuously. Weak fairness on it would allow a peer
that fails every attempt forever, and `ManagerWeakCompletion` is the config
that shows TLC finding exactly that behavior. Strong fairness says that a
peer that is given attempts again and again eventually completes one. That
is A-FAIR, stated precisely, and it is weaker than the P drivers' version,
which lets a syncer fail at most once.

The liveness properties are stated over behaviors:

```tla
GraphEventuallySynced ==
    quota > 0 /\ <>[]HasNonPinned => <>mgr.synced
```

`<>P` is "eventually P", and `[]P` is "always P", so `<>[]HasNonPinned` says
that from some point on a non-pinned peer is always connected. Connections
are bounded by `MaxSessions`, so every behavior has a last connect or
disconnect. The property says that if a peer is left connected after it, the
graph eventually syncs.

## Reading a TLC counterexample

When an invariant fails, TLC prints the shortest behavior that reaches a bad
state, since it searches breadth first. Each state is labeled with the
action that produced it, and TLC prints every variable in full. This is the
trace `cases/SyncerNoDraining.cfg` produces, a syncer with the Draining state
removed, abridged to the variables that tell the story (the `ALIAS` option that
does this arrived after tla2tools 1.7.4):

```
Error: Invariant NoCrossAttemptCredit is violated.
Error: The behavior up to this point is:
State 1: <Initial predicate>
/\ tiling = <<[first |-> 0, last |-> 3]>>
/\ sy = [st |-> "Idle", att |-> 0, qid |-> 0, ...]
/\ link = <<>>
State 2: <World line 654, col 10 to line 689, col 36 of module SyncerPairing>
/\ ev = [kind |-> "start", attempt |-> 1, ...]
State 3: <Syncer line 523, col 11 to line 652, col 52 of module SyncerPairing>
/\ sy = [st |-> "AwaitingRange", att |-> 1, qid |-> 1, ...]
/\ link = <<[first |-> 0, last |-> 3, q |-> 1, i |-> 0, complete |-> TRUE, ...]>>
/\ timer = [seq |-> 1, drain |-> FALSE, armed |-> TRUE]
State 4: <World line 654, col 10 to line 689, col 36 of module SyncerPairing>
/\ pauses = <<1, 0, 0, 0, 0, 0, 0, 0, 0>>
/\ ev = [kind |-> "timer", seq |-> 1, ...]
State 5: <Syncer line 523, col 11 to line 652, col 52 of module SyncerPairing>
/\ outcomes = <<<<"peerFault">>, <<>>, <<>>>>
/\ sy = [st |-> "Idle", ...]
State 6: <World line 654, col 10 to line 689, col 36 of module SyncerPairing>
/\ ev = [kind |-> "start", attempt |-> 2, ...]
State 7: <Syncer line 523, col 11 to line 652, col 52 of module SyncerPairing>
/\ sy = [st |-> "AwaitingRange", att |-> 2, qid |-> 2, ...]
/\ link = <<[..., q |-> 1, i |-> 0, ...], [..., q |-> 2, i |-> 0, ...]>>
State 8: <World line 654, col 10 to line 689, col 36 of module SyncerPairing>
/\ ev = [kind |-> "msg", msg |-> [first |-> 0, last |-> 3, q |-> 1, i |-> 0, ...]]
/\ sy = [st |-> "AwaitingRange", att |-> 2, qid |-> 2, ...]
```

Read it one step at a time, and look at what changed. In state 1, TLC picked
the peer's view of the chain: a single reply covering all four blocks. The
world starts attempt 1 (state 2), and the syncer sends range query 1, whose
answer goes on the link, and arms reply timer 1 (state 3). The world then
fires the timer while the reply is still on the link, which is the one long
pause A-DRAIN allows (`pauses[1]` is now 1, state 4). Without Draining, the
syncer reports a peer fault and goes straight back to Idle (state 5). The
manager starts attempt 2 (state 6), and the syncer sends query 2 while the
answer to query 1 is still in flight (state 7). In state 8 the link delivers
the old reply, tagged `q |-> 1`, to a syncer whose outstanding query is 2.
It starts at block zero and covers the last block, so the syncer would
accept it as a complete answer to attempt 2, and `NoCrossAttemptCredit` is
false in this state.

The labels such as `<World line 654, ...>` name the action and point into
the translation. Every PlusCal step of a process is one action named after
the process, so for these specs the pending `ev` is the best guide to which
branch ran.

A liveness counterexample looks different, since it has to be an infinite
behavior. TLC prints a prefix, then either `Stuttering`, meaning the
behavior stays in the last state forever, or `Back to state N`, meaning it
loops. `ManagerNoPinnedRetry` ends in `Stuttering`: a pinned peer with a
quota of zero fails its attempt with a local fault, nothing ever retries it,
and nothing that fairness requires is enabled, so the behavior may stop
there with the graph unsynced. Liveness traces are not minimal, and they
often carry connects and disconnects that play no part in the violation.

## What each spec checks

`SyncerPairing` runs with four blocks and every one of the eight ways to
split them into replies, up to two SCID batches, both outcomes of the
local lookup, three attempts, and a reply budget of four. Its properties:

| Property | Kind | Requirement |
|---|---|---|
| `NoCrossAttemptCredit` | invariant | GSS-012: a reply is credited only to the attempt whose query it answers |
| `WholeStreamCredit` | invariant | GSS-013: a range phase completes only on one whole stream |
| `OneOutstandingQuery` | invariant | GSS-012: never two outstanding queries of the same kind |
| `AtMostOneOutcome`, `QuiescentIdle` | invariants | GSS-002: one outcome per attempt, and Idle once nothing is in flight |
| `PromptPeerNeverFaulted` | invariant | GSS-009 |
| `EveryAttemptEnds` | liveness | GSS-002: every started attempt eventually has its outcome |

`ManagerLiveness` runs with two peers, three sessions, every quota from zero
to two, and every choice of pinned peers. Its properties are goals (a) to
(h) of the P `ManagerContract` monitor (`GoalA` to `GoalE` as invariants,
`GoalF` to `GoalH` as action properties), plus `IdsSuffice`, and the
liveness properties `GraphEventuallySynced` (GSM-016) and
`GraphEventuallySyncedWithPinned` (GSM-019).

Every config is in `cases/`, with a comment at the top saying what it shows.
The green cases must find no error:

| Case | What it shows |
|---|---|
| `SyncerPrompt`, `SyncerLegacyPrompt` | A prompt peer, in either reply format: every property, and no peer fault |
| `SyncerADrain`, `SyncerLegacyADrain` | A lossy peer under A-DRAIN, in either format: every pairing property, and every attempt ends |
| `SyncerVerySlow` | Any number of long pauses: whole-stream credit and one outcome per attempt still hold for an lnd-format peer |
| `SyncerByzantine` | A peer that duplicates and corrupts replies: one outcome per attempt, and back to Idle |
| `ManagerSafety` | Goals (a) to (h) over every workload |
| `ManagerFair` | Both liveness properties under `Fairness` |
| `ManagerNoTickLiveness` | Syncers that never fail need no ticks |
| `ManagerNoSettleTicks` | With fair ticks, liveness holds even without settle |

Each must-fail case removes one rule or one fairness conjunct, or runs the
production spec under an assumption the design doesn't meet, and must fail
with the named property:

| Case | Must break | Why |
|---|---|---|
| `SyncerNoDraining` | `NoCrossAttemptCredit` | The abandoned stream reaches the next attempt |
| `SyncerNoFirstReplyCheck` | `WholeStreamCredit` | A stream's tail completes a new attempt |
| `SyncerFixedDrainDeadline` | `NoCrossAttemptCredit` | A drain deadline is outlasted by a peer that paused once |
| `SyncerLegacyDrain` | `NoCrossAttemptCredit` | The old legacy drain rule ends on the first reply |
| `SyncerTwoPausesFinding` | `NoCrossAttemptCredit` | Q9: two long pauses beat the drain |
| `SyncerCompleteFlagFinding` | `OneOutstandingQuery` | Q8: a peer that sets complete on every reply ends the drain early |
| `SyncerLegacyVerySlowFinding` | `WholeStreamCredit` | Q10: a legacy peer beyond A-DRAIN completes an attempt with a partial stream |
| `SyncerOverBudgetFinding` | `OneOutstandingQuery` | A stream longer than the reply budget leaves its tail in flight, even from a prompt peer |
| `ManagerNoSettleSafety` | `GoalA` | Without settle, an eligible peer is left idle |
| `ManagerNoLocalBackoff` | `GoalH` | An unrecorded local fault is retried in the same epoch |
| `ManagerNoSettleStarves` | `GraphEventuallySynced` | No settle and no ticks: nothing starts the sync |
| `ManagerNoPinnedRetry` | `GraphEventuallySyncedWithPinned` | A failed pinned peer is never retried |
| `ManagerNoTickFairness` | `GraphEventuallySynced` | A failed peer's epoch never ends |
| `ManagerWeakCompletion` | `GraphEventuallySynced` | WF on `Complete` lets a peer fail forever |
| `ManagerNoTakeFairness` | `GraphEventuallySynced` | A start is never taken |
| `ManagerNoDrainFairness` | `GraphEventuallySynced` | A syncer drains forever and refuses every attempt |
| `ManagerNoDeliveryFairness` | `GraphEventuallySynced` | The completing outcome is never handled |

The last five manager cases drop the five conjuncts of `Fairness` one at a
time, so each conjunct is needed. There's no conjunct saying a working
syncer eventually reports, because it would be implied: `Report(s)` and
`Complete(s)` are enabled in exactly the same states, so strong fairness on
`Complete(s)` already forces a report from a syncer that works forever. An
earlier draft had that conjunct, and dropping it changed nothing.

## Results

These are the numbers from `check.sh` on a 16-core Apple laptop, with TLC
2.19 (tla2tools 1.7.4) on Java 17 and `-workers auto`. The whole run takes
about twelve minutes, most of it in the three liveness cases of the manager.
A must-fail case stops at its first violation, and since the workers race,
its counts vary a little from run to run.

| Case | Kind | States | Distinct states | Time |
|---|---|---|---|---|
| `SyncerPrompt` | green | 39,612 | 19,216 | 1s |
| `SyncerADrain` | green | 5,704,624 | 2,059,108 | 1min 09s |
| `SyncerLegacyPrompt` | green | 39,612 | 19,216 | 1s |
| `SyncerLegacyADrain` | green | 5,704,624 | 2,059,108 | 1min 08s |
| `SyncerVerySlow` | green | 27,166,681 | 9,709,098 | 12s |
| `SyncerByzantine` | green | 68,584,713 | 23,864,882 | 29s |
| `ManagerSafety` | green | 4,959,208 | 741,757 | 6s |
| `ManagerFair` | green | 4,959,208 | 741,757 | 4min 38s |
| `ManagerNoTickLiveness` | green | 57,540 | 10,628 | 1s |
| `ManagerNoSettleTicks` | green | 4,108,204 | 623,317 | 2min 53s |
| `SyncerNoDraining` | must fail | 2,908 | 2,734 | <1s |
| `SyncerNoFirstReplyCheck` | must fail | 25,690 | 20,944 | <1s |
| `SyncerFixedDrainDeadline` | must fail | 10,300 | 8,921 | <1s |
| `SyncerLegacyDrain` | must fail | 9,206 | 7,862 | <1s |
| `SyncerTwoPausesFinding` | must fail | 11,062 | 9,592 | <1s |
| `SyncerCompleteFlagFinding` | must fail | 5,601 | 4,958 | <1s |
| `SyncerLegacyVerySlowFinding` | must fail | 26,817 | 21,812 | <1s |
| `SyncerOverBudgetFinding` | must fail | 4,574 | 3,773 | <1s |
| `ManagerNoSettleSafety` | must fail | 157 | 84 | <1s |
| `ManagerNoLocalBackoff` | must fail | 1,210 | 561 | <1s |
| `ManagerNoSettleStarves` | must fail | 66,610 | 12,358 | <1s |
| `ManagerNoPinnedRetry` | must fail | 190,770 | 47,091 | 3s |
| `ManagerNoTickFairness` | must fail | 455,451 | 108,661 | 3s |
| `ManagerWeakCompletion` | must fail | 122,084 | 33,569 | 3s |
| `ManagerNoTakeFairness` | must fail | 130,834 | 35,694 | 3s |
| `ManagerNoDrainFairness` | must fail | 118,032 | 32,642 | 3s |
| `ManagerNoDeliveryFairness` | must fail | 137,464 | 37,109 | 3s |

The lnd and legacy honest cases have identical counts. That's expected: for
an honest peer, the accumulator accepts the same replies in both formats, so
the two state graphs have the same shape, and the formats only come apart
in the must-fail cases. `SyncerVerySlow` uses `MaxPauses = 6`. At four
blocks the state count stops growing at six (six and nine give the same
9,709,098 distinct states), so at this scope it is the same as any number
of pauses. The liveness property `EveryAttemptEnds` is checked in the four
honest syncer cases, and left out of `SyncerVerySlow` and `SyncerByzantine`,
where it would cost about twenty times the invariants.
`QuiescentIdle` covers the same ground there, since every behavior of the
world is finite and ends quiescent.

## What this adds over the P models

**Liveness under an explicit fairness condition.** `SPEC.md` marked GSM-016
and GSM-019 as verified only under A-FAIR, an informal assumption that the P
drivers build in by letting each syncer fail at most once and by ticking only
when needed. Here A-FAIR is the formula `Fairness`, and `ManagerFair` checks
both properties against every fair behavior at its scope, with syncers that
may fail any number of times. The drop-one cases show that each of the five
conjuncts is needed. They also answer a design question. With fair ticks,
the graph syncs even without settle (`ManagerNoSettleTicks`), because every
tick starts an attempt on an eligible peer. Settle is what starts the sync at
once (goal (a)), and what starts it at all when no tick comes
(`ManagerNoSettleStarves`), but the tick is what makes it live.

**Q9 and A-DRAIN, exhaustively.** Every pairing property holds in every
reachable state under A-DRAIN, in both reply formats, for streams that fit
in the reply budget, and `SyncerTwoPausesFinding` confirms that a second long pause is enough to beat
the drain.

**Q8, confirmed.** `SPEC.md` predicted that a peer that sets complete on
every reply ends the drain on its first absorbed reply, so that the next
query goes out while the old stream is still arriving.
`SyncerCompleteFlagFinding` shows that this happens under A-DRAIN, which
breaks `OneOutstandingQuery`.

**A new gap, Q10.** `SPEC.md` states GSS-013 for any timing: "a range phase
MUST only complete on one whole stream", and section 10 says the first-reply
check keeps the damage beyond A-DRAIN to a whole stream. P checks this only
for lnd-format peers. For a legacy peer it fails, and
`SyncerLegacyVerySlowFinding` shows how. Attempt 1 accepts the first reply of
a two-reply legacy stream, and the peer then pauses past the timeout twice
before the second reply: the reply timer fails the attempt, and the drain
timer ends the drain. Attempt 2 starts, and the old stream's last reply
arrives. It echoes the whole query and sets complete, so it skips
`checkRange` and ends the stream at once, and attempt 2 completes having
seen only the channels of one reply. The Go code behaves the same way, since
`rangeAccumulator.add` skips `checkRange` for a legacy reply. The damage is
small (the attempt completes with a subset of the peer's channels, and later
ticks pick up the rest), and it needs an old peer that pauses for more than
the reply timeout twice in one stream, or a stream longer than the reply
budget, which with the default budget of 500 needs a far larger graph than
any real one. But the requirement and section 10
overstate the guarantee, and `SPEC.md` now records it as Q10.

No disagreement was found between the TLA+ specs and the P models, or
between either and the Go code, other than Q10. The specs model the
contract, not the code, so that conclusion rests on the bridges that tie the
P models to the Go code.

## What this doesn't cover

The results hold for the modeled contract at the constants in `cases/`, not
for the Go code, and not at larger scopes. Nothing here runs against the Go
state machines. The P bridges replay P executions into the Go code, and
nothing like that exists for these specs, so a change to the Go code that
the TLA+ specs don't reflect goes unnoticed here.

The scope is small. The syncer has four blocks, one channel per block, and
plain replies, so it never splits a block across replies, never truncates a
block's channels, and never charges a zlib reply four units. Every pairing
result also assumes an honest stream fits in the reply budget: the green
cases have a budget of four, as long as the longest stream, and
`SyncerOverBudgetFinding` shows what happens when a stream is longer. That
is the same assumption GSS-007 states, and with Go's budget of 500 replies
it holds for any real graph; as in the P model, the
continuation of a reply on the previous reply's last block is unmodeled. The
missing channels are abstracted to a count of SCID batches. Role changes
(GSS-014) are left out, since they never interact with pairing. The manager
has two peers and three sessions, and its syncers are abstract. As in P, the
two specs meet only at the outcome kinds, so no check composes the manager
with real syncers. Forged outcomes, whose attempt is in flight on another
session, aren't generated, so the session check of GSM-011 is not shown to be
needed here (P's `tcManagerNoSessionCheckCounterexample` does that). Stale
outcomes from stopped sessions are generated, and `GoalF` checks them. Q2,
the untracked resync after losing every peer, has no liveness property here
either.

The action properties and invariants of the manager read the manager's own
record of attempts in flight, where P's monitor rebuilt its own ledger. Only
the failure epochs have an independent ghost copy, `ledger`, which is what
lets `ManagerNoLocalBackoff` fail.

## Running

`check.sh` needs Java 11 or later and `tla2tools.jar`, from the [TLA+
releases](https://github.com/tlaplus/tlaplus/releases):

```sh
mkdir -p ~/tools
gh release download --repo tlaplus/tlaplus --pattern tla2tools.jar --dir ~/tools
bash discovery/gossipsync/tla/check.sh
```

It translates the PlusCal in `SyncerPairing.tla`, rewriting the translation
in place, runs every green case and every must-fail case, and prints the
table above. It fails if a green case finds an error, or if a must-fail case
finds none or fails with a different error. These environment variables
tune it:

| Variable | Default | Meaning |
|---|---|---|
| `JAVA` | Homebrew's `openjdk@17`, then `java` | Java 11 or later |
| `TLA2TOOLS` | `~/tools/tla2tools.jar` | The TLA+ tools |
| `WORKERS` | `auto` | TLC worker threads |
| `CASES` | `.` | Only run the cases whose name matches this regex |

To run one case by hand and see its trace:

```sh
cd discovery/gossipsync/tla
java -cp ~/tools/tla2tools.jar pcal.trans -nocfg SyncerPairing.tla
java -XX:+UseParallelGC -cp ~/tools/tla2tools.jar tlc2.TLC -workers auto \
  -config cases/SyncerNoDraining.cfg SyncerPairing.tla
```

TLC writes its state files under `states/` unless `-metadir` points
elsewhere, and `pcal.trans` leaves a `SyncerPairing.old` backup; neither
belongs in the tree.

## Changing the specs

Edit the PlusCal in the comment of `SyncerPairing.tla`, never the text
between `BEGIN TRANSLATION` and `END TRANSLATION`, which `pcal.trans`
regenerates. Keep the rules of the production contract in green configs, and
put a known-bad variant behind a constant that only a must-fail config sets,
as the P models do with profile flags. A new rule gets a must-fail case that
removes it. When a result changes, update the table above and the matrix in
`../SPEC.md`.

## Layout

| Path | What it is |
|---|---|
| `SyncerPairing.tla` | The peer syncer contract in PlusCal, its translation, and its properties |
| `ManagerLiveness.tla` | The manager contract, its fairness condition, and its properties, in TLA+ |
| `cases/*.cfg` | One TLC configuration per green or must-fail case |
| `check.sh` | Translate, check every case, and print the results |
