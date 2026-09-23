# A Lean model of the range reply logic

This directory holds a model of the pure range reply logic in
`discovery/gossipsync`, written in [Lean 4](https://lean-lang.org), together
with machine-checked proofs about it. The model covers two pieces of Go: the
accumulator in `range_reply.go`, which validates the `reply_channel_range`
messages a peer sends in answer to one of our `query_channel_range` messages,
and the chunker in `range_chunk.go`, which splits our own answer to a peer's
query into replies. The proofs show that the accumulator only accepts a stream
that covers the query, that it accepts every stream our chunker sends when the
stream fits the limits, and exactly which replies it rejects. A differential
test in the parent package then runs the Go code and the Lean model on the
same random inputs and requires them to agree.

The rest of this document explains what Lean buys us over the P models next
door, gives enough Lean to read the model next to the Go code, walks through
one proof, and then lists what's proved, what turned out to be false, what's
assumed, and what isn't covered. How to run it is at the end.

## Why a proof assistant

The P models in [`../pmodel/`](../pmodel/README.md) check the syncer and the
manager by running them: the checker explores thousands of interleavings of a
small world and reports the first execution that breaks a property. That is
the right tool for concurrency, where the bugs live in the order of events.
But every run is bounded. The syncer model has four blocks and one channel per
block, so it never splits a block across two replies, never charges a zlib
reply, never hits the SCID limit, and never looks at a timestamp. `SPEC.md`
marks each of those as unmodeled. A TLA+ model checked with TLC would have the
same shape: a finite instance, searched exhaustively or at random.

A proof assistant works the other way round. A Lean theorem is a statement
about _every_ input of the types it names, here every query, every limit, every
clock reading, and every stream of any length over heights up to `2^32`. The
proof is a derivation that Lean's kernel checks step by step, so once the file
builds, there is no instance left to try. The price is that we have to write
the proof, and that the thing we prove it about must be a pure function. That
fits the range logic well: the accumulator and the chunker are pure already,
and they are exactly the parts the P model abstracts away. The two layers
split the work, with P covering who sends what when, and Lean covering what a
stream of replies means.

There is one more difference worth stating. A P model is a separate program,
and the bridge tests replay its executions against the Go code. The Lean model
is also a separate program, and nothing in Lean looks at the Go source. What
connects them is the differential test, which is evidence rather than proof:
the theorems hold of the Lean functions, and the test shows, on a few hundred
thousand random inputs, that the Go functions compute the same thing.

## Layout

| File | What it holds |
|---|---|
| `GossipSync/Accumulator.lean` | The executable model of `rangeAccumulator`: the wire records, `add`, and `run`, which drives `add` over a stream the way `onReply` does. |
| `GossipSync/Chunker.lean` | The executable model of `rangeChunker.replies`. |
| `GossipSync/Step.lean` | `add_eq_ok`, the exact description of one successful `add`. Every other proof goes through it. |
| `GossipSync/Soundness.lean` | Soundness of the accumulator, and the freshness filter. |
| `GossipSync/Rejection.lean` | Which replies are rejected, and with which error. |
| `GossipSync/Budget.lean` | How the reply budget is charged, and what it bounds. |
| `GossipSync/Legacy.lean` | The rules for legacy replies. |
| `GossipSync/RoundTrip.lean` | The chunker tiles the query, and the accumulator accepts every tiling that fits. |
| `GossipSync/Examples.lean` | Concrete streams, checked by evaluation, which read like unit tests. |
| `Main.lean` | The `gsync-lean` executable the differential test talks to. |
| `scripts/Axioms.lean` | Prints the axioms each headline theorem depends on, for `check.sh`. |

The project uses only Lean's core library: no Mathlib and no Batteries. The
proofs need lists, natural numbers, and the `omega` and `simp` tactics, all of
which ship with Lean, so a from-scratch build takes a few seconds rather than
the better part of an hour.

## Enough Lean to read the model

Lean code is a sequence of definitions. `def` defines a function or a
constant, and `structure` defines a record type, the analog of a Go struct. The
model mirrors the Go types field by field, so `Reply` has `first`, `num`,
`complete`, `encoding` and so on. Numbers are `Nat`, the natural numbers,
which never overflow; subtraction on `Nat` stops at zero, so `3 - 5 = 0`.
`Option Nat` is a value that is either `none` or `some n`, which is how the
model says "no previous reply yet". `Except Err α` is either `.error e` or
`.ok x`, which plays the role of Go's `(T, error)` pair. A leading dot, as in
`.error .gap`, lets Lean work out which type the constructor belongs to.

Functions are applied by juxtaposition, so `add q lim now a r` is Go's
`a.add(r, lim, now)` with the query passed explicitly. `match` is a `switch`
that also takes values apart, and functions over lists usually recurse on the
list: a case for `[]` and a case for `r :: rs`, the head `r` followed by the
tail `rs`. Lean checks that such recursion terminates, which is why a loop
over the stream becomes a recursive function.

Here is the Go range check next to its model. The Go code:

```go
if reply.FirstBlockHeight < a.query.FirstBlockHeight {
	return fmt.Errorf("%w: ... prior to query ...", ...)
}
if replyLast > queryLast {
	return fmt.Errorf("%w: ... after query ...", ...)
}
if a.prev == nil {
	if reply.FirstBlockHeight != a.query.FirstBlockHeight {
		return fmt.Errorf("%w: first reply starts at ...", ...)
	}
	return nil
}
prevLast := a.prev.LastBlockHeight()
if reply.FirstBlockHeight != prevLast &&
	reply.FirstBlockHeight != prevLast+1 {
	return fmt.Errorf("%w: ... does not continue ...", ...)
}
```

And the model, in `Accumulator.lean`:

```lean
def checkRange (q : Query) (prevLast : Option Nat) (r : Reply) :
    Except Err Unit :=
  if r.first < q.first then .error .beforeQuery
  else if r.last > q.last then .error .afterQuery
  else
    match prevLast with
    | none => if r.first = q.first then .ok () else .error .notAtStart
    | some p => if r.first = p ∨ r.first = p + 1 then .ok () else .error .gap
```

Each Go error message has its own constructor of `Err`, so the differential
test can check not just that both sides reject a reply, but that they reject
it for the same reason.

The proofs use a second kind of statement. A `Bool` is a value your program
computes, while a `Prop` is a proposition, a claim that may or may not hold.
`∀`, `∃`, `∧`, `∨`, `→` and `↔` build propositions, and `theorem name : P :=
proof` asks Lean to check that `proof` establishes `P`. Proofs are almost
always written in tactic mode, after `by`: each tactic transforms the current
goal, and Lean shows you the remaining goals as you go (in an editor with the
Lean extension, put the cursor after any tactic to see them).

### The abstractions

Every place the model departs from the Go code is deliberate, and the
differential test exercises each one:

The chain hash is a number, since the accumulator only compares it for
equality. The accumulator keeps `prevLast : Option Nat` instead of the whole
previous reply, because `checkRange` and `complete` only read whether `prev`
is nil and its `LastBlockHeight`. A reply's SCIDs and timestamps are one list
of `Entry` records plus a `withTs` flag, so a reply whose timestamp list is
shorter than its SCID list can't be written down; on the Go side, the wire
decoder rules it out, and `add` would panic on it.

Heights, counts and the budget are `Nat` rather than `uint32`. `lastBlock`
reproduces `LastBlockHeight`'s saturation at `2^32 - 1` explicitly, and Go's
subtraction form of the SCID check (`numSCIDs > MaxSCIDs - a.scids`) is the
plain sum `a.scids + n > maxSCIDs`, which is equivalent once overflow is gone.
The one `uint32` overflow the model doesn't reproduce is `replyBudgetUsed`
wrapping, which needs a `MaxReplies` within four of `2^32`.

Time is whole seconds. Go compares `now.Sub(ts)` against a `time.Duration`
horizon; the model compares `now - ts` and `ts - now` against a horizon in
seconds, and truncated subtraction makes the two one-sided checks come out
exactly as Go's signed ones do. That matches Go when `now` is a whole second
and every difference is below the 292 years a `Duration` can hold, which is
always the case for `uint32` timestamps and a sane clock.

The chunker's random shuffle is a parameter `fit`, which picks the channels of
an oversized block. The theorems hold for every `fit` that leaves a block
alone when it already fits, and the executable uses `take`, which is what Go
does with a shuffle that swaps nothing on a block whose SCIDs are sorted.

Finally, `run` is the driver: it feeds replies to `add` until one fails or
completes the stream, and returns the replies left over. That is what
`AwaitingRange.onReply` does, since the syncer leaves `AwaitingRange` on the
first error or completion.

## One theorem, line by line

`reject_first_late` in `Rejection.lean` says that before any reply has been
accepted, a reply that starts after the query's first block is rejected. This
is the check that stops the tail of an earlier stream from completing a new
query (GSS-013):

```lean
theorem reject_first_late (q : Query) (lim : Limits) (now : Nat) (a : Acc)
    (r : Reply) (hp : a.prevLast = none) (h : q.first < r.first) :
    add q lim now a r = .error .afterQuery ∨
      add q lim now a r = .error .notAtStart := by
  have hl := not_legacy_of_first_ne (q := q) (r := r) (by omega)
  have h1 : ¬ r.first < q.first := by omega
  have h2 : r.first ≠ q.first := by omega
  by_cases h3 : r.last > q.last
  · exact Or.inl (by simp [add, checkRange, hl, h1, h3])
  · exact Or.inr (by simp [add, checkRange, hl, h1, h3, hp, h2])
```

Read the statement first. The arguments in parentheses are universally
quantified: the theorem holds for every query `q`, limits `lim`, clock `now`,
accumulator `a` and reply `r`. The two named hypotheses are the conditions,
`hp` saying nothing has been accepted yet, and `h` saying the reply starts
late. After the colon comes the conclusion: `add` returns one of two errors.
Notice what isn't there. There is no hypothesis about the reply being
non-legacy, because the first line of the proof derives it.

Now the proof. `have hl := ...` proves an intermediate fact and names it. The
lemma `not_legacy_of_first_ne` says a reply that doesn't start at the query's
first block isn't legacy, and its one hypothesis, `r.first ≠ q.first`, is
discharged `by omega`. `omega` is a decision procedure for linear arithmetic
over `Nat` and `Int`: it looks at the hypotheses in scope, here `h`, and
closes any goal that follows from them by addition, subtraction, comparison
and multiplication by constants. The next two `have`s derive the facts that
will pick branches inside `add`.

`by_cases h3 : r.last > q.last` splits the proof in two: one branch where the
reply also ends after the query, and one where it doesn't. Each branch starts
with `·`. In the first, the goal is the left side of the `∨`, so `exact
Or.inl (...)` says "the left one holds, and here's why". The inner `simp [add,
checkRange, hl, h1, h3]` unfolds the definitions of `add` and `checkRange`,
and uses the listed facts to rewrite each `if` to the branch it takes:
`hl` resolves the legacy test, `h1` the first range check, and `h3` the second.
What is left is `.error .afterQuery = .error .afterQuery`, which `simp` closes.
The second branch is the same, except that the range checks pass and the
`match` on `a.prevLast` takes the `none` case (by `hp`), where `h2` makes the
`if` fail with `notAtStart`.

That is the whole pattern of the rejection proofs: state the condition, and
let `simp` run the code under it. The larger theorems add one more idea,
induction. `run_sound` in `Soundness.lean` defines an invariant `Good H a`,
a `structure` of five facts about an accumulator `a` that has accepted the
replies `H`, proves in `good_step` that one accepted reply preserves it, and
then extends that to a whole stream with `induction`. The `induction` tactic
asks for two proofs, one for the empty stream and one for a stream `r :: rs`
given the claim for `rs`, and Lean combines them into a proof for every
length.

## What's proved

Every theorem below is proved without `sorry`, and `check.sh` confirms each
depends only on Lean's three standard axioms. They hold for all inputs of the
stated types, with only the hypotheses shown.

**Soundness** (`run_sound`). If the accumulator completes a stream, and the
reply budget wasn't what ended it, then the replies it consumed lie inside the
query, start at the query's first block, end at its last block, and together
cover every block in between. When none of them is a legacy reply, each one
also starts on the previous reply's last block or the block after it, which is
the same-block continuation GSS-003 left unmodeled. The accumulator's buffer is
exactly the concatenation of the replies' channels, in order, after the
freshness filter. `received_fresh` pins down that filter: a channel from a
reply with timestamps is buffered exactly when at least one of its two
timestamps is within the horizon of `now` (GSS-016).

**Rejection** (`reject_*`). A non-legacy reply that starts before the query,
or ends after it, is rejected with the matching range error. Before any reply
has been accepted, a reply that starts after the query's first block is
rejected, legacy or not. After a reply ending on block `p`, a non-legacy reply
that starts anywhere but `p` or `p + 1` is rejected, which covers both going
backwards and leaving a gap. A reply with an encoding other than plain or zlib
is rejected, and so is one that takes the stream past the SCID limit (GSS-015).
`add_eq_ok` is the converse for all of them: `add` succeeds exactly when none
of these conditions holds.

**Budget** (`add_used_by_encoding`, `add_not_done_used`, `run_budget`). An
accepted plain reply costs one unit and an accepted zlib reply costs four
(GSS-015). A reply that doesn't end the stream always leaves budget unspent.
Over a whole stream, the accumulator consumes at most `max 1 MaxReplies`
replies, and its total charge stays below `max 1 MaxReplies + 4`.

**Legacy** (`legacy_accepted`, `legacy_done_iff`, `after_legacy`). A legacy
reply skips the range checks entirely, whatever came before it. It completes
the stream exactly when it sets complete or spends the budget, so the coverage
rule never applies to it, even though every legacy reply reaches the query's
last block. After a legacy reply, the only non-legacy reply the accumulator
accepts is the single block at the query's end, and that one completes the
stream (GSS-004).

**Round trip** (`roundtrip`, `roundtrip_budget_cut`, `roundtrip_too_large`,
`roundtrip_all_fit`). For any query whose first block is a `uint32` and any
blocks with strictly increasing heights inside it, the chunker's stream tiles
the query: each reply starts on the block after the previous one ends, and
only the last sets complete. Let `n` be the number of replies and `w` the
encoding's weight. The accumulator accepts the stream in full, with nothing
left over, exactly when `w * (n - 1) < MaxReplies` and the stream's SCIDs fit
the SCID limit (the "only if" half needs `MaxReplies > 0`). The buffer then holds every block's channels, in order, after
the freshness filter, except that a block too large for one reply contributes
only the channels `fit` picked. When the budget is too small, the accumulator
completes the stream early, at the first reply whose cost reaches the budget,
and ignores the rest; when the SCIDs don't fit, it rejects an honest peer's
stream with `tooLarge`. `go_length` bounds `n` by one more than the number of
blocks, so `roundtrip_all_fit` gives a condition stated purely in terms of the
graph. This is the range half of GSS-007.

## What turned out to be false

Two of the statements we set out to prove are false as first stated, and the
Go code behaves exactly as the model does in both cases.

**The budget can be exceeded.** The claim "the accumulated cost never exceeds
`MaxReplies`" is false. `add` charges a reply after accepting it and never
checks the budget first, so a zlib reply that arrives with fewer than four
units left is accepted and takes the total past the limit. The smallest case
is `budget_overshoot`: with `MaxReplies = 1`, one zlib reply leaves
`replyBudgetUsed = 4`. With the default budget of 500, a peer can send 499
plain replies and then one zlib reply, for a total of 503. The stream ends at
that reply either way, so the charge exceeds `MaxReplies` by at most three
units (four when `MaxReplies = 0`), and the budget still bounds the work:
`run_budget` proves at most `max 1 MaxReplies` replies are processed. We think this is harmless and have not
changed the Go code; the precise bound is what `run_budget` states.

**A wrong-chain answer never ends.** A responder that answers a query with
one reply naturally sets `first_blocknum` and `number_of_blocks` to the
query's, and that is exactly what `isLegacyReply` tests. BOLT 7 requires the
final reply to set `sync_complete`, so a conforming responder never clears it
on such a reply. One that does anyway is treated by our accumulator as a
legacy reply that isn't done: it waits for more, the attempt times out, and
the peer is charged with a fault. The theorem
`echo_without_complete_waits` states it: such a reply is accepted with `done =
false` whenever the budget has room for it. The Go accumulator does the same,
and so did the legacy syncer (`isLegacyReplyChannelRange` in
`discovery/syncer.go`). Our own responder triggers it: for a query on a
chain it doesn't serve, `responder.go` answers with exactly this reply, an
echo of the query with complete cleared, as the legacy responder did, and the
comment there says the cleared flag is meant to tell the initiator we're on a
different chain. An lnd initiator reads it as the first reply of an unfinished
legacy stream instead, waits out the reply timeout, and charges the peer with
a fault. We confirmed the Go accumulator returns `done = false` for that
reply. The cost is one slow, failed attempt against a peer on the wrong
chain, so we've left the Go code alone and recorded the question in
`SPEC.md`.

Two stricter-than-BOLT behaviors show up in the theorems as well, both
already deliberate: the first reply must start exactly at the query's first
block, where BOLT 7 allows it to start earlier, and the final reply must not
end after the query, where BOLT 7 allows it to overshoot. Separately, nothing
checks that a reply's SCIDs actually fall inside the reply's block range, so
soundness says the buffer holds exactly the replies' channels, not that those
channels are in the queried range.

## What's assumed

The theorems carry their assumptions as hypotheses, so nothing is hidden, but
three of them are worth naming. The round trip assumes the query's first
block is at most `2^32 - 1`, which every wire value is, and that the blocks
the graph returns have strictly increasing heights inside the query, which is
what `FilterChannelRange` provides. It also assumes one encoding for the whole
stream and a `fit` that leaves fitting blocks alone. `after_legacy` assumes
the reply's first block is a `uint32`, since without it a height of `2^32`
would saturate back into range. Soundness assumes nothing beyond completion
without the budget.

Beyond the hypotheses, the model itself is an assumption: the theorems are
about the Lean functions, and they say something about lnd only to the extent
that those functions compute what the Go functions do. That is the job of the
differential test.

## What the differential test adds

`lean_diff_test.go` in the parent package starts the `gsync-lean` executable
once per test and talks to it over a line protocol on standard input and
output, described at the top of `Main.lean`. `TestLeanDiffAccumulator` uses
rapid to draw a query, limits, a clock and a reply stream, runs the Go
accumulator the way `onReply` does, and requires the model's output to match
line for line: the error kind, or the done flag, budget, SCID count and last
block after each reply, and the final buffer with its timestamps. Half the
streams are our chunker's output with at most one mutation (a shifted first
block, a longer range, a bad encoding, a duplicated or dropped reply); the
other half are adversarial, built from legacy echoes, continuations,
off-by-ones around the previous reply and the query's ends, and noise, with
heights near zero and near `2^32` where `LastBlockHeight` saturates, and
timestamps on both sides of the freshness horizon. `TestLeanDiffChunker`
draws a graph and chunker settings, and requires the Go chunker, with a
shuffle that swaps nothing, to produce the same replies as the model.

Both tests skip unless `GOSSIPSYNC_LEAN_BIN` points at the executable, so a
plain `go test` needs no Lean toolchain. `check.sh` runs each with 100,000
checks, and they agree on every one.

To see that the test has teeth, we mutated the Lean model twelve ways and
reran it with 2,000 checks each: a zlib weight of three, no first-reply check,
no same-block continuation, a strict comparison swapped for a non-strict one
(or back) in each of the budget, freshness and SCID-limit checks, a legacy
test that ignores the chain, no saturation in `lastBlock`, swapped range checks, `≤` for `<` in the
chunker's pending-reply test, no halving of the chunk size for timestamps, and
reversed chunk order. The differential test caught all twelve. Ten of them
also broke a proof, which is its own lesson: the theorems pin down the logic,
but not the constants that don't affect it. The freshness comparison and the
chunk halving can change without falsifying any theorem, since the theorems
are stated in terms of the model's own filter and limit, and only the
differential test holds those to the Go values.

## What isn't covered

The model stops at the edges of the two pure functions. The syncer state
machine around them, with its timers, draining and pairing of replies to
queries, is the P model's territory. The SCID phase after the range phase,
`FilterKnownChanIDs` and the batching of `query_short_channel_ids`, is not
modeled, so GSS-006 and the rest of GSS-007 are not proved here. The chunker's
shuffle is covered only through `fit`: the theorems hold for any choice, but
the differential test only runs `take`. Wire decoding, zlib decompression, the
`uint32` wrap of `replyBudgetUsed` near `2^32`, and sub-second clock readings
are outside the model.

Future work we'd consider worthwhile: a theorem relating the draining rule in
`syncer_states.go` to `isDone` (open question Q8 in `SPEC.md` asks whether
they should agree), a model of `FilterKnownChanIDs` and `nextBatch` to finish
GSS-007, and a check that a reply's SCIDs lie in its block range, should we
decide the accumulator ought to enforce it.

## Running it

Install [elan](https://github.com/leanprover/elan), Lean's toolchain manager:

```sh
curl https://raw.githubusercontent.com/leanprover/elan/master/elan-init.sh -sSf \
	| sh -s -- -y --default-toolchain none
```

The `lean-toolchain` file pins Lean 4.34.0, which elan downloads on the first
build. Then, from this directory:

```sh
./check.sh                  # build, check every proof, run both diff tests
CHECKS=5000 ./check.sh      # fewer rapid checks
CLEAN=1 ./check.sh          # rebuild from scratch
```

`check.sh` builds the library, which is what checks the proofs, and the
executable; refuses any `sorry` or `admit`, and any theorem depending on a
nonstandard axiom; then runs the two differential tests. A clean build takes
about three seconds on an M-series laptop, and the two tests at 100,000
checks each take about twenty seconds together.

To work on the proofs, `lake build` rebuilds only what changed, and `lake env
lean GossipSync/Soundness.lean` checks one file. In VS Code with the Lean 4
extension, opening any `.lean` file shows the goal state after each tactic,
which is the best way to follow a proof. To run the differential test by hand:

```sh
lake build
cd .. && GOSSIPSYNC_LEAN_BIN=$PWD/lean/.lake/build/bin/gsync-lean \
	go test -run TestLeanDiff . -rapid.checks=10000
```
