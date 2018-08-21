package store

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"

	"github.com/lightningnetwork/lnd/channeldb"
)

type published struct {
	db *channeldb.DB

	id       uint64
	tx       *wire.MsgTx
	outputs  []OutputEntity
	pkScript lnwire.PkScript
}

// ID returns unique id within database where it is stored.
func (p *published) ID() uint64 {
	return p.id
}

// Tx is raw transaction that was published to the network.
func (p *published) Tx() *wire.MsgTx {
	return p.tx
}

// Outputs returns list of published spendable outputs in one transaction.
func (p *published) Outputs() []OutputEntity {
	return p.outputs
}

// Encode encodes published outputs to serial data.
func (p *published) Encode(w io.Writer) error {
	if err := p.tx.Serialize(w); err != nil {
		return nil
	}

	for _, output := range p.outputs {
		if err := output.Encode(w); err != nil {
			return err
		}
	}

	return nil
}

// Decode decodes current structure from stored serial data.
func (p *published) Decode(r io.Reader) error {
	if err := p.tx.Deserialize(r); err != nil && err != io.EOF {
		return err
	}

	for {
		output := &strayOutputEntity{}

		err := output.Decode(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		p.outputs = append(p.outputs, output)
	}

	return nil
}

// Save saves outputs in given bucket using general database transaction.
func (p *published) Save(outputs *bolt.Bucket, tx *bolt.Tx) error {
	var b bytes.Buffer

	if err := p.Encode(&b); err != nil {
		return err
	}

	outputID, err := outputs.NextSequence()
	if err != nil {
		return err
	}

	outputIDBytes := make([]byte, 8)
	byteOrder.PutUint64(outputIDBytes, outputID)

	return outputs.Put(outputIDBytes, b.Bytes())
}
