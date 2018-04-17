package chainntnfs

import (
	"fmt"
	"sync"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

// ChainNotifier represents a trusted source to receive notifications concerning
// targeted events on the Bitcoin blockchain. The interface specification is
// intentionally general in order to support a wide array of chain notification
// implementations such as: btcd's websockets notifications, Bitcoin Core's
// ZeroMQ notifications, various Bitcoin API services, Electrum servers, etc.
//
// Concrete implementations of ChainNotifier should be able to support multiple
// concurrent client requests, as well as multiple concurrent notification events.
// TODO(roasbeef): all events should have a Cancel() method to free up the
// resource
type ChainNotifier interface {
	// RegisterConfirmationsNtfn registers an intent to be notified once
	// txid reaches numConfs confirmations. The returned ConfirmationEvent
	// should properly notify the client once the specified number of
	// confirmations has been reached for the txid, as well as if the
	// original tx gets re-org'd out of the mainchain.  The heightHint
	// parameter is provided as a convenience to light clients. The
	// heightHint denotes the earliest height in the blockchain in which the
	// target txid _could_ have been included in the chain.  This can be
	// used to bound the search space when checking to see if a
	// notification can immediately be dispatched due to historical data.
	//
	// NOTE: Dispatching notifications to multiple clients subscribed to
	// the same (txid, numConfs) tuple MUST be supported.
	RegisterConfirmationsNtfn(txid *chainhash.Hash, numConfs,
		heightHint uint32) (*ConfirmationEvent, error)

	// RegisterSpendNtfn registers an intent to be notified once the target
	// outpoint is successfully spent within a transaction. The returned
	// SpendEvent will receive a send on the 'Spend' transaction once a
	// transaction spending the input is detected on the blockchain.  The
	// heightHint parameter is provided as a convenience to light clients.
	// The heightHint denotes the earliest height in the blockchain in
	// which the target output could have been created.
	//
	// NOTE: If mempool=true is set, then this notification should be
	// triggered on a best-effort basis once the transaction is *seen* on
	// the network. If mempool=false, it should only be triggered when the
	// spending transaction receives a single confirmation.
	//
	// NOTE: Dispatching notifications to multiple clients subscribed to a
	// spend of the same outpoint MUST be supported.
	RegisterSpendNtfn(outpoint *wire.OutPoint, heightHint uint32,
		mempool bool) (*SpendEvent, error)

	// RegisterBlockEpochNtfn registers an intent to be notified of each
	// new block connected to the tip of the main chain. The returned
	// BlockEpochEvent struct contains a channel which will be sent upon
	// for each new block discovered.
	RegisterBlockEpochNtfn() (*BlockEpochEvent, error)

	// Start the ChainNotifier. Once started, the implementation should be
	// ready, and able to receive notification registrations from clients.
	Start() error

	// Stops the concrete ChainNotifier. Once stopped, the ChainNotifier
	// should disallow any future requests from potential clients.
	// Additionally, all pending client notifications will be cancelled
	// by closing the related channels on the *Event's.
	Stop() error
}

// TxConfirmation carries some additional block-level details of the exact
// block that specified transactions was confirmed within.
type TxConfirmation struct {
	// BlockHash is the hash of the block that confirmed the original
	// transition.
	BlockHash *chainhash.Hash

	// BlockHeight is the height of the block in which the transaction was
	// confirmed within.
	BlockHeight uint32

	// TxIndex is the index within the block of the ultimate confirmed
	// transaction.
	TxIndex uint32
}

// ConfirmationEvent encapsulates a confirmation notification. With this struct,
// callers can be notified of: the instance the target txid reaches the targeted
// number of confirmations, how many confirmations are left for the target txid
// to be fully confirmed at every new block height, and also in the event that
// the original txid becomes disconnected from the blockchain as a result of a
// re-org.
//
// Once the txid reaches the specified number of confirmations, the 'Confirmed'
// channel will be sent upon fulfilling the notification.
//
// If the event that the original transaction becomes re-org'd out of the main
// chain, the 'NegativeConf' will be sent upon with a value representing the
// depth of the re-org.
type ConfirmationEvent struct {
	// Confirmed is a channel that will be sent upon once the transaction
	// has been fully confirmed. The struct sent will contain all the
	// details of the channel's confirmation.
	Confirmed chan *TxConfirmation // MUST be buffered.

	// Updates is a channel that will sent upon, at every incremental
	// confirmation, how many confirmations are left to declare the
	// transaction as fully confirmed.
	Updates chan uint32 // MUST be buffered.

	// TODO(roasbeef): all goroutines on ln channel updates should also
	// have a struct chan that's closed if funding gets re-org out. Need
	// to sync, to request another confirmation event ntfn, then re-open
	// channel after confs.

	NegativeConf chan int32 // MUST be buffered.
}

// SpendDetail contains details pertaining to a spent output. This struct itself
// is the spentness notification. It includes the original outpoint which triggered
// the notification, the hash of the transaction spending the output, the
// spending transaction itself, and finally the input index which spent the
// target output.
type SpendDetail struct {
	SpentOutPoint     *wire.OutPoint
	SpenderTxHash     *chainhash.Hash
	SpendingTx        *wire.MsgTx
	SpenderInputIndex uint32
	SpendingHeight    int32
}

// SpendEvent encapsulates a spentness notification. Its only field 'Spend' will
// be sent upon once the target output passed into RegisterSpendNtfn has been
// spent on the blockchain.
//
// NOTE: If the caller wishes to cancel their registered spend notification,
// the Cancel closure MUST be called.
type SpendEvent struct {
	// Spend is a receive only channel which will be sent upon once the
	// target outpoint has been spent.
	Spend <-chan *SpendDetail // MUST be buffered.

	// Cancel is a closure that should be executed by the caller in the
	// case that they wish to prematurely abandon their registered spend
	// notification.
	Cancel func()
}

// BlockEpoch represents metadata concerning each new block connected to the
// main chain.
type BlockEpoch struct {
	// Hash is the block hash of the latest block to be added to the tip of
	// the main chain.
	Hash *chainhash.Hash

	// Height is the height of the latest block to be added to the tip of
	// the main chain.
	Height int32
}

// BlockEpochEvent encapsulates an on-going stream of block epoch
// notifications. Its only field 'Epochs' will be sent upon for each new block
// connected to the main-chain.
//
// NOTE: If the caller wishes to cancel their registered block epoch
// notification, the Cancel closure MUST be called.
type BlockEpochEvent struct {
	// Epochs is a receive only channel that will be sent upon each time a
	// new block is connected to the end of the main chain.
	Epochs <-chan *BlockEpoch // MUST be buffered.

	// Cancel is a closure that should be executed by the caller in the
	// case that they wish to abandon their registered spend notification.
	Cancel func()
}

// NotifierDriver represents a "driver" for a particular interface. A driver is
// identified by a globally unique string identifier along with a 'New()'
// method which is responsible for initializing a particular ChainNotifier
// concrete implementation.
type NotifierDriver struct {
	// NotifierType is a string which uniquely identifies the ChainNotifier
	// that this driver, drives.
	NotifierType string

	// New creates a new instance of a concrete ChainNotifier
	// implementation given a variadic set up arguments. The function takes
	// a variadic number of interface parameters in order to provide
	// initialization flexibility, thereby accommodating several potential
	// ChainNotifier implementations.
	New func(args ...interface{}) (ChainNotifier, error)
}

var (
	notifiers   = make(map[string]*NotifierDriver)
	registerMtx sync.Mutex
)

// RegisteredNotifiers returns a slice of all currently registered notifiers.
//
// NOTE: This function is safe for concurrent access.
func RegisteredNotifiers() []*NotifierDriver {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	drivers := make([]*NotifierDriver, 0, len(notifiers))
	for _, driver := range notifiers {
		drivers = append(drivers, driver)
	}

	return drivers
}

// RegisterNotifier registers a NotifierDriver which is capable of driving a
// concrete ChainNotifier interface. In the case that this driver has already
// been registered, an error is returned.
//
// NOTE: This function is safe for concurrent access.
func RegisterNotifier(driver *NotifierDriver) error {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	if _, ok := notifiers[driver.NotifierType]; ok {
		return fmt.Errorf("notifier already registered")
	}

	notifiers[driver.NotifierType] = driver

	return nil
}

// SupportedNotifiers returns a slice of strings that represent the database
// drivers that have been registered and are therefore supported.
//
// NOTE: This function is safe for concurrent access.
func SupportedNotifiers() []string {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	supportedNotifiers := make([]string, 0, len(notifiers))
	for driverName := range notifiers {
		supportedNotifiers = append(supportedNotifiers, driverName)
	}

	return supportedNotifiers
}
