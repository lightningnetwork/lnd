# Formal models of the gossip syncer

This directory holds models of the gossip syncer written in
[P](https://p-org.github.io/P/), a language for describing distributed
systems as communicating state machines, together with a model checker that
runs those machines under many different interleavings looking for a
violated property. We model the two state machines at the heart of
`discovery/gossipsync`: the manager, which decides each peer's role and which
peer runs a historical sync, and the peer syncer, which runs the BOLT 7 query
protocol against one peer. Go tests in the parent package then replay
executions of the models against the real Go state machines, so a model that
no longer matches the code fails `go test`. Finally, `../SPEC.md` is a human
readable specification derived from the models, with every requirement tied
back to the model, the code and the test that checks it.

The rest of this document explains why we built the models, gives the P
needed to read them, and walks through what they check, how they connect
to the Go code, what they found, and what they don't cover. The reference
material for running and changing them is at the end.

## Why model the contract, not the code

The Go package already has two layers of tests. Property tests drive each
pure state machine with random inputs and check its invariants, and a
deterministic simulation (`dst_test.go`) runs whole nodes, with real actors
and faulty links, inside a `synctest` bubble. Both check the code against
properties we thought to write down, on the executions a random generator
happens to produce.

The models add two things. First, the P checker controls every
interleaving: the order in which messages, timer fires, disconnects and
outcomes arrive is its choice, and it explores thousands of such orders per
test case. A property such as "no reply is ever credited to the wrong
attempt" is checked against every order the checker reaches, not only the
orders a goroutine scheduler happens to pick.

Second, and more important, the models describe the _contract_ the code is
meant to meet, not the code itself. Where the contract leaves a choice open
(which eligible peer starts a sync, which passive peer is promoted, which
pair a rotation swaps), the model picks nondeterministically among every
option the contract allows, and the monitors check goals stated from their
own bookkeeping rather than from the machine's internal state. A
transliteration of the Go code into P would inherit the code's bugs and then
confirm them. A contract model disagrees with the code wherever the code
does something the contract doesn't allow, and those disagreements are the
point: every bug listed under "What the models found" showed up this way.

The cost of this approach is that a contract model can't be replayed into
the code step for step, since the model and the code make their choices
differently. The bridges below close that gap by aligning on decisions and
comparing only what is observable.

## Enough P to read the models

A P program is a set of _machines_. Each machine has named states, handles
_events_ sent to it by other machines, and can send events, create machines
and change state. Sends are asynchronous and each machine has its own
queue, so the relative order of events from different machines is up to the
checker. That is exactly the freedom the checker explores.

Inside a machine, `choose(xs)` returns an arbitrary element of `xs`, and the
checker tries different elements on different runs. The manager model makes
every open choice through one helper, which also prints the choice so the
Go bridge can follow it:

```p
fun Decide(kind: PickKind, allowed: set[int]): int {
  var s: int;
  s = choose(allowed);
  print format("MTRACE choice kind={0} session={1} from={2}",
    PickName(kind), s, Join(SortSet(allowed)));
  return s;
}
```

A _spec machine_ (also called a monitor) observes events without taking
part in the system. It checks safety properties with `assert`, and liveness
properties with _hot_ and _cold_ states: a hot state marks a condition that
must eventually clear, and the checker reports a liveness bug if an
execution ends, or runs past its step bound, while a monitor is still hot.
This is the monitor behind "the graph eventually syncs" (the helper
`Pending` reports whether some connected, non-pinned peer is still owed a
sync):

```p
spec GraphEventuallySynced observes eMSnap {
  start cold state Settled {
    on eMSnap do (sn: ...) {
      if (Pending(sn.members, sn.synced, sn.numActive)) {
        goto Unsynced;
      }
    }
  }

  hot state Unsynced {
    on eMSnap do (sn: ...) {
      if (!Pending(sn.members, sn.synced, sn.numActive)) {
        goto Settled;
      }
    }
  }
}
```

A _test case_ names a driver machine that sets up the scenario, and the
monitors to attach. `p check` runs a test case for a given number of
schedules, each one a different sequence of choices, and stops at the first
assertion failure or liveness violation, printing the whole execution that
led there.

## What is modeled

### The manager

`Manager` in `src/manager.p` takes the same inputs as `ManagerState`:
connects (pinned or not), disconnects, rotate ticks, historical ticks and
attempt outcomes. It records what each input means, then runs a settle step
that restores the two standing goals. While the graph is unsynced, a
tracked historical sync runs whenever some peer is eligible, and once it is
synced, the active quota is as full as the connected peers allow.

Each connected session gets an abstract `Syncer` machine, which answers
asynchronously: it completes, fails with a peer or local fault, keeps
working, or drains after a peer fault and refuses new attempts as busy in
the meantime. So the checker explores outcomes that race disconnects,
reconnects of the same peer on a new session, ticks and rotations, as well
as outcomes that arrive after their session has been stopped.

`ManagerContract` keeps its own ledger of attempts, failure epochs and
pinned peers, and after every input it checks eight goals:

  - (a) While unsynced with a quota and an eligible peer, a tracked attempt
    is in flight.
  - (b) Once synced, the active count is `min(quota, non-pinned peers)`.
  - (c) The active count never exceeds the quota.
  - (d) Pinned peers are pinned, and no other peer is.
  - (e) A session never has two attempts.
  - (f) A stale outcome changes nothing observable.
  - (g) The synced flag never goes back to false, and only a completion in
    flight sets it.
  - (h) A peer is not restarted in the epoch in which it failed, unless it
    is pinned.

It also checks that the manager's attempts match the ledger and that there
is exactly one session per connected peer. `GraphEventuallySynced` and
`GraphEventuallySyncedWithPinned` are the liveness goals: the graph
eventually syncs if a non-pinned peer or a pinned peer is connected.

### The peer syncer

`PeerSyncer` in `src/syncer.p` is the component under contract. Everything
around it is one `World` machine, which owns every choice: it delivers the
link's messages in order, may stall the rest of a stream, fires timers when
the timing profile allows, fires stale timers, starts attempts and changes
the syncer's role. In the byzantine profile it also duplicates and corrupts
replies.

The chain has four abstract blocks and the peer has at most one channel per
block. That is small, but large enough for a stream of several replies, a
range query and SCID batches, which is what the pairing properties are
about. Every query carries a ghost ID that doesn't exist on the wire, and
every peer message is tagged with the query it answers and its position in
the stream. The monitors use those tags to say what the wire can't: which
attempt a reply really belongs to.

Timers are events, not durations. Both the reply timer and the drain timer
are inactivity timers with the same timeout, so a timer can fire with
messages still in flight only if the peer paused longer than the timeout at
that point, which we call a long pause. The timing profile bounds long
pauses, and each test case picks one:

| Profile | Long pauses per stream |
|---|---|
| `PROMPT` | none |
| `SLOW` | at most one (assumption A-DRAIN in `../SPEC.md`) |
| `VERYSLOW` | any number |

The syncer monitors are `OneOutstandingQuery`, `NoCrossAttemptCredit`,
`WholeStreamCredit`, `OutcomeExactlyOnce`, `CompletedQueriedMissing` and
`PromptPeerNeverFaulted`. Each test case attaches the ones that must hold
under its profile. For example, `NoCrossAttemptCredit` holds under `SLOW`
but not under `VERYSLOW`, which is the one known limit described below.

## Green cases, counterexamples and findings

A model checker that finds nothing has told you very little, unless you
know it would have found something. So every rule the design depends on
has a counterexample: a test case that runs the model with that one rule
removed, and must fail with the assertion that rule exists to satisfy.
`check.sh` fails if a counterexample passes, and it checks that each one
fails with its named assertion, not with some unrelated error.

There are three kinds of test case:

  - A green case runs the production contract and must find no bug.
  - A counterexample removes one rule and must find the bug that rule
    prevents.
  - A finding runs the production contract under an assumption the design
    doesn't meet, and must find the bug that documents the limit. A finding
    that starts passing means the code or the model changed, and
    `../SPEC.md` needs updating.

| Case | Kind | What it shows |
|---|---|---|
| `tcManagerProduction` | green | The contract under random workloads, with stale outcomes |
| `tcManagerOnePeer` | green | One peer through many ticks |
| `tcManagerLegacyTick` | green | Picking before opening the epoch changes no property |
| `tcManagerSinglePeerLegacyTick` | green | The same, for the single peer that stalled the legacy manager |
| `tcManagerLiveness` | green | Peers that fail at most once, with ticks, always sync |
| `tcManagerNoTickLiveness` | green | Peers that always complete sync with no tick at all |
| `tcManagerPinnedOnly` | green | Only pinned peers, each failing once, sync under either quota |
| `tcManagerNoSettleCounterexample` | counterexample | Without settle, goal (a) fails |
| `tcManagerNoSettleStarvesCounterexample` | counterexample | Without settle and ticks, the graph never syncs |
| `tcManagerNoLocalBackoffCounterexample` | counterexample | A local fault that isn't backed off violates (h) |
| `tcManagerNoSessionCheckCounterexample` | counterexample | Crediting an outcome from the wrong session breaks the ledger |
| `tcManagerNoPinnedRetryCounterexample` | counterexample | Without the tick retry, a failed pinned peer is never retried while connected |
| `tcSyncerHonestPrompt` | green | Every property, and no attempt fails |
| `tcSyncerHonestLossy` | green | Every pairing property under `SLOW` with stalls |
| `tcSyncerVerySlowPeer` | green | Whole-stream credit and one outcome per attempt under `VERYSLOW` |
| `tcSyncerByzantine` | green | One outcome per attempt against duplicates and corruption |
| `tcSyncerLegacySlow`, `tcSyncerLegacyPrompt` | green | The legacy reply format, which echoes the whole query in every reply |
| `tcSyncerNoDrainingCounterexample` | counterexample | Without Draining, a reply is credited to the wrong attempt |
| `tcSyncerNoFirstReplyCheckCounterexample` | counterexample | Without the first-reply check, a stream's tail completes a new attempt |
| `tcSyncerLegacyDrainCounterexample` | counterexample | Ending a legacy drain on the first reply credits the stream to the next attempt |
| `tcSyncerFixedDrainDeadlineCounterexample` | counterexample | A drain deadline fixed at abandonment is outlasted by a slow peer |
| `tcSyncerCrossCreditBeyondDrainFinding` | finding | A peer that pauses past the timeout twice in one stream beats the drain |

## Connecting the models to the Go code

A model is only useful if it describes the code we ship. Three Go tests in
the parent package keep the two together, and all of them run under a plain
`go test ./discovery/gossipsync/`, without the P toolchain, by replaying the
traces checked in under `traces/`.

### Recorded traces

When a bridged test case runs with `-v`, the model prints one line per
step. A manager trace records each input, each choice with the set it was
chosen from, each action the contract forces with no choice, and a snapshot
of the observable state after the input:

```
begin num_active=1
in connect pub=1 pinned=0
choice kind=start session=1 from=1
snap roles=1:P synced=0 tracked=1 inflight=1 started=1 reset=1 publish=0
in hist_tick
snap roles=1:P synced=0 tracked=1 inflight=1 started=- reset=0 publish=0
in outcome session=1 attempt=1 kind=0
choice kind=promote session=1 from=1
snap roles=1:A synced=1 tracked=0 inflight=- started=- reset=0 publish=1
```

The snapshot holds every session's role, whether the graph is synced, the
session holding the tracked attempt, the sessions with an attempt in
flight, the attempts started by this input, and whether the historical
timer was reset and the graph published as synced. Syncer traces work the
same way, with the syncer's outbox in place of a snapshot.

### The manager bridge

`TestPModelManagerBridge` in `../pmodel_bridge_test.go` feeds each input to
`ManagerState` through `protofsm.ApplyEvents`. The Go manager makes its own
random picks through `ManagerEnv.Rand`, and the model made its picks with
`choose`, so the bridge has to line the two up. For each pick, it finds
the full list of sessions Go could have picked by rerunning the pure
transition once per index, and requires that set to equal the set the model
chose from. If they differ, the test fails and names both sets, which is
how a contract disagreement shows up. It then steers `Rand` so Go picks the
same session as the model, and compares the snapshot after the input.

The bridge compares the snapshot, never the order of the outbox, and never
internal fields such as failure epochs. So the code is free to change how
it does something, as long as what it does stays within the contract.

### The syncer bridge

`TestPModelSyncerBridge` in `../pmodel_syncer_bridge_test.go` maps abstract
block `b` to heights `[500b, 500b+499]`, and builds each real
`lnwire.ReplyChannelRange` from the model's block range, channels, complete
flag and corruption flag. After every input it compares the syncer's
outbox, projected onto outcomes, queries with their SCIDs, timestamp
filters, and timer arms and disarms, as a multiset. The reply and drain
timeouts have the same default, so the bridge gives the drain timer a
distinct duration to tell the two kinds of arm apart.

### The reference model

`TestRefModelManager` in `../refmodel_test.go` is a third check that
doesn't go through P at all. It is a small Go model of the manager
contract, written without any `manager_state.go` helper. rapid draws a
workload, `ManagerState` makes its own random picks, and at every pick the
test requires Go's whole candidate set to equal the set the reference
allows, then has the reference adopt Go's pick. After every input the
snapshots must match. It explores different workloads than the P traces,
since rapid generates its inputs independently of the checker.

## The spec

`../SPEC.md` is written in the style of a protocol specification: numbered
requirements (`GSM-` for the manager, `GSS-` for the syncer) using the
normative keywords of RFC 2119. Each requirement names the model element
that states it, the Go code that implements it, and the tests that check
it, and a traceability matrix marks each one verified, partially verified,
or unmodeled.

The spec is derived from the models and pinned to them. `scripts/
extract_p_model.py` builds an inventory of the model's machines, states,
events, assertions and functions, with source locations and a SHA-256
digest over the model sources. `scripts/validate_spec.py` checks that every
requirement has its model disposition and uses a normative keyword, that
every `file.p:line` citation points at a real line, and that the digest
recorded at the top of `SPEC.md` matches the current model. So editing any
`.p` file without revisiting the spec fails `check.sh`.

The spec's last section records the abstractions the models make, every
disagreement found between model and code, and the open questions: places
where the contract itself needs a decision, rather than the code.

## What the models found

The models found three problems in the Go code, each fixed before these
models landed.

A legacy peer echoes the whole query in every `reply_channel_range`, so
every one of its replies covers the query's last block. The old Draining
rule ended the drain on the first reply that covered the last block, which
for a legacy peer was the first reply of the abandoned stream. The rest of
that stream then reached the next attempt, where legacy replies skip the
range checks, and completed it. The syncer bridge failed on its first
replay against the old rule. The fix ends a legacy drain only on the reply
that sets `Complete`, and `tcSyncerLegacyDrainCounterexample` keeps the old
rule.

A pinned peer only ever ran the historical sync it starts on connect.
Neither settle nor a tick picks a pinned peer, so if that sync failed, the
peer never synced again until it reconnected. With no active syncers
configured, or with only pinned peers connected, the graph stayed unsynced.
`GraphEventuallySyncedWithPinned` caught it. The fix retries a failed
pinned peer at the next historical tick, and
`tcManagerNoPinnedRetryCounterexample` keeps the old behavior.

The drain timer was a one minute deadline from the moment an attempt was
abandoned, while a live exchange tolerates five minutes between replies. A
peer slow enough to be accepted in a live exchange could outlast the drain,
and the rest of its abandoned stream was credited to the next attempt.
`NoCrossAttemptCredit` caught it under the `SLOW` profile. The drain timer
is now an inactivity timer with the same default as the reply timer, and
`tcSyncerFixedDrainDeadlineCounterexample` keeps the fixed deadline.

The models also confirmed a design question in the other direction. The
legacy manager picked a peer on each historical tick before opening a new
failure epoch, and it looked like that order might matter.
`tcManagerLegacyTick` shows that, with settle in place, it doesn't.

## What the models don't cover

The models are checked by exploring schedules, not by proof, and each test
case explores a bounded number of them with a bounded number of steps.
The properties hold for the abstraction under the schedules explored.

The abstraction is deliberately small. The syncer model uses four blocks,
one channel per block and plain replies, so it never splits a block across
replies, never truncates one, and never exercises zlib's reply cost or the
freshness filter; those are covered by the Go unit tests, and `../SPEC.md`
lists them as unmodeled. The manager model's syncers are abstract and meet
the syncer model only at the outcome kinds.

The responder and the actor layer aren't modeled at all. Which sends may
block, mailbox bounds and shutdown are covered by the simulation and the
actor tests in the parent package.

One pairing limit remains, and `tcSyncerCrossCreditBeyondDrainFinding`
records it. A peer that pauses past the timeout twice in one stream still
beats the drain: the first pause fails the attempt, and the second looks
exactly like a peer that stopped answering, so the drain ends. A later
attempt can then be credited with the rest of the old stream. The
first-reply check limits that to a whole stream answering an equivalent
query. Closing it completely would need query IDs on the wire, which BOLT 7
doesn't have.

Finally, the bridges read a few unexported `ManagerState` fields (`members`,
`inflight`, `tracked`, `graphSynced`) to build the snapshot, so renaming
one of them breaks the build of the bridge tests.

## Running

`check.sh` needs P 3.0.4 and Python 3:

```sh
dotnet tool install --global P --version 3.0.4
bash discovery/gossipsync/pmodel/check.sh
```

It compiles the project, runs every green case (2000 schedules each by
default) and every counterexample and finding, records fresh traces for the
bridged cases and replays them, together with the checked-in ones, into the
Go code, and validates `../SPEC.md`. It takes about two minutes. These
environment variables tune it:

| Variable | Default | Meaning |
|---|---|---|
| `SCHEDULES` | 2000 | Schedules per test case |
| `MAX_STEPS` | 3000 | Step bound per schedule |
| `TRACE_SEED` | 1 | Seed of the recorded runs |
| `TRACE_RUNS` | 40 | Schedules recorded per bridged case |
| `KEEP_TRACES` | 0 | Set to 1 to write the recorded traces into `traces/` |

To run one case on its own and see its execution:

```sh
cd discovery/gossipsync/pmodel
p compile -pp gossipsync.pproj
p check PGenerated/PChecker/net8.0/GossipSyncModels.dll \
  -tc tcSyncerNoDrainingCounterexample -s 2000 -v
```

When a case fails, `p check` prints the failing assertion and writes the
full execution under `PCheckerOutput/BugFinding/`. With `-v`, the trace
lines show up prefixed with `<PrintLog> MTRACE` or `<PrintLog> STRACE`.

## Changing the code or the models

When a change to the Go state machines makes a bridge fail, first decide
which side is wrong. If the code now does something the contract doesn't
allow, the bridge caught a bug. If the contract should change, change the
model first, then refresh the traces and the spec:

  1. Edit the model. State a new rule as a monitor that keeps its own
     bookkeeping, and add a counterexample case that removes it. A
     known-bad variant goes behind a profile flag in a counterexample case,
     never in a green one.
  2. Route any new manager pick through `Decide`, so it is printed with its
     allowed set, and make its effect visible in the snapshot. A change to
     what the model prints needs the matching change in the bridge.
  3. Run `KEEP_TRACES=1 bash check.sh` to refresh `traces/`.
  4. Update `../SPEC.md`: the affected requirements, their citations, and
     the digest at the top, which `check.sh` checks.

A few P details trip people up. `inflight`, `format`, `seq` and `in` are
reserved words. Strings can't be joined with `+`, so nest `format` calls
instead. And `p check` selects test cases by prefix, so no case name may be
a prefix of another.

## Layout

| Path | What it is |
|---|---|
| `src/manager.p` | The manager contract, abstract per-session syncers, and the manager monitors |
| `src/syncer.p` | The peer syncer contract, its `World`, and the syncer monitors |
| `test/manager_test.p` | Manager drivers and test cases |
| `test/syncer_test.p` | Syncer drivers and test cases |
| `traces/` | Recorded executions the Go bridges replay on every `go test` |
| `scripts/` | The spec inventory extractor and validator |
| `check.sh` | Compile, check every case, record traces, run the bridges, validate the spec |
| `../pmodel_bridge_test.go` | `TestPModelManagerBridge` |
| `../pmodel_syncer_bridge_test.go` | `TestPModelSyncerBridge` |
| `../refmodel_test.go` | `TestRefModelManager` |
| `../SPEC.md` | The specification derived from the models |
