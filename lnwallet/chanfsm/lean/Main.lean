/-
The differential test driver. It reads cases from standard input, runs the
model on each, and writes the result to standard output, one line per fact,
so the Go test can compare it line by line with what the Go ledger does.

A case is `init I`, where I is 1 if we opened the channel, then one line per
operation:

  addours | addtheirs ID | removeours K ID | removetheirs K ID | feeours
  | feetheirs | sign | recvcommit | revoke | recvrev | restart

with K a kind number (0 add, 1 settle, 2 fail, 3 malformed, 4 fee), then
`end`. The answer is one line per operation, `ok` followed by ` K ID` per
forwarded update or `err NAME`, and after a restart the restored ledger (see
`dump`), then the final ledger, then `end`. A refused operation leaves the
ledger as it was.
-/
import ChanFSM.Ledger

open ChanFSM

def kindOf : Nat → Kind
  | 0 => .add
  | 1 => .settle
  | 2 => .fail
  | 3 => .malformed
  | _ => .fee

def kindNum : Kind → Nat
  | .add => 0
  | .settle => 1
  | .fail => 2
  | .malformed => 3
  | .fee => 4

def errName : Err → String
  | .badId => "badId"
  | .unknown => "unknown"
  | .removed => "removed"
  | .notCommitted => "notCommitted"
  | .notInitiator => "notInitiator"
  | .noWindow => "noWindow"
  | .unexpectedCommit => "unexpectedCommit"
  | .nothingToRevoke => "nothingToRevoke"
  | .unexpectedRevocation => "unexpectedRevocation"
  | .notRemoval => "notRemoval"

def nums (line : String) : List Nat :=
  ((line.trimAscii.toString.splitOn " ").drop 1).filterMap String.toNat?

def parseOp (line : String) : Option Op :=
  let w := (line.trimAscii.toString.splitOn " ").headD ""
  match w, nums line with
  | "addours", _ => some .addOurs
  | "addtheirs", [id] => some (.addTheirs id)
  | "removeours", [k, id] => some (.removeOurs (kindOf k) id)
  | "removetheirs", [k, id] => some (.removeTheirs (kindOf k) id)
  | "feeours", _ => some .feeOurs
  | "feetheirs", _ => some .feeTheirs
  | "sign", _ => some .sign
  | "recvcommit", _ => some .recvCommit
  | "revoke", _ => some .revoke
  | "recvrev", _ => some .recvRevocation
  | "restart", _ => some .restart
  | _, _ => none

/-- Insertion into a sorted list, for printing the modified sets the way Go
keeps them. -/
def insertSorted (x : Nat) : List Nat → List Nat
  | [] => [x]
  | y :: ys => if x ≤ y then x :: y :: ys else y :: insertSorted x ys

def sortNats (xs : List Nat) : List Nat := xs.foldr insertSorted []

def showNats (xs : List Nat) : String := " ".intercalate (xs.map toString)

def showLog (name : String) (g : Log) : String :=
  let es := g.entries.map fun e =>
    s!"{e.idx} {kindNum e.kind} {e.htlc} {e.hL} {e.hR}"
  s!"{name} {g.next} {g.htlcs} {g.entries.length} {" ".intercalate es} " ++
    s!"mod {showNats (sortNats g.modified)}"

def showCommit (c : Commit) : String := s!"{c.height} {c.idxL} {c.idxR}"

def showChain (name : String) (c : Chain) : String :=
  match c.pending with
  | some p => s!"{name} {showCommit c.tail} pending {showCommit p}"
  | none => s!"{name} {showCommit c.tail} none"

/-- The final ledger, one line per log and per chain. -/
def dump (l : Ledger) : List String :=
  [showLog "ours" l.ours, showLog "theirs" l.theirs, showChain "lc" l.lc,
   showChain "rc" l.rc]

/-- Answer cases until the input ends. -/
partial def loop (stdin : IO.FS.Stream) (stdout : IO.FS.Stream)
    (l : Ledger) (inCase : Bool) : IO Unit := do
  let line ← stdin.getLine
  if line.isEmpty then return
  let line := line.trimAscii.toString
  if line.startsWith "init " then
    loop stdin stdout (Ledger.init (nums line == [1])) true
  else if line == "end" then
    for d in dump l do
      IO.println d
    IO.println "end"
    stdout.flush
    loop stdin stdout l false
  else
    match parseOp line with
    | none =>
      IO.println "err parse"
      loop stdin stdout l inCase
    | some op =>
      match step l op with
      | .error e =>
        IO.println s!"err {errName e}"
        loop stdin stdout l inCase
      | .ok (l', fs) =>
        let fws := fs.map fun f => s!" {kindNum f.kind} {f.id}"
        IO.println ("ok" ++ String.join fws)
        if let .restart := op then
          for d in dump l' do
            IO.println d
        loop stdin stdout l' inCase

def main : IO Unit := do
  loop (← IO.getStdin) (← IO.getStdout) (Ledger.init true) false
