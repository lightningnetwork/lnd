package chainntnfs

import "github.com/btcsuite/btcd/wire"

// ChainNotifier ...
// TODO(roasbeef): finish
//  * multiple backends for interface
//   * btcd - websockets
//   * core - rpc polling or ZeroMQ
//   * direct p2p
//   * random bitcoin API?
//   * electrum?
//   * SPV bloomfilter
//   * other stuff maybe...
type ChainNotifier interface {
	RegisterConfirmationsNtfn(txid *wire.ShaHash, numConfs uint32) (*ConfirmationEvent, error)
	RegisterSpendNtfn(outpoint *wire.OutPoint) (*SpendEvent, error)

	Start() error
	Stop() error
}

// TODO(roasbeef): ln channels should request spend ntfns for counterparty's
// inputs to funding tx also, consider channel closed if funding tx re-org'd
// out and inputs double spent.

// ConfirmationEvent ...
type ConfirmationEvent struct {
	Confirmed chan struct{} // MUST be buffered.

	// TODO(roasbeef): all goroutines on ln channel updates should also
	// have a struct chan that's closed if funding gets re-org out. Need
	// to sync, to request another confirmation event ntfn, then re-open
	// channel after confs.

	NegativeConf chan uint32 // MUST be buffered.
}

// SpendDetail ...
type SpendDetail struct {
	SpentOutPoint *wire.OutPoint

	SpendingTx        *wire.MsgTx
	SpenderTxHash     *wire.ShaHash
	SpenderInputIndex uint32
}

// SpendEvent ...
type SpendEvent struct {
	Spend chan *SpendDetail // MUST be buffered.
}
