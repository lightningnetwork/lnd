package shachain

import (
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// Producer is an interface which serves as an abstraction over the data
// structure responsible for efficiently generating the secrets for a
// particular index based on a root seed.  The generation of secrets should be
// made in such way that secret store might efficiently store and retrieve the
// secrets. This is typically implemented as a tree-based PRF.
type Producer interface {
	// AtIndex produces a secret by evaluating using the initial seed and a
	// particular index.
	AtIndex(uint64) (*chainhash.Hash, error)

	// Encode writes a binary serialization of the Producer implementation
	// to the passed io.Writer.
	Encode(io.Writer) error
}

// RevocationProducer is an implementation of Producer interface using the
// shachain PRF construct. Starting with a single 32-byte element generated
// from a CSPRNG, shachain is able to efficiently generate a nearly unbounded
// number of secrets while maintaining a constant amount of storage. The
// original description of shachain can be found here:
// https://github.com/rustyrussell/ccan/blob/master/ccan/crypto/shachain/design.txt
// with supplementary material here:
// https://github.com/lightningnetwork/lightning-rfc/blob/master/03-transactions.md#per-commitment-secret-requirements
type RevocationProducer struct {
	// root is the element from which we may generate all hashes which
	// corresponds to the index domain [281474976710655,0].
	root *element
}

// A compile time check to ensure RevocationProducer implements the Producer
// interface.
var _ Producer = (*RevocationProducer)(nil)

// NewRevocationProducer creates new instance of shachain producer.
func NewRevocationProducer(root chainhash.Hash) *RevocationProducer {
	return &RevocationProducer{
		root: &element{
			index: rootIndex,
			hash:  root,
		}}
}

// NewRevocationProducerFromBytes deserializes an instance of a
// RevocationProducer encoded in the passed byte slice, returning a fully
// initialized instance of a RevocationProducer.
func NewRevocationProducerFromBytes(data []byte) (*RevocationProducer, error) {
	root, err := chainhash.NewHash(data)
	if err != nil {
		return nil, err
	}

	return &RevocationProducer{
		root: &element{
			index: rootIndex,
			hash:  *root,
		},
	}, nil
}

// AtIndex produces a secret by evaluating using the initial seed and a
// particular index.
//
// NOTE: Part of the Producer interface.
func (p *RevocationProducer) AtIndex(v uint64) (*chainhash.Hash, error) {
	ind := newIndex(v)

	element, err := p.root.derive(ind)
	if err != nil {
		return nil, err
	}

	return &element.hash, nil
}

// Encode writes a binary serialization of the Producer implementation to the
// passed io.Writer.
//
// NOTE: Part of the Producer interface.
func (p *RevocationProducer) Encode(w io.Writer) error {
	_, err := w.Write(p.root.hash[:])
	return err
}
