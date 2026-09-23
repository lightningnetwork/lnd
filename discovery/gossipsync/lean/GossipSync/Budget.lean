/-
This file proves how the reply budget is charged and what it bounds.
-/
import GossipSync.Soundness

namespace GossipSync

/-- The only encodings with a weight are plain, at one unit, and zlib, at
four. `match e with` on a `Nat` splits into the cases `0`, `1` and
`n + 2`, which covers every number. -/
theorem weight_cases {e w : Nat} (h : weight e = some w) :
    (e = 0 ∧ w = 1) ∨ (e = 1 ∧ w = 4) := by
  match e, h with
  | 0, h => simp [weight] at h; omega
  | 1, h => simp [weight] at h; omega
  | n + 2, h => simp [weight] at h

/-- An accepted reply is charged its weight, and nothing else changes the
budget. -/
theorem add_used {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {r : Reply} {d : Bool} (h : add q lim now a r = .ok (a', d)) :
    ∃ w, weight r.encoding = some w ∧ a'.used = a.used + w := by
  obtain ⟨_, w, hw, _, rfl, _⟩ := add_eq_ok.mp h
  exact ⟨w, hw, rfl⟩

/-- **Budget: zlib costs four.** An accepted zlib reply spends four units
of the budget, and an accepted plain reply spends one (GSS-015). -/
theorem add_used_by_encoding {q : Query} {lim : Limits} {now : Nat}
    {a a' : Acc} {r : Reply} {d : Bool} (h : add q lim now a r = .ok (a', d)) :
    (r.encoding = 0 ∧ a'.used = a.used + 1) ∨
      (r.encoding = 1 ∧ a'.used = a.used + 4) := by
  obtain ⟨w, hw, hu⟩ := add_used h
  rcases weight_cases hw with ⟨he, rfl⟩ | ⟨he, rfl⟩
  · exact Or.inl ⟨he, hu⟩
  · exact Or.inr ⟨he, hu⟩

/-- A reply that doesn't end the stream leaves the budget unspent: the done
flag is set whenever the budget is reached. -/
theorem add_not_done_used {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {r : Reply} (h : add q lim now a r = .ok (a', false)) :
    a'.used < lim.maxReplies := by
  obtain ⟨_, _, _, _, _, hd⟩ := add_eq_ok.mp h
  have : isDone q lim a' r = false := hd.symm
  simp only [isDone, Bool.or_eq_false_iff, decide_eq_false_iff_not] at this
  omega

/-- The SCID counter never passes the limit. -/
theorem add_scids {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {r : Reply} {d : Bool} (h : add q lim now a r = .ok (a', d)) :
    a'.scids = a.scids + r.entries.length ∧ a'.scids ≤ lim.maxSCIDs := by
  obtain ⟨_, w, _, hs, rfl, _⟩ := add_eq_ok.mp h
  exact ⟨rfl, hs⟩

/-- Along an accepted prefix, every reply costs at least one unit, so the
budget grows by at least the number of replies. -/
theorem accepts_used {q : Query} {lim : Limits} {now : Nat} {a b : Acc}
    {rs : List Reply} (h : Accepts q lim now a rs b) :
    a.used + rs.length ≤ b.used := by
  induction h with
  | nil => simp
  | cons hadd _ ih =>
    obtain ⟨w, hw, hu⟩ := add_used hadd
    rcases weight_cases hw with ⟨_, rfl⟩ | ⟨_, rfl⟩ <;> simp at * <;> omega

/-- After a non-empty accepted prefix, the budget is still unspent. -/
theorem accepts_used_lt {q : Query} {lim : Limits} {now : Nat} {a b : Acc}
    {rs : List Reply} (h : Accepts q lim now a rs b) (hne : rs ≠ []) :
    b.used < lim.maxReplies := by
  induction h with
  | nil => simp at hne
  | cons hadd hrest ih =>
    rename_i rs'
    cases rs' with
    | nil => cases hrest; exact add_not_done_used hadd
    | cons _ _ => exact ih (by simp)

/-- **Budget.** When a stream completes from the empty accumulator, the
accumulator consumed at most `max 1 maxReplies` replies, and its budget is
below `max 1 maxReplies + 4`. The `max 1` only matters for a budget of zero,
where the first reply is still accepted and ends the stream.

The bound is not `maxReplies`, because the budget is charged after a reply
is accepted and never checked before it. A zlib reply that arrives with
fewer than four units left is still accepted, and takes the total past
`maxReplies` by up to three units (four with a budget of zero);
`budget_overshoot` below is a concrete case. What the budget does bound is
the work: every reply before the last leaves budget unspent, so the stream
ends within `max 1 maxReplies` replies. -/
theorem run_budget {q : Query} {lim : Limits} {now : Nat} {a : Acc}
    {rs rest : List Reply} (h : run q lim now {} rs = .complete a rest) :
    rs.length - rest.length ≤ max 1 lim.maxReplies ∧
      a.used < max 1 lim.maxReplies + 4 := by
  obtain ⟨pre, r, b, hrs, hacc, hlast⟩ := run_complete h
  obtain ⟨w, hw, hu⟩ := add_used hlast
  have hw4 : w ≤ 4 := by rcases weight_cases hw with ⟨_, rfl⟩ | ⟨_, rfl⟩ <;> omega
  have hlen := accepts_used hacc
  simp only at hlen
  subst hrs
  cases pre with
  | nil =>
    cases hacc
    have h0 : ({} : Acc).used = 0 := rfl
    simp at hu ⊢
    omega
  | cons x xs =>
    have hlt := accepts_used_lt hacc (by simp)
    simp at hlen ⊢
    omega

/-- The budget can end above `maxReplies`. With a budget of one unit, a
single zlib reply is accepted, completes the stream, and leaves the
accumulator at four units. `rfl` proves this by evaluating `run`: the two
sides are equal by computation, so the statement is checked the way a unit
test would be. -/
theorem budget_overshoot :
    run ⟨0, 0, 10⟩ ⟨1, 10, 0⟩ 0 {}
        [⟨0, 0, 1, false, 1, false, []⟩] =
      .complete ⟨some 0, [], 4, 0⟩ [] := by
  rfl

end GossipSync
