package htlcswitch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/kvdb"
)

const (
	// defaultDbDirectory is the default directory where our decayed log
	// will store our (sharedHash, CLTV) key-value pairs.
	defaultDbDirectory = "sharedhashes"
)

var (
	// sharedHashBucket is a bucket which houses the first HashPrefixSize
	// bytes of a received HTLC's hashed shared secret as the key and the HTLC's
	// CLTV expiry as the value.
	sharedHashBucket = []byte("shared-hash")

	// batchReplayBucket was a bucket used for debugging purposes, it will
	// be removed if it exists.
	batchReplayBucket = []byte("batch-replay")
)

var (
	// ErrDecayedLogInit is used to indicate a decayed log failed to create
	// the proper bucketing structure on startup.
	ErrDecayedLogInit = errors.New("unable to initialize decayed log")

	// ErrDecayedLogCorrupted signals that the anticipated bucketing
	// structure has diverged since initialization.
	ErrDecayedLogCorrupted = errors.New("decayed log structure corrupted")
)

// NewBoltBackendCreator returns a function that creates a new bbolt backend for
// the decayed logs database.
func NewBoltBackendCreator(dbPath,
	dbFileName string) func(boltCfg *kvdb.BoltConfig) (kvdb.Backend, error) {

	return func(boltCfg *kvdb.BoltConfig) (kvdb.Backend, error) {
		cfg := &kvdb.BoltBackendConfig{
			DBPath:            dbPath,
			DBFileName:        dbFileName,
			NoFreelistSync:    boltCfg.NoFreelistSync,
			AutoCompact:       boltCfg.AutoCompact,
			AutoCompactMinAge: boltCfg.AutoCompactMinAge,
			DBTimeout:         boltCfg.DBTimeout,
		}

		// Use default path for log database.
		if dbPath == "" {
			cfg.DBPath = defaultDbDirectory
		}

		db, err := kvdb.GetBoltBackend(cfg)
		if err != nil {
			return nil, fmt.Errorf("could not open boltdb: %v", err)
		}

		return db, nil
	}
}

// DecayedLog implements the PersistLog interface. It stores the first
// HashPrefixSize bytes of a sha256-hashed shared secret along with a node's
// CLTV value. It is a decaying log meaning there will be a garbage collector
// to collect entries which are expired according to their stored CLTV value
// and the current block height. DecayedLog wraps boltdb for simplicity and
// batches writes to the database to decrease write contention.
type DecayedLog struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	db kvdb.Backend

	notifier chainntnfs.ChainNotifier

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewDecayedLog creates a new DecayedLog, which caches recently seen hash
// shared secrets. Entries are evicted as their cltv expires using block epochs
// from the given notifier.
func NewDecayedLog(db kvdb.Backend,
	notifier chainntnfs.ChainNotifier) *DecayedLog {

	return &DecayedLog{
		db:       db,
		notifier: notifier,
		quit:     make(chan struct{}),
	}
}

// Start opens the database we will be using to store hashed shared secrets.
// It also starts the garbage collector in a goroutine to remove stale
// database entries.
func (d *DecayedLog) Start() error {
	if !atomic.CompareAndSwapInt32(&d.started, 0, 1) {
		return nil
	}

	// Delete the batchReplayBucket (if it exists). Previously, this bucket
	// was filled, but never read.
	if err := d.deleteBatchReplayBucket(); err != nil {
		return err
	}

	// Initialize the primary bucket used by the decayed log.
	if err := d.initBucket(); err != nil {
		return err
	}

	// Start garbage collector.
	if d.notifier != nil {
		epochClient, err := d.notifier.RegisterBlockEpochNtfn(nil)
		if err != nil {
			return fmt.Errorf("unable to register for epoch "+
				"notifications: %v", err)
		}

		d.wg.Add(1)
		go d.garbageCollector(epochClient)
	}

	return nil
}

// deleteBatchReplayBucket deletes the batchReplayBucket, if it exists.
func (d *DecayedLog) deleteBatchReplayBucket() error {
	return kvdb.Update(d.db, func(tx kvdb.RwTx) error {
		err := tx.DeleteTopLevelBucket(batchReplayBucket)
		if err != nil {
			// ignore non-existing bucket
			return nil
		}

		return nil
	}, func() {})
}

// initBucket initializes the primary bucket used by the decayed log, namely
// the shared hash bucket.
func (d *DecayedLog) initBucket() error {
	return kvdb.Update(d.db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(sharedHashBucket)
		if err != nil {
			return ErrDecayedLogInit
		}

		return nil
	}, func() {})
}

// Stop halts the garbage collector and closes boltdb.
func (d *DecayedLog) Stop() error {
	if !atomic.CompareAndSwapInt32(&d.stopped, 0, 1) {
		return nil
	}

	// Stop garbage collector.
	close(d.quit)

	d.wg.Wait()

	return nil
}

// garbageCollector deletes entries from sharedHashBucket whose expiry height
// has already passed. This function MUST be run as a goroutine.
func (d *DecayedLog) garbageCollector(epochClient *chainntnfs.BlockEpochEvent) {
	defer d.wg.Done()
	defer epochClient.Cancel()

	for {
		select {
		case epoch, ok := <-epochClient.Epochs:
			if !ok {
				// Block epoch was canceled, shutting down.
				log.Infof("Block epoch canceled, " +
					"decaying hash log shutting down")
				return
			}

			// Perform a bout of garbage collection using the
			// epoch's block height.
			height := uint32(epoch.Height)
			numExpired, err := d.gcExpiredHashes(height)
			if err != nil {
				log.Errorf("unable to expire hashes at "+
					"height=%d", height)
			}

			if numExpired > 0 {
				log.Infof("Garbage collected %v shared "+
					"secret hashes at height=%v",
					numExpired, height)
			}

		case <-d.quit:
			// Received shutdown request.
			log.Infof("Decaying hash log received " +
				"shutdown request")
			return
		}
	}
}

// gcExpiredHashes purges the decaying log of all entries whose CLTV expires
// below the provided height.
func (d *DecayedLog) gcExpiredHashes(height uint32) (uint32, error) {
	var numExpiredHashes uint32

	err := kvdb.Batch(d.db, func(tx kvdb.RwTx) error {
		numExpiredHashes = 0

		// Grab the shared hash bucket
		sharedHashes := tx.ReadWriteBucket(sharedHashBucket)
		if sharedHashes == nil {
			return fmt.Errorf("sharedHashBucket " +
				"is nil")
		}

		var expiredCltv [][]byte
		if err := sharedHashes.ForEach(func(k, v []byte) error {
			// Deserialize the CLTV value for this entry.
			cltv := binary.BigEndian.Uint32(v)

			if cltv < height {
				// This CLTV is expired. We must add it to an
				// array which we'll loop over and delete every
				// hash contained from the db.
				expiredCltv = append(expiredCltv, k)
				numExpiredHashes++
			}

			return nil
		}); err != nil {
			return err
		}

		// Delete every item in the array. This must
		// be done explicitly outside of the ForEach
		// function for safety reasons.
		for _, hash := range expiredCltv {
			err := sharedHashes.Delete(hash)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	return numExpiredHashes, nil
}

// Delete removes a <shared secret hash, CLTV> key-pair from the
// sharedHashBucket.
func (d *DecayedLog) Delete(hash *sphinx.HashPrefix) error {
	return kvdb.Batch(d.db, func(tx kvdb.RwTx) error {
		sharedHashes := tx.ReadWriteBucket(sharedHashBucket)
		if sharedHashes == nil {
			return ErrDecayedLogCorrupted
		}

		return sharedHashes.Delete(hash[:])
	})
}

// Get retrieves the CLTV of a processed HTLC given the first 20 bytes of the
// Sha-256 hash of the shared secret.
func (d *DecayedLog) Get(hash *sphinx.HashPrefix) (uint32, error) {
	var value uint32

	err := kvdb.View(d.db, func(tx kvdb.RTx) error {
		// Grab the shared hash bucket which stores the mapping from
		// truncated sha-256 hashes of shared secrets to CLTV's.
		sharedHashes := tx.ReadBucket(sharedHashBucket)
		if sharedHashes == nil {
			return fmt.Errorf("sharedHashes is nil, could " +
				"not retrieve CLTV value")
		}

		// Retrieve the bytes which represents the CLTV
		valueBytes := sharedHashes.Get(hash[:])
		if valueBytes == nil {
			return sphinx.ErrLogEntryNotFound
		}

		// The first 4 bytes represent the CLTV, store it in value.
		value = binary.BigEndian.Uint32(valueBytes)

		return nil
	}, func() {
		value = 0
	})
	if err != nil {
		return value, err
	}

	return value, nil
}

// Put stores a shared secret hash as the key and the CLTV as the value.
func (d *DecayedLog) Put(hash *sphinx.HashPrefix, cltv uint32) error {
	// Optimistically serialize the cltv value into the scratch buffer.
	var scratch [4]byte
	binary.BigEndian.PutUint32(scratch[:], cltv)

	return kvdb.Batch(d.db, func(tx kvdb.RwTx) error {
		sharedHashes := tx.ReadWriteBucket(sharedHashBucket)
		if sharedHashes == nil {
			return ErrDecayedLogCorrupted
		}

		// Check to see if this hash prefix has been recorded before. If
		// a value is found, this packet is being replayed.
		valueBytes := sharedHashes.Get(hash[:])
		if valueBytes != nil {
			return sphinx.ErrReplayedPacket
		}

		return sharedHashes.Put(hash[:], scratch[:])
	})
}

// PutBatch accepts a pending batch of hashed secret entries to write to disk.
// Each hashed secret is inserted with a corresponding time value, dictating
// when the entry will be evicted from the log.
func (d *DecayedLog) PutBatch(b *sphinx.Batch) (*sphinx.ReplaySet, error) {
	// Since batched boltdb txns may be executed multiple times before
	// succeeding, we will create a new replay set for each invocation to
	// avoid any side effects. If the txn is successful, this replay set
	// will be merged with the replay set computed during batch construction
	// to generate the complete replay set. If this batch was previously
	// processed, the replay set will be deserialized from disk.
	var replays *sphinx.ReplaySet
	if err := kvdb.Batch(d.db, func(tx kvdb.RwTx) error {
		sharedHashes := tx.ReadWriteBucket(sharedHashBucket)
		if sharedHashes == nil {
			return ErrDecayedLogCorrupted
		}

		// The CLTV will be stored into scratch and then stored into the
		// sharedHashBucket.
		var scratch [4]byte

		replays = sphinx.NewReplaySet()
		err := b.ForEach(func(seqNum uint16, hashPrefix *sphinx.HashPrefix, cltv uint32) error {
			// Retrieve the bytes which represents the CLTV
			valueBytes := sharedHashes.Get(hashPrefix[:])
			if valueBytes != nil {
				replays.Add(seqNum)
				return nil
			}

			// Serialize the cltv value and write an entry keyed by
			// the hash prefix.
			binary.BigEndian.PutUint32(scratch[:], cltv)
			return sharedHashes.Put(hashPrefix[:], scratch[:])
		})
		if err != nil {
			return err
		}

		// Merge the replay set computed from checking the on-disk
		// entries with the in-batch replays computed during this
		// batch's construction.
		replays.Merge(b.ReplaySet)

		return nil
	}); err != nil {
		return nil, err
	}

	b.ReplaySet = replays
	b.IsCommitted = true

	return replays, nil
}

// A compile-time check to see if DecayedLog adheres to the PersistLog
// interface.
var _ sphinx.ReplayLog = (*DecayedLog)(nil)
