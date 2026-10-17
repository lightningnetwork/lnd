/-
The ledger of `lnwallet/chanfsm/ledger.go`: one side's view of a channel's
BOLT 2 commitment protocol. It holds our update log and the peer's, and our
commitment chain and the peer's, each a tail and at most one pending
commitment. An update is abstract: its kind, the HTLC it adds or removes, and
the height of the first commitment on each chain that includes it (zero for
none). There are no amounts, scripts or signatures, which the channel checks.

`step` applies one operation and either refuses it with an error, leaving the
ledger alone, or returns the new ledger and the peer updates the operation
forwards. Only a revocation forwards anything. The Go test
`lean_diff_test.go` runs this model and `Ledger` in `ledger.go` on the same
random operation sequences and compares them after every step.
-/

namespace ChanFSM

inductive Kind where
  | add
  | settle
  | fail
  | malformed
  | fee
  deriving DecidableEq, Repr, Inhabited

/-- Whether a kind removes an HTLC. -/
def Kind.isRemoval : Kind → Bool
  | .settle | .fail | .malformed => true
  | _ => false

/-- An update log entry. `htlc` is the ID of the HTLC an add adds, or of the
HTLC a removal removes, which lives in the other log. `hL` and `hR` are the
heights of the first commitment on our chain and on the peer's that include
the entry, or zero. -/
structure Entry where
  idx : Nat
  kind : Kind
  htlc : Nat
  hL : Nat
  hR : Nat
  deriving DecidableEq, Repr, Inhabited

/-- One party's update log. `next` is the index the next entry takes and
`htlcs` the ID the next add takes. `modified` lists the adds of this log the
other party already proposed removing. -/
structure Log where
  next : Nat
  htlcs : Nat
  entries : List Entry
  modified : List Nat
  deriving Repr, Inhabited

/-- A commitment: its height, and the indexes below which it includes our
updates (`idxL`) and the peer's (`idxR`). -/
structure Commit where
  height : Nat
  idxL : Nat
  idxR : Nat
  deriving DecidableEq, Repr, Inhabited

/-- A commitment chain: the current commitment, and the next one if it was
signed but the previous one isn't revoked yet. -/
structure Chain where
  tail : Commit
  pending : Option Commit
  deriving Repr, Inhabited

def Chain.tip (c : Chain) : Commit := c.pending.getD c.tail

/-- A ledger. `ours` and `lc` are our log and chain, `theirs` and `rc` the
peer's. -/
structure Ledger where
  ours : Log
  theirs : Log
  lc : Chain
  rc : Chain
  weInitiated : Bool
  deriving Repr, Inhabited

/-- The ledger of a new channel. -/
def Ledger.init (weInitiated : Bool) : Ledger :=
  let empty : Log := ⟨0, 0, [], []⟩
  let zero : Chain := ⟨⟨0, 0, 0⟩, none⟩
  ⟨empty, empty, zero, zero, weInitiated⟩

inductive Op where
  | addOurs
  | addTheirs (id : Nat)
  | removeOurs (k : Kind) (id : Nat)
  | removeTheirs (k : Kind) (id : Nat)
  | feeOurs
  | feeTheirs
  | sign
  | recvCommit
  | revoke
  | recvRevocation
  | restart
  deriving Repr

inductive Err where
  | badId
  | unknown
  | removed
  | notCommitted
  | notInitiator
  | noWindow
  | unexpectedCommit
  | nothingToRevoke
  | unexpectedRevocation
  | notRemoval
  deriving DecidableEq, Repr

/-- A forwarded update: its kind, and the ID of the HTLC it adds or removes.
`idx` is its index in the peer's log, which identifies it. -/
structure Fwd where
  kind : Kind
  id : Nat
  idx : Nat
  deriving DecidableEq, Repr

/-- Whether our current commitment includes an entry. -/
def committedL (l : Ledger) (e : Entry) : Bool :=
  0 < e.hL && e.hL ≤ l.lc.tail.height

/-- Whether the peer's current commitment includes an entry. -/
def committedR (l : Ledger) (e : Entry) : Bool :=
  0 < e.hR && e.hR ≤ l.rc.tail.height

/-- Whether an entry is irrevocably committed: both current commitments
include it, each having revoked the one before. -/
def lockedIn (l : Ledger) (e : Entry) : Bool :=
  committedL l e && committedR l e

/-- Append an entry of the given kind to a log. -/
def Log.push (g : Log) (k : Kind) (htlc : Nat) : Log :=
  { g with next := g.next + 1,
           entries := g.entries ++ [⟨g.next, k, htlc, 0, 0⟩] }

/-- The add with the given ID, if the log holds one. -/
def Log.findAdd (g : Log) (id : Nat) : Option Entry :=
  g.entries.find? fun e => e.kind == .add && e.htlc == id

/-- Stamp an entry below `below` with our chain's height `h`, if no
commitment of ours includes it yet. -/
def stampL (h below : Nat) (e : Entry) : Entry :=
  if e.idx < below && e.hL == 0 then { e with hL := h } else e

/-- Stamp an entry below `below` with the peer's chain's height `h`, if no
commitment of the peer includes it yet. -/
def stampR (h below : Nat) (e : Entry) : Entry :=
  if e.idx < below && e.hR == 0 then { e with hR := h } else e

def Log.mapEntries (g : Log) (f : Entry → Entry) : Log :=
  { g with entries := g.entries.map f }

/-- Whether an entry is done: a removal or fee update both current
commitments include, which no unrevoked commitment needs anymore. -/
def isDone (l : Ledger) (e : Entry) : Bool :=
  e.kind != .add && lockedIn l e

/-- The HTLCs a log's done removals remove. -/
def goneOf (l : Ledger) (g : Log) : List Nat :=
  (g.entries.filter fun e => e.kind.isRemoval && lockedIn l e).map (·.htlc)

/-- Drop a log's done entries, and the adds the other log's done removals
remove. -/
def Log.compact (l : Ledger) (g : Log) (gone : List Nat) : Log :=
  { g with
    entries := (g.entries.filter fun e => !isDone l e).filter
      fun e => !(e.kind == .add && gone.contains e.htlc),
    modified := g.modified.filter fun i => !gone.contains i }

/-- Compaction, as lnd's `compactLogs` does it after a revocation. -/
def compact (l : Ledger) : Ledger :=
  { l with ours := l.ours.compact l (goneOf l l.theirs),
           theirs := l.theirs.compact l (goneOf l l.ours) }

/-- Whether an entry of the peer's log is forwarded by the revocation that
makes `h` the peer's current height: it is not a fee update, `h` is the
first of the peer's commitments to include it, and our current commitment
includes it. -/
def fresh (l : Ledger) (h : Nat) (e : Entry) : Bool :=
  e.kind != .fee && e.hR == h && committedL l e

def toFwd (e : Entry) : Fwd := ⟨e.kind, e.htlc, e.idx⟩

/-- The error of a removal of HTLC `id` from log `g`, by us if `ours`. Our
removals need the HTLC irrevocably committed; the peer's need it in our
current commitment. These are BOLT 2's sender and receiver rules. -/
def removalErr (l : Ledger) (g : Log) (ours : Bool) (id : Nat) :
    Option Err :=
  match g.findAdd id with
  | none => some .unknown
  | some e =>
    if g.modified.contains id then some .removed
    else if ours && !lockedIn l e then some .notCommitted
    else if !ours && !committedL l e then some .notCommitted
    else none

/-- Whether a log's newest fee update is in no commitment yet, in which case
a new fee update replaces it rather than taking a new entry. -/
def Log.feeCoalesces (g : Log) : Bool :=
  match g.entries.reverse.find? (·.kind == .fee) with
  | some e => e.hL == 0 && e.hR == 0
  | none => false

/-- The ledger after we sign a new commitment for the peer: all of our
updates, and the peer's updates our current commitment includes. -/
def signL (l : Ledger) : Ledger :=
  let c : Commit := ⟨l.rc.tail.height + 1, l.ours.next, l.lc.tail.idxR⟩
  { l with
    ours := l.ours.mapEntries (stampR c.height c.idxL),
    theirs := l.theirs.mapEntries (stampR c.height c.idxR),
    rc := ⟨l.rc.tail, some c⟩ }

/-- The ledger after the peer signs a new commitment for us: all of its
updates, and our updates its current commitment includes. -/
def recvCommitL (l : Ledger) : Ledger :=
  let c : Commit := ⟨l.lc.tail.height + 1, l.rc.tail.idxL, l.theirs.next⟩
  { l with
    ours := l.ours.mapEntries (stampL c.height c.idxL),
    theirs := l.theirs.mapEntries (stampL c.height c.idxR),
    lc := ⟨l.lc.tail, some c⟩ }

/-- The ledger once the peer's commitment `c` becomes its current one. -/
def advance (l : Ledger) (c : Commit) : Ledger :=
  { l with rc := ⟨c, none⟩ }

/-- The updates the revocation making `c` the peer's current commitment
forwards, in log order. -/
def forwards (l : Ledger) (c : Commit) : List Fwd :=
  ((advance l c).theirs.entries.filter (fresh (advance l c) c.height)).map
    toFwd

/-- The peer's log after it proposes an add, a removal or a fee update. -/
def pushTheirs (l : Ledger) (k : Kind) (htlc : Nat) : Ledger :=
  { l with theirs := l.theirs.push k htlc }

/-- Whether a log holds a removal of HTLC `id` that a commitment includes,
by the given test of inclusion. -/
def Log.removes (g : Log) (onCommit : Entry → Bool) (id : Nat) : Bool :=
  g.entries.any fun r => r.kind.isRemoval && r.htlc == id && onCommit r

/-- The ID the next add of a log whose next index is `n` takes: that of its
first add at or above `n`, or its counter if there is none. -/
def Log.htlcsBelow (g : Log) (n : Nat) : Nat :=
  ((g.entries.find? fun e => e.kind == .add && n ≤ e.idx).map (·.htlc)).getD
    g.htlcs

/-- The HTLCs the removals among some entries remove. -/
def removedIds (es : List Entry) : List Nat :=
  (es.filter (·.kind.isRemoval)).map (·.htlc)

/-- Whether a restart keeps a peer entry: an add on our current commitment
that no removal of ours there removes, or a removal or fee update our
current commitment includes and the peer's lacks. -/
def keepTheirs (l : Ledger) (e : Entry) : Bool :=
  committedL l e &&
    if e.kind == .add then !l.ours.removes (committedL l) e.htlc
    else !committedR l e

/-- Whether a restart keeps one of our entries: an add on the peer's current
commitment that no removal of the peer's there removes, anything the
pending commitment of the peer's added, or a removal or fee update the
peer's current commitment includes and ours lacks. -/
def keepOurs (l : Ledger) (e : Entry) : Bool :=
  (e.kind == .add && committedR l e &&
    !l.theirs.removes (committedR l) e.htlc) ||
  (!committedR l e && 0 < e.hR) ||
  (e.kind != .add && committedR l e && !committedL l e)

/-- A kept entry's heights, rebuilt from the commitments that include it:
the current height of each chain, or the pending one of the peer's. -/
def rebuild (l : Ledger) (e : Entry) : Entry :=
  { e with hL := if committedL l e then l.lc.tail.height else 0,
           hR := if committedR l e then l.rc.tail.height else e.hR }

/-- The ledger lnd loads from disk, as `Ledger.Restore` in `restore.go`
rebuilds it: our current commitment and none pending, the peer's chain as
it was, and the entries lnd persists, with their heights rebuilt. Each
log's next index falls back to the newest commitment that includes it.
The Go code decides what to keep by log index; this model decides by
height, which the differential test checks is the same. -/
def restore (l : Ledger) : Ledger :=
  let theirs := (l.theirs.entries.filter (keepTheirs l)).map (rebuild l)
  let ours := (l.ours.entries.filter (keepOurs l)).map (rebuild l)
  { l with
    ours := ⟨l.rc.tip.idxL, l.ours.htlcsBelow l.rc.tip.idxL, ours,
      removedIds theirs⟩,
    theirs := ⟨l.lc.tail.idxR, l.theirs.htlcsBelow l.lc.tail.idxR, theirs,
      removedIds ours⟩,
    lc := ⟨l.lc.tail, none⟩ }

/-- Apply one operation. -/
def step (l : Ledger) : Op → Except Err (Ledger × List Fwd)
  | .addOurs =>
    .ok ({ l with ours := { l.ours.push .add l.ours.htlcs with
      htlcs := l.ours.htlcs + 1 } }, [])
  | .addTheirs id =>
    if id != l.theirs.htlcs then .error .badId
    else .ok ({ pushTheirs l .add id with
      theirs := { (pushTheirs l .add id).theirs with
        htlcs := l.theirs.htlcs + 1 } }, [])
  | .removeOurs k id =>
    if !k.isRemoval then .error .notRemoval
    else match removalErr l l.theirs true id with
      | some err => .error err
      | none => .ok ({ l with
          ours := l.ours.push k id,
          theirs := { l.theirs with modified := id :: l.theirs.modified } },
          [])
  | .removeTheirs k id =>
    if !k.isRemoval then .error .notRemoval
    else match removalErr l l.ours false id with
      | some err => .error err
      | none => .ok ({ pushTheirs l k id with
          ours := { l.ours with modified := id :: l.ours.modified } }, [])
  | .feeOurs =>
    if !l.weInitiated then .error .notInitiator
    else if l.ours.feeCoalesces then .ok (l, [])
    else .ok ({ l with ours := l.ours.push .fee 0 }, [])
  | .feeTheirs =>
    if l.weInitiated then .error .notInitiator
    else if l.theirs.feeCoalesces then .ok (l, [])
    else .ok (pushTheirs l .fee 0, [])
  | .sign =>
    match l.rc.pending with
    | some _ => .error .noWindow
    | none => .ok (signL l, [])
  | .recvCommit =>
    match l.lc.pending with
    | some _ => .error .unexpectedCommit
    | none => .ok (recvCommitL l, [])
  | .revoke =>
    match l.lc.pending with
    | none => .error .nothingToRevoke
    | some c => .ok ({ l with lc := ⟨c, none⟩ }, [])
  | .recvRevocation =>
    match l.rc.pending with
    | none => .error .unexpectedRevocation
    | some c => .ok (compact (advance l c), forwards l c)
  | .restart => .ok (restore l, [])

end ChanFSM
