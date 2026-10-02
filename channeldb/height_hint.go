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

// CommitSpendHints commits the given spend hints to the cache in a single
// transaction.
func (c *HeightHintCache) CommitSpendHints(
	hints chainntnfs.SpendHints) error {

	if len(hints) == 0 {
		return nil
	}

	log.Tracef("Updating spend hints %v", hints)

	return kvdb.Batch(c.db, func(tx kvdb.RwTx) error {
		spendHints := tx.ReadWriteBucket(spendHintBucket)
		if spendHints == nil {
			return chainntnfs.ErrCorruptedHeightHintCache
		}

		for spendRequest, hint := range hints {
			spendHintKey, err := spendHintKey(&spendRequest)
			if err != nil {
				return err
			}

			value, err := encodeHeightHint(hint)
			if err != nil {
				return err
			}
			err = spendHints.Put(spendHintKey, value)
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
	spendRequest chainntnfs.SpendRequest) (chainntnfs.HeightHint, error) {

	var hint chainntnfs.HeightHint
	if c.cfg.QueryDisable {
		log.Debugf("Ignoring spend height hint for %v (height hint "+
			"cache query disabled)", spendRequest)
		return hint, nil
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

		hint, err = decodeHeightHint(spendHint)

		return err
	}, func() {
		hint = chainntnfs.HeightHint{}
	})
	if err != nil {
		return chainntnfs.HeightHint{}, err
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

// CommitConfirmHints commits the given confirm hints to the cache in a single
// transaction.
func (c *HeightHintCache) CommitConfirmHints(
	hints chainntnfs.ConfirmHints) error {

	if len(hints) == 0 {
		return nil
	}

	log.Tracef("Updating confirm hints %v", hints)

	return kvdb.Batch(c.db, func(tx kvdb.RwTx) error {
		confirmHints := tx.ReadWriteBucket(confirmHintBucket)
		if confirmHints == nil {
			return chainntnfs.ErrCorruptedHeightHintCache
		}

		for confRequest, hint := range hints {
			confHintKey, err := confHintKey(&confRequest)
			if err != nil {
				return err
			}

			value, err := encodeHeightHint(hint)
			if err != nil {
				return err
			}
			err = confirmHints.Put(confHintKey, value)
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
	confRequest chainntnfs.ConfRequest) (chainntnfs.HeightHint, error) {

	var hint chainntnfs.HeightHint
	if c.cfg.QueryDisable {
		log.Debugf("Ignoring confirmation height hint for %v (height "+
			"hint cache query disabled)", confRequest)
		return hint, nil
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

		hint, err = decodeHeightHint(confirmHint)

		return err
	}, func() {
		hint = chainntnfs.HeightHint{}
	})
	if err != nil {
		return chainntnfs.HeightHint{}, err
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

// encodeHeightHint serializes a hint as its height followed by its origin.
// The height comes first so that a binary which only reads the leading four
// bytes still recovers it.
func encodeHeightHint(hint chainntnfs.HeightHint) ([]byte, error) {
	var b bytes.Buffer
	if err := WriteElements(&b, hint.Height, hint.Origin); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// decodeHeightHint deserializes a hint written by encodeHeightHint. A legacy
// value holds only the height, and is returned with an unknown (zero) origin.
func decodeHeightHint(value []byte) (chainntnfs.HeightHint, error) {
	var hint chainntnfs.HeightHint
	r := bytes.NewReader(value)
	if err := ReadElement(r, &hint.Height); err != nil {
		return hint, err
	}

	// Legacy hints were written before origins were persisted.
	if r.Len() == 0 {
		return hint, nil
	}

	err := ReadElement(r, &hint.Origin)

	return hint, err
}
