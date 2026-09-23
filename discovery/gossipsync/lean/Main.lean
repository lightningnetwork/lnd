/-
The differential test driver. It reads cases from standard input, runs the
model on each, and writes the result to standard output, one line per fact,
so the Go test can compare it line by line with what the Go code does.

The format is whitespace separated numbers. An accumulator case is

  acc CHAIN FIRST NUM MAXREPLIES MAXSCIDS HORIZON NOW NREPLIES

followed by NREPLIES lines of

  reply CHAIN FIRST NUM COMPLETE ENCODING WITHTS N SCID T1 T2 ...

and its answer is one line per reply processed, `ok DONE USED SCIDS PREV`
or `err KIND`, stopping at the first error or completion as the syncer
does, then `chans N SCID T1 T2 ...` for the final buffer, then `end`.

A chunker case is

  chunk CHAIN FIRST NUM ENCODING WITHTS CHUNKSIZE NBLOCKS

followed by NBLOCKS lines of

  block HEIGHT N SCID T1 T2 ...

and its answer is one `reply ...` line per reply, in the input format
above, then `end`.
-/
import GossipSync.Accumulator
import GossipSync.Chunker

open GossipSync

/-- Parse a line into numbers after its leading keyword. `String.splitOn`
splits on a separator, and `filterMap` keeps the pieces that parse as a
`Nat`, since `String.toNat?` returns an `Option`. -/
def fields (line : String) : List Nat :=
  ((line.trimAscii.toString.splitOn " ").drop 1).filterMap String.toNat?

/-- Read `n` entries of three numbers each. -/
def entries : Nat → List Nat → List Entry
  | 0, _ => []
  | n + 1, s :: t1 :: t2 :: rest => ⟨s, t1, t2⟩ :: entries n rest
  | _, _ => []

/-- Render entries in the wire order of the protocol. -/
def showEntries (es : List Entry) : String :=
  " ".intercalate (es.map fun e => s!"{e.scid} {e.t1} {e.t2}")

def showBool (b : Bool) : String := if b then "1" else "0"

def showErr : Err → String
  | .beforeQuery => "beforeQuery"
  | .afterQuery => "afterQuery"
  | .notAtStart => "notAtStart"
  | .gap => "gap"
  | .badEncoding => "badEncoding"
  | .tooLarge => "tooLarge"

/-- Render a reply. A reply without timestamps ignores its entries'
timestamps, so they print as zero, as Go's reply carries none. -/
def showReply (r : Reply) : String :=
  let shown := if r.withTs then r.entries
    else r.entries.map fun e => { e with t1 := 0, t2 := 0 }
  let es := if shown.isEmpty then "" else " " ++ showEntries shown
  s!"reply {r.chain} {r.first} {r.num} {showBool r.complete} " ++
    s!"{r.encoding} {showBool r.withTs} {r.entries.length}{es}"

def parseReply (line : String) : Reply :=
  match fields line with
  | c :: f :: n :: comp :: enc :: ts :: k :: rest =>
    { chain := c, first := f, num := n, complete := comp != 0,
      encoding := enc, withTs := ts != 0, entries := entries k rest }
  | _ => { chain := 0, first := 0, num := 0, complete := false,
           encoding := 0, withTs := false, entries := [] }

def parseBlock (line : String) : Block :=
  match fields line with
  | h :: k :: rest => { height := h, chans := entries k rest }
  | _ => { height := 0, chans := [] }

/-- Fold the replies into the accumulator the way `onReply` does, printing
one line per step. `IO` is the type of programs with side effects, and
`do` notation sequences them, much like a Go function body. -/
def runAcc (q : Query) (lim : Limits) (now : Nat) : Acc → List Reply → IO Acc
  | a, [] => pure a
  | a, r :: rs => do
    match add q lim now a r with
    | .error e =>
      IO.println s!"err {showErr e}"
      pure a
    | .ok (a', d) =>
      let prev := match a'.prevLast with
        | some p => toString p
        | none => "none"
      IO.println s!"ok {showBool d} {a'.used} {a'.scids} {prev}"
      if d then pure a' else runAcc q lim now a' rs

/-- Read `n` lines from standard input. -/
def readLines (stdin : IO.FS.Stream) : Nat → IO (List String)
  | 0 => pure []
  | n + 1 => do
    let line ← stdin.getLine
    let rest ← readLines stdin n
    pure (line :: rest)

/-- Answer cases until the input ends. `partial def` tells Lean not to
check that this loop terminates, since it runs for as long as the input
does. -/
partial def loop (stdin : IO.FS.Stream) (stdout : IO.FS.Stream) : IO Unit := do
  let line ← stdin.getLine
  if line.isEmpty then return
  let line := line.trimAscii.toString
  if line.startsWith "acc " then
    match fields line with
    | c :: f :: n :: mr :: ms :: hz :: now :: k :: _ =>
      let rs := (← readLines stdin k).map parseReply
      let q : Query := ⟨c, f, n⟩
      let lim : Limits := ⟨mr, ms, hz⟩
      let a ← runAcc q lim now {} rs
      let es := if a.chans.isEmpty then "" else " " ++ showEntries a.chans
      IO.println s!"chans {a.chans.length}{es}"
    | _ => IO.println "err parse"
  else if line.startsWith "chunk " then
    match fields line with
    | c :: f :: n :: enc :: ts :: size :: k :: _ =>
      let bs := (← readLines stdin k).map parseBlock
      let ch : Chunker :=
        { query := ⟨c, f, n⟩, encoding := enc, chunkSize := size,
          withTs := ts != 0, fit := fun lim cs => cs.take lim }
      for r in ch.replies bs do
        IO.println (showReply r)
    | _ => IO.println "err parse"
  else
    IO.println "err parse"
  IO.println "end"
  stdout.flush
  loop stdin stdout

def main : IO Unit := do
  loop (← IO.getStdin) (← IO.getStdout)
