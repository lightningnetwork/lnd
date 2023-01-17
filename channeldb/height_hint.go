package channeldb

import (
	"bytes"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// spendHintBucket is the name of the bucket which houses the height
	// hint for outpoints. Each height hint represents the earliest height
	// at which its corresponding outpoint could have been spent within.
	spendHintBucket = []byte("spend-hints")

	// confirmHintBucket is the name of the bucket which houses the height
	// hints for transactions. Each height hint represents the earliest
	// height at which its corresponding transaction could have been
	// confirmed within.
	confirmHintBucket = []byte("confirm-hints")
)

// CacheConfig contains the HeightHintCache configuration.
type CacheConfig struct {
	// QueryDisable prevents reliance on the Height Hint Cache.  This is
	// necessary to recover from an edge case when the height recorded in
	// the cache is higher than the actual height of a spend, causing a
	// channel to become "stuck" in a pending close state.
	QueryDisable bool
}

// HeightHintCache is an implementation of the SpendHintCache and
// ConfirmHintCache interfaces backed by a channeldb DB instance where the hints
// will be stored.
type HeightHintCache struct {
	cfg CacheConfig
	db  kvdb.Backend
}

// Compile-time checks to ensure HeightHintCache satisfies the SpendHintCache
// and ConfirmHintCache interfaces.
var _ chainntnfs.SpendHintCache = (*HeightHintCache)(nil)
var _ chainntnfs.ConfirmHintCache = (*HeightHintCache)(nil)

// NewHeightHintCache returns a new height hint cache backed by a database.
func NewHeightHintCache(cfg CacheConfig, db kvdb.Backend) (*HeightHintCache,
	error) {

	cache := &HeightHintCache{cfg, db}
	if err := cache.initBuckets(); err != nil {
		return nil, err
	}

	return cache, nil
}

// initBuckets ensures that the primary buckets used by the circuit are
// initialized so that we can assume their existence after startup.
func (c *HeightHintCache) initBuckets() error {
	return kvdb.Batch(c.db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(spendHintBucket)
		if err != nil {
			return err
		}

		_, err = tx.CreateTopLevelBucket(confirmHintBucket)
		return err
	})
}

// CommitSpendHint commits a spend hint for the outpoints to the cache.
func (c *HeightHintCache) CommitSpendHint(height uint32,
	spendRequests ...chainntnfs.SpendRequest) error {

	if len(spendRequests) == 0 {
		return nil
	}

	log.Tracef("Updating spend hint to height %d for %v", height,
		spendRequests)

	return kvdb.Batch(c.db, func(tx kvdb.RwTx) error {
		spendHints := tx.ReadWriteBucket(spendHintBucket)
		if spendHints == nil {
			return chainntnfs.ErrCorruptedHeightHintCache
		}

		var hint bytes.Buffer
		if err := WriteElement(&hint, height); err != nil {
			return err
		}

		for _, spendRequest := range spendRequests {
			spendRequest := spendRequest
			spendHintKey, err := spendHintKey(&spendRequest)
			if err != nil {
				return err
			}
			err = spendHints.Put(spendHintKey, hint.Bytes())
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// QuerySpendHint returns the latest spend hint for an outpoint.
// ErrSpendHintNotFound is returned if a spend hint does not exist within the
// cache for the outpoint.
func (c *HeightHintCache) QuerySpendHint(
	spendRequest chainntnfs.SpendRequest) (uint32, error) {

	var hint uint32
	if c.cfg.QueryDisable {
		log.Debugf("Ignoring spend height hint for %v (height hint "+
			"cache query disabled)", spendRequest)
		return 0, nil
	}
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		spendHints := tx.ReadBucket(spendHintBucket)
		if spendHints == nil {
			return chainntnfs.ErrCorruptedHeightHintCache
		}

		spendHintKey, err := spendHintKey(&spendRequest)
		if err != nil {
			return err
		}
		spendHint := spendHints.Get(spendHintKey)
		if spendHint == nil {
			return chainntnfs.ErrSpendHintNotFound
		}

		return ReadElement(bytes.NewReader(spendHint), &hint)
	}, func() {
		hint = 0
	})
	if err != nil {
		return 0, err
	}

	return hint, nil
}

// PurgeSpendHint removes the spend hint for the outpoints from the cache.
func (c *HeightHintCache) PurgeSpendHint(
	spendRequests ...chainntnfs.SpendRequest) error {

	if len(spendRequests) == 0 {
		return nil
	}

	log.Tracef("Removing spend hints for %v", spendRequests)

	return kvdb.Batch(c.db, func(tx kvdb.RwTx) error {
		spendHints := tx.ReadWriteBucket(spendHintBucket)
		if spendHints == nil {
			return chainntnfs.ErrCorruptedHeightHintCache
		}

		for _, spendRequest := range spendRequests {
			spendRequest := spendRequest
			spendHintKey, err := spendHintKey(&spendRequest)
			if err != nil {
				return err
			}
			if err := spendHints.Delete(spendHintKey); err != nil {
				return err
			}
		}

		return nil
	})
}

// CommitConfirmHint commits a confirm hint for the transactions to the cache.
func (c *HeightHintCache) CommitConfirmHint(height uint32,
	confRequests ...chainntnfs.ConfRequest) error {

	if len(confRequests) == 0 {
		return nil
	}

	log.Tracef("Updating confirm hints to height %d for %v", height,
		confRequests)

	return kvdb.Batch(c.db, func(tx kvdb.RwTx) error {
		confirmHints := tx.ReadWriteBucket(confirmHintBucket)
		if confirmHints == nil {
			return chainntnfs.ErrCorruptedHeightHintCache
		}

		var hint bytes.Buffer
		if err := WriteElement(&hint, height); err != nil {
			return err
		}

		for _, confRequest := range confRequests {
			confRequest := confRequest
			confHintKey, err := confHintKey(&confRequest)
			if err != nil {
				return err
			}
			err = confirmHints.Put(confHintKey, hint.Bytes())
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// QueryConfirmHint returns the latest confirm hint for a transaction hash.
// ErrConfirmHintNotFound is returned if a confirm hint does not exist within
// the cache for the transaction hash.
func (c *HeightHintCache) QueryConfirmHint(
	confRequest chainntnfs.ConfRequest) (uint32, error) {

	var hint uint32
	if c.cfg.QueryDisable {
		log.Debugf("Ignoring confirmation height hint for %v (height "+
			"hint cache query disabled)", confRequest)
		return 0, nil
	}
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		confirmHints := tx.ReadBucket(confirmHintBucket)
		if confirmHints == nil {
			return chainntnfs.ErrCorruptedHeightHintCache
		}

		confHintKey, err := confHintKey(&confRequest)
		if err != nil {
			return err
		}
		confirmHint := confirmHints.Get(confHintKey)
		if confirmHint == nil {
			return chainntnfs.ErrConfirmHintNotFound
		}

		return ReadElement(bytes.NewReader(confirmHint), &hint)
	}, func() {
		hint = 0
	})
	if err != nil {
		return 0, err
	}

	return hint, nil
}

// PurgeConfirmHint removes the confirm hint for the transactions from the
// cache.
func (c *HeightHintCache) PurgeConfirmHint(
	confRequests ...chainntnfs.ConfRequest) error {

	if len(confRequests) == 0 {
		return nil
	}

	log.Tracef("Removing confirm hints for %v", confRequests)

	return kvdb.Batch(c.db, func(tx kvdb.RwTx) error {
		confirmHints := tx.ReadWriteBucket(confirmHintBucket)
		if confirmHints == nil {
			return chainntnfs.ErrCorruptedHeightHintCache
		}

		for _, confRequest := range confRequests {
			confRequest := confRequest
			confHintKey, err := confHintKey(&confRequest)
			if err != nil {
				return err
			}
			if err := confirmHints.Delete(confHintKey); err != nil {
				return err
			}
		}

		return nil
	})
}

// confHintKey returns the key that will be used to index the confirmation
// request's hint within the height hint cache.
func confHintKey(r *chainntnfs.ConfRequest) ([]byte, error) {
	if r.TxID == chainntnfs.ZeroHash {
		return r.PkScript.Script(), nil
	}

	var txid bytes.Buffer
	if err := WriteElement(&txid, r.TxID); err != nil {
		return nil, err
	}

	return txid.Bytes(), nil
}

// spendHintKey returns the key that will be used to index the spend request's
// hint within the height hint cache.
func spendHintKey(r *chainntnfs.SpendRequest) ([]byte, error) {
	if r.OutPoint == chainntnfs.ZeroOutPoint {
		return r.PkScript.Script(), nil
	}

	var outpoint bytes.Buffer
	err := WriteElement(&outpoint, r.OutPoint)
	if err != nil {
		return nil, err
	}

	return outpoint.Bytes(), nil
}
