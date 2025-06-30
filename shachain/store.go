package shachain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// Store is an interface which serves as an abstraction over data structure
// responsible for efficiently storing and restoring of hash secrets by given
// indexes.
//
// Description: The Lightning Network wants a chain of (say 1 million)
// unguessable 256 bit values; we generate them and send them one at a time to
// a remote node.  We don't want the remote node to have to store all the
// values, so it's better if they can derive them once they see them.
type Store interface {
	// LookUp function is used to restore/lookup/fetch the previous secret
	// by its index.
	LookUp(uint64) (*chainhash.Hash, error)

	// AddNextEntry attempts to store the given hash within its internal
	// storage in an efficient manner.
	//
	// NOTE: The hashes derived from the shachain MUST be inserted in the
	// order they're produced by a shachain.Producer.
	AddNextEntry(*chainhash.Hash) error

	// Encode writes a binary serialization of the shachain elements
	// currently saved by implementation of shachain.Store to the passed
	// io.Writer.
	Encode(io.Writer) error
}

// RevocationStore is a concrete implementation of the Store interface. The
// revocation store is able to efficiently store N derived shachain elements in
// a space efficient manner with a space complexity of O(log N). The original
// description of the storage methodology can be found here:
// https://github.com/lightningnetwork/lightning-rfc/blob/master/03-transactions.md#efficient-per-commitment-secret-storage
type RevocationStore struct {
	// lenBuckets stores the number of currently active buckets.
	lenBuckets uint8

	// buckets is an array of elements from which we may derive all
	// previous elements, each bucket corresponds to the element with the
	// particular number of trailing zeros.
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
func NewRevocationStoreFromBytes(r io.Reader) (*RevocationStore, error) {
	store := &RevocationStore{}

	if err := binary.Read(r, binary.BigEndian, &store.lenBuckets); err != nil {
		return nil, err
	}

	for i := uint8(0); i < store.lenBuckets; i++ {
		var hashIndex index
		err := binary.Read(r, binary.BigEndian, &hashIndex)
		if err != nil {
			return nil, err
		}

		var nextHash chainhash.Hash
		if _, err := io.ReadFull(r, nextHash[:]); err != nil {
			return nil, err
		}

		store.buckets[i] = element{
			index: hashIndex,
			hash:  nextHash,
		}
	}

	if err := binary.Read(r, binary.BigEndian, &store.index); err != nil {
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

	return nil, fmt.Errorf("unable to derive hash #%v", ind)
}

// AddNextEntry attempts to store the given hash within its internal storage in
// an efficient manner.
//
// NOTE: The hashes derived from the shachain MUST be inserted in the order
// they're produced by a shachain.Producer.
//
// NOTE: This function is part of the Store interface.
func (store *RevocationStore) AddNextEntry(hash *chainhash.Hash) error {
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
			return errors.New("hash isn't derivable from " +
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

// Encode writes a binary serialization of the shachain elements currently
// saved by implementation of shachain.Store to the passed io.Writer.
//
// NOTE: This function is part of the Store interface.
func (store *RevocationStore) Encode(w io.Writer) error {
	err := binary.Write(w, binary.BigEndian, store.lenBuckets)
	if err != nil {
		return err
	}

	for i := uint8(0); i < store.lenBuckets; i++ {
		element := store.buckets[i]

		err := binary.Write(w, binary.BigEndian, element.index)
		if err != nil {
			return err
		}

		if _, err = w.Write(element.hash[:]); err != nil {
			return err
		}

	}

	return binary.Write(w, binary.BigEndian, store.index)
}
