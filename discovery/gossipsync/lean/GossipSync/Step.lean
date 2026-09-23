/-
This file proves what one call of `add` does. Every theorem about whole
streams reduces to these facts, so they are the one place where the proofs
look inside `add`.
-/
import GossipSync.Accumulator

namespace GossipSync

/-- The range rules of `checkRange`, as a proposition rather than a
program. A `Prop` is a statement that may be true or false; unlike a
`Bool`, it need not be computable, which lets us state rules with `∀` and
`∃` elsewhere. `∧` is "and", `∨` is "or". -/
def RangeOK (q : Query) (prevLast : Option Nat) (r : Reply) : Prop :=
  q.first ≤ r.first ∧ r.last ≤ q.last ∧
    match prevLast with
    | none => r.first = q.first
    | some p => r.first = p ∨ r.first = p + 1

/-- `checkRange` accepts a reply exactly when the reply meets `RangeOK`.

`↔` is "if and only if". The proof unfolds both definitions, splits on
whether there is a previous reply (`cases`), and then on each `if` in turn
(`split`). In every branch the goal is a small fact about `<` and `≤` that
`omega`, a decision procedure for linear arithmetic over `Nat` and `Int`,
settles on its own. `simp_all` rewrites the goal and the hypotheses with
the facts in scope. -/
theorem checkRange_ok_iff (q : Query) (p : Option Nat) (r : Reply) :
    checkRange q p r = .ok () ↔ RangeOK q p r := by
  unfold checkRange RangeOK
  cases p <;> simp only <;> split <;> (try split) <;> (try split) <;>
    simp_all <;> omega

/-- Whether a reply passes the range stage of `add`: a legacy reply skips
the range checks, and any other reply must meet `RangeOK`. -/
def Admissible (q : Query) (prevLast : Option Nat) (r : Reply) : Prop :=
  isLegacy q r = true ∨ RangeOK q prevLast r

/-- The complete description of a successful `add`. The reply is
admissible, its encoding has a weight `w`, it keeps the SCID count within
the limit, and the result is the accumulator `accept` builds, with the done
flag `isDone` computes. Every other theorem about `add` succeeding follows
from this one. -/
theorem add_eq_ok {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {r : Reply} {d : Bool} :
    add q lim now a r = .ok (a', d) ↔
      Admissible q a.prevLast r ∧
        ∃ w, weight r.encoding = some w ∧
          a.scids + r.entries.length ≤ lim.maxSCIDs ∧
          a' = accept lim now a r w ∧ d = isDone q lim a' r := by
  unfold add Admissible
  by_cases hl : isLegacy q r = true
  · simp only [hl, ite_true]
    split
    · simp_all
    · rename_i w hw
      by_cases hs : a.scids + r.entries.length > lim.maxSCIDs
      · simp only [hs, ite_true]
        simp only [reduceCtorEq, false_iff]
        intro ⟨_, w', hw', hle, _⟩
        simp_all
        omega
      · simp only [hs, ite_false]
        constructor
        · intro h
          cases h
          exact ⟨by simp, w, hw, by omega, rfl, rfl⟩
        · intro ⟨_, w', hw', _, ha, hd⟩
          rw [hw] at hw'
          cases hw'
          subst ha hd
          rfl
  · have hl' : isLegacy q r = false := by simpa using hl
    simp only [hl', Bool.false_eq_true, ite_false]
    cases hc : checkRange q a.prevLast r with
    | error e =>
      have : ¬ RangeOK q a.prevLast r := by
        rw [← checkRange_ok_iff, hc]
        simp
      simp_all
    | ok u =>
      have hr : RangeOK q a.prevLast r := by
        rw [← checkRange_ok_iff, hc]
      simp only
      split
      · rename_i hw
        simp_all
      · rename_i w hw
        by_cases hs : a.scids + r.entries.length > lim.maxSCIDs
        · simp only [hs, ite_true, reduceCtorEq, false_iff]
          intro ⟨_, w', hw', hle, _⟩
          simp_all
          omega
        · simp only [hs, ite_false]
          constructor
          · intro h
            cases h
            exact ⟨Or.inr hr, w, hw, by omega, rfl, rfl⟩
          · intro ⟨_, w', hw', _, ha, hd⟩
            rw [hw] at hw'
            cases hw'
            subst ha hd
            rfl

end GossipSync
