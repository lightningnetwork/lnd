/-
This file is the executable model of `rangeChunker` in `range_chunk.go`,
which splits the channels answering a query into replies.
-/
import GossipSync.Accumulator

namespace GossipSync

/-- One block's channels, as `graphdb.BlockChannelRange`. -/
structure Block where
  height : Nat
  chans : List Entry
  deriving Repr, DecidableEq

/-- The chunker's configuration, as the fields of `rangeChunker`.

Go picks the channels of an oversized block with a random shuffle and then
sorts them. The model takes that choice as a function `fit`, which gets the
limit and the block's channels. The theorems hold for every `fit` that
leaves a block alone when it already fits, and the executable uses `take`,
which is what Go does with a shuffle that swaps nothing on a sorted block. -/
structure Chunker where
  query : Query
  encoding : Nat
  chunkSize : Nat
  withTs : Bool
  fit : Nat → List Entry → List Entry

/-- The most channels per reply: the chunk size, halved when the replies
carry timestamps. `/` on `Nat` rounds down, like Go's integer division. -/
def Chunker.limit (c : Chunker) : Nat :=
  if c.withTs then c.chunkSize / 2 else c.chunkSize

/-- `rangeChunker.reply`: one reply covering `[first, last]`. Go computes
`NumBlocks` as `last - first + 1` in `uint32`, and that can't wrap: it would
need `first = 0` and `last = u32Max`, but a query that starts at zero ends
at `u32Max - 1` or below. -/
def Chunker.mkReply (c : Chunker) (chunk : List Entry) (first last : Nat)
    (final : Bool) : Reply :=
  { chain := c.query.chain
    first := first
    num := last - first + 1
    complete := final
    encoding := c.encoding
    withTs := c.withTs
    entries := chunk }

/-- The loop of `rangeChunker.replies`, with the two loop variables
`firstHeight` and `chunk` as arguments. Go's `for` loop becomes recursion
on the list of blocks: each call handles the head block `b` and recurses
on the rest `bs`.

A block that fits in the pending chunk joins it. Otherwise the pending
chunk is sent, ending on the block before this one, unless this block is
where the chunk starts, and this block starts a new chunk, trimmed by `fit`
if it is too large on its own. At the end, the last chunk is sent as the
final reply, ending on the query's last block. -/
def Chunker.go (c : Chunker) : Nat → List Entry → List Block → List Reply
  | first, chunk, [] => [c.mkReply chunk first c.query.last true]
  | first, chunk, b :: bs =>
    if b.chans.length ≤ c.limit - chunk.length then
      c.go first (chunk ++ b.chans) bs
    else
      let pending :=
        if first < b.height then [c.mkReply chunk first (b.height - 1) false]
        else []
      pending ++ c.go b.height (c.fit c.limit b.chans) bs

/-- `rangeChunker.replies`: the reply stream for the given blocks. -/
def Chunker.replies (c : Chunker) (blocks : List Block) : List Reply :=
  c.go c.query.first [] blocks

end GossipSync
