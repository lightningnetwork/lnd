/-
This file proves which replies `add` rejects, and with which error. Each
theorem fixes a class of malformed reply and shows that `add` returns an
error for it, whatever the accumulator's state and the limits.
-/
import GossipSync.Step

namespace GossipSync

/-- A reply that starts at a height other than the query's first block
can't be legacy, since a legacy reply echoes the query's first block. -/
theorem not_legacy_of_first_ne {q : Query} {r : Reply}
    (h : r.first ≠ q.first) : isLegacy q r = false := by
  simp [isLegacy, h]

/-- A non-legacy reply that starts before the query is rejected with
`beforeQuery`. The proof just runs `add`: `simp` unfolds the definitions
and uses the hypotheses to pick the branch of each `if`. -/
theorem reject_before_query (q : Query) (lim : Limits) (now : Nat) (a : Acc)
    (r : Reply) (hl : isLegacy q r = false) (h : r.first < q.first) :
    add q lim now a r = .error .beforeQuery := by
  simp [add, checkRange, hl, h]

/-- A non-legacy reply that starts inside the query but ends after it is
rejected with `afterQuery`. -/
theorem reject_after_query (q : Query) (lim : Limits) (now : Nat) (a : Acc)
    (r : Reply) (hl : isLegacy q r = false) (h1 : q.first ≤ r.first)
    (h2 : r.last > q.last) :
    add q lim now a r = .error .afterQuery := by
  have : ¬ r.first < q.first := by omega
  simp [add, checkRange, hl, this, h2]

/-- **Rejection: outside the query.** Every non-legacy reply that reaches
outside the query, at either end, is rejected. -/
theorem reject_outside_query (q : Query) (lim : Limits) (now : Nat) (a : Acc)
    (r : Reply) (hl : isLegacy q r = false)
    (h : r.first < q.first ∨ r.last > q.last) :
    add q lim now a r = .error .beforeQuery ∨
      add q lim now a r = .error .afterQuery := by
  by_cases h1 : r.first < q.first
  · exact Or.inl (reject_before_query q lim now a r hl h1)
  · have h2 : r.last > q.last := by omega
    exact Or.inr (reject_after_query q lim now a r hl (by omega) h2)

/-- **Rejection: a first reply that starts late.** If no reply has been
accepted yet, a reply that starts after the query's first block is
rejected, as BOLT 7 requires the first reply to start at or before it. No
legacy hypothesis is needed: such a reply can't echo the query. It fails
with `afterQuery` if it also ends after the query, and with `notAtStart`
otherwise. -/
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

/-- **Rejection: out of order.** After a reply ending on block `p`, a
non-legacy reply must start on `p` or `p + 1`. One that starts anywhere
else, before `p` (out of order) or after `p + 1` (a gap), is rejected. It
fails with `gap` when it lies inside the query, and with one of the range
errors otherwise. -/
theorem reject_out_of_order (q : Query) (lim : Limits) (now : Nat) (a : Acc)
    (r : Reply) (p : Nat) (hp : a.prevLast = some p)
    (hl : isLegacy q r = false) (h : r.first ≠ p ∧ r.first ≠ p + 1) :
    add q lim now a r = .error .beforeQuery ∨
      add q lim now a r = .error .afterQuery ∨
      add q lim now a r = .error .gap := by
  by_cases h1 : r.first < q.first
  · exact Or.inl (reject_before_query q lim now a r hl h1)
  · by_cases h2 : r.last > q.last
    · exact Or.inr (Or.inl (reject_after_query q lim now a r hl (by omega) h2))
    · right; right
      simp [add, checkRange, hl, h1, h2, hp, h.1, h.2]

/-- **Rejection: unknown encoding.** A reply that passes the range stage
but uses an encoding other than plain or zlib is rejected. -/
theorem reject_encoding (q : Query) (lim : Limits) (now : Nat) (a : Acc)
    (r : Reply) (hadm : Admissible q a.prevLast r)
    (he : weight r.encoding = none) :
    add q lim now a r = .error .badEncoding := by
  have hrange : (if isLegacy q r then (Except.ok () : Except Err Unit)
      else checkRange q a.prevLast r) = .ok () := by
    rcases hadm with h | h
    · simp [h]
    · by_cases hl : isLegacy q r = true
      · simp [hl]
      · simp only [hl, Bool.false_eq_true, ite_false]
        exact (checkRange_ok_iff q a.prevLast r).mpr h
  simp only [add, hrange, he]

/-- **Rejection: too many SCIDs.** A reply that passes the range and
encoding checks but takes the stream past the SCID limit is rejected with
`tooLarge`. -/
theorem reject_too_large (q : Query) (lim : Limits) (now : Nat) (a : Acc)
    (r : Reply) (w : Nat) (hadm : Admissible q a.prevLast r)
    (he : weight r.encoding = some w)
    (hs : a.scids + r.entries.length > lim.maxSCIDs) :
    add q lim now a r = .error .tooLarge := by
  have hrange : (if isLegacy q r then (Except.ok () : Except Err Unit)
      else checkRange q a.prevLast r) = .ok () := by
    rcases hadm with h | h
    · simp [h]
    · by_cases hl : isLegacy q r = true
      · simp [hl]
      · simp only [hl, Bool.false_eq_true, ite_false]
        exact (checkRange_ok_iff q a.prevLast r).mpr h
  simp only [add, hrange, he, hs, ite_true]

end GossipSync
