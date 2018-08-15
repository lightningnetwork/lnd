package strayoutputpool

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// StrayOutputsPool periodically batch outputs into single transaction
// to sweep back into the wallet.
type StrayOutputsPool interface {
	// AddSpendableOutputs adds outputs to the pool for late processing.
	AddSpendableOutput(output lnwallet.SpendableOutput) error

	// GenSweepTx generates transaction for all added outputs.
	GenSweepTx() (*btcutil.Tx, error)

	// Sweep generates transaction and broadcast it to the network.
	Sweep() error
}

// StrayOutputsPoolServer is server implementation for running 'Sweep'
// automatically by schedule.
type StrayOutputsPoolServer interface {
	StrayOutputsPool

	// Start launches background processing of scheduled outputs
	Start() error

	// Stop finalises processing
	Stop()
}

// PoolConfig is contextual storage for dependant data for pool
type PoolConfig struct {
	// DB provides access to the stray outputs, allowing the pool
	// to store them persistently
	DB *channeldb.DB

	// Estimator is used by the pool to determine an appropriate
	// fee level when generating, signing, and broadcasting sweep
	// transactions.
	Estimator lnwallet.FeeEstimator

	// GenSweepScript generates the receiving scripts for swept outputs.
	GenSweepScript func() ([]byte, error)

	// Notifier provides a publish/subscribe interface for event driven
	// notifications regarding the confirmation of txids.
	Notifier chainntnfs.ChainNotifier

	// PublishTransaction facilitates the process of broadcasting a
	// transaction to the network.
	PublishTransaction func(*wire.MsgTx) error

	// Signer is used by the pool to generate sweep transactions,
	// which move coins from previously unsuccessfully swept outputs
	// to the user's wallet.
	Signer lnwallet.Signer
}
