package store

import (
	"io"

	"github.com/lightningnetwork/lnd/lnwallet"
)

type outputType int

const (
	outputUndefined   outputType = 0
	outputNurseryKid  outputType = 1
	outputNurseryBaby outputType = 2
	outputBreached    outputType = 3
	outputContract    outputType = 4
)

// OutputStore is interface of storage for stray outputs pool.
type OutputStore interface {
	// AddStrayOutput add spendable output to persistent storage.
	AddStrayOutput(OutputEntity) error

	// FetchAllStrayOutputs receives stray spendable outputs
	// from persistent storage.
	FetchAllStrayOutputs() ([]OutputEntity, error)
}

// OutputEntity is representation of entity for storing spendable outputs.
type OutputEntity interface {
	// TxWeight returns weight of spendable output.
	TxWeight() int64

	// OutputType returns type of spendable output.
	OutputType() outputType

	// Output returns spendable output entity.
	Output() lnwallet.SpendableOutput

	// Encode serializes data of spendable output to serial data.
	Encode(w io.Writer) error

	// Decode de-serializes data of spendable output from serial data.
	Decode(r io.Reader) error
}
