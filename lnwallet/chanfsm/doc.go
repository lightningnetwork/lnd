// Package chanfsm models the BOLT 2 commitment protocol of a channel.
//
// A Ledger tracks both update logs and both commitment chains, and decides
// whether each update, signature and revocation is allowed before the
// channel applies it. In particular, a revoke_and_ack with no commitment of
// the peer's outstanding is refused without changing anything.
//
// A channel just loaded from disk is a RestoredLedger, which only the peer's
// channel_reestablish turns into a Ledger, together with the SyncPlan of
// what to retransmit.
package chanfsm
