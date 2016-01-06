package chainntnfs

import "github.com/btcsuite/btcd/wire"

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
	RegisterConfirmationsNotification(txid *wire.ShaHash, numConfs uint32, trigger *NotificationTrigger) error
	RegisterSpendNotification(outpoint *wire.OutPoint, trigger *NotificationTrigger) error

	Start() error
	Stop() error
}

type NotificationTrigger struct {
	TriggerChan chan struct{}
	Callback    func()
}
