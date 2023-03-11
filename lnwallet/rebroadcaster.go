package lnwallet

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Rebroadcaster is an abstract rebroadcaster instance that'll continually
// rebroadcast transactions in the background until they're confirmed.
type Rebroadcaster interface {
	// Start launches all goroutines the rebroadcaster needs to operate.
	Start() error

	// Started returns true if the broadcaster is already active.
	Started() bool

	// Stop terminates the rebroadcaster and all goroutines it spawned.
	Stop()

	// Broadcast enqueues a transaction to be rebroadcast until it's been
	// confirmed.
	Broadcast(tx *wire.MsgTx) error

	// MarkAsConfirmed marks a transaction as confirmed, so it won't be
	// rebroadcast.
	MarkAsConfirmed(txid chainhash.Hash)
}
