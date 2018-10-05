package channeldb

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrNoWitnesses is an error that's returned when no new witnesses have
	// been added to the WitnessCache.
	ErrNoWitnesses = fmt.Errorf("no witnesses")

	// ErrUnknownWitnessType is returned if a caller attempts to
	ErrUnknownWitnessType = fmt.Errorf("unknown witness type")

	// ErrNoChannels is an error that's returned when there are no ShortChannelIDs
	// stored in the ChannelBucket.
	ErrNoChannels = fmt.Errorf("no channels or channel states")
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

// ChannelState is an enum that gives us two possible states for a stored
// ShortChannelID to be in. The states are used by implementations of the
// contractcourt.WitnessBeacon to garbage collect old witnesses based on
// this state and some other heuristic.
// TODO - Revise last bit of comment with what we actually decide to do re: gc.
type ChannelState uint16

const (
	// WAITING is the state a ShortChanId is in when it is first added to
	// the ChannelBucket.
	WAITING ChannelState = 0

	// FINALIZED is the state a ShortChanId is in when a witness associated
	// with this ShortChanId has been used in a sweep tx.
	FINALIZED ChannelState = 1
)

var (
	// witnessBucketKey is the name of the bucket that we use to store all
	// witnesses encountered. Within this bucket, we'll create a sub-bucket for
	// each witness type.
	witnessBucketKey = []byte("witness-key")

	// channelBucketKey is a sub-bucket of the witnessTypeBucket that stores
	// channel information. It currently maps shortChanId -> ChannelState.
	channelBucketKey = []byte("channel-key")

	// NOTE: Within each witness type bucket, there is the nested channel
	// bucket which stores channel information and also nested sub-buckets
	// that are keyed by an lnwire.ShortChannelID and maps, in the preimage
	// case, RHash -> R. We key these sub-buckets by the lnwire.ShortChannelID
	// because there can be multiple witnesses we need to clean up for a
	// given channel.
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
// witness and the passed-in chanID will be used to determine which bucket to
// place the witness in and will map the witness to the key that will be used to
// look it up. The chanID will be stored in a separate ChannelBucket that also
// stores channel info (currently state).
//
// TODO(roasbeef): fake closure to map instead a constructor?
func (w *WitnessCache) AddWitness(wType WitnessType, witness []byte,
	chanID lnwire.ShortChannelID) error {
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

		channelBucket, err := witnessTypeBucket.CreateBucketIfNotExists(
			channelBucketKey,
		)
		if err != nil {
			return err
		}

		var cid [8]byte
		byteOrder.PutUint64(cid, chanID.ToUint64())

		// This is the specific channel bucket that houses all witnesses
		// for a particular channel.
		channelWitnessBucket, err := witnessTypeBucket.CreateBucketIfNotExists(
			cid,
		)

		// Now that we have the proper bucket for this witness, we'll map the
		// witness type to the proper key.
		var witnessKey []byte
		switch wType {
		case Sha256HashWitness:
			key := sha256.Sum256(witness)
			witnessKey = key[:]
		}

		// If ShortChannelID exists, don't replace its ChannelState.
		if channelBucket.Get(cid) == nil {
			// Store the chanID with the default, WAITING state in the channelBucket
			// and store the witness along with its key in the channelWitnessBucket.
			// Also store a block counter starting at 0.
			var writer bytes.Buffer
			if err := WriteElements(&writer, WAITING, uint16(0)); err != nil {
				return err
			}

			if err = channelBucket.Put(cid, writer.Bytes()); err != nil {
				return err
			}
		}

		return channelWitnessBucket.Put(witnessKey, witness)
	})
}

// LookupWitness attempts to lookup a witness according to its type, witness key,
// and the channel's ShortChannelID. If a ShortChannelID is not provided because
// the calling function does not know the ShortChannelID, then this function will
// look in all ChannelWitnessBuckets in an attempt to find the witness. In the
// case that the witness isn't found, ErrNoWitnesses will be returned.
func (w *WitnessCache) LookupWitness(wType WitnessType, witnessKey []byte,
	chanID lnwire.ShortChannelID) ([]byte, error) {
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

		var dbWitness []byte
		if chanID == nil {
			// Loop through all sub-buckets of witnessTypeBucket
			// to find the witness.
			cursor := witnessTypeBucket.Cursor()
			for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
				if v == nil {
					// This is a bucket since there is no value.
					// We create the ChannelWitnessBucket.
					witBucket := witnessTypeBucket.Bucket(k)
					if witBucket == nil {
						continue
					}

					wit := witBucket.Get(witnessKey)
					if wit == nil {
						continue
					} else {
						copy(dbWitness[:], wit)
						break
					}
				}
			}

			// If no witness retrieved, return ErrNoWitnesses.
			if len(dbWitness) == 0 {
				return ErrNoWitnesses
			}
		} else {
			// TODO - Multi-hop payments, ShortChannelID might be wrong?
			// Get the witnessTypeBucket's ChannelWitnessBucket and
			// look to see if the witness is housed in this sub-bucket.
			var cid [8]byte
			byteOrder.PutUint64(cid, chanID.ToUint64())

			channelWitnessBucket := witnessTypeBucket.Bucket(cid)
			if channelWitnessBucket == nil {
				return ErrNoWitnesses
			}

			dbWitness = channelWitnessBucket.Get(witnessKey)
			if dbWitness == nil {
				return ErrNoWitnesses
			}
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

// UpdateChannelCounter updates a channel's block counter with the passed-in variable
// incremented by 1. We use the passed-in variable to save time with a wasteful
// disk lookup.
func (w *WitnessCache) UpdateChannelCounter(wType WitnessType, chanID lnwire.ShortChannelID,
	counter uint16) error {
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

		channelBucket, err := witnessTypeBucket.CreateBucketIfNotExists(
			channelBucketKey,
		)
		if err != nil {
			return err
		}

		var cid [8]byte
		byteOrder.PutUint64(cid, chanID.ToUint64())

		var writer bytes.Buffer
		counter++
		if err := WriteElements(&writer, FINALIZED, counter); err != nil {
			return err
		}

		return channelBucket.Put(cid, writer.Bytes())
	})
}

// FinalizeChannelState takes a WitnessType and a ShortChannelID and attempts to
// update the internally stored ChannelState to the FINALIZED state. Returns an
// error if one occurs.
func (w *WitnessCache) FinalizeChannelState(wType WitnessType, chanID lnwire.ShortChannelID) error {
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

		channelBucket, err := witnessTypeBucket.CreateBucketIfNotExists(
			channelBucketKey,
		)
		if err != nil {
			return err
		}

		var cid [8]byte
		byteOrder.PutUint64(cid[:], chanID.ToUint64())

		var writer bytes.Buffer
		if err := WriteElements(&writer, FINALIZED, uint16(0)); err != nil {
			return err
		}

		return channelBucket.Put(cid, writer.Bytes())
	});
}

// FetchAllChannelStates retrieves all ShortChannelIDs and ChannelStates and
// block counters from the ChannelBucket for a given WitnessType. This is used
// by the garbage collector to determine if the ShortChannelID's set of witnesses
// should be collected.
func (w *WitnessCache) FetchAllChannelStates(wType WitnessType) ([]lnwire.ShortChannelID,
	[]ChannelState, []uint16, error) {

	var chanIDs []lnwire.ShortChannelID
	var chanStates []ChannelState
	var counters []uint16
	err := w.db.View(func(tx *bolt.Tx) error {
		witnessBucket := tx.Bucket(witnessBucketKey)
		if witnessBucket == nil {
			return ErrNoChannels
		}

		witnessTypeBucketKey, err := wType.toDBKey()
		if err != nil {
			return err
		}
		witnessTypeBucket := witnessBucket.Bucket(witnessTypeBucketKey)
		if witnessTypeBucket == nil {
			return ErrNoChannels
		}

		channelBucket := witnessTypeBucket.Bucket(channelBucketKey)
		if channelBucket == nil {
			return ErrNoChannels
		}

		// Loop through the bucket and return all ShortChannelIDs with
		// their ChannelStates.
		if err = channelBucket.ForEach(func(id, state []byte) error {

			var chanID lnwire.ShortChannelID
			var chanState ChannelState
			var counter uint16

			r := bytes.NewReader(id)
			if err := ReadElements(r, &chanID); err != nil {
				return err
			}

			r2 := bytes.NewReader(state)
			if err := ReadElements(r2, &chanState, &counter); err != nil {
				return err
			}

			chanIDs = append(chanIDs, chanID)
			chanStates = append(chanStates, chanState)
			counters = append(counters, counter)

			return nil

		}); err != nil {
			return err
		}

		return nil
	})

	return chanIDs, chanStates, counters, err
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

// DeleteChannel deletes a given ShortChannelID from the ChannelBucket and all
// of its associated witnesses in the WitnessTypeBucket sub-bucket keyed by
// this ShortChannelID.
func (w *WitnessCache) DeleteChannel(wType WitnessType, chanID lnwire.ShortChannelID) error {
	return w.db.Batch(func(tx *bolt.Tx) error {
		witnessBucket, err := tx.CreateBucketIfNotExists(witnessBucketKey)
		if err != nil {
			return err
		}

		witnessTypeBucketKey, err := wType.toDBKey()
		if err != nil {
			return err
		}
		witnessTypeBucket, err := witnessBucket.CreateBucketIfNotExists(witnessTypeBucketKey)
		if err != nil {
			return err
		}

		channelBucket, err := witnessTypeBucket.CreateBucketIfNotExists(channelBucketKey)
		if err != nil {
			return err
		}

		var cid [8]byte
		byteOrder.PutUint64(cid, chanID.ToUint64())

		if err := channelBucket.Delete(cid[:]); err != nil {
			return err
		}

		return witnessTypeBucket.DeleteBucket(cid[:])
	})
}
