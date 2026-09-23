/-
Worked examples. Each one runs the model on a concrete stream and checks
the answer by computation, so this file reads like a table of unit tests.
`example` is a theorem without a name, and `by decide` proves a decidable
statement by evaluating it; both sides here are plain data, so it amounts
to running the code and comparing the output.
-/
import GossipSync.Chunker

namespace GossipSync

/-- A query for blocks 100 to 199 on chain 1. The anonymous constructor
`⟨1, 100, 100⟩` fills the fields of `Query` in order. -/
def exQuery : Query := ⟨1, 100, 100⟩

/-- Generous limits: 500 replies, 100 SCIDs, a 10 second horizon. -/
def exLimits : Limits := ⟨500, 100, 10⟩

/-- A plain reply without timestamps on chain 1. -/
def plain (first num : Nat) (complete : Bool) (scids : List Nat) : Reply :=
  { chain := 1, first := first, num := num, complete := complete,
    encoding := 0, withTs := false,
    entries := scids.map fun s => ⟨s, 0, 0⟩ }

/-- The result of running a stream, reduced to what the examples compare:
the error, or the SCIDs buffered and the number of replies left over.
`Sum` is Lean's either type, with `.inl` and `.inr` as its two sides. -/
def summary (q : Query) (lim : Limits) (rs : List Reply) :
    Option (Sum Err (List Nat × Nat)) :=
  match run q lim 0 {} rs with
  | .failed e => some (.inl e)
  | .complete a rest => some (.inr (a.chans.map (·.scid), rest.length))
  | .pending _ => none

/-! ## Honest streams -/

/-- Two replies that tile the query complete it, with both replies'
channels. -/
example :
    summary exQuery exLimits
        [plain 100 50 false [1, 2], plain 150 50 true [3]] =
      some (.inr ([1, 2, 3], 0)) := by decide

/-- The second reply may continue the first reply's last block, when a
block's channels don't fit in one reply. -/
example :
    summary exQuery exLimits
        [plain 100 50 false [1, 2], plain 149 51 true [3]] =
      some (.inr ([1, 2, 3], 0)) := by decide

/-- A non-legacy reply that reaches the last block completes the stream
even without the complete flag. -/
example :
    summary exQuery exLimits
        [plain 100 50 false [1], plain 150 50 false [2]] =
      some (.inr ([1, 2], 0)) := by decide

/-! ## Rejected streams -/

/-- A gap between replies is a peer fault. -/
example :
    summary exQuery exLimits
        [plain 100 50 false [1], plain 151 49 true [2]] =
      some (.inl .gap) := by decide

/-- The tail of an earlier stream, arriving first, is rejected: it ends on
the query's last block, but a first reply must start at the query's first
block. Without this check it would complete the new query at once. -/
example :
    summary exQuery exLimits [plain 150 50 true [9]] =
      some (.inl .notAtStart) := by decide

/-- A zlib reply costs four units, so a budget of eight takes two of
them, and the second ends the stream even though blocks 150 to 199 were
never covered. -/
example :
    summary exQuery ⟨8, 100, 10⟩
        [{ plain 100 25 false [1] with encoding := 1 },
         { plain 125 25 false [2] with encoding := 1 },
         plain 150 50 true [3]] =
      some (.inr ([1, 2], 1)) := by decide

/-! ## Legacy streams -/

/-- A legacy peer echoes the query in every reply, and only the complete
flag ends its stream. -/
example :
    summary exQuery exLimits
        [plain 100 100 false [1], plain 100 100 false [2],
         plain 100 100 true [3]] =
      some (.inr ([1, 2, 3], 0)) := by decide

/-! ## The chunker -/

/-- A chunker with a limit of two SCIDs per reply, which keeps the first
channels of an oversized block. -/
def exChunker : Chunker :=
  { query := exQuery, encoding := 0, chunkSize := 2, withTs := false,
    fit := fun n cs => cs.take n }

/-- Three blocks, the last one oversized. The first two blocks share a
reply, the oversized block is cut to its first two channels, and the final
reply runs to the query's last block. -/
example :
    (exChunker.replies
        [⟨110, [⟨1, 0, 0⟩]⟩, ⟨120, [⟨2, 0, 0⟩]⟩,
         ⟨130, [⟨3, 0, 0⟩, ⟨4, 0, 0⟩, ⟨5, 0, 0⟩]⟩]).map
        (fun r => (r.first, r.last, r.complete, r.entries.map (·.scid))) =
      [(100, 129, false, [1, 2]), (130, 199, true, [3, 4])] := by decide

end GossipSync
