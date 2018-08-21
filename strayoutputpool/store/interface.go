package store

import (
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/coreos/bbolt"

	"github.com/lightningnetwork/lnd/lnwallet"
)

// outputType is type of encoded spendable output, needed to restore
// correct output.
type outputType byte

const (
	outputUndefined   outputType = 0
	outputNurseryKid             = 1
	outputNurseryBaby            = 2
	outputBreached               = 3
	outputContract               = 4
)

// OutputStore is interface of storage for stray outputs pool.
type OutputStore interface {
	// AddStrayOutput add spendable output to persistent storage.
	AddStrayOutput(OutputEntity) error

	// FetchAllStrayOutputs retrieves stray spendable outputs
	// from persistent storage.
	FetchAllStrayOutputs() ([]OutputEntity, error)

	// FetchPublishedOutputs retrieves published outputs from storage.
	FetchPublishedOutputs() ([]PublishedOutputs, error)

	// PublishOutputs marks all spendable outputs as published and
	// add them to storage with raw transaction.
	PublishOutputs(tx *btcutil.Tx, outputs []OutputEntity) (PublishedOutputs, error)

	// Sweep finalises stored published outputs in storage.
	Sweep(published PublishedOutputs) error
}

// OutputEntity is representation of entity for storing spendable outputs.
type OutputEntity interface {
	// ID returns unique identification of output entity stored in database.
	ID() uint64

	// Type returns type of spendable output.
	Type() outputType

	// Output returns spendable output entity.
	Output() lnwallet.SpendableOutput

	// Encode serializes data of spendable output to serial data.
	Encode(w io.Writer) error

	// Decode de-serializes data of spendable output from serial data.
	Decode(r io.Reader) error
}

// PublishedOutputs contains a list of translated to the network outputs.
type PublishedOutputs interface {
	// ID returns unique identification of published outputs stored in database.
	ID() uint64

	// Tx returns published transaction.
	Tx() *wire.MsgTx

	// Outputs returns list of stored outputs that were published.
	Outputs() []OutputEntity

	// Save encodes data and saves in passed bucket using giving transaction.
	Save(*bolt.Bucket, *bolt.Tx) error
}
