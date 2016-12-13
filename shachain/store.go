package shachain

import (
	"bytes"
	"encoding/binary"
	"github.com/go-errors/errors"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// Store is an interface which serves as an abstraction over data structure
// responsible for efficient storing and restoring of hash secrets by given
// indexes.
//
// Description: The Lightning Network wants a chain of (say 1 million)
// unguessable 256 bit values; we generate them and send them one at a time
// to a remote node.  We don't want the remote node to have to store all the
// values, so it's better if they can derive them once they see them.
type Store interface {
	// LookUp function is used to restore/lookup/fetch the previous secret
	// by its index.
	LookUp(uint64) (*chainhash.Hash, error)

	// Store is used to store the given sha hash in efficient manner.
	Store(*chainhash.Hash) error

	// ToBytes convert store to the binary representation.
	ToBytes() ([]byte, error)
}

// RevocationStore implementation of SecretStore. This version of shachain store
// slightly changed in terms of method naming. Initial concept might be found
// here:
// https://github.com/rustyrussell/ccan/blob/master/ccan/crypto/shachain/design.txt
type RevocationStore struct {
	// lenBuckets stores the number of currently active buckets.
	lenBuckets uint8

	// buckets is an array of elements from which we may derive all previous
	// elements, each bucket corresponds to the element index number of
	// trailing zeros.
	buckets [maxHeight]element

	// index is an available index which will be assigned to the new
	// element.
	index index
}

// A compile time check to ensure RevocationStore implements the Store
// interface.
var _ Store = (*RevocationStore)(nil)

// NewRevocationStore creates the new shachain store.
func NewRevocationStore() *RevocationStore {
	return &RevocationStore{
		lenBuckets: 0,
		index:      startIndex,
	}
}

// NewRevocationStoreFromBytes recreates the initial store state from the given
// binary shachain store representation.
func NewRevocationStoreFromBytes(data []byte) (*RevocationStore, error) {
	var err error

	store := &RevocationStore{}
	buf := bytes.NewBuffer(data)

	err = binary.Read(buf, binary.BigEndian, &store.lenBuckets)
	if err != nil {
		return nil, err
	}

	i := uint8(0)
	for ; i < store.lenBuckets; i++ {
		e := &element{}

		err = binary.Read(buf, binary.BigEndian, &e.index)
		if err != nil {
			return nil, err
		}

		hash, err := chainhash.NewHash(buf.Next(chainhash.HashSize))
		if err != nil {
			return nil, err
		}
		e.hash = *hash
		store.buckets[i] = *e
	}

	err = binary.Read(buf, binary.BigEndian, &store.index)
	if err != nil {
		return nil, err
	}

	return store, nil
}

// LookUp function is used to restore/lookup/fetch the previous secret by its
// index. If secret which corresponds to given index was not previously placed
// in store we will not able to derive it and function will fail.
//
// NOTE: This function is part of the Store interface.
func (store *RevocationStore) LookUp(v uint64) (*chainhash.Hash, error) {
	ind := newIndex(v)

	// Trying to derive the index from one of the existing buckets elements.
	for i := uint8(0); i < store.lenBuckets; i++ {
		element, err := store.buckets[i].derive(ind)
		if err != nil {
			continue
		}

		return &element.hash, nil
	}

	return nil, errors.Errorf("unable to derive hash #%v", ind)
}

// Store is used to store the given sha hash in efficient manner. Given hash
// should be computable with previous ones, and derived from the previous index
// otherwise the function will return the error.
//
// NOTE: This function is part of the Store interface.
func (store *RevocationStore) Store(hash *chainhash.Hash) error {
	newElement := &element{
		index: store.index,
		hash:  *hash,
	}

	bucket := countTrailingZeros(newElement.index)

	for i := uint8(0); i < bucket; i++ {
		e, err := newElement.derive(store.buckets[i].index)
		if err != nil {
			return err
		}

		if !e.isEqual(&store.buckets[i]) {
			return errors.New("hash isn't deriavable from " +
				"previous ones")
		}
	}

	store.buckets[bucket] = *newElement
	if bucket+1 > store.lenBuckets {
		store.lenBuckets = bucket + 1
	}

	store.index--
	return nil
}

// ToBytes convert store to the binary representation.
// NOTE: This function is part of the Store interface.
func (store *RevocationStore) ToBytes() ([]byte, error) {
	var buf bytes.Buffer
	var err error

	err = binary.Write(&buf, binary.BigEndian, store.lenBuckets)
	if err != nil {
		return nil, err
	}

	i := uint8(0)
	for ; i < store.lenBuckets; i++ {
		element := store.buckets[i]

		err = binary.Write(&buf, binary.BigEndian, element.index)
		if err != nil {
			return nil, err
		}

		_, err = buf.Write(element.hash.CloneBytes())
		if err != nil {
			return nil, err
		}

	}

	err = binary.Write(&buf, binary.BigEndian, store.index)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
