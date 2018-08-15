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
	// minimise fee rate to proceed the transaction
	strayOutputBucket = []byte("stray-output")

	// ErrNoStrayOutputCreated is returned when bucket of stray outputs
	// hasn't been created.
	ErrNoStrayOutputCreated = fmt.Errorf("there are no existing stray outputs")

	byteOrder = binary.BigEndian
)

type outputdb struct {
	db *channeldb.DB
}

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

// strayOutputEntity
type strayOutputEntity struct {
	// txVSize
	txVSize int64

	// totalAmt
	oType outputType

	// outputs
	output lnwallet.SpendableOutput
}

func (s *strayOutputEntity) TxVSize() int64 {
	return s.txVSize
}

func (s *strayOutputEntity) OutputType() outputType {
	return s.oType
}

func (s *strayOutputEntity) Output() lnwallet.SpendableOutput {
	return s.output
}


// Encode
func (s *strayOutputEntity) Encode(w io.Writer) error {
	var scratch [8]byte

	byteOrder.PutUint64(scratch[:], uint64(s.txVSize))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(s.oType))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	return s.output.Encode(w)
}

// Decode
func (s *strayOutputEntity) Decode(r io.Reader) error {
	var (
		scratch [8]byte
		err     error
	)

	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	s.txVSize = int64(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	s.oType = outputType(byteOrder.Uint64(scratch[:]))

	switch s.oType {
	case outputContract:
		s.output, err = contractcourt.NewDecodedContractOutput(r)

	case outputNurseryBaby:
		s.output, err = nursery.NewDecodedBabyOutput(r)

	case outputNurseryKid:
		s.output, err = nursery.NewDecodedKidOutput(r)

	case outputBreached:
		s.output, err = breacharbiter.NewDecodedBreachedOutput(r)
	}

	return err
}
