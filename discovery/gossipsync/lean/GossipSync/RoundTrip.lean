/-
This file proves the round trip: every stream our chunker produces is
accepted by our accumulator, with exactly the channels the chunker sent,
whenever it fits the limits, and it says precisely what happens when it
doesn't.

The proof has two halves that meet at `Tiles`, a description of a well
formed stream. The first half shows the accumulator accepts every stream
that tiles the query. The second shows the chunker only produces such
streams.
-/
import GossipSync.Chunker
import GossipSync.Legacy
import GossipSync.Rejection

namespace GossipSync

/-! ## Streams that tile the query -/

/-- `Tiles q s rs`: the replies `rs` tile the query from height `s` to its
last block, each starting on the block after the one before, with only the
last reply setting complete. This is the shape of every stream our chunker
sends. It never continues a block across replies, which the accumulator
also allows. -/
inductive Tiles (q : Query) : Nat → List Reply → Prop where
  | last {s : Nat} {r : Reply} :
      r.first = s → r.last = q.last → r.complete = true → Tiles q s [r]
  | cons {s : Nat} {r : Reply} {rs : List Reply} :
      r.first = s → r.first ≤ r.last → r.last < q.last →
      r.complete = false → Tiles q (r.last + 1) rs → Tiles q s (r :: rs)

/-- The SCIDs a stream carries, in order. -/
def sent (rs : List Reply) : List Entry := rs.flatMap (·.entries)

/-- A tiling is never empty, so its length is some `n + 1`. The
`obtain ⟨n, hn⟩ : ∃ n, ... := ...` pattern used below pulls the witness
out of an existential. -/
theorem Tiles.length_pos {q : Query} {s : Nat} {rs : List Reply}
    (h : Tiles q s rs) : ∃ n, rs.length = n + 1 := by
  cases h <;> simp

/-- Where the accumulator stands relative to the next reply's first block
`s`: either nothing has arrived and `s` is the query's first block, or the
last reply ended on the block before `s`. -/
def AtStart (q : Query) (a : Acc) (s : Nat) : Prop :=
  (a.prevLast = none ∧ s = q.first) ∨
    (∃ p, a.prevLast = some p ∧ s = p + 1 ∧ q.first ≤ s)

/-- The next reply of a tiling passes the range stage of `add`. -/
theorem tiles_admissible {q : Query} {a : Acc} {s : Nat} {r : Reply}
    (hs : AtStart q a s) (hf : r.first = s) (hl : r.last ≤ q.last) :
    Admissible q a.prevLast r := by
  right
  rcases hs with ⟨hp, rfl⟩ | ⟨p, hp, rfl, hq⟩
  · exact ⟨by omega, hl, by simp [hp, hf]⟩
  · exact ⟨by omega, hl, by simp [hp, hf]⟩

/-- **The accumulator accepts every tiling that fits.** A stream that tiles
the query, with one encoding of weight `w` throughout, is accepted as
complete with nothing left over, provided that every reply but the last
leaves budget unspent and the SCIDs fit the limit. The buffer then holds
every reply's channels after the freshness filter.

The proof is by induction on the `Tiles` derivation. Each step shows that
`add` accepts the head reply without completing, then applies the
induction hypothesis to the rest. -/
theorem tiles_accepted {q : Query} {lim : Limits} {now w : Nat} :
    ∀ {s : Nat} {rs : List Reply} {a : Acc}, Tiles q s rs → AtStart q a s →
      (∀ r ∈ rs, weight r.encoding = some w) →
      a.used + w * (rs.length - 1) < lim.maxReplies →
      a.scids + (sent rs).length ≤ lim.maxSCIDs →
      ∃ a', run q lim now a rs = .complete a' [] ∧
        a'.chans = a.chans ++ rs.flatMap (received lim now) := by
  intro s rs a ht
  induction ht generalizing a with
  | @last s r hf hl hc =>
    intro hs hw _ hsc
    have hadm := tiles_admissible hs hf (by omega)
    have hw' := hw r (by simp)
    have hadd := add_eq_ok (q := q) (lim := lim) (now := now) (a := a) (r := r)
      (a' := accept lim now a r w) (d := true) |>.mpr
    refine ⟨accept lim now a r w, ?_, by simp [accept]⟩
    have hdone : isDone q lim (accept lim now a r w) r = true := by
      unfold isDone
      by_cases hleg : isLegacy q r = true <;> simp [hleg, hc, hl]
    simp only [run, hadd ⟨hadm, w, hw', by simpa [sent] using hsc, rfl,
      hdone.symm⟩]
  | @cons s r rs hf hle hlt hc _ ih =>
    intro hs hw hb hsc
    have hadm := tiles_admissible hs hf (by omega)
    have hw' := hw r (by simp)
    obtain ⟨n, hn⟩ := (by assumption : Tiles q (r.last + 1) rs).length_pos
    simp only [List.length_cons, hn, Nat.add_sub_cancel] at hb
    have hwn : w ≤ w * (n + 1) := Nat.le_mul_of_pos_right w (by omega)
    have hnotdone : isDone q lim (accept lim now a r w) r = false := by
      unfold isDone
      by_cases hleg : isLegacy q r = true
      · simp [hleg, hc, accept]; omega
      · simp [hleg, accept]; omega
    have hadd := add_eq_ok (q := q) (lim := lim) (now := now) (a := a) (r := r)
      (a' := accept lim now a r w) (d := false) |>.mpr
      ⟨hadm, w, hw', by simp [sent] at hsc; omega, rfl, hnotdone.symm⟩
    have hs' : AtStart q (accept lim now a r w) (r.last + 1) :=
      Or.inr ⟨r.last, rfl, rfl, by
        rcases hs with ⟨_, rfl⟩ | ⟨_, _, rfl, _⟩ <;> omega⟩
    have hb' : (accept lim now a r w).used + w * (rs.length - 1) <
        lim.maxReplies := by
      simp only [accept, hn, Nat.add_sub_cancel]
      rw [Nat.mul_succ] at hb
      omega
    have hsc' : (accept lim now a r w).scids + (sent rs).length ≤
        lim.maxSCIDs := by
      simp [accept, sent] at hsc ⊢; omega
    obtain ⟨a', hrun, hchans⟩ :=
      ih hs' (fun x hx => hw x (by simp [hx])) hb' hsc'
    refine ⟨a', ?_, ?_⟩
    · simp only [run, hadd]; exact hrun
    · rw [hchans]; simp [accept]

/-- **The budget cuts a long tiling short.** If the stream is too long for
the budget, the accumulator completes it early, at the first reply `k`
whose cost reaches the budget, and leaves the rest unread. The buffer then
holds only the first `k` replies' channels. This is GSS-004's third way to
end a stream, applied to an honest peer. -/
theorem tiles_budget_cut {q : Query} {lim : Limits} {now w : Nat} :
    ∀ {s : Nat} {rs : List Reply} {a : Acc}, Tiles q s rs → AtStart q a s →
      (∀ r ∈ rs, weight r.encoding = some w) →
      a.used < lim.maxReplies →
      lim.maxReplies ≤ a.used + w * (rs.length - 1) →
      a.scids + (sent rs).length ≤ lim.maxSCIDs →
      ∃ k a', 1 ≤ k ∧ k < rs.length ∧
        a.used + w * (k - 1) < lim.maxReplies ∧
        lim.maxReplies ≤ a.used + w * k ∧
        run q lim now a rs = .complete a' (rs.drop k) ∧
        a'.chans = a.chans ++ (rs.take k).flatMap (received lim now) := by
  intro s rs a ht
  induction ht generalizing a with
  | last => intro _ _ hlt hge _; simp at hge; omega
  | @cons s r rs hf hle hlt hc hrest ih =>
    intro hs hw hu hb hsc
    have hadm := tiles_admissible hs hf (by omega)
    have hw' := hw r (by simp)
    obtain ⟨n, hn⟩ := hrest.length_pos
    simp only [List.length_cons, hn, Nat.add_sub_cancel] at hb
    have hsc1 : a.scids + r.entries.length ≤ lim.maxSCIDs := by
      simp [sent] at hsc; omega
    -- Either this reply spends the budget, and the stream ends here with
    -- `k = 1`, or it doesn't, and the induction hypothesis finds `k` in
    -- the rest of the stream.
    by_cases hspent : lim.maxReplies ≤ a.used + w
    · have hdone : isDone q lim (accept lim now a r w) r = true := by
        simp [isDone, accept]; omega
      have hadd := add_eq_ok (q := q) (lim := lim) (now := now) (a := a) (r := r)
        (a' := accept lim now a r w) (d := true) |>.mpr
        ⟨hadm, w, hw', hsc1, rfl, hdone.symm⟩
      refine ⟨1, accept lim now a r w, Nat.le_refl 1, by simp; omega,
        by simpa using hu, by simpa using hspent, ?_, ?_⟩
      · simp only [run, hadd, List.drop_succ_cons, List.drop_zero]
      · simp [accept]
    · have hnotdone : isDone q lim (accept lim now a r w) r = false := by
        unfold isDone
        by_cases hleg : isLegacy q r = true
        · simp [hleg, hc, accept]; omega
        · simp [hleg, accept]; omega
      have hadd := add_eq_ok (q := q) (lim := lim) (now := now) (a := a) (r := r)
        (a' := accept lim now a r w) (d := false) |>.mpr
        ⟨hadm, w, hw', hsc1, rfl, hnotdone.symm⟩
      have hs' : AtStart q (accept lim now a r w) (r.last + 1) :=
        Or.inr ⟨r.last, rfl, rfl, by
          rcases hs with ⟨_, rfl⟩ | ⟨_, _, rfl, _⟩ <;> omega⟩
      have hb' : lim.maxReplies ≤
          (accept lim now a r w).used + w * (rs.length - 1) := by
        simp only [accept, hn, Nat.add_sub_cancel]
        rw [Nat.mul_succ] at hb
        omega
      have hsc' : (accept lim now a r w).scids + (sent rs).length ≤
          lim.maxSCIDs := by
        simp [accept, sent] at hsc ⊢; omega
      obtain ⟨k, a', hk1, hkn, hlo, hhi, hrun, hchans⟩ :=
        ih hs' (fun x hx => hw x (by simp [hx]))
          (by simp [accept]; omega) hb' hsc'
      refine ⟨k + 1, a', by omega, by simp; omega, ?_, ?_, ?_, ?_⟩
      · simp only [accept] at hlo
        obtain ⟨j, rfl⟩ : ∃ j, k = j + 1 := ⟨k - 1, by omega⟩
        simp only [Nat.add_sub_cancel, Nat.mul_succ] at hlo ⊢
        omega
      · simp only [accept] at hhi
        rw [Nat.mul_succ]
        omega
      · simp only [run, hadd, List.drop_succ_cons]; exact hrun
      · rw [hchans]; simp [accept]

/-- **Too many SCIDs fails an honest stream.** If the budget fits but the
stream carries more SCIDs than the limit, the accumulator rejects it with
`tooLarge`, at the reply that crosses the limit. -/
theorem tiles_too_large {q : Query} {lim : Limits} {now w : Nat} :
    ∀ {s : Nat} {rs : List Reply} {a : Acc}, Tiles q s rs → AtStart q a s →
      (∀ r ∈ rs, weight r.encoding = some w) →
      a.used + w * (rs.length - 1) < lim.maxReplies →
      lim.maxSCIDs < a.scids + (sent rs).length →
      run q lim now a rs = .failed .tooLarge := by
  intro s rs a ht
  induction ht generalizing a with
  | @last s r hf hl hc =>
    intro hs hw _ hsc
    have hadm := tiles_admissible hs hf (by omega)
    have hw' := hw r (by simp)
    have := reject_too_large q lim now a r w hadm hw' (by simpa [sent] using hsc)
    simp [run, this]
  | @cons s r rs hf hle hlt hc hrest ih =>
    intro hs hw hb hsc
    have hadm := tiles_admissible hs hf (by omega)
    have hw' := hw r (by simp)
    by_cases hover : lim.maxSCIDs < a.scids + r.entries.length
    · have := reject_too_large q lim now a r w hadm hw' hover
      simp [run, this]
    · obtain ⟨n, hn⟩ := hrest.length_pos
      simp only [List.length_cons, hn, Nat.add_sub_cancel] at hb
      have hwn : w ≤ w * (n + 1) := Nat.le_mul_of_pos_right w (by omega)
      have hnotdone : isDone q lim (accept lim now a r w) r = false := by
        unfold isDone
        by_cases hleg : isLegacy q r = true
        · simp [hleg, hc, accept]; omega
        · simp [hleg, accept]; omega
      have hadd := add_eq_ok (q := q) (lim := lim) (now := now) (a := a) (r := r)
        (a' := accept lim now a r w) (d := false) |>.mpr
        ⟨hadm, w, hw', by omega, rfl, hnotdone.symm⟩
      have hs' : AtStart q (accept lim now a r w) (r.last + 1) :=
        Or.inr ⟨r.last, rfl, rfl, by
          rcases hs with ⟨_, rfl⟩ | ⟨_, _, rfl, _⟩ <;> omega⟩
      have hb' : (accept lim now a r w).used + w * (rs.length - 1) <
          lim.maxReplies := by
        simp only [accept, hn, Nat.add_sub_cancel]
        rw [Nat.mul_succ] at hb
        omega
      have hsc' : lim.maxSCIDs < (accept lim now a r w).scids +
          (sent rs).length := by
        simp [accept, sent] at hsc ⊢; omega
      simp only [run, hadd]
      exact ih hs' (fun x hx => hw x (by simp [hx])) hb' hsc'

/-- The converse of `tiles_accepted`: if a tiling is accepted with nothing
left over, it fit the budget and the SCID limit. Together they say a tiling
is accepted in full exactly when it fits. -/
theorem tiles_fit_of_complete {q : Query} {lim : Limits} {now w : Nat} :
    ∀ {s : Nat} {rs : List Reply} {a a' : Acc}, Tiles q s rs →
      (∀ r ∈ rs, weight r.encoding = some w) →
      a.used < lim.maxReplies →
      run q lim now a rs = .complete a' [] →
      a.used + w * (rs.length - 1) < lim.maxReplies ∧
        a.scids + (sent rs).length ≤ lim.maxSCIDs := by
  intro s rs a a' ht
  induction ht generalizing a with
  | @last s r _ _ _ =>
    intro _ hu hrun
    simp only [run] at hrun
    split at hrun
    · simp at hrun
    · rename_i a₁ hadd
      have := (add_scids hadd)
      simp [sent]; omega
    · simp at hrun
  | @cons s r rs _ _ _ _ hrest ih =>
    intro hw hu hrun
    obtain ⟨n, hn⟩ := hrest.length_pos
    simp only [run] at hrun
    split at hrun
    · simp at hrun
    · -- Completing here would leave the non-empty rest of the tiling over.
      simp at hrun
      obtain ⟨_, rfl⟩ := hrun
      simp at hn
    · rename_i a₁ hadd
      have hu₁ := add_not_done_used hadd
      obtain ⟨w', hw', hused⟩ := add_used hadd
      have : w' = w := by
        have := hw r (by simp); rw [this] at hw'; cases hw'; rfl
      subst this
      have hsc := (add_scids hadd).1
      obtain ⟨hb, hs⟩ := ih (fun x hx => hw x (by simp [hx])) hu₁ hrun
      simp only [hn, Nat.add_sub_cancel] at hb
      simp only [List.length_cons, hn, Nat.add_sub_cancel]
      refine ⟨?_, ?_⟩
      · rw [Nat.mul_succ]
        rw [hused] at hb
        omega
      · simp [sent] at hs ⊢
        omega

/-! ## The chunker only produces tilings -/

/-- Block heights the chunker can work with, starting from height `first`:
strictly increasing, no lower than `first`, and inside the query.
`List.Pairwise R bs` says `R` holds for every pair of elements in order,
so here it says the heights strictly increase. -/
def HeightsOK (q : Query) (first : Nat) (bs : List Block) : Prop :=
  bs.Pairwise (fun x y => x.height < y.height) ∧
    ∀ b ∈ bs, first ≤ b.height ∧ b.height ≤ q.last

/-- The query's last block is a `uint32` and no lower than its first, when
the first is a `uint32`. -/
theorem query_last_bounds {q : Query} (hq : q.first ≤ u32Max) :
    q.first ≤ q.last ∧ q.last ≤ u32Max := by
  unfold Query.last lastBlock
  split <;> omega

/-- A chunker reply ends where it was asked to. -/
theorem mkReply_last (c : Chunker) (chunk : List Entry) (first last : Nat)
    (final : Bool) (h1 : first ≤ last) (h2 : last ≤ u32Max) :
    (c.mkReply chunk first last final).last = last := by
  show lastBlock first (last - first + 1) = last
  unfold lastBlock
  split <;> omega

/-- **The chunker tiles the query.** For any blocks with sorted heights
inside the query, the chunker's stream tiles the query from its first
block. -/
theorem go_tiles (c : Chunker) (hq : c.query.first ≤ u32Max) :
    ∀ (bs : List Block) (first : Nat) (chunk : List Entry),
      c.query.first ≤ first → first ≤ c.query.last →
      HeightsOK c.query first bs →
      Tiles c.query first (c.go first chunk bs) := by
  have ⟨_, hlastmax⟩ := query_last_bounds hq
  intro bs
  induction bs with
  | nil =>
    intro first chunk h1 h2 _
    simp only [Chunker.go]
    exact .last rfl (mkReply_last c _ _ _ _ h2 hlastmax) rfl
  | cons b bs ih =>
    intro first chunk h1 h2 ⟨hsorted, hin⟩
    simp only [List.pairwise_cons] at hsorted
    have hb := hin b (by simp)
    -- The rest of the blocks sit strictly above `b`.
    have hrest : ∀ f, f ≤ b.height → HeightsOK c.query f bs := fun f hf =>
      ⟨hsorted.2, fun x hx =>
        ⟨by have := hsorted.1 x hx; omega, (hin x (by simp [hx])).2⟩⟩
    simp only [Chunker.go]
    split
    · exact ih first _ h1 h2 (hrest first hb.1)
    · split
      · rename_i hlt
        have hlast := mkReply_last c chunk first (b.height - 1) false
          (by omega) (by omega)
        refine .cons rfl (by rw [hlast]; simp [Chunker.mkReply]; omega)
          (by rw [hlast]; omega) rfl ?_
        rw [hlast, Nat.sub_add_cancel (by omega)]
        exact ih b.height _ (by omega) hb.2 (hrest b.height (Nat.le_refl _))
      · rename_i hge
        have : b.height = first := by omega
        simp only [List.nil_append]
        rw [this]
        exact ih first _ h1 h2 (hrest first (by omega))

/-- How the chunker picks a block's channels: all of them if they fit in
one reply, and `fit`'s choice otherwise. -/
def Chunker.pick (c : Chunker) (b : Block) : List Entry :=
  if b.chans.length ≤ c.limit then b.chans else c.fit c.limit b.chans

/-- **The chunker sends the picked channels, in order.** Provided `fit`
leaves a block alone when it already fits, the stream carries each block's
picked channels, block by block, after whatever was pending. The condition
on `chunk` says a pending chunk can only exist once the first block is
behind us, which `go` itself maintains. -/
theorem go_sent (c : Chunker)
    (hfit : ∀ n cs, cs.length ≤ n → c.fit n cs = cs) :
    ∀ (bs : List Block) (first : Nat) (chunk : List Entry),
      HeightsOK c.query first bs →
      (∀ b, bs.head? = some b → b.height ≤ first → chunk = []) →
      sent (c.go first chunk bs) = chunk ++ bs.flatMap c.pick := by
  intro bs
  induction bs with
  | nil =>
    intro first chunk _ _
    simp [Chunker.go, sent, Chunker.mkReply]
  | cons b bs ih =>
    intro first chunk ⟨hsorted, hin⟩ hhead
    simp only [List.pairwise_cons] at hsorted
    have hb := hin b (by simp)
    have hrest : ∀ f, f ≤ b.height → HeightsOK c.query f bs := fun f hf =>
      ⟨hsorted.2, fun x hx =>
        ⟨by have := hsorted.1 x hx; omega, (hin x (by simp [hx])).2⟩⟩
    -- After `b`, every block is strictly higher than `b`, so the head
    -- condition holds vacuously for any new chunk.
    have hvac : ∀ f, f ≤ b.height → ∀ ch : List Entry,
        ∀ x, bs.head? = some x → x.height ≤ f → ch = [] := by
      intro f hf ch x hx hle
      have := hsorted.1 x (List.mem_of_head? hx)
      omega
    simp only [Chunker.go]
    split
    · rename_i hfits
      rw [ih first _ (hrest first hb.1) (hvac first hb.1 _)]
      have : c.pick b = b.chans := by simp [Chunker.pick]; omega
      simp [this]
    · rename_i hnofit
      have hpick : c.fit c.limit b.chans = c.pick b := by
        unfold Chunker.pick
        split
        · exact hfit _ _ (by assumption)
        · rfl
      split
      · have := ih b.height (c.fit c.limit b.chans) (hrest b.height (Nat.le_refl _))
          (hvac b.height (Nat.le_refl _) _)
        simp [sent] at this ⊢
        rw [this, hpick]
        simp [Chunker.mkReply]
      · rename_i hge
        have hch : chunk = [] := hhead b rfl (by omega)
        subst hch
        have := ih b.height (c.fit c.limit b.chans) (hrest b.height (Nat.le_refl _))
          (hvac b.height (Nat.le_refl _) _)
        simp only [List.nil_append]
        rw [this, hpick]
        simp

/-- Every chunker reply carries the chunker's encoding and timestamp
flag. -/
theorem go_uniform (c : Chunker) :
    ∀ (bs : List Block) (first : Nat) (chunk : List Entry),
      ∀ r ∈ c.go first chunk bs, r.encoding = c.encoding ∧ r.withTs = c.withTs := by
  intro bs
  induction bs with
  | nil => intro first chunk r hr; simp [Chunker.go] at hr; subst hr; simp [Chunker.mkReply]
  | cons b bs ih =>
    intro first chunk r hr
    simp only [Chunker.go] at hr
    split at hr
    · exact ih _ _ r hr
    · split at hr
      · simp at hr
        rcases hr with rfl | hr
        · simp [Chunker.mkReply]
        · exact ih _ _ r hr
      · simp at hr
        exact ih _ _ r hr

/-- The freshness filter distributes over concatenation. -/
theorem freshEntries_append (lim : Limits) (now : Nat) (t : Bool)
    (xs ys : List Entry) :
    freshEntries lim now t (xs ++ ys) =
      freshEntries lim now t xs ++ freshEntries lim now t ys := by
  cases t <;> simp [freshEntries]

/-- When every reply has the same timestamp flag, the buffer is the
freshness filter applied to everything sent. -/
theorem flatMap_received (lim : Limits) (now : Nat) (t : Bool) :
    ∀ rs : List Reply, (∀ r ∈ rs, r.withTs = t) →
      rs.flatMap (received lim now) = freshEntries lim now t (sent rs) := by
  intro rs
  induction rs with
  | nil => intro _; simp [sent, freshEntries]
  | cons r rs ih =>
    intro h
    have hr := h r (by simp)
    simp only [List.flatMap_cons, sent] at ih ⊢
    rw [ih (fun x hx => h x (by simp [hx])), freshEntries_append, received, hr]

/-! ## The round trip -/

/-- **Round trip.** Take any query whose first block is a `uint32`, any
blocks with strictly increasing heights inside it, and a chunker with a
known encoding of weight `w` and a `fit` that leaves fitting blocks alone.
Let `rs` be the chunker's stream. Then:

1. If every reply but the last leaves budget unspent,
   `w * (rs.length - 1) < maxReplies`, and the SCIDs fit the limit, the
   accumulator accepts `rs` as complete with nothing left over, and its
   buffer is exactly the picked channels of every block, in order, after
   the freshness filter.
2. Conversely, if the accumulator accepts `rs` in full, both conditions
   held.

So an honest stream is accepted in full exactly when it fits. The
theorems `roundtrip_budget_cut` and `roundtrip_too_large` below say what
happens when it doesn't. -/
theorem roundtrip (c : Chunker) (lim : Limits) (now w : Nat)
    (blocks : List Block) (hq : c.query.first ≤ u32Max)
    (hb : HeightsOK c.query c.query.first blocks)
    (hw : weight c.encoding = some w)
    (hfit : ∀ n cs, cs.length ≤ n → c.fit n cs = cs) :
    (w * ((c.replies blocks).length - 1) < lim.maxReplies →
      (sent (c.replies blocks)).length ≤ lim.maxSCIDs →
      ∃ a, run c.query lim now {} (c.replies blocks) = .complete a [] ∧
        a.chans = freshEntries lim now c.withTs (blocks.flatMap c.pick)) ∧
    (0 < lim.maxReplies →
      (∃ a, run c.query lim now {} (c.replies blocks) = .complete a []) →
      w * ((c.replies blocks).length - 1) < lim.maxReplies ∧
        (sent (c.replies blocks)).length ≤ lim.maxSCIDs) := by
  unfold Chunker.replies at *
  have hle := query_last_bounds hq
  have ht := go_tiles c hq blocks c.query.first [] (Nat.le_refl _) hle.1 hb
  have hu := go_uniform c blocks c.query.first []
  have hws : ∀ r ∈ c.replies blocks, weight r.encoding = some w := by
    intro r hr; rw [(hu r hr).1]; exact hw
  have hstart : AtStart c.query {} c.query.first := Or.inl ⟨rfl, rfl⟩
  refine ⟨?_, ?_⟩
  · intro hbud hsc
    obtain ⟨a, hrun, hchans⟩ := tiles_accepted (now := now) ht hstart hws
      (by simpa using hbud) (by simpa using hsc)
    refine ⟨a, hrun, ?_⟩
    rw [hchans, flatMap_received lim now c.withTs _ (fun r hr => (hu r hr).2)]
    have := go_sent c hfit blocks c.query.first [] hb (fun _ _ _ => rfl)
    simp at this ⊢
    rw [this]
  · intro hpos ⟨a, hrun⟩
    have := tiles_fit_of_complete ht hws hpos hrun
    simpa using this

/-- **Round trip, over budget.** If the chunker's stream is too long for
the budget, the accumulator completes it at the first reply `k` whose cost
reaches the budget, leaving the rest unread, with only those replies'
channels in the buffer. For plain replies `k` is `maxReplies`, and for
zlib replies it is `maxReplies / 4` rounded up. -/
theorem roundtrip_budget_cut (c : Chunker) (lim : Limits) (now w : Nat)
    (blocks : List Block) (hq : c.query.first ≤ u32Max)
    (hb : HeightsOK c.query c.query.first blocks)
    (hw : weight c.encoding = some w) (hpos : 0 < lim.maxReplies)
    (hbud : lim.maxReplies ≤ w * ((c.replies blocks).length - 1))
    (hsc : (sent (c.replies blocks)).length ≤ lim.maxSCIDs) :
    ∃ k a, 1 ≤ k ∧ k < (c.replies blocks).length ∧
      w * (k - 1) < lim.maxReplies ∧ lim.maxReplies ≤ w * k ∧
      run c.query lim now {} (c.replies blocks) =
        .complete a ((c.replies blocks).drop k) ∧
      a.chans = ((c.replies blocks).take k).flatMap (received lim now) := by
  unfold Chunker.replies at *
  have hle := query_last_bounds hq
  have ht := go_tiles c hq blocks c.query.first [] (Nat.le_refl _) hle.1 hb
  have hu := go_uniform c blocks c.query.first []
  have hws : ∀ r ∈ c.replies blocks, weight r.encoding = some w := by
    intro r hr; rw [(hu r hr).1]; exact hw
  obtain ⟨k, a, h1, h2, h3, h4, h5, h6⟩ :=
    tiles_budget_cut (lim := lim) (now := now) (a := {}) ht
      (Or.inl ⟨rfl, rfl⟩) hws hpos (by simpa using hbud) (by simpa using hsc)
  exact ⟨k, a, h1, h2, by simpa using h3, by simpa using h4, h5,
    by simpa using h6⟩

/-- **Round trip, too many SCIDs.** If the chunker's stream fits the
budget but carries more SCIDs than the limit, the accumulator rejects it
with `tooLarge`, and the attempt fails with a peer fault even though the
peer is honest. The default limit is 100,000 SCIDs. -/
theorem roundtrip_too_large (c : Chunker) (lim : Limits) (now w : Nat)
    (blocks : List Block) (hq : c.query.first ≤ u32Max)
    (hb : HeightsOK c.query c.query.first blocks)
    (hw : weight c.encoding = some w)
    (hbud : w * ((c.replies blocks).length - 1) < lim.maxReplies)
    (hsc : lim.maxSCIDs < (sent (c.replies blocks)).length) :
    run c.query lim now {} (c.replies blocks) = .failed .tooLarge := by
  unfold Chunker.replies at *
  have hle := query_last_bounds hq
  have ht := go_tiles c hq blocks c.query.first [] (Nat.le_refl _) hle.1 hb
  have hu := go_uniform c blocks c.query.first []
  have hws : ∀ r ∈ c.replies blocks, weight r.encoding = some w := by
    intro r hr; rw [(hu r hr).1]; exact hw
  exact tiles_too_large ht (Or.inl ⟨rfl, rfl⟩) hws (by simpa using hbud)
    (by simpa using hsc)

/-- When every block fits in one reply, the picked channels are all the
channels. -/
theorem pick_all (c : Chunker) (blocks : List Block)
    (h : ∀ b ∈ blocks, b.chans.length ≤ c.limit) :
    blocks.flatMap c.pick = blocks.flatMap (·.chans) := by
  induction blocks with
  | nil => rfl
  | cons b bs ih =>
    simp only [List.flatMap_cons]
    rw [ih (fun x hx => h x (by simp [hx]))]
    simp [Chunker.pick, h b (by simp)]

/-- The chunker sends at most one reply per block, plus the final one. -/
theorem go_length (c : Chunker) :
    ∀ (bs : List Block) (first : Nat) (chunk : List Entry),
      (c.go first chunk bs).length ≤ bs.length + 1 := by
  intro bs
  induction bs with
  | nil => intro _ _; simp [Chunker.go]
  | cons b bs ih =>
    intro first chunk
    simp only [Chunker.go]
    split
    · have := ih first (chunk ++ b.chans); simp; omega
    · have := ih b.height (c.fit c.limit b.chans)
      split <;> simp <;> omega

/-- **Round trip, in terms of the blocks.** If every block fits in one
reply, the budget covers one reply per block, `w * blocks.length <
maxReplies`, and the channels fit the SCID limit, then the accumulator
accepts the chunker's stream in full, with exactly every channel of every
block, in order, after the freshness filter. With plain replies and the
default budget of 500, that is any graph of up to 499 blocks' worth of
replies; with zlib, up to 124. -/
theorem roundtrip_all_fit (c : Chunker) (lim : Limits) (now w : Nat)
    (blocks : List Block) (hq : c.query.first ≤ u32Max)
    (hb : HeightsOK c.query c.query.first blocks)
    (hw : weight c.encoding = some w)
    (hfit : ∀ n cs, cs.length ≤ n → c.fit n cs = cs)
    (hsmall : ∀ b ∈ blocks, b.chans.length ≤ c.limit)
    (hbud : w * blocks.length < lim.maxReplies)
    (hsc : (blocks.flatMap (·.chans)).length ≤ lim.maxSCIDs) :
    ∃ a, run c.query lim now {} (c.replies blocks) = .complete a [] ∧
      a.chans = freshEntries lim now c.withTs (blocks.flatMap (·.chans)) := by
  have hlen := go_length c blocks c.query.first []
  have hsent : sent (c.replies blocks) = blocks.flatMap (·.chans) := by
    rw [← pick_all c blocks hsmall]
    have := go_sent c hfit blocks c.query.first [] hb (fun _ _ _ => rfl)
    simpa [Chunker.replies] using this
  have hmul : w * ((c.replies blocks).length - 1) ≤ w * blocks.length :=
    Nat.mul_le_mul_left w (by simp [Chunker.replies]; omega)
  obtain ⟨a, hrun, hchans⟩ := (roundtrip c lim now w blocks hq hb hw hfit).1
    (by omega) (by rw [hsent]; exact hsc)
  exact ⟨a, hrun, by rw [hchans, pick_all c blocks hsmall]⟩

end GossipSync
