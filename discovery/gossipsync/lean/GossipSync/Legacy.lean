/-
This file states the rules for legacy replies precisely. Old lnd nodes
answered with replies that each echo the whole query, so the range checks
can't apply to them, and only the complete flag says when the stream has
ended.
-/
import GossipSync.Budget

namespace GossipSync

/-- **Legacy: no range checks.** A legacy reply is accepted whatever came
before it, as long as its encoding is known and the SCID limit holds. Note
that `a.prevLast` appears nowhere in the hypotheses. -/
theorem legacy_accepted (q : Query) (lim : Limits) (now : Nat) (a : Acc)
    (r : Reply) (w : Nat) (hl : isLegacy q r = true)
    (hw : weight r.encoding = some w)
    (hs : a.scids + r.entries.length ≤ lim.maxSCIDs) :
    add q lim now a r =
      .ok (accept lim now a r w, isDone q lim (accept lim now a r w) r) :=
  add_eq_ok.mpr ⟨Or.inl hl, w, hw, hs, rfl, rfl⟩

/-- **Legacy: only the flag or the budget ends the stream.** An accepted
legacy reply completes the stream exactly when it sets complete or spends
the budget. Every legacy reply reaches the query's last block, so this
says the coverage rule never applies to it. -/
theorem legacy_done_iff {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {r : Reply} {d : Bool} (hl : isLegacy q r = true)
    (h : add q lim now a r = .ok (a', d)) :
    d = true ↔ (a'.used ≥ lim.maxReplies ∨ r.complete = true) := by
  obtain ⟨_, _, _, _, _, rfl⟩ := add_eq_ok.mp h
  simp [isDone, hl]

/-- A legacy reply leaves the accumulator positioned at the query's last
block, since that is where it ends. -/
theorem legacy_prev {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {r : Reply} {d : Bool} (hl : isLegacy q r = true)
    (h : add q lim now a r = .ok (a', d)) : a'.prevLast = some q.last := by
  obtain ⟨_, w, _, _, rfl, _⟩ := add_eq_ok.mp h
  simp [accept, (legacy_span hl).2]

/-- A reply's range reaches at least its first block, as long as that block
fits in a `uint32`. -/
theorem first_le_last (r : Reply) (h : r.first ≤ u32Max) : r.first ≤ r.last := by
  unfold Reply.last lastBlock
  split <;> omega

/-- **Legacy: what may follow.** After a legacy reply, a non-legacy reply
is accepted only if it is the single block at the query's end, and then it
completes the stream. So a stream that mixes the formats gains nothing by
it: once a legacy reply has arrived, the only non-legacy reply left to send
is the final block. -/
theorem after_legacy {q : Query} {lim : Limits} {now : Nat} {a a' : Acc}
    {r : Reply} {d : Bool} (hp : a.prevLast = some q.last)
    (hl : isLegacy q r = false) (hu : r.first ≤ u32Max)
    (h : add q lim now a r = .ok (a', d)) :
    r.first = q.last ∧ r.last = q.last ∧ d = true := by
  obtain ⟨hadm, _, _, _, _, rfl⟩ := add_eq_ok.mp h
  rcases hadm with h' | ⟨hlo, hhi, hpr⟩
  · simp [hl] at h'
  · rw [hp] at hpr
    have := first_le_last r hu
    have hfirst : r.first = q.last := by omega
    refine ⟨hfirst, by omega, ?_⟩
    simp [isDone, hl]
    omega

/-- **Finding: a wrong-chain answer never ends.** A responder that answers
a query in one reply echoes the query's first block and block count, which
is exactly what makes a reply legacy. BOLT 7 requires the final reply to set
`sync_complete`, but if a responder clears it anyway, the accumulator treats
the reply as a legacy reply that isn't done, and waits for more. As long as the budget
isn't spent, the stream stays open, and the attempt ends on the reply
timeout. Our own responder sends exactly this reply for a query on a chain
it doesn't serve (`responder.go`), meaning "wrong chain". The Go accumulator
behaves the same way, and so did the legacy syncer. -/
theorem echo_without_complete_waits (q : Query) (lim : Limits) (now : Nat)
    (r : Reply) (hc : r.chain = q.chain) (hf : r.first = q.first)
    (hn : r.num = q.num) (hcomplete : r.complete = false)
    (he : r.encoding = 0) (hs : r.entries.length ≤ lim.maxSCIDs)
    (hb : 1 < lim.maxReplies) :
    ∃ a', add q lim now {} r = .ok (a', false) := by
  have hl : isLegacy q r = true := by simp [isLegacy, hc, hf, hn]
  have hw : weight r.encoding = some 1 := by simp [he, weight]
  refine ⟨accept lim now {} r 1, ?_⟩
  rw [legacy_accepted q lim now {} r 1 hl hw (by simpa using hs)]
  have hd : isDone q lim (accept lim now {} r 1) r = false := by
    simp [isDone, hl, hcomplete, accept]
    omega
  rw [hd]

end GossipSync
