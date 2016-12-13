package shachain

import (
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// Producer is an interface which serves as an abstraction over data
// structure responsible for efficient generating the secrets by given index.
// The generation of secrets should be made in such way that secret store
// might efficiently store and retrieve the secrets.
type Producer interface {
	// AtIndex produce secret by given index.
	AtIndex(uint64) (*chainhash.Hash, error)

	// ToBytes convert producer to the binary representation.
	ToBytes() ([]byte, error)
}

// RevocationProducer implementation of Producer. This version of shachain
// slightly changed in terms of method naming. Initial concept might be found
// here:
// https://github.com/rustyrussell/ccan/blob/master/ccan/crypto/shachain/design.txt
type RevocationProducer struct {
	// root is the element from which we may generate all hashes which
	// corresponds to the index domain [281474976710655,0].
	root *element
}

// A compile time check to ensure RevocationProducer implements the Producer
// interface.
var _ Producer = (*RevocationProducer)(nil)

// NewRevocationProducer create new instance of shachain producer.
func NewRevocationProducer(root *chainhash.Hash) *RevocationProducer {
	return &RevocationProducer{
		root: &element{
			index: rootIndex,
			hash:  *root,
		}}
}

// NewRevocationProducerFromBytes deserialize an instance of a RevocationProducer
// encoded in the passed byte slice, returning a fully initialize instance of a
// RevocationProducer.
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

// AtIndex produce secret by given index.
// NOTE: Part of the Producer interface.
func (p *RevocationProducer) AtIndex(v uint64) (*chainhash.Hash, error) {
	ind := newIndex(v)

	element, err := p.root.derive(ind)
	if err != nil {
		return nil, err
	}

	return &element.hash, nil
}

// ToBytes convert producer to the binary representation.
// NOTE: Part of the Producer interface.
func (p *RevocationProducer) ToBytes() ([]byte, error) {
	return p.root.hash.CloneBytes(), nil
}
