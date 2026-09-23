// Package gossipsync keeps lnd's channel graph in sync with its peers using
// the BOLT 7 gossip queries. It is built from two pure protofsm state
// machines, the per-peer syncer and the manager, each driven by an actor that
// owns its state, plus a responder actor per peer that serves the peer's
// queries and gossip filter.
//
// See README.md for the design, the threat model, and how the package is
// tested.
package gossipsync
