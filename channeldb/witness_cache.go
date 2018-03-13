package channeldb

import (
	"crypto/sha256"
	"fmt"

	"github.com/coreos/bbolt"
)

var (
	// ErrNoWitnesses is an error that's returned when no new witnesses have
	// been added to the WitnessCache.
	ErrNoWitnesses = fmt.Errorf("no witnesses")

	// ErrUnknownWitnessType is returned if a caller attempts to
	ErrUnknownWitnessType = fmt.Errorf("unknown witness type")
)

// WitnessType is enum that denotes what "type" of witness is being
// stored/retrieved. As the WitnessCache itself is agnostic and doesn't enforce
// any structure on added witnesses, we use this type to partition the
// witnesses on disk, and also to know how to map a witness to its look up key.
type WitnessType uint8

var (
	// Sha256HashWitness is a witness that is simply the pre image to a
	// hash image. In order to map to its key, we'll use sha256.
	Sha256HashWitness WitnessType = 1
)

// toDBKey is a helper method that maps a witness type to the key that we'll
// use to store it within the database.
func (w WitnessType) toDBKey() ([]byte, error) {
	switch w {

	case Sha256HashWitness:
		return []byte{byte(w)}, nil

	default:
		return nil, ErrUnknownWitnessType
	}
}

var (
	// witnessBucketKey is the name of the bucket that we use to store all
	// witnesses encountered. Within this bucket, we'll create a sub-bucket for
	// each witness type.
	witnessBucketKey = []byte("byte")
)

// WitnessCache is a persistent cache of all witnesses we've encountered on the
// network. In the case of multi-hop, multi-step contracts, a cache of all
// witnesses can be useful in the case of partial contract resolution. If
// negotiations break down, we may be forced to locate the witness for a
// portion of the contract on-chain. In this case, we'll then add that witness
// to the cache so the incoming contract can fully resolve witness.
// Additionally, as one MUST always use a unique witness on the network, we may
// use this cache to detect duplicate witnesses.
//
// TODO(roasbeef): need expiry policy?
//  * encrypt?
type WitnessCache struct {
	db *DB
}

// NewWitnessCache returns a new instance of the witness cache.
func (d *DB) NewWitnessCache() *WitnessCache {
	return &WitnessCache{
		db: d,
	}
}

// AddWitness adds a new witness of wType to the witness cache. The type of the
// witness will be used to map the witness to the key that will be used to look
// it up.
//
// TODO(roasbeef): fake closure to map instead a constructor?
func (w *WitnessCache) AddWitness(wType WitnessType, witness []byte) error {
	return w.db.Batch(func(tx *bolt.Tx) error {
		witnessBucket, err := tx.CreateBucketIfNotExists(witnessBucketKey)
		if err != nil {
			return err
		}

		witnessTypeBucketKey, err := wType.toDBKey()
		if err != nil {
			return err
		}
		witnessTypeBucket, err := witnessBucket.CreateBucketIfNotExists(
			witnessTypeBucketKey,
		)
		if err != nil {
			return err
		}

		// Now that we have the proper bucket for this witness, we'll map the
		// witness type to the proper key.
		var witnessKey []byte
		switch wType {
		case Sha256HashWitness:
			key := sha256.Sum256(witness)
			witnessKey = key[:]
		}

		return witnessTypeBucket.Put(witnessKey, witness)
	})
}

// LookupWitness attempts to lookup a witness according to its type and also
// its witness key. In the case that the witness isn't found, ErrNoWitnesses
// will be returned.
func (w *WitnessCache) LookupWitness(wType WitnessType, witnessKey []byte) ([]byte, error) {
	var witness []byte
	err := w.db.View(func(tx *bolt.Tx) error {
		witnessBucket := tx.Bucket(witnessBucketKey)
		if witnessBucket == nil {
			return ErrNoWitnesses
		}

		witnessTypeBucketKey, err := wType.toDBKey()
		if err != nil {
			return err
		}
		witnessTypeBucket := witnessBucket.Bucket(witnessTypeBucketKey)
		if witnessTypeBucket == nil {
			return ErrNoWitnesses
		}

		dbWitness := witnessTypeBucket.Get(witnessKey)
		if dbWitness == nil {
			return ErrNoWitnesses
		}

		witness = make([]byte, len(dbWitness))
		copy(witness[:], dbWitness)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return witness, nil
}

// DeleteWitness attempts to delete a particular witness from the database.
func (w *WitnessCache) DeleteWitness(wType WitnessType, witnessKey []byte) error {
	return w.db.Batch(func(tx *bolt.Tx) error {
		witnessBucket, err := tx.CreateBucketIfNotExists(witnessBucketKey)
		if err != nil {
			return err
		}

		witnessTypeBucketKey, err := wType.toDBKey()
		if err != nil {
			return err
		}
		witnessTypeBucket, err := witnessBucket.CreateBucketIfNotExists(
			witnessTypeBucketKey,
		)
		if err != nil {
			return err
		}

		return witnessTypeBucket.Delete(witnessKey)
	})
}

// DeleteWitnessClass attempts to delete an *entire* class of witnesses. After
// this function return with a non-nil error,
func (w *WitnessCache) DeleteWitnessClass(wType WitnessType) error {
	return w.db.Batch(func(tx *bolt.Tx) error {
		witnessBucket, err := tx.CreateBucketIfNotExists(witnessBucketKey)
		if err != nil {
			return err
		}

		witnessTypeBucketKey, err := wType.toDBKey()
		if err != nil {
			return err
		}

		return witnessBucket.DeleteBucket(witnessTypeBucketKey)
	})
}
