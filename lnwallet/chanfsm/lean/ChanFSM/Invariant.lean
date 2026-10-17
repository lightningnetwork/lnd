/-
The invariant behind safe forwarding, and the proof that every operation
preserves it.

`Inv l F` relates a ledger to `F`, the log indexes of every peer update
forwarded so far. It is ghost state: neither the Go ledger nor lnd keeps it.
The heart of it is `fwd_iff`: a peer update has been forwarded exactly when
it is not a fee update and the peer's current commitment includes it. What
makes that hold after each revocation is `hRL`, the invariant the advisor
review pointed at: a peer update gets into a commitment we sign only after
our own current commitment includes it, since we sign only the peer updates
our current commitment includes. So when the peer's current commitment
first includes an update, ours already does, and the update is irrevocably
committed.
-/
import ChanFSM.Ledger

namespace ChanFSM

/-! ## Unfolding the boolean predicates -/

@[simp] theorem committedL_eq (l : Ledger) (e : Entry) :
    committedL l e = true ↔ 0 < e.hL ∧ e.hL ≤ l.lc.tail.height := by
  simp [committedL]

@[simp] theorem committedR_eq (l : Ledger) (e : Entry) :
    committedR l e = true ↔ 0 < e.hR ∧ e.hR ≤ l.rc.tail.height := by
  simp [committedR]

@[simp] theorem lockedIn_eq (l : Ledger) (e : Entry) :
    lockedIn l e = true ↔ (0 < e.hL ∧ e.hL ≤ l.lc.tail.height) ∧
      (0 < e.hR ∧ e.hR ≤ l.rc.tail.height) := by
  simp [lockedIn]

@[simp] theorem fresh_eq (l : Ledger) (h : Nat) (e : Entry) :
    fresh l h e = true ↔ e.kind ≠ .fee ∧ e.hR = h ∧
      (0 < e.hL ∧ e.hL ≤ l.lc.tail.height) := by
  simp [fresh, and_assoc]

/-! ## Stamping changes one height and nothing else -/

@[simp] theorem stampR_idx (h b : Nat) (e : Entry) : (stampR h b e).idx = e.idx := by
  unfold stampR; split <;> rfl

@[simp] theorem stampR_kind (h b : Nat) (e : Entry) :
    (stampR h b e).kind = e.kind := by
  unfold stampR; split <;> rfl

@[simp] theorem stampR_hL (h b : Nat) (e : Entry) : (stampR h b e).hL = e.hL := by
  unfold stampR; split <;> rfl

theorem stampR_hR (h b : Nat) (e : Entry) :
    (stampR h b e).hR = if e.idx < b ∧ e.hR = 0 then h else e.hR := by
  unfold stampR; split <;> simp_all

@[simp] theorem stampL_idx (h b : Nat) (e : Entry) : (stampL h b e).idx = e.idx := by
  unfold stampL; split <;> rfl

@[simp] theorem stampL_kind (h b : Nat) (e : Entry) :
    (stampL h b e).kind = e.kind := by
  unfold stampL; split <;> rfl

@[simp] theorem stampL_hR (h b : Nat) (e : Entry) : (stampL h b e).hR = e.hR := by
  unfold stampL; split <;> rfl

theorem stampL_hL (h b : Nat) (e : Entry) :
    (stampL h b e).hL = if e.idx < b ∧ e.hL = 0 then h else e.hL := by
  unfold stampL; split <;> simp_all

theorem map_idx_stampR (h b : Nat) (L : List Entry) :
    (L.map (stampR h b)).map (·.idx) = L.map (·.idx) := by
  simp [List.map_map, Function.comp_def]

theorem map_idx_stampL (h b : Nat) (L : List Entry) :
    (L.map (stampL h b)).map (·.idx) = L.map (·.idx) := by
  simp [List.map_map, Function.comp_def]

theorem stampL_hL_ne {h b : Nat} {e : Entry} (hz : e.hL ≠ 0) :
    (stampL h b e).hL = e.hL := by
  rw [stampL_hL]; split <;> simp_all

theorem stampL_hL_zero {h b : Nat} {e : Entry} (hz : e.hL = 0)
    (hb : e.idx < b) : (stampL h b e).hL = h := by
  rw [stampL_hL]; simp [hz, hb]

theorem stampL_hL_cases (h b : Nat) (e : Entry) :
    (stampL h b e).hL = e.hL ∨ ((stampL h b e).hL = h ∧ e.hL = 0) := by
  rw [stampL_hL]; split
  · rename_i hc; exact Or.inr ⟨rfl, hc.2⟩
  · exact Or.inl rfl

theorem stampR_hR_cases (h b : Nat) (e : Entry) :
    (stampR h b e).hR = e.hR ∨
      ((stampR h b e).hR = h ∧ e.hR = 0 ∧ e.idx < b) := by
  rw [stampR_hR]; split
  · rename_i hc; exact Or.inr ⟨rfl, hc.2, hc.1⟩
  · exact Or.inl rfl

/-! ## Entries are identified by their index -/

/-- In a log whose indexes increase, two entries with the same index are the
same entry. -/
theorem idx_inj : ∀ {L : List Entry},
    (L.map (·.idx)).Pairwise (· < ·) →
    ∀ {e e' : Entry}, e ∈ L → e' ∈ L → e.idx = e'.idx → e = e'
  | [], _, _, _, he, _, _ => by simp at he
  | a :: L, hs, e, e', he, he', hi => by
    simp only [List.map_cons, List.pairwise_cons, List.mem_map] at hs
    obtain ⟨hlt, hs⟩ := hs
    simp only [List.mem_cons] at he he'
    rcases he with rfl | he <;> rcases he' with rfl | he'
    · rfl
    · exact absurd hi (Nat.ne_of_lt (hlt _ ⟨e', he', rfl⟩))
    · exact absurd hi.symm (Nat.ne_of_lt (hlt _ ⟨e, he, rfl⟩))
    · exact idx_inj hs he he' hi

/-- An entry's index is among the indexes of the entries a filter keeps
exactly when the filter keeps the entry. -/
theorem idx_mem_filter {L : List Entry} {p : Entry → Bool}
    (hs : (L.map (·.idx)).Pairwise (· < ·)) {e : Entry} (he : e ∈ L) :
    e.idx ∈ (L.filter p).map (·.idx) ↔ p e = true := by
  constructor
  · intro hm
    obtain ⟨e', he', hi⟩ := List.mem_map.1 hm
    obtain ⟨he'L, hp⟩ := List.mem_filter.1 he'
    rw [idx_inj hs he he'L hi.symm]
    exact hp
  · intro hp
    exact List.mem_map.2 ⟨e, List.mem_filter.2 ⟨he, hp⟩, rfl⟩

theorem nodup_of_sorted {L : List Nat} (h : L.Pairwise (· < ·)) : L.Nodup :=
  h.imp Nat.ne_of_lt

/-! ## The invariant -/

/-- The invariant of a ledger `l` whose forwarded peer updates are `F`. -/
structure Inv (l : Ledger) (F : List Nat) : Prop where
  /-- Every peer entry is below the peer log's next index. -/
  idx_lt : ∀ e ∈ l.theirs.entries, e.idx < l.theirs.next
  /-- The peer log's indexes increase. -/
  sorted : (l.theirs.entries.map (·.idx)).Pairwise (· < ·)
  /-- Every forwarded update was in the peer's log. -/
  f_lt : ∀ i ∈ F, i < l.theirs.next
  /-- No update was forwarded twice. -/
  f_nodup : F.Nodup
  /-- A pending commitment of the peer's is one above its current one. -/
  rpend : ∀ c, l.rc.pending = some c → c.height = l.rc.tail.height + 1
  /-- A pending commitment of ours is one above our current one, and
  includes every peer update below its index, each stamped no higher. -/
  lpend : ∀ c, l.lc.pending = some c → c.height = l.lc.tail.height + 1 ∧
    c.idxR ≤ l.theirs.next ∧
    ∀ e ∈ l.theirs.entries, e.idx < c.idxR → 0 < e.hL ∧ e.hL ≤ c.height
  /-- Our current commitment's peer index is within the peer's log. -/
  ltail_le : l.lc.tail.idxR ≤ l.theirs.next
  /-- Our current commitment includes every peer update below its index. -/
  ltail : ∀ e ∈ l.theirs.entries, e.idx < l.lc.tail.idxR →
    0 < e.hL ∧ e.hL ≤ l.lc.tail.height
  /-- No peer entry is stamped above our newest commitment. -/
  hL_le : ∀ e ∈ l.theirs.entries, e.hL ≤ l.lc.tip.height
  /-- No peer entry is stamped above the peer's newest commitment. -/
  hR_le : ∀ e ∈ l.theirs.entries, e.hR ≤ l.rc.tip.height
  /-- A peer update in any commitment of the peer's is already in our
  current commitment. -/
  hRL : ∀ e ∈ l.theirs.entries, 0 < e.hR → 0 < e.hL ∧ e.hL ≤ l.lc.tail.height
  /-- A peer update has been forwarded exactly when it is not a fee update
  and the peer's current commitment includes it. -/
  fwd_iff : ∀ e ∈ l.theirs.entries,
    (e.idx ∈ F ↔ (e.kind ≠ .fee ∧ 0 < e.hR ∧ e.hR ≤ l.rc.tail.height))

theorem inv_init (b : Bool) : Inv (Ledger.init b) [] := by
  refine ⟨?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_⟩ <;>
    simp [Ledger.init]

/-- Dropping peer entries preserves the invariant. -/
theorem Inv.sublist {l l' : Ledger} {F : List Nat} (h : Inv l F)
    (hsub : l'.theirs.entries.Sublist l.theirs.entries)
    (hn : l'.theirs.next = l.theirs.next) (hlc : l'.lc = l.lc)
    (hrc : l'.rc = l.rc) : Inv l' F := by
  have hs := fun {e} (he : e ∈ l'.theirs.entries) => hsub.subset he
  refine ⟨?_, ?_, ?_, h.f_nodup, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_⟩
  · intro e he; rw [hn]; exact h.idx_lt e (hs he)
  · exact h.sorted.sublist (hsub.map _)
  · intro i hi; rw [hn]; exact h.f_lt i hi
  · intro c hc; rw [hrc] at hc ⊢; exact h.rpend c hc
  · intro c hc
    rw [hlc] at hc ⊢; rw [hn]
    obtain ⟨h1, h2, h3⟩ := h.lpend c hc
    exact ⟨h1, h2, fun e he => h3 e (hs he)⟩
  · rw [hlc, hn]; exact h.ltail_le
  · intro e he; rw [hlc]; exact h.ltail e (hs he)
  · intro e he; rw [hlc]; exact h.hL_le e (hs he)
  · intro e he; rw [hrc]; exact h.hR_le e (hs he)
  · intro e he; rw [hlc]; exact h.hRL e (hs he)
  · intro e he; rw [hrc]; exact h.fwd_iff e (hs he)

/-- The invariant only looks at the peer's log and at both chains. -/
theorem Inv.congr {l l' : Ledger} {F : List Nat} (h : Inv l F)
    (he : l'.theirs.entries = l.theirs.entries)
    (hn : l'.theirs.next = l.theirs.next) (hlc : l'.lc = l.lc)
    (hrc : l'.rc = l.rc) : Inv l' F :=
  h.sublist (he ▸ List.Sublist.refl _) hn hlc hrc

/-- Appending a new entry, in no commitment, to the peer's log preserves
the invariant. -/
theorem Inv.push {l l' : Ledger} {F : List Nat} (h : Inv l F) (k : Kind)
    (htlc : Nat)
    (he : l'.theirs.entries =
      l.theirs.entries ++ [⟨l.theirs.next, k, htlc, 0, 0⟩])
    (hn : l'.theirs.next = l.theirs.next + 1) (hlc : l'.lc = l.lc)
    (hrc : l'.rc = l.rc) : Inv l' F := by
  have mem : ∀ e, e ∈ l'.theirs.entries →
      e ∈ l.theirs.entries ∨ e = ⟨l.theirs.next, k, htlc, 0, 0⟩ := by
    intro e hm; rw [he] at hm; simpa using hm
  refine ⟨?_, ?_, ?_, h.f_nodup, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_⟩
  · intro e hm; rw [hn]
    rcases mem e hm with hm | rfl
    · exact Nat.lt_succ_of_lt (h.idx_lt e hm)
    · exact Nat.lt_succ_self _
  · rw [he, List.map_append, List.pairwise_append]
    refine ⟨h.sorted, by simp, ?_⟩
    intro a ha b hb
    simp only [List.map_cons, List.map_nil, List.mem_singleton] at hb
    obtain ⟨e, he, rfl⟩ := List.mem_map.1 ha
    rw [hb]; exact h.idx_lt e he
  · intro i hi; rw [hn]; exact Nat.lt_succ_of_lt (h.f_lt i hi)
  · intro c hc; rw [hrc] at hc ⊢; exact h.rpend c hc
  · intro c hc
    rw [hlc] at hc ⊢; rw [hn]
    obtain ⟨h1, h2, h3⟩ := h.lpend c hc
    refine ⟨h1, Nat.le_succ_of_le h2, fun e hm hi => ?_⟩
    rcases mem e hm with hm | rfl
    · exact h3 e hm hi
    · exact absurd hi (Nat.not_lt.2 h2)
  · rw [hlc, hn]; exact Nat.le_succ_of_le h.ltail_le
  · intro e hm hi; rw [hlc] at hi ⊢
    rcases mem e hm with hm | rfl
    · exact h.ltail e hm hi
    · exact absurd hi (Nat.not_lt.2 h.ltail_le)
  · intro e hm; rw [hlc]
    rcases mem e hm with hm | rfl
    · exact h.hL_le e hm
    · exact Nat.zero_le _
  · intro e hm; rw [hrc]
    rcases mem e hm with hm | rfl
    · exact h.hR_le e hm
    · exact Nat.zero_le _
  · intro e hm hpos; rw [hlc]
    rcases mem e hm with hm | rfl
    · exact h.hRL e hm hpos
    · exact absurd hpos (Nat.lt_irrefl 0)
  · intro e hm; rw [hrc]
    rcases mem e hm with hm | rfl
    · exact h.fwd_iff e hm
    · simp only [Nat.lt_irrefl, false_and, and_false, iff_false]
      intro hi
      exact Nat.lt_irrefl _ (h.f_lt _ hi)

/-! ## Each operation preserves the invariant -/

theorem inv_sign {l : Ledger} {F : List Nat} (h : Inv l F)
    (hp : l.rc.pending = none) : Inv (signL l) F := by
  have hRtail : ∀ e ∈ l.theirs.entries, e.hR ≤ l.rc.tail.height := by
    intro e he
    have := h.hR_le e he
    simpa [Chain.tip, hp] using this
  refine ⟨?_, ?_, h.f_lt, h.f_nodup, ?_, ?_, h.ltail_le, ?_, ?_, ?_, ?_, ?_⟩
  · intro e' hm
    simp only [signL, Log.mapEntries, List.mem_map] at hm ⊢
    obtain ⟨e, he, rfl⟩ := hm
    simpa using h.idx_lt e he
  · simp only [signL, Log.mapEntries, map_idx_stampR]
    exact h.sorted
  · intro c hc
    simp only [signL, Option.some.injEq] at hc
    subst hc
    rfl
  · intro c hc
    obtain ⟨h1, h2, h3⟩ := h.lpend c hc
    refine ⟨h1, h2, fun e' hm hi => ?_⟩
    simp only [signL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampR_idx, stampR_hL] at hi ⊢
    exact h3 e he hi
  · intro e' hm hi
    simp only [signL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampR_idx, stampR_hL] at hi ⊢
    exact h.ltail e he hi
  · intro e' hm
    simp only [signL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampR_hL]
    exact h.hL_le e he
  · intro e' hm
    simp only [signL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [signL, Chain.tip, Option.getD_some]
    have := hRtail e he
    rcases stampR_hR_cases (l.rc.tail.height + 1) l.lc.tail.idxR e with
      hc | ⟨hc, _, _⟩ <;> rw [hc] <;> omega
  · intro e' hm hpos
    simp only [signL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampR_hL]
    rcases stampR_hR_cases (l.rc.tail.height + 1) l.lc.tail.idxR e with
      hc | ⟨_, _, hi⟩
    · rw [hc] at hpos
      exact h.hRL e he hpos
    · exact h.ltail e he hi
  · intro e' hm
    simp only [signL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampR_idx, stampR_kind]
    have hf := h.fwd_iff e he
    rcases stampR_hR_cases (l.rc.tail.height + 1) l.lc.tail.idxR e with
      hc | ⟨hc, hz, _⟩
    · rw [hc]; exact hf
    · rw [hc]
      rw [hz] at hf
      simp only [Nat.lt_irrefl, false_and, and_false, iff_false] at hf
      simp only [hf, false_iff, not_and, signL]
      intro _ _
      omega

theorem inv_recvCommit {l : Ledger} {F : List Nat} (h : Inv l F)
    (hp : l.lc.pending = none) : Inv (recvCommitL l) F := by
  have hLtail : ∀ e ∈ l.theirs.entries, e.hL ≤ l.lc.tail.height := by
    intro e he
    have := h.hL_le e he
    simpa [Chain.tip, hp] using this
  refine ⟨?_, ?_, h.f_lt, h.f_nodup, h.rpend, ?_, h.ltail_le, ?_, ?_, ?_, ?_,
    ?_⟩
  · intro e' hm
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at hm ⊢
    obtain ⟨e, he, rfl⟩ := hm
    simpa using h.idx_lt e he
  · simp only [recvCommitL, Log.mapEntries, map_idx_stampL]
    exact h.sorted
  · intro c hc
    simp only [recvCommitL, Option.some.injEq] at hc
    subst hc
    refine ⟨rfl, Nat.le_refl _, fun e' hm _ => ?_⟩
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    have := hLtail e he
    dsimp only
    by_cases hz : e.hL = 0
    · rw [stampL_hL_zero hz (h.idx_lt e he)]
      exact ⟨Nat.succ_pos _, Nat.le_refl _⟩
    · rw [stampL_hL_ne hz]
      exact ⟨Nat.pos_of_ne_zero hz, Nat.le_succ_of_le this⟩
  · intro e' hm hi
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampL_idx] at hi
    have ht := h.ltail e he hi
    rw [stampL_hL_ne (Nat.pos_iff_ne_zero.1 ht.1)]
    exact ht
  · intro e' hm
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [recvCommitL, Chain.tip, Option.getD_some]
    have := hLtail e he
    rcases stampL_hL_cases (l.lc.tail.height + 1) l.theirs.next e with
      hc | ⟨hc, _⟩ <;> rw [hc]
    · exact Nat.le_succ_of_le this
    · exact Nat.le_refl _
  · intro e' hm
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampL_hR]
    exact h.hR_le e he
  · intro e' hm hpos
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampL_hR] at hpos
    have hr := h.hRL e he hpos
    rw [stampL_hL_ne (Nat.pos_iff_ne_zero.1 hr.1)]
    exact hr
  · intro e' hm
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at hm
    obtain ⟨e, he, rfl⟩ := hm
    simp only [stampL_idx, stampL_kind, stampL_hR]
    exact h.fwd_iff e he

theorem inv_revoke {l : Ledger} {F : List Nat} {c : Commit} (h : Inv l F)
    (hp : l.lc.pending = some c) :
    Inv { l with lc := ⟨c, none⟩ } F := by
  obtain ⟨h1, h2, h3⟩ := h.lpend c hp
  have tip : l.lc.tip = c := by simp [Chain.tip, hp]
  refine ⟨h.idx_lt, h.sorted, h.f_lt, h.f_nodup, h.rpend, ?_, h2, h3, ?_,
    h.hR_le, ?_, h.fwd_iff⟩
  · intro c' hc'
    simp at hc'
  · intro e he
    have := h.hL_le e he
    rw [tip] at this
    simpa [Chain.tip] using this
  · intro e he hpos
    obtain ⟨a, b⟩ := h.hRL e he hpos
    refine ⟨a, ?_⟩
    show e.hL ≤ c.height
    omega

/-- The forwarded indexes are the indexes of the fresh entries. -/
theorem forwards_idx (l : Ledger) (c : Commit) :
    (forwards l c).map (·.idx) =
      ((advance l c).theirs.entries.filter
        (fresh (advance l c) c.height)).map (·.idx) := by
  simp [forwards, List.map_map, Function.comp_def, toFwd]

theorem inv_advance {l : Ledger} {F : List Nat} {c : Commit} (h : Inv l F)
    (hp : l.rc.pending = some c) :
    Inv (advance l c) (F ++ (forwards l c).map (·.idx)) := by
  have hc := h.rpend c hp
  have hRtip : ∀ e ∈ l.theirs.entries, e.hR ≤ c.height := by
    intro e he
    have := h.hR_le e he
    simpa [Chain.tip, hp] using this
  have hsorted : ((advance l c).theirs.entries.map (·.idx)).Pairwise
      (· < ·) := h.sorted
  -- A fresh entry is exactly one the new current commitment includes and
  -- the old one didn't.
  have fresh_iff : ∀ e ∈ l.theirs.entries,
      (fresh (advance l c) c.height e = true ↔
        (e.kind ≠ .fee ∧ e.hR = c.height)) := by
    intro e he
    rw [fresh_eq]
    constructor
    · exact fun ⟨a, b, _⟩ => ⟨a, b⟩
    · intro ⟨a, b⟩
      exact ⟨a, b, h.hRL e he (by omega)⟩
  have mem_fwd : ∀ e ∈ l.theirs.entries,
      (e.idx ∈ (forwards l c).map (·.idx) ↔
        (e.kind ≠ .fee ∧ e.hR = c.height)) := by
    intro e he
    rw [forwards_idx, idx_mem_filter hsorted he]
    exact fresh_iff e he
  refine ⟨h.idx_lt, h.sorted, ?_, ?_, ?_, h.lpend, h.ltail_le, h.ltail,
    h.hL_le, ?_, h.hRL, ?_⟩
  · intro i hi
    rcases List.mem_append.1 hi with hi | hi
    · exact h.f_lt i hi
    · rw [forwards_idx] at hi
      obtain ⟨e, he, rfl⟩ := List.mem_map.1 hi
      exact h.idx_lt e (List.mem_filter.1 he).1
  · rw [List.nodup_append]
    refine ⟨h.f_nodup, ?_, ?_⟩
    · rw [forwards_idx]
      exact nodup_of_sorted (hsorted.sublist (List.filter_sublist.map _))
    · intro a ha b hb hab
      subst hab
      rw [forwards_idx] at hb
      obtain ⟨e, he, hi⟩ := List.mem_map.1 hb
      obtain ⟨heL, hf⟩ := List.mem_filter.1 he
      have hR := ((fresh_iff e heL).1 hf).2
      rw [← hi] at ha
      have := ((h.fwd_iff e heL).1 ha).2.2
      omega
  · intro c' hc'
    simp [advance] at hc'
  · intro e he
    simpa [advance, Chain.tip] using hRtip e he
  · intro e he
    have hf := h.fwd_iff e he
    have hm := mem_fwd e he
    have hle := hRtip e he
    show e.idx ∈ F ++ (forwards l c).map (·.idx) ↔
      (e.kind ≠ .fee ∧ 0 < e.hR ∧ e.hR ≤ c.height)
    rw [List.mem_append, hf, hm]
    constructor
    · rintro (⟨a, b, d⟩ | ⟨a, b⟩)
      · exact ⟨a, b, by omega⟩
      · exact ⟨a, by omega, by omega⟩
    · rintro ⟨a, b, d⟩
      by_cases hr : e.hR = c.height
      · exact Or.inr ⟨a, hr⟩
      · exact Or.inl ⟨a, b, by omega⟩

theorem inv_compact {l : Ledger} {F : List Nat} (h : Inv l F) :
    Inv (compact l) F :=
  h.sublist (List.filter_sublist.trans List.filter_sublist) rfl rfl rfl

/-! ## Where the peer's entries sit against our commitments

A restart cuts the peer's log back to our current commitment, and the peer
proposes again from that index. That is only safe if nothing forwarded, and
nothing our current commitment includes, lies above it. `IdxInv` says so: it
ties the heights stamped on the peer's entries to the indexes of our
commitments, where `Inv` only ties them one way (`ltail`). -/

/-- The index facts a restart needs, for a ledger `l` whose forwarded peer
updates are `F`. -/
structure IdxInv (l : Ledger) (F : List Nat) : Prop where
  /-- Every forwarded update is below our current commitment's index. -/
  f_ltail : ∀ i ∈ F, i < l.lc.tail.idxR
  /-- A peer entry our current commitment includes is below its index. -/
  hL_tail : ∀ e ∈ l.theirs.entries, 0 < e.hL → e.hL ≤ l.lc.tail.height →
    e.idx < l.lc.tail.idxR
  /-- A pending commitment of ours includes at least what our current one
  does, and every stamped peer entry is below its index. -/
  lpend_idx : ∀ c, l.lc.pending = some c → l.lc.tail.idxR ≤ c.idxR ∧
    ∀ e ∈ l.theirs.entries, 0 < e.hL → e.idx < c.idxR

theorem idx_init (b : Bool) : IdxInv (Ledger.init b) [] :=
  ⟨by simp, by simp [Ledger.init], by simp [Ledger.init]⟩

/-- The index facts only look at our chain and at the heights and indexes
of the peer's entries: they survive any change that keeps our chain and
leaves each peer entry either unstamped by us or as it was. -/
theorem IdxInv.mono {l l' : Ledger} {F : List Nat} (hi : IdxInv l F)
    (hlc : l'.lc = l.lc)
    (hm : ∀ e' ∈ l'.theirs.entries, e'.hL = 0 ∨
      ∃ e ∈ l.theirs.entries, e.idx = e'.idx ∧ e.hL = e'.hL) :
    IdxInv l' F := by
  refine ⟨?_, ?_, ?_⟩
  · rw [hlc]; exact hi.f_ltail
  · intro e' he' hpos hle
    rw [hlc] at hle ⊢
    rcases hm e' he' with hz | ⟨e, he, hidx, hhl⟩
    · omega
    · rw [← hidx]; exact hi.hL_tail e he (by omega) (by omega)
  · intro c hc
    rw [hlc] at hc ⊢
    obtain ⟨h1, h2⟩ := hi.lpend_idx c hc
    refine ⟨h1, fun e' he' hpos => ?_⟩
    rcases hm e' he' with hz | ⟨e, he, hidx, hhl⟩
    · omega
    · rw [← hidx]; exact h2 e he (by omega)

theorem IdxInv.congr {l l' : Ledger} {F : List Nat} (hi : IdxInv l F)
    (he : l'.theirs.entries = l.theirs.entries) (hlc : l'.lc = l.lc) :
    IdxInv l' F :=
  hi.mono hlc fun e' he' => Or.inr ⟨e', he ▸ he', rfl, rfl⟩

theorem IdxInv.push {l l' : Ledger} {F : List Nat} (hi : IdxInv l F)
    (k : Kind) (htlc : Nat)
    (he : l'.theirs.entries =
      l.theirs.entries ++ [⟨l.theirs.next, k, htlc, 0, 0⟩])
    (hlc : l'.lc = l.lc) : IdxInv l' F := by
  refine hi.mono hlc fun e' he' => ?_
  rw [he, List.mem_append, List.mem_singleton] at he'
  rcases he' with he' | rfl
  · exact Or.inr ⟨e', he', rfl, rfl⟩
  · exact Or.inl rfl

theorem idx_sign {l : Ledger} {F : List Nat} (hi : IdxInv l F) :
    IdxInv (signL l) F := by
  refine hi.mono rfl fun e' he' => Or.inr ?_
  simp only [signL, Log.mapEntries, List.mem_map] at he'
  obtain ⟨e, he, rfl⟩ := he'
  exact ⟨e, he, by simp, by simp⟩

theorem idx_recvCommit {l : Ledger} {F : List Nat} (h : Inv l F)
    (hi : IdxInv l F) : IdxInv (recvCommitL l) F := by
  refine ⟨hi.f_ltail, ?_, ?_⟩
  · intro e' he' hpos hle
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at he'
    obtain ⟨e, he, rfl⟩ := he'
    simp only [recvCommitL, stampL_idx] at hle ⊢
    rcases stampL_hL_cases (l.lc.tail.height + 1) l.theirs.next e with
      hc | ⟨hc, _⟩ <;> rw [hc] at hpos hle
    · exact hi.hL_tail e he hpos hle
    · omega
  · intro c hc
    simp only [recvCommitL, Option.some.injEq] at hc
    subst hc
    refine ⟨h.ltail_le, fun e' he' _ => ?_⟩
    simp only [recvCommitL, Log.mapEntries, List.mem_map] at he'
    obtain ⟨e, he, rfl⟩ := he'
    simpa using h.idx_lt e he

theorem idx_revoke {l : Ledger} {F : List Nat} {c : Commit}
    (hi : IdxInv l F) (hp : l.lc.pending = some c) :
    IdxInv { l with lc := ⟨c, none⟩ } F := by
  obtain ⟨h1, h2⟩ := hi.lpend_idx c hp
  refine ⟨fun i hin => Nat.lt_of_lt_of_le (hi.f_ltail i hin) h1,
    fun e he hpos _ => h2 e he hpos, ?_⟩
  intro c' hc'
  simp at hc'

theorem idx_advance {l : Ledger} {F : List Nat} {c : Commit}
    (hi : IdxInv l F) :
    IdxInv (advance l c) (F ++ (forwards l c).map (·.idx)) := by
  refine ⟨?_, hi.hL_tail, hi.lpend_idx⟩
  intro i hin
  rcases List.mem_append.1 hin with hin | hin
  · exact hi.f_ltail i hin
  · rw [forwards_idx] at hin
    obtain ⟨e, he, rfl⟩ := List.mem_map.1 hin
    obtain ⟨heL, hf⟩ := List.mem_filter.1 he
    rw [fresh_eq] at hf
    exact hi.hL_tail e heL hf.2.2.1 hf.2.2.2

theorem idx_compact {l : Ledger} {F : List Nat} (hi : IdxInv l F) :
    IdxInv (compact l) F :=
  hi.mono rfl fun e' he' => Or.inr
    ⟨e', (List.filter_sublist.trans List.filter_sublist).subset he', rfl, rfl⟩

/-! ## A restart preserves the invariant -/

@[simp] theorem rebuild_idx (l : Ledger) (e : Entry) :
    (rebuild l e).idx = e.idx := rfl

@[simp] theorem rebuild_kind (l : Ledger) (e : Entry) :
    (rebuild l e).kind = e.kind := rfl

theorem keepTheirs_committedL {l : Ledger} {e : Entry}
    (h : keepTheirs l e = true) : 0 < e.hL ∧ e.hL ≤ l.lc.tail.height := by
  unfold keepTheirs at h
  rw [Bool.and_eq_true, committedL_eq] at h
  exact h.1

/-- A peer entry a restart keeps: one our current commitment includes,
whose heights are rebuilt. -/
theorem mem_restore_theirs {l : Ledger} {e' : Entry}
    (he' : e' ∈ (restore l).theirs.entries) :
    ∃ e ∈ l.theirs.entries, (0 < e.hL ∧ e.hL ≤ l.lc.tail.height) ∧
      e' = rebuild l e := by
  simp only [restore, List.mem_map, List.mem_filter] at he'
  obtain ⟨e, ⟨he, hk⟩, rfl⟩ := he'
  exact ⟨e, he, keepTheirs_committedL hk, rfl⟩

theorem rebuild_hL {l : Ledger} {e : Entry}
    (hc : 0 < e.hL ∧ e.hL ≤ l.lc.tail.height) :
    (rebuild l e).hL = l.lc.tail.height := by
  have : committedL l e = true := (committedL_eq l e).2 hc
  simp [rebuild, this]

theorem rebuild_hR (l : Ledger) (e : Entry) :
    (rebuild l e).hR = if 0 < e.hR ∧ e.hR ≤ l.rc.tail.height
      then l.rc.tail.height else e.hR := by
  by_cases hc : 0 < e.hR ∧ e.hR ≤ l.rc.tail.height
  · have : committedR l e = true := (committedR_eq l e).2 hc
    simp [rebuild, this, hc]
  · have : committedR l e = false := by
      cases hb : committedR l e
      · rfl
      · exact absurd ((committedR_eq l e).1 hb) hc
    simp [rebuild, this, hc]

/-- A restart preserves the invariant, with the same forwarded set: what
it keeps of the peer's log is what our current commitment includes, every
update forwarded is below where the peer's log starts again, and a kept
update is in the peer's current commitment exactly when it was. -/
theorem inv_restore {l : Ledger} {F : List Nat} (h : Inv l F)
    (hi : IdxInv l F) : Inv (restore l) F := by
  have tip : l.rc.tail.height ≤ l.rc.tip.height := by
    cases hp : l.rc.pending with
    | none => simp [Chain.tip, hp]
    | some c => have := h.rpend c hp; simp [Chain.tip, hp]; omega
  refine ⟨?_, ?_, hi.f_ltail, h.f_nodup, h.rpend, ?_, Nat.le_refl _, ?_, ?_,
    ?_, ?_, ?_⟩
  · intro e' he'
    obtain ⟨e, he, hc, rfl⟩ := mem_restore_theirs he'
    exact hi.hL_tail e he hc.1 hc.2
  · have hs : ((l.theirs.entries.filter (keepTheirs l)).map (·.idx)).Pairwise
        (· < ·) := h.sorted.sublist (List.filter_sublist.map _)
    simpa [restore, List.map_map, Function.comp_def] using hs
  · intro c hc
    simp [restore] at hc
  · intro e' he' _
    obtain ⟨e, _, hc, rfl⟩ := mem_restore_theirs he'
    rw [rebuild_hL hc]
    exact ⟨by omega, Nat.le_refl _⟩
  · intro e' he'
    obtain ⟨e, _, hc, rfl⟩ := mem_restore_theirs he'
    rw [rebuild_hL hc]
    simp [restore, Chain.tip]
  · intro e' he'
    obtain ⟨e, he, _, rfl⟩ := mem_restore_theirs he'
    have := h.hR_le e he
    show (rebuild l e).hR ≤ l.rc.tip.height
    rw [rebuild_hR]
    split <;> omega
  · intro e' he' _
    obtain ⟨e, _, hc, rfl⟩ := mem_restore_theirs he'
    rw [rebuild_hL hc]
    exact ⟨by omega, Nat.le_refl _⟩
  · intro e' he'
    obtain ⟨e, he, _, rfl⟩ := mem_restore_theirs he'
    show e.idx ∈ F ↔
      (e.kind ≠ .fee ∧ 0 < (rebuild l e).hR ∧
        (rebuild l e).hR ≤ l.rc.tail.height)
    rw [h.fwd_iff e he, rebuild_hR]
    split
    · rename_i hc
      exact ⟨fun ⟨a, _, _⟩ => ⟨a, by omega, Nat.le_refl _⟩,
        fun ⟨a, _, _⟩ => ⟨a, hc⟩⟩
    · rename_i hc
      exact ⟨fun ⟨a, b, d⟩ => absurd ⟨b, d⟩ hc,
        fun ⟨a, b, d⟩ => absurd ⟨b, d⟩ hc⟩

theorem idx_restore {l : Ledger} {F : List Nat} (hi : IdxInv l F) :
    IdxInv (restore l) F := by
  refine ⟨hi.f_ltail, ?_, ?_⟩
  · intro e' he' _ _
    obtain ⟨e, he, hc, rfl⟩ := mem_restore_theirs he'
    exact hi.hL_tail e he hc.1 hc.2
  · intro c hc
    simp [restore] at hc

/-- Every operation the ledger accepts preserves the invariant, with the
updates it forwards added to the forwarded set. -/
theorem inv_step {l l' : Ledger} {F : List Nat} {op : Op} {fs : List Fwd}
    (h : Inv l F) (hi : IdxInv l F) (hs : step l op = .ok (l', fs)) :
    Inv l' (F ++ fs.map (·.idx)) := by
  cases op with
  | addOurs =>
    simp only [step, Except.ok.injEq, Prod.mk.injEq] at hs
    obtain ⟨rfl, rfl⟩ := hs
    simp only [List.map_nil, List.append_nil]
    exact h.congr rfl rfl rfl rfl
  | addTheirs id =>
    simp only [step] at hs
    split at hs
    · cases hs
    · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      simp only [List.map_nil, List.append_nil]
      exact h.push .add id rfl rfl rfl rfl
  | removeOurs k id =>
    simp only [step] at hs
    split at hs
    · cases hs
    · split at hs
      · cases hs
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simp only [List.map_nil, List.append_nil]
        exact h.congr rfl rfl rfl rfl
  | removeTheirs k id =>
    simp only [step] at hs
    split at hs
    · cases hs
    · split at hs
      · cases hs
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simp only [List.map_nil, List.append_nil]
        exact h.push k id rfl rfl rfl rfl
  | feeOurs =>
    simp only [step] at hs
    split at hs
    · cases hs
    · split at hs
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simpa using h
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simp only [List.map_nil, List.append_nil]
        exact h.congr rfl rfl rfl rfl
  | feeTheirs =>
    simp only [step] at hs
    split at hs
    · cases hs
    · split at hs
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simpa using h
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simp only [List.map_nil, List.append_nil]
        exact h.push .fee 0 rfl rfl rfl rfl
  | sign =>
    simp only [step] at hs
    split at hs
    · cases hs
    · rename_i hp
      simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      simpa using inv_sign h hp
  | recvCommit =>
    simp only [step] at hs
    split at hs
    · cases hs
    · rename_i hp
      simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      simpa using inv_recvCommit h hp
  | revoke =>
    simp only [step] at hs
    split at hs
    · cases hs
    · rename_i c hp
      simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      simpa using inv_revoke h hp
  | recvRevocation =>
    simp only [step] at hs
    split at hs
    · cases hs
    · rename_i c hp
      simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      exact inv_compact (inv_advance h hp)
  | restart =>
    simp only [step, Except.ok.injEq, Prod.mk.injEq] at hs
    obtain ⟨rfl, rfl⟩ := hs
    simpa using inv_restore h hi

/-- Every operation the ledger accepts preserves the index facts. -/
theorem idx_step {l l' : Ledger} {F : List Nat} {op : Op} {fs : List Fwd}
    (h : Inv l F) (hi : IdxInv l F) (hs : step l op = .ok (l', fs)) :
    IdxInv l' (F ++ fs.map (·.idx)) := by
  cases op with
  | addOurs =>
    simp only [step, Except.ok.injEq, Prod.mk.injEq] at hs
    obtain ⟨rfl, rfl⟩ := hs
    simp only [List.map_nil, List.append_nil]
    exact hi.congr rfl rfl
  | addTheirs id =>
    simp only [step] at hs
    split at hs
    · cases hs
    · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      simp only [List.map_nil, List.append_nil]
      exact hi.push .add id rfl rfl
  | removeOurs k id =>
    simp only [step] at hs
    split at hs
    · cases hs
    · split at hs
      · cases hs
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simp only [List.map_nil, List.append_nil]
        exact hi.congr rfl rfl
  | removeTheirs k id =>
    simp only [step] at hs
    split at hs
    · cases hs
    · split at hs
      · cases hs
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simp only [List.map_nil, List.append_nil]
        exact hi.push k id rfl rfl
  | feeOurs =>
    simp only [step] at hs
    split at hs
    · cases hs
    · split at hs
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simpa using hi
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simp only [List.map_nil, List.append_nil]
        exact hi.congr rfl rfl
  | feeTheirs =>
    simp only [step] at hs
    split at hs
    · cases hs
    · split at hs
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simpa using hi
      · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
        obtain ⟨rfl, rfl⟩ := hs
        simp only [List.map_nil, List.append_nil]
        exact hi.push .fee 0 rfl rfl
  | sign =>
    simp only [step] at hs
    split at hs
    · cases hs
    · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      simpa using idx_sign hi
  | recvCommit =>
    simp only [step] at hs
    split at hs
    · cases hs
    · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      simpa using idx_recvCommit h hi
  | revoke =>
    simp only [step] at hs
    split at hs
    · cases hs
    · rename_i c hp
      simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      simpa using idx_revoke hi hp
  | recvRevocation =>
    simp only [step] at hs
    split at hs
    · cases hs
    · simp only [Except.ok.injEq, Prod.mk.injEq] at hs
      obtain ⟨rfl, rfl⟩ := hs
      exact idx_compact (idx_advance hi)
  | restart =>
    simp only [step, Except.ok.injEq, Prod.mk.injEq] at hs
    obtain ⟨rfl, rfl⟩ := hs
    simpa using idx_restore hi

end ChanFSM
