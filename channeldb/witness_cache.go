package channeldb

import (
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
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
//   - encrypt?
type WitnessCache struct {
	db *DB
}

// NewWitnessCache returns a new instance of the witness cache.
func (d *DB) NewWitnessCache() *WitnessCache {
	return &WitnessCache{
		db: d,
	}
}

// witnessEntry is a key-value struct that holds each key -> witness pair, used
// when inserting records into the cache.
type witnessEntry struct {
	key     []byte
	witness []byte
}

// AddSha256Witnesses adds a batch of new sha256 preimages into the witness
// cache. This is an alias for AddWitnesses that uses Sha256HashWitness as the
// preimages' witness type.
func (w *WitnessCache) AddSha256Witnesses(preimages ...lntypes.Preimage) error {
	// Optimistically compute the preimages' hashes before attempting to
	// start the db transaction.
	entries := make([]witnessEntry, 0, len(preimages))
	for i := range preimages {
		hash := preimages[i].Hash()
		entries = append(entries, witnessEntry{
			key:     hash[:],
			witness: preimages[i][:],
		})
	}

	return w.addWitnessEntries(Sha256HashWitness, entries)
}

// addWitnessEntries inserts the witnessEntry key-value pairs into the cache,
// using the appropriate witness type to segment the namespace of possible
// witness types.
func (w *WitnessCache) addWitnessEntries(wType WitnessType,
	entries []witnessEntry) error {

	// Exit early if there are no witnesses to add.
	if len(entries) == 0 {
		return nil
	}

	return kvdb.Batch(w.db.Backend, func(tx kvdb.RwTx) error {
		witnessBucket, err := tx.CreateTopLevelBucket(witnessBucketKey)
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

		for _, entry := range entries {
			err = witnessTypeBucket.Put(entry.key, entry.witness)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// LookupSha256Witness attempts to lookup the preimage for a sha256 hash. If
// the witness isn't found, ErrNoWitnesses will be returned.
func (w *WitnessCache) LookupSha256Witness(hash lntypes.Hash) (lntypes.Preimage, error) {
	witness, err := w.lookupWitness(Sha256HashWitness, hash[:])
	if err != nil {
		return lntypes.Preimage{}, err
	}

	return lntypes.MakePreimage(witness)
}

// lookupWitness attempts to lookup a witness according to its type and also
// its witness key. In the case that the witness isn't found, ErrNoWitnesses
// will be returned.
func (w *WitnessCache) lookupWitness(wType WitnessType, witnessKey []byte) ([]byte, error) {
	var witness []byte
	err := kvdb.View(w.db, func(tx kvdb.RTx) error {
		witnessBucket := tx.ReadBucket(witnessBucketKey)
		if witnessBucket == nil {
			return ErrNoWitnesses
		}

		witnessTypeBucketKey, err := wType.toDBKey()
		if err != nil {
			return err
		}
		witnessTypeBucket := witnessBucket.NestedReadBucket(witnessTypeBucketKey)
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
	}, func() {
		witness = nil
	})
	if err != nil {
		return nil, err
	}

	return witness, nil
}

// DeleteSha256Witness attempts to delete a sha256 preimage identified by hash.
func (w *WitnessCache) DeleteSha256Witness(hash lntypes.Hash) error {
	return w.deleteWitness(Sha256HashWitness, hash[:])
}

// deleteWitness attempts to delete a particular witness from the database.
func (w *WitnessCache) deleteWitness(wType WitnessType, witnessKey []byte) error {
	return kvdb.Batch(w.db.Backend, func(tx kvdb.RwTx) error {
		witnessBucket, err := tx.CreateTopLevelBucket(witnessBucketKey)
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
	return kvdb.Batch(w.db.Backend, func(tx kvdb.RwTx) error {
		witnessBucket, err := tx.CreateTopLevelBucket(witnessBucketKey)
		if err != nil {
			return err
		}

		witnessTypeBucketKey, err := wType.toDBKey()
		if err != nil {
			return err
		}

		return witnessBucket.DeleteNestedBucket(witnessTypeBucketKey)
	})
}
