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

	// strayOutputStateBucket stores links to keys of strayOutputBucket,
	// where keys are contain prefix build on state of the output.
	strayOutputStateBucket = []byte("stray-output-state")

	// ErrNoStrayOutputCreated is returned when bucket of stray outputs
	// hasn't been created.
	ErrNoStrayOutputCreated = fmt.Errorf("there are no existing stray outputs")

	// ErrNotSupportedOutputType is returned when we can't recognize the type
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

// AddStrayOutput saves serialized stray output to the database in order
// to combine them to one transaction to pay common fee.
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

		states, err := tx.CreateBucketIfNotExists(strayOutputStateBucket)
		if err != nil {
			return err
		}

		outputID, err := outputs.NextSequence()
		if err != nil {
			return err
		}

		stateID, err := states.NextSequence()
		if err != nil {
			return err
		}

		outputIDBytes := make([]byte, 8)
		byteOrder.PutUint64(outputIDBytes, outputID)

		if err := outputs.Put(outputIDBytes, b.Bytes()); err != nil {
			return err
		}

		stateIDBytes := make([]byte, 10)
		copy(stateIDBytes[:2], output.State().Prefix())
		byteOrder.PutUint64(stateIDBytes[2:], stateID)

		return states.Put(stateIDBytes, outputIDBytes)
	})
}

// FetchAllStrayOutputs returns all stray outputs in DB.
func (o *outputdb) FetchAllStrayOutputs(state OutputState) ([]OutputEntity, error) {
	var outputs []OutputEntity
	if err := o.db.View(func(tx *bolt.Tx) error {
		outputBucket := tx.Bucket(strayOutputBucket)
		stateBucket := tx.Bucket(strayOutputStateBucket)
		if outputBucket == nil || stateBucket == nil {
			return nil
		}

		c := stateBucket.Cursor()
		for k, v := c.Seek(state.Prefix()); k != nil &&
			bytes.HasPrefix(k, state.Prefix()); k, v = c.Next() {
			output := &strayOutputEntity{id: byteOrder.Uint64(k)}

			if err := output.Decode(bytes.NewReader(
				outputBucket.Get(v))); err != nil {
				return err
			}

			outputs = append(outputs, output)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return outputs, nil
}

// ChangeState migrates state for passed output entity.
func (o *outputdb) ChangeState(output OutputEntity, state OutputState) error {
	if output.State() == state {
		return nil
	}

	return o.db.Update(func(tx *bolt.Tx) error {
		stateBucket := tx.Bucket(strayOutputStateBucket)
		if stateBucket == nil {
			return ErrNoStrayOutputCreated
		}

		stateIDBytes := make([]byte, 10)
		binary.BigEndian.PutUint64(stateIDBytes[2:], output.ID())

		// Set old prefix to get value of this key that contains.
		copy(stateIDBytes[:2], output.State().Prefix())

		// Get key of strayOutputBucket where contains encoded data
		// with spendable output.
		outputLinkBytes := stateBucket.Get(stateIDBytes)

		// We need to remove previous link to this prefix.
		if err := stateBucket.Delete(stateIDBytes); err != nil {
			return err
		}

		// Set a new changed prefix for specified output entity.
		copy(stateIDBytes[:2], state.Prefix())

		return stateBucket.Put(stateIDBytes, outputLinkBytes)
	})
}

// strayOutputEntity contains information about stray spendable output.
type strayOutputEntity struct {
	id         uint64
	outputType outputType
	state      OutputState
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
		state:      OutputPending,
		output:     output,
	}
}

// ID returns id of current entity stored in database.
func (s *strayOutputEntity) ID() uint64 {
	return s.id
}

// Type returns type of current output.
func (s *strayOutputEntity) Type() outputType {
	return s.outputType
}

// Type returns type of current output.
func (s *strayOutputEntity) State() OutputState {
	return s.state
}

// Output returns output entity.
func (s *strayOutputEntity) Output() lnwallet.SpendableOutput {
	return s.output
}

// Encode encodes spendable output to serial data.
func (s *strayOutputEntity) Encode(w io.Writer) error {
	var scratch [1]byte

	scratch[0] = byte(s.outputType)
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	scratch[0] = byte(s.state)
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	return s.output.Encode(w)
}

// Decode decodes spendable output from serial data.
func (s *strayOutputEntity) Decode(r io.Reader) error {
	var scratch [1]byte

	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	s.outputType = outputType(scratch[0])

	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	s.state = OutputState(scratch[0])

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
