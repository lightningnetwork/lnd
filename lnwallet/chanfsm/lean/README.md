# Lean proofs of safe forwarding

This directory holds a Lean 4 model of the ledger in `../ledger.go` and
machine-checked proofs that it forwards peer updates safely. Building the
library checks every proof; `check.sh` also refuses unfinished proofs and
nonstandard axioms, then runs a differential test that ties the model to the
Go code.

```bash
lnwallet/chanfsm/lean/check.sh
```

The toolchain is pinned in `lean-toolchain` (Lean 4.34.0). There are no
dependencies beyond Lean's core library.

## What "safe forwarding" means here

A node hands a peer update to the switch, i.e. forwards it, when a
revocation makes it irrevocably committed. BOLT 2 defines that as both
nodes' current commitments including it, each having revoked the one before.
Forwarding an add early lets the peer take the incoming HTLC back while
we're bound on the outgoing one; forwarding one twice pays out twice; never
forwarding one strands the HTLC until it times out on chain. A revocation
that doesn't complete a commitment we signed must be refused outright: that
is the bug the old `LightningChannel.ReceiveRevocation` had, where such a
message rotated the peer's revocation points before its database write
failed.

The Go tests check these properties on the executions a random generator
produces, and the P models on the interleavings the checker explores. The
proofs check them for every sequence of ledger operations, of any length,
valid or not, with a restart anywhere in it.

## Enough Lean to read the proofs

Lean is a functional programming language whose type checker also checks
proofs. The model is ordinary functional code: a `structure` is a record, a
`def` is a function, and `step` is one big `match` over the operations, each
returning either `.error` with the reason or `.ok` with the new ledger and
the updates it forwards. `Main.lean` compiles that same code into the binary
the differential test runs.

A `theorem` is a function too. Its type is the claim, and its body is the
proof, which Lean refuses to compile unless it really proves the claim. A
claim that holds over any history is stated over an _inductive
proposition_, a rule for building evidence:

```lean
inductive Reach : Ledger → List Nat → Prop where
  | init (b : Bool) : Reach (Ledger.init b) []
  | step {l l' : Ledger} {F : List Nat} {op : Op} {fs : List Fwd} :
      Reach l F → step l op = .ok (l', fs) →
      Reach l' (F ++ fs.map (·.idx))
```

Read `Reach l F` as "some history leads from a new channel to ledger `l`,
forwarding the log indexes `F` along the way". The only ways to build that
evidence are to start from a new channel, or to extend a history by one
operation the ledger accepts, adding whatever it forwards to `F`. A restart
is one of those operations. So a theorem that takes `h : Reach l F` holds
for every history, and its proof works by induction over how `h` was built.
Here is at-most-once in full:

```lean
theorem forward_at_most_once {l : Ledger} {F : List Nat} (h : Reach l F) :
    F.Nodup :=
  (reach_inv h).f_nodup
```

`F.Nodup` says no index appears twice in `F`. The proof takes the invariant
`reach_inv` establishes for every reachable ledger, and reads off one of its
fields. The real work is in `Invariant.lean`, where `inv_step` shows each
operation preserves the invariant. Most of those proofs are tactic scripts
(the part after `by`): `simp` rewrites with definitions and known lemmas,
`omega` closes goals of integer arithmetic, `cases` and `split` take a proof
apart by the shape of its data, and `exact` supplies a term that has the
goal's type directly.

To follow a proof, read the statement and the invariant's fields first. The
tactic scripts only matter when a proof breaks, and Lean then says which
goal it couldn't close.

## Layout

| File | Contents |
|------|----------|
| `ChanFSM/Ledger.lean` | The model: logs, chains, `restore`, and `step`, one case per ledger method and one for a restart. |
| `ChanFSM/Invariant.lean` | `Inv` and `IdxInv`, and `inv_step` and `idx_step`: every accepted operation, a restart included, preserves them. |
| `ChanFSM/Forwarding.lean` | The headline theorems, below. |
| `ChanFSM/Examples.lean` | Concrete runs the kernel checks on every build. |
| `Main.lean` | The line-oriented driver `lean_diff_test.go` talks to. |
| `scripts/Axioms.lean` | Prints each headline theorem's axioms for `check.sh`. |

## The theorems

| Theorem | Statement |
|---------|-----------|
| `revocation_needs_pending` | With no commitment of ours outstanding, a revocation is refused. |
| `revocation_accepted_iff` | A revocation is accepted exactly when one is outstanding. |
| `forwards_nil`, `only_revocation_forwards` | Nothing but an accepted revocation forwards anything. |
| `forward_sound` | Every update a revocation forwards is in our current commitment, and the peer commitment the revocation makes current is the first to include it. |
| `fee_never_forwarded` | Fee updates are never forwarded. |
| `forward_at_most_once` | Over any history from a new channel, no update is forwarded twice. |
| `forward_complete` | Over any history, every irrevocably committed peer update still in the log has been forwarded. |
| `forwarded_locked_in` | Over any history, every forwarded update still in the log is irrevocably committed. |
| `forwards_are_new` | A revocation never forwards an update forwarded before. |
| `inv_restore` | A restart preserves the invariant, with the same forwarded set. |
| `restart_safe`, `restart_at_most_once`, `restart_complete` | The same guarantees from a ledger restored from disk whose history is unknown, counting the forwarding packages written before the restart. |

"Any history" is `Reach`: every sequence of operations the ledger accepts,
restarts anywhere in it included, not the ones a test draws. The operations are unconstrained: the model
accepts or refuses each on the ledger's own rules, so the theorems cover a
byzantine peer as well as an honest one.

## How the proof works

`Inv l F` relates a ledger to `F`, the log indexes of every peer update
forwarded so far, which is ghost state that neither the Go ledger nor lnd
keeps. Its central clause, `fwd_iff`, says a peer update has been forwarded
exactly when it isn't a fee update and the peer's current commitment
includes it. `hRL` makes that survive a revocation: we only sign the peer
updates our own current commitment includes, so an update is in ours before
it can be in any commitment of the peer's. So when a revocation makes the
peer's current commitment include an update for the first time, the update
is irrevocably committed, and the revocation forwards it. Heights are set
once and the peer's current height only grows, so no later revocation can
forward it again. The other clauses are bookkeeping: log indexes increase,
pending commitments sit one above the current ones, and so on.

A restart (`restore`) keeps what lnd persists and cuts the peer's log back
to our current commitment, so the peer proposes its later updates again at
the same indexes. `IdxInv` is what makes that safe: every forwarded update,
and every peer update our current commitment includes, is below that
commitment's index, so a reused index was never forwarded. Every kept peer
update is in our current commitment, and its peer height is rebuilt to the
peer's current height exactly when the peer's current commitment already
included it, so `fwd_iff` holds with the same forwarded set.

At-most-once doesn't need lnd's `isForwarded` flag: freshness, the rule that
a revocation forwards only the updates whose first including commitment it
makes current, is enough. The Go ledger has no such flag.

## How the model maps to the Go code

`step` has one case per ledger method (`AddHtlc`, `RemoveHtlc`,
`UpdateFee`, `SignCommitment`, `ReceiveCommitment`, `RevokeCommitment`,
`ReceiveRevocation`), including lnd's fee update coalescing and log
compaction, and one for `Restore`. `lean_diff_test.go` runs random operation
sequences, valid and not, through both, and requires the same accept or
refuse decision and the same forwards for every operation, the same logs
and chains after every restart, and the same at the end. `Restore` decides
what to keep by log index, as lnd does, and `restore` by height, which is
what the proofs need; the differential test is what ties the two.

The ledger in turn mirrors `LightningChannel`: `mirror_test.go` checks it
against `ProtocolSnapshot` after every step of random honest executions, and
the channel actor checks it after every channel operation when
`CheckLedger` is set. So the chain of evidence is proof about the model,
differential test from model to ledger, and differential test from ledger
to channel.

## What is not modeled

Amounts, reserves, dust, fee rates and signatures: the channel checks those,
and the state machine fails the channel if it refuses a peer update.
`channel_reestablish` itself is not modeled here: a restart is modeled as
`restore` alone, and the retransmissions that follow are ordinary
operations (the P models check the retransmission rules). A signature
`Reestablish` makes after a restart is `.sign` in the model.
