/-
Concrete runs of the ledger, checked by the kernel when the library builds.
They pin down the behavior the theorems are about on small cases, so a
change to the model that breaks them fails the build.
-/
import ChanFSM.Ledger

namespace ChanFSM

/-- Apply operations in order, stopping at the first refusal, and collect
what they forward. -/
def run (l : Ledger) : List Op → Except Err (Ledger × List Fwd)
  | [] => .ok (l, [])
  | op :: ops =>
    match step l op with
    | .error e => .error e
    | .ok (l', fs) =>
      match run l' ops with
      | .error e => .error e
      | .ok (l'', fs') => .ok (l'', fs ++ fs')

/-- The forwards of a run, or the refusal that stopped it. -/
def outcome (l : Ledger) (ops : List Op) : Except Err (List Fwd) :=
  (run l ops).map (·.2)

/-- What the peer's side of one full exchange looks like to us: the peer
adds HTLC 0 and signs, we revoke and sign back, the peer revokes. -/
def peerAddExchange : List Op :=
  [.addTheirs 0, .recvCommit, .revoke, .sign, .recvRevocation]

/-- The premature revocation: with no commitment of ours outstanding, a
revocation is refused. -/
example : outcome (Ledger.init true) [.recvRevocation] =
    .error .unexpectedRevocation := by rfl

/-- The same after a full exchange, when the peer has nothing left to
revoke. -/
example : outcome (Ledger.init true) (peerAddExchange ++ [.recvRevocation]) =
    .error .unexpectedRevocation := by rfl

/-- The peer's add is forwarded on the revocation that locks it in, once. -/
example : outcome (Ledger.init true) peerAddExchange =
    .ok [⟨.add, 0, 0⟩] := by rfl

/-- A second exchange carrying only a fee update forwards nothing. -/
example : outcome (Ledger.init false)
    (peerAddExchange ++ [.feeTheirs, .recvCommit, .revoke, .sign,
      .recvRevocation]) = .ok [⟨.add, 0, 0⟩] := by rfl

/-- We can't remove the peer's HTLC before it is irrevocably committed:
after the peer signs and we revoke, it's in our commitment only. -/
example : outcome (Ledger.init true)
    [.addTheirs 0, .recvCommit, .revoke, .removeOurs .settle 0] =
    .error .notCommitted := by rfl

/-- Once it is, we can, and the settle is forwarded by the peer's side, not
ours: our own removals are never in the forwards we compute. -/
example : outcome (Ledger.init true)
    (peerAddExchange ++ [.removeOurs .settle 0, .sign, .recvRevocation]) =
    .ok [⟨.add, 0, 0⟩] := by rfl

/-- The peer settling our HTLC is forwarded once the settle is locked in. -/
example : outcome (Ledger.init true)
    [.addOurs, .sign, .recvRevocation, .recvCommit, .revoke,
     .removeTheirs .settle 0, .recvCommit, .revoke, .sign,
     .recvRevocation] = .ok [⟨.settle, 0, 0⟩] := by rfl

end ChanFSM
