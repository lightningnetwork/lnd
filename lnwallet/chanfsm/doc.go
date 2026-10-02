// Package chanfsm runs the BOLT 2 commitment protocol of a channel as a pure
// state machine wrapped in an actor.
//
// The state machine decides, from an abstract ledger of both update logs and
// both commitment chains, whether each command and peer message is allowed,
// and only then authorizes the matching operation on the channel, which
// builds, signs, verifies and persists the commitments. A message the
// protocol does not allow never reaches the channel. In particular, a
// revoke_and_ack is only ever applied in the AwaitingRevocation state, so a
// peer that sends one with no commitment outstanding fails the channel
// without changing it.
//
// Every connection starts from the channel as lnd loads it from disk, a
// RestoredLedger, which only the peer's channel_reestablish turns into a
// Ledger, together with the SyncPlan of what to retransmit.
//
// See SPEC.md for the requirements, pmodel/ for the P models they were
// derived from, lean/ for the proofs about forwarding, and dst_test.go for
// the deterministic simulation of both actors over real channels with
// crashes and disconnections.
package chanfsm
