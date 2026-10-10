/-
Print the axioms every headline theorem depends on. `check.sh` runs this
file and fails if any theorem depends on `sorryAx`, the axiom behind an
unfinished proof, or on anything beyond Lean's three standard axioms:
`propext`, `Classical.choice` and `Quot.sound`.
-/
import ChanFSM

open ChanFSM

#print axioms inv_init
#print axioms inv_step
#print axioms idx_step
#print axioms inv_restore
#print axioms idx_restore
#print axioms revocation_needs_pending
#print axioms revocation_accepted_iff
#print axioms forwards_nil
#print axioms only_revocation_forwards
#print axioms forward_sound
#print axioms fee_never_forwarded
#print axioms reach_safe
#print axioms reach_inv
#print axioms forward_at_most_once
#print axioms forward_complete
#print axioms forwarded_locked_in
#print axioms forwards_are_new
#print axioms restart_safe
#print axioms restart_safe_idx
#print axioms from_safe
#print axioms from_inv
#print axioms restart_at_most_once
#print axioms restart_complete
