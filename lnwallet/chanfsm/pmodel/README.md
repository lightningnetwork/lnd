# Formal models of the channel commitment protocol

This directory holds models of the BOLT 2 commitment protocol written in
[P](https://p-org.github.io/P/), a language for describing distributed
systems as communicating state machines, together with a model checker that
runs those machines under many different interleavings looking for a
violated property. The models are two channel peers, each mirroring the
ledger and state machine in `lnwallet/chanfsm`, and a world that connects
them, disconnects them, restarts them from what lnd persists, and lets one of
them misbehave. A Go test in the parent package replays recorded runs of the
models into the real state machine, so a model that no longer matches the
code fails `go test`. `../SPEC.md` is the requirements spec derived from the
models, with every requirement tied back to the model, the code and the test
that checks it.

## What the models add

The Go package already tests each piece on its own. Unit tests pin each
ledger rule. The mirror test checks the ledger against a real
`LightningChannel`, and the differential and simulation tests run the actor
over real channels. All of them check the properties we thought to write
down, on the executions a random generator happens to produce, and most of
them look at one node at a time.

The models add three things. First, the checker controls every
interleaving: which node acts next, which message is delivered, when a
disconnection drops everything in flight, and which message a byzantine peer
injects are all its choice, and it explores thousands of such orders per
test case.

Second, the monitors judge a node against its peer's actual state, which no
single-node test can see. "Forwards only irrevocably committed updates"
needs both nodes' current commitments. "Both sides agree on every
commitment" needs what the signer built and what the verifier builds. The
monitors keep that bookkeeping from the events both nodes announce.

Third, every rule the design depends on has a counterexample: a variant of
the node with that one rule removed, which must fail with the assertion the
rule exists to satisfy. A model checker that finds nothing has told you
little unless you know it would have found something, and the
counterexamples are how we know.

The node model is close to the Go code rather than an independent contract:
it mirrors `Ledger` and the state machine function for function, and its
comments cite them. That is deliberate. The protocol's contract is BOLT 2,
and the monitors state it independently of the node. What the node model has
to get right is what lnd does, so that the bridge can hold the Go code to it.

## Enough P to read the models

A P program is a set of _machines_. Each machine has named states, handles
_events_ sent to it by other machines, and can send events, create machines
and change state. Sends are asynchronous and each machine has its own
queue, so the relative order of events from different machines is up to the
checker.

Inside a machine, `choose(n)` returns an arbitrary integer below `n`, and
the checker tries different values on different runs. `World` makes every
random decision this way: which node acts, which command it issues, whether
to deliver a message, disconnect, or inject one.

A _spec machine_ (a monitor) observes events without taking part in the
system. Nodes `announce` what they do, and each monitor that observes that
event runs its handler. A monitor checks safety properties with `assert`.
This is the whole of the at-most-once property:

```p
spec ForwardAtMostOnce observes eAnForward {
  var seen: map[int, set[int]];

  start state Watch {
    on eAnForward do (p: (node: int, kind: Kind, id: int, logIndex: int)) {
      assert !(p.logIndex in seen[p.node]),
        format("node {0} forwarded peer update {1} twice", p.node,
          p.logIndex);
      seen[p.node] += (p.logIndex);
    }
  }
}
```

Liveness uses _hot_ and _cold_ states. A hot state marks a condition that
must eventually clear, and the checker reports a liveness bug if a run ends
while a monitor is still hot. `EventuallyLockedInAndForwarded` goes hot when
a node proposes an update, and cools down only once every proposed update is
in both current commitments and every add and removal has been forwarded by
the peer.

A _test case_ names a driver machine that sets up the scenario, and the
monitors to attach. `p check` runs a test case for a given number of
schedules, each one a different sequence of choices, and stops at the first
assertion failure or liveness violation, printing the whole execution that
led there.

## What is modeled

**`Node`** (`src/node.p`) is one side of the channel. It holds both update
logs and both commitment chains, and handles two kinds of input: commands
from its owner (add, settle, fail, fee update, sign) and messages from its
peer. Like the Go state machine, it checks each one against the ledger
first. A refused command gets an error reply; a refused peer message fails
the channel. Its states are the Go states `Synced`, `AwaitingRevocation`,
`Reestablishing` and `Failed`. `Applying` has no counterpart, because the
model handles each event atomically.

Messages are abstract. An update carries its HTLC ID. A `commitment_signed`
carries, as ghost fields, the height and log indexes of the commitment the
sender built, the point it used, and what the commitment holds: the HTLCs on
it and the fee update that sets its rate. The verifier compares all of that
with the commitment it builds, which stands in for checking a signature. A
`revoke_and_ack` carries the height whose secret it reveals.

On a restart, a node rebuilds its ledger the way lnd loads a channel from
disk (`Restore`, mirroring `Ledger.Restore`), forgets everything else, sends
its `channel_reestablish`, and waits in `Reestablishing` for the peer's. It
then answers with what the peer missed, in the order it first sent it
(`OnReestablish`, mirroring `RestoredLedger.Reestablish`).

**`World`** (`src/world.p`) runs two nodes, Alice (the initiator) and Bob,
over two in-order links. For a bounded number of steps it picks a node and
has it act: issue a command, deliver the next message on its link, or, in a
byzantine run, have Bob send Alice a message he shouldn't. In a reconnecting
run it may disconnect, which drops every message in flight and restarts both
nodes. At the end it drains both links and has both nodes sign until
nothing is owed, which is when the liveness monitor must be cold.

Bob's injections cover the ways a peer can break the protocol with one
message: a premature or replayed `revoke_and_ack`, an add with the wrong ID,
a settle of an unknown HTLC or of one that isn't committed yet, a fee update
from the non-initiator, a malformed fail without the BADONION bit, and a
`commitment_signed` for the wrong commitment.

**The monitors** (`src/spec.p`):

| Monitor | Property |
|---|---|
| `ForwardOnlyLockedIn` | A node forwards a peer update only once both current commitments include it |
| `ForwardAtMostOnce` | A node forwards each peer update at most once |
| `RemoveOnlyLockedIn` | A node proposes removing an HTLC only once it is irrevocably committed |
| `CommitmentsAgree` | Every `commitment_signed` an honest node sends signs the commitment the peer builds |
| `RevocationNeedsPending` | A `revoke_and_ack` with nothing pending is refused without changing anything |
| `RefusalChangesNothing` | A peer message the ledger refuses fails the channel without changing it |
| `HonestNeverFails` | In an honest run, disconnections included, no node fails the channel |
| `EventuallyLockedInAndForwarded` | Every proposed update is eventually locked in and forwarded (liveness) |

A restart loses the updates no persisted commitment covered, which the
owner never had to see through. The liveness monitor forgets those (`Drop`),
so it asks only that the updates a restart kept are eventually locked in.

## Green cases and counterexamples

There are two kinds of test case. A green case runs the production node and
must find no bug. A counterexample turns on one profile flag, which removes
one rule from the node, and must fail with the assertion that rule exists to
satisfy. `check.sh` fails if a counterexample passes, and checks that each
one fails with its named message, not with some unrelated error.

| Case | Kind | What it shows |
|---|---|---|
| `tcHonest` | green | Every monitor, liveness included, with no disconnection |
| `tcReconnect` | green | Every monitor across up to three disconnections and restarts |
| `tcByzantine` | green | Every safety monitor against any of Bob's injections |
| `tcPrematureRevocation` | green | The premature revocation, in every run that can reach it |
| `tcLegacyRevocationCounterexample` | counterexample | The old `ReceiveRevocation`, which rotates the peer's points before failing, breaks `RevocationNeedsPending` |
| `tcForwardOnCommitCounterexample` | counterexample | Forwarding once our own commitment includes an update breaks `ForwardOnlyLockedIn` |
| `tcNoFreshnessCounterexample` | counterexample | Forwarding every locked-in update on every revocation breaks `ForwardAtMostOnce` |
| `tcEarlyLocalRemovalCounterexample` | counterexample | Removing an HTLC only our commitment includes breaks `RemoveOnlyLockedIn` |
| `tcSignAllRemoteCounterexample` | counterexample | Signing peer updates we haven't acknowledged breaks `CommitmentsAgree` |
| `tcNoResendCounterexample` | counterexample | Not resending the commitment the peer missed breaks liveness |
| `tcRevokeFirstCounterexample` | counterexample | Resending the revocation first, whatever order we sent things in, breaks `HonestNeverFails` |
| `tcUnpersistedPeerAckedCounterexample` | counterexample | Losing our updates the peer acked but hasn't signed breaks `HonestNeverFails` |

The last counterexample is the channeldb bug fixed at the bottom of this
stack: `AdvanceCommitChainTail` didn't persist those updates before our
first revocation. The mirror test found the bug in the Go code. The model
shows which property it breaks, and keeps the old behavior as a
counterexample.

## What building the models taught us

The models found no bug in the Go state machine. They did show where the
first version of the model was too weak to catch what it should.

That first version of `commitment_signed` carried only the commitment's
height and log indexes. With it, the unpersisted-updates counterexample
passed: a node that lost a fee update's contents across a restart still
built a commitment with the same indexes as its peer, so the two agreed.
Real signatures cover what the commitment holds, not its indexes, so the
message now carries the commitment's contents as a ghost, along with a fee
history that survives a restart only for the updates lnd persists. With
that, the counterexample fails as it should.

## Connecting the models to the Go code

A model is only useful if it describes the code we ship. `TestPModelBridge`
in the parent package replays each node of every recorded run into the Go
state machine, under a plain `go test ./lnwallet/chanfsm/` with no P
toolchain, from the traces checked in under `traces/`.

When a bridged case runs with `-v`, each node prints one line per input and
one per output. Here is Alice restarting, having her command refused while
she waits, and receiving Bob's `channel_reestablish`:

```
alice in restart
alice out send reest next=2 tail=1
alice in cmd sign
alice out reply err
alice in msg reest next=2 tail=0
alice out send add id=1
```

The bridge feeds each `in` line to the Go machine as an event, through
`protofsm.ApplyEvents`, and completes each channel operation the machine
authorizes with the outcome the model saw. For a signature or a revocation,
the model prints whether the channel would accept it (`msg sig ok=1`). A
restart restores the Go ledger with `Ledger.Restore` and starts it in
`Connecting`. After every input, the bridge compares the messages the Go
machine sent, the updates it forwarded, whether it failed the channel and
the command's reply with the model's `out` lines, and checks every
transition against `ChannelTransitions`.

The bridge doesn't run a `LightningChannel`; the mirror and differential
tests do. It also can't see log compaction, which changes which error a
removal of a compacted HTLC gets but not whether it's refused.

## The spec

`../SPEC.md` is written like a protocol specification: 36 numbered
requirements (`CFSM-001` on) using the normative keywords of RFC 2119, with
BOLT 2 as the wire authority. Each requirement names the model lines that
state it, the Go code that implements it and the tests that check it, and a
traceability matrix marks each one verified, partially verified, or
unmodeled.

The spec is derived from the models and pinned to them.
`scripts/extract_p_model.py` builds an inventory of the model's machines,
states, events and functions, with a SHA-256 digest over the model sources.
`scripts/validate_spec.py` checks that every requirement uses a normative
keyword and has its model disposition, that every `file.p:line` citation
points at a real line, and that the digest recorded at the top of `SPEC.md`
matches the current model. So editing any `.p` file without revisiting the
spec fails `check.sh`.

## What the models don't cover

The models are checked by exploring schedules, not by proof, and each case
explores a bounded number of them with a bounded number of steps. The
properties hold for the abstraction under the schedules explored. The Lean
proofs in `../lean/` cover the forwarding properties for every sequence of
ledger operations.

Amounts, reserves, dust, fee rates and signatures are abstracted away, and
so are the data loss protection fields of `channel_reestablish`: a node
that learns it lost state fails the channel outright. The actor, its
mailbox and the channel beside the ledger aren't modeled, so the forwarding
package cross check and the ledger snapshot check are unmodeled in the
spec, and covered by the Go tests.

## Running

`check.sh` needs P 3.0.4, the .NET SDK it runs on, and Python 3:

```sh
dotnet tool install --global P --version 3.0.4
lnwallet/chanfsm/pmodel/check.sh
```

It compiles the project, runs every green case (2000 schedules each by
default) and every counterexample, records fresh traces for the bridged
cases and replays them into the Go code, and validates `../SPEC.md`. These
environment variables tune it:

| Variable | Default | Meaning |
|---|---|---|
| `SCHEDULES` | 2000 | Schedules per test case |
| `MAX_STEPS` | 5000 | Step bound per schedule |
| `TRACE_SEED` | 1 | Seed of the recorded runs |
| `TRACE_RUNS` | 200 | Schedules recorded and replayed per bridged case |
| `KEEP_TRACES` | 0 | Set to 1 to write a sample of the recorded runs into `traces/` |
| `KEPT_RUNS` | 25 | Schedules per case that `KEEP_TRACES=1` writes |

To run one case on its own and see its execution:

```sh
cd lnwallet/chanfsm/pmodel
p compile -pp chanfsm.pproj
p check PGenerated/PChecker/net8.0/ChanFSMModels.dll \
  -tc tcRevokeFirstCounterexample -s 2000 -v
```

When a case fails, `p check` prints the failing assertion and writes the
full execution under `PCheckerOutput/BugFinding/`. With `-v`, the trace
lines show up prefixed with `<PrintLog> CTRACE`.

## Changing the code or the models

When a change to the Go state machine makes the bridge fail, first decide
which side is wrong. If the code now does something BOLT 2 or lnd doesn't
allow, the bridge caught a bug. If the protocol should change, change the
model first, then refresh the traces and the spec:

  1. Edit the node model. State a new rule as a monitor that keeps its own
     bookkeeping, and add a counterexample case that removes the rule
     behind a profile flag. A known-bad variant goes in a counterexample
     case, never in a green one.
  2. If the model prints something new, teach the bridge to read it.
  3. Run `KEEP_TRACES=1 lnwallet/chanfsm/pmodel/check.sh` to refresh
     `traces/`.
  4. Update `../SPEC.md`: the affected requirements, their citations, and
     the digest at the top, which `check.sh` checks.

A few P details trip people up. Strings can't be joined with `+`, so nest
`format` calls instead. Variables must be declared before the first
statement of a function. `to` and `from` are reserved words. And `p check`
selects test cases by prefix, so no case name may be a prefix of another.

## Layout

| Path | What it is |
|---|---|
| `src/node.p` | `Node`: one side of the channel, with the profiles counterexamples use |
| `src/world.p` | `World`: the links, both owners, Bob's injections, disconnections, and the drain |
| `src/spec.p` | The monitors |
| `test/channel_test.p` | Drivers and test cases |
| `traces/` | A recorded sample of the bridged cases, replayed on every `go test` |
| `scripts/` | The spec inventory extractor and validator |
| `check.sh` | Compile, check every case, record traces, run the bridge, validate the spec |
| `../pmodel_bridge_test.go` | `TestPModelBridge` |
| `../SPEC.md` | The specification derived from the models |
