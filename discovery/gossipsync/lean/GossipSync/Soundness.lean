/-
This file proves that the accumulator is sound: a stream it accepts as
complete, without running out of budget, covers exactly the queried range,
and the channels it returns are exactly the replies' channels.
-/
import GossipSync.Step

namespace GossipSync

/-! ## Stream vocabulary -/

/-- The replies `rs` together cover every block from `lo` to `hi`: each
such height lies inside some reply's range. `∃ r ∈ rs, P r` reads "there is
an `r` in `rs` with `P r`". -/
def Covers (lo hi : Nat) (rs : List Reply) : Prop :=
  ∀ h, lo ≤ h → h ≤ hi → ∃ r ∈ rs, r.first ≤ h ∧ h ≤ r.last

/-- Each reply starts on the previous reply's last block, when a block's
channels span two replies, or on the block after it. This is a definition by
pattern matching on the shape of the list: a list with at least two elements
checks its first pair and recurses, and shorter lists are trivially
linked. `True` is the proposition that always holds. -/
def Linked : List Reply → Prop
  | r :: s :: rest => (s.first = r.last ∨ s.first = r.last + 1) ∧ Linked (s :: rest)
  | _ => True

/-- An accepted prefix of a stream: starting from `a`, the accumulator
accepts each reply of `rs` in turn without completing, and ends at `b`.

This is an inductive proposition. Its two constructors are the only ways to
build a proof of `Accepts a rs b`: the empty prefix, or one accepted reply
followed by an accepted prefix. Proving something for every `Accepts` then
means handling those two cases, which is what the `induction` tactic does
below. -/
inductive Accepts (q : Query) (lim : Limits) (now : Nat) :
    Acc → List Reply → Acc → Prop where
  | nil (a : Acc) : Accepts q lim now a [] a
  | cons {a a₁ b : Acc} {r : Reply} {rs : List Reply} :
      add q lim now a r = .ok (a₁, false) →
      Accepts q lim now a₁ rs b →
      Accepts q lim now a (r :: rs) b

/-- A completed run splits into an accepted prefix, the reply that
completed the stream, and the replies left over. The proof is by induction
on the stream: `induction rs generalizing a` asks us to prove the claim
for the empty stream and for `r :: rs`, given it for `rs` and every starting
accumulator. -/
theorem run_complete {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {rs rest : List Reply} (h : run q lim now a rs = .complete a' rest) :
    ∃ pre r b, rs = pre ++ r :: rest ∧ Accepts q lim now a pre b ∧
      add q lim now b r = .ok (a', true) := by
  induction rs generalizing a with
  | nil => simp [run] at h
  | cons r rs ih =>
    simp only [run] at h
    split at h
    · simp at h
    · rename_i a₁ hadd
      simp only [Outcome.complete.injEq] at h
      obtain ⟨rfl, rfl⟩ := h
      exact ⟨[], r, a, rfl, .nil a, hadd⟩
    · rename_i a₁ hadd
      obtain ⟨pre, r', b, hrs, hacc, hlast⟩ := ih h
      exact ⟨r :: pre, r', b, by simp [hrs], .cons hadd hacc, hlast⟩

/-! ## Facts about a single accepted reply -/

/-- A legacy reply spans exactly the query. `isLegacy` is a chain of `&&`
and `==`, and `simp` turns `isLegacy q r = true` into the three
equalities it stands for. -/
theorem legacy_span {q : Query} {r : Reply} (h : isLegacy q r = true) :
    r.first = q.first ∧ r.last = q.last := by
  simp only [isLegacy, Bool.and_eq_true, beq_iff_eq] at h
  obtain ⟨⟨_, h1⟩, h2⟩ := h
  simp [Reply.last, Query.last, h1, h2]

/-- The buffered channels grow by exactly the reply's contribution. -/
theorem add_chans {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {r : Reply} {d : Bool} (h : add q lim now a r = .ok (a', d)) :
    a'.chans = a.chans ++ received lim now r := by
  obtain ⟨_, w, _, _, rfl, _⟩ := add_eq_ok.mp h
  rfl

/-- Over an accepted prefix, the buffer is the starting buffer followed by
every reply's contribution, in order. `List.flatMap f rs` applies `f` to
each element and concatenates the results, like a Go loop of `append`. -/
theorem accepts_chans {q : Query} {lim : Limits} {now : Nat} {a b : Acc}
    {rs : List Reply} (h : Accepts q lim now a rs b) :
    b.chans = a.chans ++ rs.flatMap (received lim now) := by
  induction h with
  | nil => simp
  | cons hadd _ ih => simp [ih, add_chans hadd]

/-! ## The invariant

`Good H a` says what holds of the accumulator `a` after it has accepted the
history `H`. A `structure` whose fields are propositions is a bundle of
facts: to build one we prove each field, and from one we can read off any
field by name.
-/

/-- The invariant of an accumulator that has accepted the replies `H`. -/
structure Good (q : Query) (H : List Reply) (a : Acc) : Prop where
  prev : a.prevLast = H.getLast?.map Reply.last
  within : ∀ r ∈ H, q.first ≤ r.first ∧ r.last ≤ q.last
  head : ∀ r, H.head? = some r → r.first = q.first
  linked : (∀ r ∈ H, isLegacy q r = false) → Linked H
  covers : (∃ r ∈ H, isLegacy q r = true) ∨
    ∀ l, H.getLast? = some l → Covers q.first l.last H

/-- Appending a reply that is linked to the last one keeps a list linked.
The proof is by induction on the list, where the three cases of `match`
in `Linked` show up as the three cases of the induction. -/
theorem linked_append (H : List Reply) (r : Reply) (hH : Linked H)
    (hl : ∀ l, H.getLast? = some l → r.first = l.last ∨ r.first = l.last + 1) :
    Linked (H ++ [r]) := by
  induction H with
  | nil => simp [Linked]
  | cons x xs ih =>
    cases xs with
    | nil => simpa [Linked] using hl x rfl
    | cons y ys =>
      simp only [Linked] at hH
      simp only [List.cons_append, Linked]
      refine ⟨hH.1, ?_⟩
      have := ih hH.2 (fun l hl' => hl l (by simpa using hl'))
      simpa using this

/-- One accepted reply preserves the invariant. This is the heart of the
soundness proof; everything after it is bookkeeping over lists. -/
theorem good_step {q : Query} {lim : Limits} {now : Nat} {H : List Reply}
    {a a' : Acc} {r : Reply} {d : Bool} (hg : Good q H a)
    (hadd : add q lim now a r = .ok (a', d)) : Good q (H ++ [r]) a' := by
  obtain ⟨hadm, w, _, _, rfl, _⟩ := add_eq_ok.mp hadd
  -- `by_cases` splits the proof on whether the reply is legacy. The
  -- legacy case uses `legacy_span`; the other uses the range rules.
  by_cases hl : isLegacy q r = true
  · obtain ⟨hf, hlast⟩ := legacy_span hl
    refine ⟨by simp [accept], ?_, ?_, ?_, ?_⟩
    · intro x hx
      rcases List.mem_append.mp hx with hx | hx
      · exact hg.within x hx
      · simp at hx; subst hx; omega
    · intro x hx
      cases H with
      | nil => simp at hx; subst hx; exact hf
      | cons y ys => exact hg.head x (by simpa using hx)
    · intro hall
      have := hall r (by simp)
      simp_all
    · exact Or.inl ⟨r, by simp, hl⟩
  · have hml : isLegacy q r = false := by simpa using hl
    have hr : RangeOK q a.prevLast r := by
      rcases hadm with h | h
      · exact absurd h hl
      · exact h
    obtain ⟨hlo, hhi, hprev⟩ := hr
    refine ⟨by simp [accept], ?_, ?_, ?_, ?_⟩
    · intro x hx
      rcases List.mem_append.mp hx with hx | hx
      · exact hg.within x hx
      · simp at hx; subst hx; exact ⟨hlo, hhi⟩
    · intro x hx
      cases H with
      | nil =>
        simp at hx; subst hx
        simp [hg.prev] at hprev
        exact hprev
      | cons y ys => exact hg.head x (by simpa using hx)
    · intro hall
      apply linked_append H r (hg.linked (fun x hx => hall x (by simp [hx])))
      intro l hl'
      rw [hg.prev, hl'] at hprev
      exact hprev
    · rcases hg.covers with hleg | hcov
      · obtain ⟨x, hx, hxl⟩ := hleg
        exact Or.inl ⟨x, by simp [hx], hxl⟩
      · right
        intro l hl'
        simp at hl'
        subst hl'
        intro h hlo' hhi'
        -- Split on whether there was a previous reply. With none, this
        -- reply starts at the query's first block and covers `h` itself.
        cases hH : H.getLast? with
        | none =>
          rw [hg.prev, hH] at hprev
          exact ⟨r, by simp, by simp at hprev; omega, hhi'⟩
        | some p =>
          rw [hg.prev, hH] at hprev
          simp at hprev
          -- A height up to the previous reply's last block was already
          -- covered; a later one is covered by this reply, which starts
          -- no later than the block after the previous one.
          by_cases hle : h ≤ p.last
          · obtain ⟨x, hx, hx1, hx2⟩ := hcov p hH h hlo' hle
            exact ⟨x, by simp [hx], hx1, hx2⟩
          · exact ⟨r, by simp, by omega, hhi'⟩

/-- The invariant holds along any accepted prefix. -/
theorem good_accepts {q : Query} {lim : Limits} {now : Nat} {a b : Acc}
    {rs : List Reply} (h : Accepts q lim now a rs b) :
    ∀ H, Good q H a → Good q (H ++ rs) b := by
  induction h with
  | nil => simp
  | cons hadd _ ih =>
    intro H hg
    have := ih _ (good_step hg hadd)
    simpa using this

/-- The empty accumulator satisfies the invariant for the empty history. -/
theorem good_nil (q : Query) : Good q [] {} := by
  refine ⟨rfl, ?_, ?_, ?_, ?_⟩ <;> simp [Covers, Linked]

/-! ## Soundness -/

/-- What a complete, sound stream looks like. `pre` is every reply the
accumulator consumed, including the one that completed the stream, and `a`
is the accumulator it ended with. -/
structure SoundStream (q : Query) (lim : Limits) (now : Nat)
    (pre : List Reply) (a : Acc) : Prop where
  /-- At least one reply was consumed. -/
  nonempty : pre ≠ []
  /-- Every reply lies inside the query. -/
  within : ∀ r ∈ pre, q.first ≤ r.first ∧ r.last ≤ q.last
  /-- Together the replies cover every block of the query. -/
  covers : Covers q.first q.last pre
  /-- The first reply starts at the query's first block. -/
  starts : ∀ r, pre.head? = some r → r.first = q.first
  /-- The last reply ends at the query's last block. -/
  ends : ∀ r, pre.getLast? = some r → r.last = q.last
  /-- Without legacy replies, each reply continues from the one before. -/
  linked : (∀ r ∈ pre, isLegacy q r = false) → Linked pre
  /-- The buffer holds exactly the replies' channels, in order, after the
  freshness filter. -/
  chans : a.chans = pre.flatMap (received lim now)

/-- **Soundness.** If a stream completes, and the reply budget was not what
ended it, then the replies the accumulator consumed form a `SoundStream`:
they lie in the query, start at its first block, end at its last, cover
every block in between, are linked one to the next when none is legacy, and
the buffer holds exactly their channels.

The budget hypothesis `a.used < lim.maxReplies` is needed: a stream cut off
by the budget covers only a prefix of the query, by design (GSS-004). The
theorem holds for every query, every limit and every stream, of any
length. -/
theorem run_sound {q : Query} {lim : Limits} {now : Nat} {a : Acc}
    {rs rest : List Reply} (h : run q lim now {} rs = .complete a rest)
    (hb : a.used < lim.maxReplies) :
    ∃ pre, rs = pre ++ rest ∧ SoundStream q lim now pre a := by
  obtain ⟨pre, r, b, hrs, hacc, hlast⟩ := run_complete h
  have hg := good_step (good_accepts hacc [] (good_nil q)) hlast
  simp only [List.nil_append] at hg
  -- The last reply completed the stream, and not through the budget, so
  -- it is a legacy reply or it reaches the query's last block.
  obtain ⟨_, w, _, _, ha, hd⟩ := add_eq_ok.mp hlast
  have hdone : isDone q lim a r = true := hd.symm
  have hused : ¬ (a.used ≥ lim.maxReplies) := by omega
  have hend : r.last = q.last := by
    have hw := hg.within r (by simp)
    unfold isDone at hdone
    by_cases hl : isLegacy q r = true
    · exact (legacy_span hl).2
    · simp only [hl, Bool.false_eq_true, ite_false, Bool.or_eq_true,
        decide_eq_true_eq] at hdone
      omega
  refine ⟨pre ++ [r], by simp [hrs], ⟨by simp, hg.within, ?_, hg.head, ?_,
    hg.linked, ?_⟩⟩
  · intro h hlo hhi
    rcases hg.covers with ⟨x, hx, hxl⟩ | hcov
    · have := legacy_span hxl
      exact ⟨x, hx, by omega, by omega⟩
    · have := hcov r (by simp) h hlo (by omega)
      exact this
  · intro x hx
    simp at hx
    subst hx
    exact hend
  · rw [add_chans hlast, accepts_chans hacc]
    simp

/-! ## Freshness -/

/-- A timestamp is within the horizon when it is at most `horizon` seconds
before or after `now`. -/
def Within (lim : Limits) (now ts : Nat) : Prop :=
  now - ts ≤ lim.horizon ∧ ts - now ≤ lim.horizon

/-- **Freshness (GSS-016).** For a reply with timestamps, a channel is
buffered exactly when it is one of the reply's channels and at least one of
its two timestamps is within the horizon. Together with `run_sound`, which
says the buffer is the concatenation of every reply's `received` channels,
this means no channel whose timestamps are both stale or skewed is ever
buffered, and every other channel of a timestamped reply is. -/
theorem received_fresh (lim : Limits) (now : Nat) (r : Reply) (e : Entry)
    (hts : r.withTs = true) :
    e ∈ received lim now r ↔
      e ∈ r.entries ∧ (Within lim now e.t1 ∨ Within lim now e.t2) := by
  simp only [received, freshEntries, hts, ite_true, List.mem_filter, keep,
    outOfBounds, Within]
  constructor
  · intro ⟨hm, hk⟩
    refine ⟨hm, ?_⟩
    simp at hk
    omega
  · intro ⟨hm, hw⟩
    refine ⟨hm, ?_⟩
    simp
    omega

end GossipSync
