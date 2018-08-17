package store

import (
	"io"

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

// OutputState contains information about state of stored spendable stray
// output.
type OutputState byte

const (
	OutputPending   OutputState = 0
	OutputPublished             = 1
	OutputSwept                 = 2
)

// Prefix returns prefix of state needed to store in database.
func (s OutputState) Prefix() []byte {
	switch s {
	case OutputPending:
		return []byte("pn")

	case OutputPublished:
		return []byte("pb")

	case OutputSwept:
		return []byte("sw")
	}

	return nil
}

// OutputStore is interface of storage for stray outputs pool.
type OutputStore interface {
	// AddStrayOutput add spendable output to persistent storage.
	AddStrayOutput(OutputEntity) error

	// FetchAllStrayOutputs receives stray spendable outputs
	// from persistent storage.
	FetchAllStrayOutputs(state OutputState) ([]OutputEntity, error)

	// ChangeState used for migration states for spendable outputs.
	ChangeState(output OutputEntity, state OutputState) error
}

// OutputEntity is representation of entity for storing spendable outputs.
type OutputEntity interface {
	// ID returns unique ident of output entity stored in database.
	ID() uint64

	// Type returns type of spendable output.
	Type() outputType

	// State contains state information about current spendable stray output,
	// it's: ready, transaction is published, already swept.
	State() OutputState

	// Output returns spendable output entity.
	Output() lnwallet.SpendableOutput

	// Encode serializes data of spendable output to serial data.
	Encode(w io.Writer) error

	// Decode de-serializes data of spendable output from serial data.
	Decode(r io.Reader) error
}
