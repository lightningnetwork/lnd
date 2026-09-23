/-
Print the axioms every headline theorem depends on. `check.sh` runs this
file and fails if any theorem depends on `sorryAx`, the axiom behind an
unfinished proof, or on anything beyond Lean's three standard axioms:
`propext`, `Classical.choice` and `Quot.sound`.
-/
import GossipSync

open GossipSync

#print axioms checkRange_ok_iff
#print axioms add_eq_ok
#print axioms run_complete
#print axioms run_sound
#print axioms reject_before_query
#print axioms reject_after_query
#print axioms reject_outside_query
#print axioms reject_first_late
#print axioms reject_out_of_order
#print axioms reject_encoding
#print axioms reject_too_large
#print axioms add_used_by_encoding
#print axioms add_not_done_used
#print axioms add_scids
#print axioms run_budget
#print axioms budget_overshoot
#print axioms legacy_accepted
#print axioms legacy_done_iff
#print axioms legacy_prev
#print axioms after_legacy
#print axioms echo_without_complete_waits
#print axioms tiles_accepted
#print axioms tiles_budget_cut
#print axioms tiles_too_large
#print axioms tiles_fit_of_complete
#print axioms go_tiles
#print axioms go_sent
#print axioms go_length
#print axioms roundtrip
#print axioms roundtrip_budget_cut
#print axioms roundtrip_too_large
#print axioms roundtrip_all_fit
#print axioms received_fresh
