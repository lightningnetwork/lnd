package chainntnfs

import (
	"bytes"
	"errors"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	bolt "github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
)

const (
	// dbName is the default name of the database storing the height hints.
	dbName = "heighthint.db"

	// dbFilePermission is the default permission of the database file
	// storing the height hints.
	dbFilePermission = 0600
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

	// ErrCorruptedHeightHintCache indicates that the on-disk bucketing
	// structure has altered since the height hint cache instance was
	// initialized.
	ErrCorruptedHeightHintCache = errors.New("height hint cache has been " +
		"corrupted")

	// ErrSpendHintNotFound is an error returned when a spend hint for an
	// outpoint was not found.
	ErrSpendHintNotFound = errors.New("spend hint not found")

	// ErrConfirmHintNotFound is an error returned when a confirm hint for a
	// transaction was not found.
	ErrConfirmHintNotFound = errors.New("confirm hint not found")
)

// SpendHintCache is an interface whose duty is to cache spend hints for
// outpoints. A spend hint is defined as the earliest height in the chain at
// which an outpoint could have been spent within.
type SpendHintCache interface {
	// CommitSpendHint commits a spend hint for the outpoints to the cache.
	CommitSpendHint(height uint32, ops ...wire.OutPoint) error

	// QuerySpendHint returns the latest spend hint for an outpoint.
	// ErrSpendHintNotFound is returned if a spend hint does not exist
	// within the cache for the outpoint.
	QuerySpendHint(op wire.OutPoint) (uint32, error)

	// PurgeSpendHint removes the spend hint for the outpoints from the
	// cache.
	PurgeSpendHint(ops ...wire.OutPoint) error
}

// ConfirmHintCache is an interface whose duty is to cache confirm hints for
// transactions. A confirm hint is defined as the earliest height in the chain
// at which a transaction could have been included in a block.
type ConfirmHintCache interface {
	// CommitConfirmHint commits a confirm hint for the transactions to the
	// cache.
	CommitConfirmHint(height uint32, txids ...chainhash.Hash) error

	// QueryConfirmHint returns the latest confirm hint for a transaction
	// hash. ErrConfirmHintNotFound is returned if a confirm hint does not
	// exist within the cache for the transaction hash.
	QueryConfirmHint(txid chainhash.Hash) (uint32, error)

	// PurgeConfirmHint removes the confirm hint for the transactions from
	// the cache.
	PurgeConfirmHint(txids ...chainhash.Hash) error
}

// HeightHintCache is an implementation of the SpendHintCache and
// ConfirmHintCache interfaces backed by a channeldb DB instance where the hints
// will be stored.
type HeightHintCache struct {
	db *channeldb.DB
}

// Compile-time checks to ensure HeightHintCache satisfies the SpendHintCache
// and ConfirmHintCache interfaces.
var _ SpendHintCache = (*HeightHintCache)(nil)
var _ ConfirmHintCache = (*HeightHintCache)(nil)

// NewHeightHintCache returns a new height hint cache backed by a database.
func NewHeightHintCache(db *channeldb.DB) (*HeightHintCache, error) {
	cache := &HeightHintCache{db}
	if err := cache.initBuckets(); err != nil {
		return nil, err
	}

	return cache, nil
}

// initBuckets ensures that the primary buckets used by the circuit are
// initialized so that we can assume their existence after startup.
func (c *HeightHintCache) initBuckets() error {
	return c.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(spendHintBucket)
		if err != nil {
			return err
		}

		_, err = tx.CreateBucketIfNotExists(confirmHintBucket)
		return err
	})
}

// CommitSpendHint commits a spend hint for the outpoints to the cache.
func (c *HeightHintCache) CommitSpendHint(height uint32, ops ...wire.OutPoint) error {
	if len(ops) == 0 {
		return nil
	}

	Log.Tracef("Updating spend hint to height %d for %v", height, ops)

	return c.db.Batch(func(tx *bolt.Tx) error {
		spendHints := tx.Bucket(spendHintBucket)
		if spendHints == nil {
			return ErrCorruptedHeightHintCache
		}

		var hint bytes.Buffer
		if err := channeldb.WriteElement(&hint, height); err != nil {
			return err
		}

		for _, op := range ops {
			var outpoint bytes.Buffer
			err := channeldb.WriteElement(&outpoint, op)
			if err != nil {
				return err
			}

			err = spendHints.Put(outpoint.Bytes(), hint.Bytes())
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
func (c *HeightHintCache) QuerySpendHint(op wire.OutPoint) (uint32, error) {
	var hint uint32
	err := c.db.View(func(tx *bolt.Tx) error {
		spendHints := tx.Bucket(spendHintBucket)
		if spendHints == nil {
			return ErrCorruptedHeightHintCache
		}

		var outpoint bytes.Buffer
		if err := channeldb.WriteElement(&outpoint, op); err != nil {
			return err
		}

		spendHint := spendHints.Get(outpoint.Bytes())
		if spendHint == nil {
			return ErrSpendHintNotFound
		}

		return channeldb.ReadElement(bytes.NewReader(spendHint), &hint)
	})
	if err != nil {
		return 0, err
	}

	return hint, nil
}

// PurgeSpendHint removes the spend hint for the outpoints from the cache.
func (c *HeightHintCache) PurgeSpendHint(ops ...wire.OutPoint) error {
	if len(ops) == 0 {
		return nil
	}

	Log.Tracef("Removing spend hints for %v", ops)

	return c.db.Batch(func(tx *bolt.Tx) error {
		spendHints := tx.Bucket(spendHintBucket)
		if spendHints == nil {
			return ErrCorruptedHeightHintCache
		}

		for _, op := range ops {
			var outpoint bytes.Buffer
			err := channeldb.WriteElement(&outpoint, op)
			if err != nil {
				return err
			}

			err = spendHints.Delete(outpoint.Bytes())
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// CommitConfirmHint commits a confirm hint for the transactions to the cache.
func (c *HeightHintCache) CommitConfirmHint(height uint32, txids ...chainhash.Hash) error {
	if len(txids) == 0 {
		return nil
	}

	Log.Tracef("Updating confirm hints to height %d for %v", height, txids)

	return c.db.Batch(func(tx *bolt.Tx) error {
		confirmHints := tx.Bucket(confirmHintBucket)
		if confirmHints == nil {
			return ErrCorruptedHeightHintCache
		}

		var hint bytes.Buffer
		if err := channeldb.WriteElement(&hint, height); err != nil {
			return err
		}

		for _, txid := range txids {
			var txHash bytes.Buffer
			err := channeldb.WriteElement(&txHash, txid)
			if err != nil {
				return err
			}

			err = confirmHints.Put(txHash.Bytes(), hint.Bytes())
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
func (c *HeightHintCache) QueryConfirmHint(txid chainhash.Hash) (uint32, error) {
	var hint uint32
	err := c.db.View(func(tx *bolt.Tx) error {
		confirmHints := tx.Bucket(confirmHintBucket)
		if confirmHints == nil {
			return ErrCorruptedHeightHintCache
		}

		var txHash bytes.Buffer
		if err := channeldb.WriteElement(&txHash, txid); err != nil {
			return err
		}

		confirmHint := confirmHints.Get(txHash.Bytes())
		if confirmHint == nil {
			return ErrConfirmHintNotFound
		}

		return channeldb.ReadElement(bytes.NewReader(confirmHint), &hint)
	})
	if err != nil {
		return 0, err
	}

	return hint, nil
}

// PurgeConfirmHint removes the confirm hint for the transactions from the
// cache.
func (c *HeightHintCache) PurgeConfirmHint(txids ...chainhash.Hash) error {
	if len(txids) == 0 {
		return nil
	}

	Log.Tracef("Removing confirm hints for %v", txids)

	return c.db.Batch(func(tx *bolt.Tx) error {
		confirmHints := tx.Bucket(confirmHintBucket)
		if confirmHints == nil {
			return ErrCorruptedHeightHintCache
		}

		for _, txid := range txids {
			var txHash bytes.Buffer
			err := channeldb.WriteElement(&txHash, txid)
			if err != nil {
				return err
			}

			err = confirmHints.Delete(txHash.Bytes())
			if err != nil {
				return err
			}
		}

		return nil
	})
}
