/-
Safe forwarding: the headline theorems.

A node hands a peer update to the switch (forwards it) when a revocation
makes it irrevocably committed. Forwarding one too early lets the peer take
the incoming HTLC back while we're bound on the outgoing one; forwarding one
twice pays out twice; never forwarding one strands it. The theorems below
rule each of these out for the ledger, for every sequence of operations, not
just the ones a test happens to draw:

* `revocation_needs_pending`: a revocation with no commitment outstanding
  is refused, which is the bug the old `ReceiveRevocation` had.
* `only_revocation_forwards` and `fee_never_forwarded`.
* `forward_sound`: every update a revocation forwards is irrevocably
  committed by it.
* `forward_at_most_once`: over any history, no update is forwarded twice.
* `forward_complete`: over any history, every irrevocably committed peer
  update still in the log has been forwarded.

A history may restart at any point, which is `Op.restart`: the ledger is
rebuilt from what lnd persists (`restore`), and the peer's log starts again
from our current commitment. So the theorems above hold across any number
of crashes, and in particular no update is forwarded again after a restart,
though its index may be proposed anew.

* `restart_safe`: a ledger restored from disk whose history we don't know,
  in a consistent state whose forwarding packages cover what its
  commitments already lock in, keeps all of the above from then on.
-/
import ChanFSM.Invariant

namespace ChanFSM

/-- A revocation that doesn't complete a commitment we signed is refused.
A refused operation returns no ledger, so it changes nothing. -/
theorem revocation_needs_pending {l : Ledger} (h : l.rc.pending = none) :
    step l .recvRevocation = .error .unexpectedRevocation := by
  simp [step, h]

/-- A revocation is accepted exactly when a commitment we signed is
outstanding. -/
theorem revocation_accepted_iff (l : Ledger) :
    (∃ r, step l .recvRevocation = .ok r) ↔ l.rc.pending.isSome := by
  cases hp : l.rc.pending <;> simp [step, hp]

/-- An operation other than a revocation forwards nothing. -/
theorem forwards_nil {l l' : Ledger} {op : Op} {fs : List Fwd}
    (hs : step l op = .ok (l', fs)) (hop : op ≠ .recvRevocation) :
    fs = [] := by
  cases op
  case recvRevocation => exact absurd rfl hop
  all_goals
    simp only [step] at hs
    (repeat' split at hs) <;> first
      | (simp only [Except.ok.injEq, Prod.mk.injEq] at hs
         exact hs.2.symm)
      | cases hs

/-- Only a revocation forwards anything, and only one that completes a
commitment we signed. -/
theorem only_revocation_forwards {l l' : Ledger} {op : Op} {fs : List Fwd}
    (hs : step l op = .ok (l', fs)) (hne : fs ≠ []) :
    op = .recvRevocation ∧ l.rc.pending.isSome := by
  by_cases hop : op = .recvRevocation
  · subst hop
    refine ⟨rfl, ?_⟩
    cases hp : l.rc.pending
    · rw [revocation_needs_pending hp] at hs
      cases hs
    · rfl
  · exact absurd (forwards_nil hs hop) hne

/-- Every update a revocation forwards is irrevocably committed by it: our
current commitment includes it, and the peer's commitment the revocation
makes current is the first of the peer's to include it. -/
theorem forward_sound {l l' : Ledger} {fs : List Fwd}
    (hs : step l .recvRevocation = .ok (l', fs)) :
    ∃ c, l.rc.pending = some c ∧ ∀ f ∈ fs, ∃ e ∈ l.theirs.entries,
      toFwd e = f ∧ e.kind ≠ .fee ∧
      (0 < e.hL ∧ e.hL ≤ l.lc.tail.height) ∧ e.hR = c.height := by
  simp only [step] at hs
  split at hs
  · cases hs
  · rename_i c hp
    simp only [Except.ok.injEq, Prod.mk.injEq] at hs
    obtain ⟨_, rfl⟩ := hs
    refine ⟨c, hp, fun f hf => ?_⟩
    obtain ⟨e, he, rfl⟩ := List.mem_map.1 hf
    obtain ⟨heL, hfr⟩ := List.mem_filter.1 he
    rw [fresh_eq] at hfr
    exact ⟨e, heL, rfl, hfr.1, hfr.2.2, hfr.2.1⟩

/-- A fee update is never forwarded. -/
theorem fee_never_forwarded {l l' : Ledger} {op : Op} {fs : List Fwd}
    (hs : step l op = .ok (l', fs)) : ∀ f ∈ fs, f.kind ≠ .fee := by
  intro f hf
  by_cases hop : op = .recvRevocation
  · subst hop
    obtain ⟨_, _, h⟩ := forward_sound hs
    obtain ⟨e, _, rfl, hk, _⟩ := h f hf
    exact hk
  · rw [forwards_nil hs hop] at hf
    cases hf

/-- The histories of a ledger: it starts empty, and each operation it
accepts, a restart included, extends the history, adding what the operation
forwards to `F`. -/
inductive Reach : Ledger → List Nat → Prop where
  | init (b : Bool) : Reach (Ledger.init b) []
  | step {l l' : Ledger} {F : List Nat} {op : Op} {fs : List Fwd} :
      Reach l F → step l op = .ok (l', fs) →
      Reach l' (F ++ fs.map (·.idx))

theorem reach_safe {l : Ledger} {F : List Nat} (h : Reach l F) :
    Inv l F ∧ IdxInv l F := by
  induction h with
  | init b => exact ⟨inv_init b, idx_init b⟩
  | step _ hs ih => exact ⟨inv_step ih.1 ih.2 hs, idx_step ih.1 ih.2 hs⟩

theorem reach_inv {l : Ledger} {F : List Nat} (h : Reach l F) : Inv l F :=
  (reach_safe h).1

/-- Over any history, no update is forwarded twice. -/
theorem forward_at_most_once {l : Ledger} {F : List Nat} (h : Reach l F) :
    F.Nodup :=
  (reach_inv h).f_nodup

/-- Over any history, every peer update in the log that is irrevocably
committed, other than a fee update, has been forwarded. -/
theorem forward_complete {l : Ledger} {F : List Nat} (h : Reach l F) :
    ∀ e ∈ l.theirs.entries, e.kind ≠ .fee → lockedIn l e = true →
      e.idx ∈ F := by
  intro e he hk hl
  rw [lockedIn_eq] at hl
  exact ((reach_inv h).fwd_iff e he).2 ⟨hk, hl.2⟩

/-- Over any history, every forwarded peer update still in the log is
irrevocably committed. -/
theorem forwarded_locked_in {l : Ledger} {F : List Nat} (h : Reach l F) :
    ∀ e ∈ l.theirs.entries, e.idx ∈ F → lockedIn l e = true := by
  intro e he hf
  have inv := reach_inv h
  obtain ⟨_, hpos, hle⟩ := (inv.fwd_iff e he).1 hf
  rw [lockedIn_eq]
  exact ⟨inv.hRL e he hpos, hpos, hle⟩

/-- A revocation never forwards an update that was already forwarded. -/
theorem forwards_are_new {l l' : Ledger} {F : List Nat} {fs : List Fwd}
    (h : Reach l F) (hs : step l .recvRevocation = .ok (l', fs)) :
    ∀ f ∈ fs, f.idx ∉ F := by
  intro f hf hin
  have hnd := (inv_step (reach_inv h) (reach_safe h).2 hs).f_nodup
  rw [List.nodup_append] at hnd
  exact hnd.2.2 f.idx hin f.idx (List.mem_map.2 ⟨f, hf, rfl⟩) rfl

/-- The forwarded set of a restored ledger: the peer updates its forwarding
packages already cover, which are the ones the peer's current commitment
includes. -/
def restoredForwards (l : Ledger) : List Nat :=
  (l.theirs.entries.filter fun e => e.kind != .fee && committedR l e).map
    (·.idx)

/-- What lnd's restore gives: no pending commitment of ours (lnd writes our
revocation to disk before it advances, so it never restores one), possibly
a pending commitment of the peer's one above its current one (the state
after a crash between our `commitment_signed` and the peer's
`revoke_and_ack`), peer entries below the log's next index in increasing
order, our current commitment including every peer update below its index,
and every peer update in any commitment of the peer's already in ours. Our
current commitment includes exactly the peer updates below its index. No
height exceeds the newest commitment of its chain. -/
structure Restored (l : Ledger) : Prop where
  lnone : l.lc.pending = none
  rpend : ∀ c, l.rc.pending = some c → c.height = l.rc.tail.height + 1
  idx_lt : ∀ e ∈ l.theirs.entries, e.idx < l.theirs.next
  sorted : (l.theirs.entries.map (·.idx)).Pairwise (· < ·)
  ltail_le : l.lc.tail.idxR ≤ l.theirs.next
  ltail : ∀ e ∈ l.theirs.entries, e.idx < l.lc.tail.idxR →
    0 < e.hL ∧ e.hL ≤ l.lc.tail.height
  hL_le : ∀ e ∈ l.theirs.entries, e.hL ≤ l.lc.tail.height
  hR_le : ∀ e ∈ l.theirs.entries, e.hR ≤ l.rc.tip.height
  hRL : ∀ e ∈ l.theirs.entries, 0 < e.hR → 0 < e.hL ∧ e.hL ≤ l.lc.tail.height
  hL_tail : ∀ e ∈ l.theirs.entries, 0 < e.hL → e.hL ≤ l.lc.tail.height →
    e.idx < l.lc.tail.idxR

/-- A restored ledger satisfies the invariant with its forwarding packages
as the forwarded set, so every theorem above holds from the restart on: no
update is forwarded again, and every update locked in later is forwarded
exactly once. -/
theorem restart_safe {l : Ledger} (h : Restored l) :
    Inv l (restoredForwards l) := by
  have hsorted := h.sorted
  refine ⟨h.idx_lt, h.sorted, ?_, ?_, ?_, ?_, h.ltail_le, h.ltail, ?_, ?_,
    h.hRL, ?_⟩
  · intro i hi
    obtain ⟨e, he, rfl⟩ := List.mem_map.1 hi
    exact h.idx_lt e (List.mem_filter.1 he).1
  · exact nodup_of_sorted (hsorted.sublist (List.filter_sublist.map _))
  · exact h.rpend
  · intro c hc; rw [h.lnone] at hc; cases hc
  · intro e he; simpa [Chain.tip, h.lnone] using h.hL_le e he
  · exact h.hR_le
  · intro e he
    unfold restoredForwards
    rw [idx_mem_filter hsorted he]
    simp

/-- A restored ledger satisfies the index facts too. -/
theorem restart_safe_idx {l : Ledger} (h : Restored l) :
    IdxInv l (restoredForwards l) := by
  refine ⟨?_, h.hL_tail, ?_⟩
  · intro i hi
    obtain ⟨e, he, rfl⟩ := List.mem_map.1 hi
    obtain ⟨heL, hk⟩ := List.mem_filter.1 he
    rw [Bool.and_eq_true, committedR_eq] at hk
    have := h.hRL e heL hk.2.1
    exact h.hL_tail e heL this.1 this.2
  · intro c hc
    rw [h.lnone] at hc
    cases hc

/-- The histories that start from a given ledger and forwarded set, such as
a restored ledger and what its forwarding packages cover. -/
inductive From (l₀ : Ledger) (F₀ : List Nat) : Ledger → List Nat → Prop where
  | here : From l₀ F₀ l₀ F₀
  | step {l l' : Ledger} {F : List Nat} {op : Op} {fs : List Fwd} :
      From l₀ F₀ l F → step l op = .ok (l', fs) →
      From l₀ F₀ l' (F ++ fs.map (·.idx))

theorem from_safe {l₀ l : Ledger} {F₀ F : List Nat} (h₀ : Inv l₀ F₀)
    (i₀ : IdxInv l₀ F₀) (h : From l₀ F₀ l F) : Inv l F ∧ IdxInv l F := by
  induction h with
  | here => exact ⟨h₀, i₀⟩
  | step _ hs ih => exact ⟨inv_step ih.1 ih.2 hs, idx_step ih.1 ih.2 hs⟩

theorem from_inv {l₀ l : Ledger} {F₀ F : List Nat} (h₀ : Inv l₀ F₀)
    (i₀ : IdxInv l₀ F₀) (h : From l₀ F₀ l F) : Inv l F :=
  (from_safe h₀ i₀ h).1

/-- After a restart, no update is forwarded twice, counting the forwarding
packages written before the restart. -/
theorem restart_at_most_once {l₀ l : Ledger} {F : List Nat}
    (hr : Restored l₀) (h : From l₀ (restoredForwards l₀) l F) : F.Nodup :=
  (from_inv (restart_safe hr) (restart_safe_idx hr) h).f_nodup

/-- After a restart, every irrevocably committed peer update still in the
log has been forwarded, before or after the restart. -/
theorem restart_complete {l₀ l : Ledger} {F : List Nat}
    (hr : Restored l₀) (h : From l₀ (restoredForwards l₀) l F) :
    ∀ e ∈ l.theirs.entries, e.kind ≠ .fee → lockedIn l e = true →
      e.idx ∈ F := by
  intro e he hk hl
  rw [lockedIn_eq] at hl
  have inv := from_inv (restart_safe hr) (restart_safe_idx hr) h
  exact (inv.fwd_iff e he).2 ⟨hk, hl.2⟩

end ChanFSM
