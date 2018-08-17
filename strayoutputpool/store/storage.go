package store

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/coreos/bbolt"

	"github.com/lightningnetwork/lnd/breacharbiter"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/nursery"
)

var (
	// strayOutputBucket is the name of the bucket within the database that
	// stores all data related to small outputs that should be combined to
	// minimise fee rate to proceed the transaction.
	strayOutputBucket = []byte("stray-output")

	// ErrNoStrayOutputCreated is returned when bucket of stray outputs
	// hasn't been created.
	ErrNoStrayOutputCreated = fmt.Errorf("there are no existing stray outputs")

	// ErrNotSupportedOutputType is returned when we can't recognize type
	// of stored output entity.
	ErrNotSupportedOutputType = fmt.Errorf("undefined type of stray output")

	byteOrder = binary.BigEndian
)

type outputdb struct {
	db *channeldb.DB
}

// NewOutputDB creates new instance of storage for stray outputs.
func NewOutputDB(db *channeldb.DB) OutputStore {
	return &outputdb{db: db}
}

// AddStrayOutput saves serialized stray output to database in order to combine
// them to one transaction to pay fee for one transaction.
func (o *outputdb) AddStrayOutput(output OutputEntity) error {
	var b bytes.Buffer
	if err := output.Encode(&b); err != nil {
		return err
	}

	return o.db.Batch(func(tx *bolt.Tx) error {
		outputs, err := tx.CreateBucketIfNotExists(strayOutputBucket)
		if err != nil {
			return err
		}

		outputID, err := outputs.NextSequence()
		if err != nil {
			return err
		}

		outputIDBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(outputIDBytes, outputID)

		return outputs.Put(outputIDBytes, b.Bytes())
	})
}

// FetchAllStrayOutputs returns all stray outputs in DB.
func (o *outputdb) FetchAllStrayOutputs() ([]OutputEntity, error) {
	var outputs []OutputEntity
	err := o.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(strayOutputBucket)
		if bucket == nil {
			return ErrNoStrayOutputCreated
		}

		return bucket.ForEach(func(k, v []byte) error {
			output := &strayOutputEntity{}
			if err := output.Decode(bytes.NewReader(v)); err != nil {
				return err
			}

			outputs = append(outputs, output)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return outputs, nil
}

// strayOutputEntity contains information about stray spendable output.
type strayOutputEntity struct {
	outputType outputType
	output     lnwallet.SpendableOutput
}

// NewOutputEntity creates new output entity.
func NewOutputEntity(output lnwallet.SpendableOutput) OutputEntity {
	var outputType = outputUndefined

	switch output.(type) {
	case *nursery.KidOutput:
		outputType = outputNurseryKid

	case *nursery.BabyOutput:
		outputType = outputNurseryBaby

	case *breacharbiter.BreachedOutput:
		outputType = outputBreached

	case *contractcourt.ContractOutput:
		outputType = outputContract
	}

	return &strayOutputEntity{
		outputType: outputType,
		output:     output,
	}
}

// OutputType returns type of current output.
func (s *strayOutputEntity) OutputType() outputType {
	return s.outputType
}

// Output returns output entity.
func (s *strayOutputEntity) Output() lnwallet.SpendableOutput {
	return s.output
}

// Encode encodes spendable output to serial data.
func (s *strayOutputEntity) Encode(w io.Writer) error {
	var scratch [8]byte

	byteOrder.PutUint64(scratch[:], uint64(s.outputType))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	return s.output.Encode(w)
}

// Decode decodes spendable output from serial data.
func (s *strayOutputEntity) Decode(r io.Reader) error {
	var scratch [8]byte

	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	s.outputType = outputType(byteOrder.Uint64(scratch[:]))

	var err error
	switch s.outputType {

	case outputContract:
		s.output, err = contractcourt.NewDecodedContractOutput(r)

	case outputNurseryBaby:
		s.output, err = nursery.NewDecodedBabyOutput(r)

	case outputNurseryKid:
		s.output, err = nursery.NewDecodedKidOutput(r)

	case outputBreached:
		s.output, err = breacharbiter.NewDecodedBreachedOutput(r)

	default:
		return ErrNotSupportedOutputType
	}

	return err
}
