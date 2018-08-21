package store

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcutil"
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
	strayOutputPublishedBucket = []byte("stray-output-published")

	// strayOutputSweptBucket bucket with the list of stray outputs
	// for which was created sweep transaction and it has enough confirmations.
	strayOutputSweptBucket = []byte("stray-output-swept")

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

		outputID, err := outputs.NextSequence()
		if err != nil {
			return err
		}

		outputIDBytes := make([]byte, 8)
		byteOrder.PutUint64(outputIDBytes, outputID)

		return outputs.Put(outputIDBytes, b.Bytes())
	})
}

// FetchAllStrayOutputs returns stray outputs in DB by prefix.
func (o *outputdb) FetchAllStrayOutputs() ([]OutputEntity, error) {
	var outputs []OutputEntity
	if err := o.db.View(func(tx *bolt.Tx) error {
		outputBucket := tx.Bucket(strayOutputBucket)
		if outputBucket == nil {
			return nil
		}

		return outputBucket.ForEach(func(k, v []byte) error {
			output := &strayOutputEntity{id: byteOrder.Uint64(k)}

			if err := output.Decode(bytes.NewReader(
				outputBucket.Get(v))); err != nil {
				return err
			}

			outputs = append(outputs, output)

			return nil
		})
	}); err != nil {
		return nil, err
	}

	return outputs, nil
}

// FetchPublishedOutputs retrieves published outputs as a raw transaction
// to the network.
func (o *outputdb) FetchPublishedOutputs() ([]PublishedOutputs, error) {
	var outputs []PublishedOutputs
	return outputs, o.db.View(func(tx *bolt.Tx) error {
		pubBucket, err := tx.CreateBucketIfNotExists(strayOutputPublishedBucket)
		if err != nil {
			return err
		}

		return pubBucket.ForEach(func(k, v []byte) error {
			pub := &published{db: o.db, id: byteOrder.Uint64(k)}
			outputs = append(outputs, pub)

			return pub.Decode(bytes.NewReader(v))
		})
	})
}

// PublishOutputs migrates spendable outputs from awaiting list to published
// storage with created raw transaction.
func (o *outputdb) PublishOutputs(tx *btcutil.Tx,
	outputs []OutputEntity) (PublishedOutputs, error) {
	pub := &published{
		db:      o.db,
		tx:      tx.MsgTx(),
		outputs: outputs,
	}

	return pub, o.db.Batch(func(tx *bolt.Tx) error {
		outputsBucket, err := tx.CreateBucketIfNotExists(strayOutputBucket)
		if err != nil {
			return err
		}

		pubBucket, err := tx.CreateBucketIfNotExists(strayOutputPublishedBucket)
		if err != nil {
			return err
		}

		if err := pub.Save(pubBucket, tx); err != nil {
			return err
		}

		outputIDBytes := make([]byte, 8)
		for _, output := range outputs {
			byteOrder.PutUint64(outputIDBytes, output.ID())
			if err := outputsBucket.Delete(outputIDBytes); err != nil {
				return err
			}
		}

		return nil
	})
}

// Sweep marks published sweep transaction with the list of outputs as
// completed after several confirmations on the blockchain.
func (o *outputdb) Sweep(published PublishedOutputs) error {
	return o.db.Batch(func(tx *bolt.Tx) error {
		pubBucket, err := tx.CreateBucketIfNotExists(strayOutputPublishedBucket)
		if err != nil {
			return err
		}

		sweptBucket, err := tx.CreateBucketIfNotExists(strayOutputSweptBucket)
		if err != nil {
			return err
		}

		outputIDBytes := make([]byte, 8)
		byteOrder.PutUint64(outputIDBytes, published.ID())

		if err := pubBucket.Delete(outputIDBytes); err != nil {
			return err
		}

		return published.Save(sweptBucket, tx)
	})
}

// strayOutputEntity contains information about stray spendable output.
type strayOutputEntity struct {
	id         uint64
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

// ID returns id of current entity stored in database.
func (s *strayOutputEntity) ID() uint64 {
	return s.id
}

// Type returns type of current output.
func (s *strayOutputEntity) Type() outputType {
	return s.outputType
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
