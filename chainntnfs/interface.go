package chainntnfs

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

var (
	// ErrChainNotifierShuttingDown is used when we are trying to
	// measure a spend notification when notifier is already stopped.
	ErrChainNotifierShuttingDown = errors.New("chain notifier shutting down")
)

// TxConfStatus denotes the status of a transaction's lookup.
type TxConfStatus uint8

const (
	// TxFoundMempool denotes that the transaction was found within the
	// backend node's mempool.
	TxFoundMempool TxConfStatus = iota

	// TxFoundIndex denotes that the transaction was found within the
	// backend node's txindex.
	TxFoundIndex

	// TxNotFoundIndex denotes that the transaction was not found within the
	// backend node's txindex.
	TxNotFoundIndex

	// TxFoundManually denotes that the transaction was found within the
	// chain by scanning for it manually.
	TxFoundManually

	// TxNotFoundManually denotes that the transaction was not found within
	// the chain by scanning for it manually.
	TxNotFoundManually
)

// String returns the string representation of the TxConfStatus.
func (t TxConfStatus) String() string {
	switch t {
	case TxFoundMempool:
		return "TxFoundMempool"

	case TxFoundIndex:
		return "TxFoundIndex"

	case TxNotFoundIndex:
		return "TxNotFoundIndex"

	case TxFoundManually:
		return "TxFoundManually"

	case TxNotFoundManually:
		return "TxNotFoundManually"

	default:
		return "unknown"
	}
}

// ChainNotifier represents a trusted source to receive notifications concerning
// targeted events on the Bitcoin blockchain. The interface specification is
// intentionally general in order to support a wide array of chain notification
// implementations such as: btcd's websockets notifications, Bitcoin Core's
// ZeroMQ notifications, various Bitcoin API services, Electrum servers, etc.
//
// Concrete implementations of ChainNotifier should be able to support multiple
// concurrent client requests, as well as multiple concurrent notification events.
type ChainNotifier interface {
	// RegisterConfirmationsNtfn registers an intent to be notified once
	// txid reaches numConfs confirmations. We also pass in the pkScript as
	// the default light client instead needs to match on scripts created in
	// the block. If a nil txid is passed in, then not only should we match
	// on the script, but we should also dispatch once the transaction
	// containing the script reaches numConfs confirmations. This can be
	// useful in instances where we only know the script in advance, but not
	// the transaction containing it.
	//
	// The returned ConfirmationEvent should properly notify the client once
	// the specified number of confirmations has been reached for the txid,
	// as well as if the original tx gets re-org'd out of the mainchain. The
	// heightHint parameter is provided as a convenience to light clients.
	// It heightHint denotes the earliest height in the blockchain in which
	// the target txid _could_ have been included in the chain. This can be
	// used to bound the search space when checking to see if a notification
	// can immediately be dispatched due to historical data.
	//
	// NOTE: Dispatching notifications to multiple clients subscribed to
	// the same (txid, numConfs) tuple MUST be supported.
	RegisterConfirmationsNtfn(txid *chainhash.Hash, pkScript []byte,
		numConfs, heightHint uint32) (*ConfirmationEvent, error)

	// RegisterSpendNtfn registers an intent to be notified once the target
	// outpoint is successfully spent within a transaction. The script that
	// the outpoint creates must also be specified. This allows this
	// interface to be implemented by BIP 158-like filtering. If a nil
	// outpoint is passed in, then not only should we match on the script,
	// but we should also dispatch once a transaction spends the output
	// containing said script. This can be useful in instances where we only
	// know the script in advance, but not the outpoint itself.
	//
	// The returned SpendEvent will receive a send on the 'Spend'
	// transaction once a transaction spending the input is detected on the
	// blockchain. The heightHint parameter is provided as a convenience to
	// light clients. It denotes the earliest height in the blockchain in
	// which the target output could have been spent.
	//
	// NOTE: The notification should only be triggered when the spending
	// transaction receives a single confirmation.
	//
	// NOTE: Dispatching notifications to multiple clients subscribed to a
	// spend of the same outpoint MUST be supported.
	RegisterSpendNtfn(outpoint *wire.OutPoint, pkScript []byte,
		heightHint uint32) (*SpendEvent, error)

	// RegisterBlockEpochNtfn registers an intent to be notified of each
	// new block connected to the tip of the main chain. The returned
	// BlockEpochEvent struct contains a channel which will be sent upon
	// for each new block discovered.
	//
	// Clients have the option of passing in their best known block.
	// If they specify a block, the ChainNotifier checks whether the client
	// is behind on blocks. If they are, the ChainNotifier sends a backlog
	// of block notifications for the missed blocks. If they do not provide
	// one, then a notification will be dispatched immediately for the
	// current tip of the chain upon a successful registration.
	RegisterBlockEpochNtfn(*BlockEpoch) (*BlockEpochEvent, error)

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

	// Tx is the transaction for which the notification was requested for.
	Tx *wire.MsgTx
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
//
// NOTE: If the caller wishes to cancel their registered spend notification,
// the Cancel closure MUST be called.
type ConfirmationEvent struct {
	// Confirmed is a channel that will be sent upon once the transaction
	// has been fully confirmed. The struct sent will contain all the
	// details of the channel's confirmation.
	//
	// NOTE: This channel must be buffered.
	Confirmed chan *TxConfirmation

	// Updates is a channel that will sent upon, at every incremental
	// confirmation, how many confirmations are left to declare the
	// transaction as fully confirmed.
	//
	// NOTE: This channel must be buffered with the number of required
	// confirmations.
	Updates chan uint32

	// NegativeConf is a channel that will be sent upon if the transaction
	// confirms, but is later reorged out of the chain. The integer sent
	// through the channel represents the reorg depth.
	//
	// NOTE: This channel must be buffered.
	NegativeConf chan int32

	// Done is a channel that gets sent upon once the confirmation request
	// is no longer under the risk of being reorged out of the chain.
	//
	// NOTE: This channel must be buffered.
	Done chan struct{}

	// Cancel is a closure that should be executed by the caller in the case
	// that they wish to prematurely abandon their registered confirmation
	// notification.
	Cancel func()
}

// NewConfirmationEvent constructs a new ConfirmationEvent with newly opened
// channels.
func NewConfirmationEvent(numConfs uint32, cancel func()) *ConfirmationEvent {
	return &ConfirmationEvent{
		Confirmed:    make(chan *TxConfirmation, 1),
		Updates:      make(chan uint32, numConfs),
		NegativeConf: make(chan int32, 1),
		Done:         make(chan struct{}, 1),
		Cancel:       cancel,
	}
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
	//
	// NOTE: This channel must be buffered.
	Spend chan *SpendDetail

	// Reorg is a channel that will be sent upon once we detect the spending
	// transaction of the outpoint in question has been reorged out of the
	// chain.
	//
	// NOTE: This channel must be buffered.
	Reorg chan struct{}

	// Done is a channel that gets sent upon once the confirmation request
	// is no longer under the risk of being reorged out of the chain.
	//
	// NOTE: This channel must be buffered.
	Done chan struct{}

	// Cancel is a closure that should be executed by the caller in the case
	// that they wish to prematurely abandon their registered spend
	// notification.
	Cancel func()
}

// NewSpendEvent constructs a new SpendEvent with newly opened channels.
func NewSpendEvent(cancel func()) *SpendEvent {
	return &SpendEvent{
		Spend:  make(chan *SpendDetail, 1),
		Reorg:  make(chan struct{}, 1),
		Done:   make(chan struct{}, 1),
		Cancel: cancel,
	}
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
	//
	// NOTE: This channel must be buffered.
	Epochs <-chan *BlockEpoch

	// Cancel is a closure that should be executed by the caller in the case
	// that they wish to abandon their registered block epochs notification.
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

// ChainConn enables notifiers to pass in their chain backend to interface
// functions that require it.
type ChainConn interface {
	// GetBlockHeader returns the block header for a hash.
	GetBlockHeader(blockHash *chainhash.Hash) (*wire.BlockHeader, error)

	// GetBlockHeaderVerbose returns the verbose block header for a hash.
	GetBlockHeaderVerbose(blockHash *chainhash.Hash) (
		*btcjson.GetBlockHeaderVerboseResult, error)

	// GetBlockHash returns the hash from a block height.
	GetBlockHash(blockHeight int64) (*chainhash.Hash, error)
}

// GetCommonBlockAncestorHeight takes in:
// (1) the hash of a block that has been reorged out of the main chain
// (2) the hash of the block of the same height from the main chain
// It returns the height of the nearest common ancestor between the two hashes,
// or an error
func GetCommonBlockAncestorHeight(chainConn ChainConn, reorgHash,
	chainHash chainhash.Hash) (int32, error) {

	for reorgHash != chainHash {
		reorgHeader, err := chainConn.GetBlockHeader(&reorgHash)
		if err != nil {
			return 0, fmt.Errorf("unable to get header for hash=%v: %v",
				reorgHash, err)
		}
		chainHeader, err := chainConn.GetBlockHeader(&chainHash)
		if err != nil {
			return 0, fmt.Errorf("unable to get header for hash=%v: %v",
				chainHash, err)
		}
		reorgHash = reorgHeader.PrevBlock
		chainHash = chainHeader.PrevBlock
	}

	verboseHeader, err := chainConn.GetBlockHeaderVerbose(&chainHash)
	if err != nil {
		return 0, fmt.Errorf("unable to get verbose header for hash=%v: %v",
			chainHash, err)
	}

	return verboseHeader.Height, nil
}

// GetClientMissedBlocks uses a client's best block to determine what blocks
// it missed being notified about, and returns them in a slice. Its
// backendStoresReorgs parameter tells it whether or not the notifier's
// chainConn stores information about blocks that have been reorged out of the
// chain, which allows GetClientMissedBlocks to find out whether the client's
// best block has been reorged out of the chain, rewind to the common ancestor
// and return blocks starting right after the common ancestor.
func GetClientMissedBlocks(chainConn ChainConn, clientBestBlock *BlockEpoch,
	notifierBestHeight int32, backendStoresReorgs bool) ([]BlockEpoch, error) {

	startingHeight := clientBestBlock.Height
	if backendStoresReorgs {
		// If a reorg causes the client's best hash to be incorrect,
		// retrieve the closest common ancestor and dispatch
		// notifications from there.
		hashAtBestHeight, err := chainConn.GetBlockHash(
			int64(clientBestBlock.Height))
		if err != nil {
			return nil, fmt.Errorf("unable to find blockhash for "+
				"height=%d: %v", clientBestBlock.Height, err)
		}

		startingHeight, err = GetCommonBlockAncestorHeight(
			chainConn, *clientBestBlock.Hash, *hashAtBestHeight,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to find common ancestor: "+
				"%v", err)
		}
	}

	// We want to start dispatching historical notifications from the block
	// right after the client's best block, to avoid a redundant notification.
	missedBlocks, err := getMissedBlocks(
		chainConn, startingHeight+1, notifierBestHeight+1,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to get missed blocks: %v", err)
	}

	return missedBlocks, nil
}

// RewindChain handles internal state updates for the notifier's TxNotifier It
// has no effect if given a height greater than or equal to our current best
// known height. It returns the new best block for the notifier.
func RewindChain(chainConn ChainConn, txNotifier *TxNotifier,
	currBestBlock BlockEpoch, targetHeight int32) (BlockEpoch, error) {

	newBestBlock := BlockEpoch{
		Height: currBestBlock.Height,
		Hash:   currBestBlock.Hash,
	}

	for height := currBestBlock.Height; height > targetHeight; height-- {
		hash, err := chainConn.GetBlockHash(int64(height - 1))
		if err != nil {
			return newBestBlock, fmt.Errorf("unable to "+
				"find blockhash for disconnected height=%d: %v",
				height, err)
		}

		Log.Infof("Block disconnected from main chain: "+
			"height=%v, sha=%v", height, newBestBlock.Hash)

		err = txNotifier.DisconnectTip(uint32(height))
		if err != nil {
			return newBestBlock, fmt.Errorf("unable to "+
				" disconnect tip for height=%d: %v",
				height, err)
		}
		newBestBlock.Height = height - 1
		newBestBlock.Hash = hash
	}
	return newBestBlock, nil
}

// HandleMissedBlocks is called when the chain backend for a notifier misses a
// series of blocks, handling a reorg if necessary. Its backendStoresReorgs
// parameter tells it whether or not the notifier's chainConn stores
// information about blocks that have been reorged out of the chain, which allows
// HandleMissedBlocks to check whether the notifier's best block has been
// reorged out, and rewind the chain accordingly. It returns the best block for
// the notifier and a slice of the missed blocks. The new best block needs to be
// returned in case a chain rewind occurs and partially completes before
// erroring. In the case where there is no rewind, the notifier's
// current best block is returned.
func HandleMissedBlocks(chainConn ChainConn, txNotifier *TxNotifier,
	currBestBlock BlockEpoch, newHeight int32,
	backendStoresReorgs bool) (BlockEpoch, []BlockEpoch, error) {

	startingHeight := currBestBlock.Height

	if backendStoresReorgs {
		// If a reorg causes our best hash to be incorrect, rewind the
		// chain so our best block is set to the closest common
		// ancestor, then dispatch notifications from there.
		hashAtBestHeight, err :=
			chainConn.GetBlockHash(int64(currBestBlock.Height))
		if err != nil {
			return currBestBlock, nil, fmt.Errorf("unable to find "+
				"blockhash for height=%d: %v",
				currBestBlock.Height, err)
		}

		startingHeight, err = GetCommonBlockAncestorHeight(
			chainConn, *currBestBlock.Hash, *hashAtBestHeight,
		)
		if err != nil {
			return currBestBlock, nil, fmt.Errorf("unable to find "+
				"common ancestor: %v", err)
		}

		currBestBlock, err = RewindChain(chainConn, txNotifier,
			currBestBlock, startingHeight)
		if err != nil {
			return currBestBlock, nil, fmt.Errorf("unable to "+
				"rewind chain: %v", err)
		}
	}

	// We want to start dispatching historical notifications from the block
	// right after our best block, to avoid a redundant notification.
	missedBlocks, err := getMissedBlocks(chainConn, startingHeight+1, newHeight)
	if err != nil {
		return currBestBlock, nil, fmt.Errorf("unable to get missed "+
			"blocks: %v", err)
	}

	return currBestBlock, missedBlocks, nil
}

// getMissedBlocks returns a slice of blocks: [startingHeight, endingHeight)
// fetched from the chain.
func getMissedBlocks(chainConn ChainConn, startingHeight,
	endingHeight int32) ([]BlockEpoch, error) {

	numMissedBlocks := endingHeight - startingHeight
	if numMissedBlocks < 0 {
		return nil, fmt.Errorf("starting height %d is greater than "+
			"ending height %d", startingHeight, endingHeight)
	}

	missedBlocks := make([]BlockEpoch, 0, numMissedBlocks)
	for height := startingHeight; height < endingHeight; height++ {
		hash, err := chainConn.GetBlockHash(int64(height))
		if err != nil {
			return nil, fmt.Errorf("unable to find blockhash for "+
				"height=%d: %v", height, err)
		}
		missedBlocks = append(missedBlocks,
			BlockEpoch{Hash: hash, Height: height})
	}

	return missedBlocks, nil
}
