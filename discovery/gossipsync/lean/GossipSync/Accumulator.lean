/-
This file is the executable model of the reply accumulator in
`range_reply.go`. It is plain functional code: every definition below
computes, so the same definitions that the theorems talk about are the ones
the differential test runs.

A Lean file starts with its imports. This one needs nothing beyond the core
library, which Lean loads implicitly.
-/

/-
A `namespace` works like a Go package name prefix. Inside it we can say
`Reply`, and outside it the full name is `GossipSync.Reply`.
-/
namespace GossipSync

/-! ## Heights and ranges -/

/-- The largest value a Go `uint32` holds. Heights and block counts are
`uint32` on the wire, and `LastBlockHeight` saturates at this value. -/
def u32Max : Nat := 4294967295

/-- The last block of a range that starts at `first` and spans `num` blocks.

This mirrors `LastBlockHeight` in `lnwire`, which exists on both the query
and the reply: a count of zero names the single block `first`, and the sum
saturates at `u32Max` rather than wrapping. `Nat` is Lean's type of natural
numbers, which never overflow, so `first + num - 1` here is the exact sum
that Go computes in `uint64`. -/
def lastBlock (first num : Nat) : Nat :=
  if num = 0 then first else min (first + num - 1) u32Max

/-! ## Wire records

A `structure` is Lean's struct. The `deriving` clause asks the compiler to
generate instances, which are Lean's version of Go interfaces satisfied
automatically: `Repr` lets `#eval` print a value, and `DecidableEq` gives us
a computable `==`.
-/

/-- A `query_channel_range`. The chain hash is abstracted to a number, since
the accumulator only compares it for equality. Query options are left out:
the accumulator never reads them, and the chunker takes the timestamp flag
separately. -/
structure Query where
  chain : Nat
  first : Nat
  num : Nat
  deriving Repr, DecidableEq

/-- The query's last block, as `QueryChannelRange.LastBlockHeight`. Writing
`def Query.last` puts `last` in the `Query` namespace, so for `q : Query` we
can write `q.last`, the same way Go methods work. -/
def Query.last (q : Query) : Nat := lastBlock q.first q.num

/-- One channel in a reply: its SCID and the two update timestamps. The SCID
is an opaque number, since the accumulator never decodes it. -/
structure Entry where
  scid : Nat
  t1 : Nat
  t2 : Nat
  deriving Repr, DecidableEq

/-- A `reply_channel_range`.

On the wire, the timestamps are a separate list that is either empty or has
one entry per SCID, which the wire decoder enforces. We model that as one
list of entries plus a flag saying whether the timestamps are present, so a
reply whose two lists disagree in length can't be written down at all. When
`withTs` is false the entries' timestamps are ignored. -/
structure Reply where
  chain : Nat
  first : Nat
  num : Nat
  /-- The `Complete` byte, abstracted to whether it is nonzero, which is all
  the Go code tests. -/
  complete : Bool
  /-- The encoding type byte: 0 is `EncodingSortedPlain` and 1 is
  `EncodingSortedZlib`. Any other value is an encoding we don't handle. -/
  encoding : Nat
  withTs : Bool
  entries : List Entry
  deriving Repr, DecidableEq

/-- The reply's last block, as `ReplyChannelRange.LastBlockHeight`. -/
def Reply.last (r : Reply) : Nat := lastBlock r.first r.num

/-- The limits of `RangeLimits`. The freshness horizon is a whole number of
seconds, where Go uses a `time.Duration`. -/
structure Limits where
  maxReplies : Nat
  maxSCIDs : Nat
  horizon : Nat
  deriving Repr, DecidableEq

/-! ## The accumulator -/

/-- The state of `rangeAccumulator`.

Go keeps the whole previous reply in `prev`, but only ever reads whether it
is nil and its last block, so we keep just that: `Option Nat` is `none`
before the first reply and `some h` after a reply whose last block is `h`.
The query isn't part of the state, since it never changes; every function
takes it as an argument instead.

The `:= ...` after each field is a default value, so `{}` is the empty
accumulator that `newRangeAccumulator` returns. -/
structure Acc where
  prevLast : Option Nat := none
  chans : List Entry := []
  used : Nat := 0
  scids : Nat := 0
  deriving Repr, DecidableEq

/-- Why a reply was rejected. The first five are the different
`ErrInvalidRangeReply` messages, in the order Go checks them, and the last
is `ErrRangeReplyTooLarge`. An `inductive` type with constructors that take
no arguments is Lean's enum. -/
inductive Err where
  | beforeQuery
  | afterQuery
  | notAtStart
  | gap
  | badEncoding
  | tooLarge
  deriving Repr, DecidableEq

/-- The budget a reply costs, by encoding: `Option Nat` is `none` for an
encoding we reject. This definition is by pattern matching on the number,
like a Go `switch` whose last case is `default`. -/
def weight : Nat → Option Nat
  | 0 => some 1
  | 1 => some 4
  | _ => none

/-- Whether a timestamp lies outside the freshness horizon, either in the
past or in the future.

This is where `Nat` pays off. Subtraction on `Nat` is truncated, so
`now - ts` is zero when `ts` is in the future, and `now - ts > horizon` is
then false, which is exactly what Go's signed `now.Sub(ts) > horizon` gives.
The same holds the other way round, so no signed arithmetic is needed.

`decide` turns a proposition that has a decision procedure, here `>`, into
a `Bool`. -/
def outOfBounds (now horizon ts : Nat) : Bool :=
  decide (now - ts > horizon) || decide (ts - now > horizon)

/-- Whether we keep a channel whose reply carried timestamps: it is skipped
only when both of its timestamps are out of bounds, as in
`bothOutOfBounds`. -/
def keep (lim : Limits) (now : Nat) (e : Entry) : Bool :=
  !(outOfBounds now lim.horizon e.t1 && outOfBounds now lim.horizon e.t2)

/-- The channels a list of entries contributes to the buffer. With
timestamps, the freshness filter applies. Without them, every SCID is kept
with zero timestamps, which is what `NewV1ChannelUpdateInfo` records for a
zero time. `fun e => ...` is an anonymous function, like a Go closure, and
the `{ e with t1 := 0, t2 := 0 }` syntax copies `e` with two fields
replaced, like assigning to a copy of a Go struct. -/
def freshEntries (lim : Limits) (now : Nat) (withTs : Bool)
    (es : List Entry) : List Entry :=
  if withTs then es.filter (keep lim now)
  else es.map fun e => { e with t1 := 0, t2 := 0 }

/-- The channels a reply contributes to the buffer. -/
def received (lim : Limits) (now : Nat) (r : Reply) : List Entry :=
  freshEntries lim now r.withTs r.entries

/-- Whether a reply echoes the whole query, as `isLegacyReply` does. `==`
on numbers returns a `Bool`, and `&&` is the Boolean and. -/
def isLegacy (q : Query) (r : Reply) : Bool :=
  r.chain == q.chain && r.first == q.first && r.num == q.num

/-- The range checks of `checkRange`, in Go's order. `Except Err Unit` is
Lean's version of a Go function that returns only an `error`: it is either
`.error e` or `.ok ()`, where `()` is the unit value, the one value of a
type that carries no information. -/
def checkRange (q : Query) (prevLast : Option Nat) (r : Reply) :
    Except Err Unit :=
  if r.first < q.first then .error .beforeQuery
  else if r.last > q.last then .error .afterQuery
  else
    match prevLast with
    | none => if r.first = q.first then .ok () else .error .notAtStart
    | some p => if r.first = p ∨ r.first = p + 1 then .ok () else .error .gap

/-- The accumulator after accepting `r` at weight `w`. This is the tail of
`add` once every check has passed, pulled out so the theorems can name
it. -/
def accept (lim : Limits) (now : Nat) (a : Acc) (r : Reply) (w : Nat) : Acc :=
  { prevLast := some r.last
    chans := a.chans ++ received lim now r
    used := a.used + w
    scids := a.scids + r.entries.length }

/-- Whether the stream ends with `r`, as `complete`: the budget is spent,
or a legacy reply sets complete, or a non-legacy reply reaches the query's
last block. `a'` is the accumulator after `r` was accepted, so its budget
already includes `r`. -/
def isDone (q : Query) (lim : Limits) (a' : Acc) (r : Reply) : Bool :=
  decide (a'.used ≥ lim.maxReplies) ||
    (if isLegacy q r then r.complete else decide (r.last ≥ q.last))

/-- `rangeAccumulator.add`: validate `r` and fold it into `a`, returning
the new accumulator and whether the stream is done, or the reason `r` was
rejected.

The Go method returns `(rangeAccumulator, bool, error)`. Here the result is
`Except Err (Acc × Bool)`: an error, or a pair, where `×` builds a pair
type. The checks run in Go's order: range checks for a non-legacy reply,
then the encoding, then the SCID limit. Go writes the SCID check in a
subtraction form to avoid `uint32` overflow; with `Nat` the plain sum is
exact and means the same thing. -/
def add (q : Query) (lim : Limits) (now : Nat) (a : Acc) (r : Reply) :
    Except Err (Acc × Bool) :=
  let range : Except Err Unit :=
    if isLegacy q r then .ok () else checkRange q a.prevLast r
  match range with
  | .error e => .error e
  | .ok () =>
    match weight r.encoding with
    | none => .error .badEncoding
    | some w =>
      if a.scids + r.entries.length > lim.maxSCIDs then .error .tooLarge
      else
        let a' := accept lim now a r w
        .ok (a', isDone q lim a' r)

/-! ## Running a stream

The Go accumulator is driven by `AwaitingRange.onReply`, which feeds it one
reply at a time and stops at the first error or at the first reply that
completes the stream. `run` is that driver, over a whole list of replies.
-/

/-- The result of running a stream: rejected, complete with the replies
left over after the one that completed it, or still waiting for more. -/
inductive Outcome where
  | failed (e : Err)
  | complete (a : Acc) (rest : List Reply)
  | pending (a : Acc)
  deriving Repr

/-- Feed the replies to the accumulator until one fails or completes the
stream. The definition recurses on the list, and Lean checks on its own that
the recursion terminates, since each call gets a shorter list. -/
def run (q : Query) (lim : Limits) (now : Nat) : Acc → List Reply → Outcome
  | a, [] => .pending a
  | a, r :: rs =>
    match add q lim now a r with
    | .error e => .failed e
    | .ok (a', true) => .complete a' rs
    | .ok (a', false) => run q lim now a' rs

end GossipSync
