package htlcswitch

import (
	"bytes"
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
	// metaBucket stores all the meta information concerning the state of
	// the database.
	metaBucket = []byte("metadata")

	// sharedHashBucket is a bucket which houses the first HashPrefixSize
	// bytes of a received HTLC's hashed shared secret as the key and the HTLC's
	// CLTV expiry as the value.
	sharedHashBucket = []byte("shared-hash")

	// batchReplayBucket is a bucket that maps batch identifiers to
	// serialized ReplaySets. This is used to give idempotency in the event
	// that a batch is processed more than once.
	batchReplayBucket = []byte("batch-replay")

	// replaySetExpiryBucket is a bucket that maps each replay set (batch)
	// ID to the maximum expiry height so that entries in batchReplayBucket
	// can be garbage collected.
	replaySetExpiryBucket = []byte("replay-set-expiry")

	// allReplaySetsHaveExpiryKey is a boltdb key. The value is set to 1 if
	// and only if all entries in batchReplayBucket also have a
	// corresponding entry in replaySetExpiryBucket.
	allReplaySetsHaveExpiryKey = []byte("all-replay-sets-have-expiry")

	// MaxNumberOfSetsRemovedPerBlock the maximum number of replay sets
	// removed in one GC run (which happens once per block). As larger nodes
	// may have several hundred million sets that should be garbage
	// collected, working on all of them at once would consume too many
	// system resources.
	MaxNumberOfSetsRemovedPerBlock = 500000
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

	// Initialize the primary buckets used by the decayed log.
	if err := d.initBuckets(); err != nil {
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

// initBuckets initializes the primary buckets used by the decayed log, namely
// the meta bucket, shared hash bucket, the batch replay bucket, and the replay
// set expiry bucket.
func (d *DecayedLog) initBuckets() error {
	return kvdb.Update(d.db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(metaBucket)
		if err != nil {
			return ErrDecayedLogInit
		}

		_, err = tx.CreateTopLevelBucket(sharedHashBucket)
		if err != nil {
			return ErrDecayedLogInit
		}

		_, err = tx.CreateTopLevelBucket(batchReplayBucket)
		if err != nil {
			return ErrDecayedLogInit
		}

		_, err = tx.CreateTopLevelBucket(replaySetExpiryBucket)
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

			numExpiredHashes, err := d.gcExpiredHashes(height)
			if err != nil {
				log.Errorf("unable to expire hashes at "+
					"height=%d", height)
			}

			if numExpiredHashes > 0 {
				log.Infof("Garbage collected %v shared "+
					"secret hashes at height=%v",
					numExpiredHashes, height)
			}

			numExpiredReplaySets, err := d.gcExpiredSets(height)
			if err != nil {
				log.Errorf("unable to expire replay "+
					"sets at height=%d", height)
			}

			if numExpiredReplaySets > 0 {
				log.Infof("Garbage collected %v replay "+
					"sets at height=%v",
					numExpiredReplaySets, height)
			}

		case <-d.quit:
			// Received shutdown request.
			log.Infof("Decaying hash log received " +
				"shutdown request")
			return
		}
	}
}

// gcExpiredHashes purges the sharedHashes bucket of all entries whose CLTV
// expires below the provided height.
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

		// Delete every item in the array. This must be done explicitly
		// outside of the ForEach function for safety reasons.
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

// gcExpiredSets purges the batchReplay bucket of all entries whose CLTV
// expires below the provided height.
func (d *DecayedLog) gcExpiredSets(height uint32) (int, error) {
	breakForEach := errors.New("break out of ForEach")

	numExpiredSets := 0
	err := kvdb.Batch(d.db, func(tx kvdb.RwTx) error {
		// There may be replay sets without expiry information.
		if err := InitReplaySetExpiryForOldSets(tx); err != nil {
			return err
		}
		setExpiries := tx.ReadWriteBucket(replaySetExpiryBucket)

		var expiredSets [][]byte
		err := setExpiries.ForEach(func(id, v []byte) error {
			if numExpiredSets >= MaxNumberOfSetsRemovedPerBlock {
				return breakForEach
			}

			expiryHeight := binary.BigEndian.Uint32(v)
			if expiryHeight < height {
				expiredSets = append(expiredSets, id)
				numExpiredSets++
			}

			return nil
		})
		if err != nil && err.Error() != breakForEach.Error() {
			return err
		}

		if numExpiredSets == 0 {
			return nil
		}

		batchReplay := tx.ReadWriteBucket(batchReplayBucket)

		// Delete every expired replay set and also delete the expiry
		// height for the set. This must be done explicitly outside of
		// the ForEach function for safety reasons.
		for _, id := range expiredSets {
			if err := batchReplay.Delete(id); err != nil {
				return err
			}
			if err := setExpiries.Delete(id); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return 0, err
	}

	return numExpiredSets, nil
}

// InitReplaySetExpiryForOldSets initializes the expiry for replay sets where no
// expiry is known. The maximum expiry height derived from all (possibly
// unrelated) CLTVs persisted in sharedHashBucket is used as an
// over-approximation of the expiry used for the replay sets. This way, the
// garbage collector can remove (possibly very old) sets from batchReplayBucket
// once all currently known CLTVs have timed out.
// Larger nodes may have several hundred million sets without expiry
// information. To avoid overloading these systems, we only work on up to
// MaxNumberOfSetsRemovedPerBlock entries in one invocation of this function.
func InitReplaySetExpiryForOldSets(tx kvdb.RwTx) error {
	if AllReplaySetsHaveExpiry(tx) {
		// There are no (old) replay sets without expiry, and we
		// persist expiry information for all new sets. As such,
		// there is no need to do anything!
		return nil
	}

	// There might old replay set data for which no expiry is registered.
	// Find them, so that we can prepare them for garbage collection.
	batchReplay := tx.ReadBucket(batchReplayBucket)
	setExpiry := tx.ReadBucket(replaySetExpiryBucket)
	var setsWithoutExpiry [][]byte
	numSetsWithoutExpiry := 0

	// As the bucket may contain several hundred million entries, we cannot
	// just load those IDs into memory at once. Instead, we scan
	// sequentially and abort after MaxNumberOfSetsRemovedPerBlock entries
	// without expiry have been found. This way at least a fraction of
	// all old entries will be garbage collected while keeping resource
	// usage limited.
	breakForEachPseudoError := errors.New("break out of ForEach")
	err := batchReplay.ForEach(func(id, v []byte) error {
		if numSetsWithoutExpiry >= MaxNumberOfSetsRemovedPerBlock {
			return breakForEachPseudoError
		}
		if expiry := setExpiry.Get(id); expiry == nil {
			setsWithoutExpiry = append(setsWithoutExpiry, id)
			numSetsWithoutExpiry++
		}

		return nil
	})
	if err != nil && err.Error() != breakForEachPseudoError.Error() {
		return err
	}

	if numSetsWithoutExpiry == 0 {
		log.Info("Expiry information for all replay sets already " +
			"known")
		// Remember that there are no replay sets without expiry. As we
		// store the expiry for new sets, we can stop scanning for old
		// sets.
		meta := tx.ReadWriteBucket(metaBucket)
		err := meta.Put(allReplaySetsHaveExpiryKey, []byte{1})
		if err != nil {
			log.Errorf("Unable to update bucket meta "+
				"data: %v", err)
			return err
		}

		return nil
	}

	// If we found at least one replay set without expiry, we
	// compute the maximum CLTV of all entries in sharedHashBucket
	// and use this value to over-approximate the expiry of the
	// replay sets without expiry.
	maximumCltv, err := GetMaxCltvForSharedHashes(tx, err)
	if err != nil {
		return err
	}
	log.Infof("Setting expiry=%d for %d sets without expiry",
		maximumCltv, numSetsWithoutExpiry)

	// Set the expiry for each of the collected sets to the maximum
	// CLTV so that the garbage collector will remove them
	// eventually.
	var scratch [4]byte
	binary.BigEndian.PutUint32(scratch[:], maximumCltv)

	setExpiryReadWrite := tx.ReadWriteBucket(replaySetExpiryBucket)
	for _, id := range setsWithoutExpiry {
		if err := setExpiryReadWrite.Put(id, scratch[:]); err != nil {
			return err
		}
	}

	return nil
}

// AllReplaySetsHaveExpiry returns true only if for all entries in
// batchReplayBucket we have a corresponding expiry entry in
// replaySetExpiryBucket.
func AllReplaySetsHaveExpiry(tx kvdb.RwTx) bool {
	meta := tx.ReadBucket(metaBucket)
	data := meta.Get(allReplaySetsHaveExpiryKey)
	if data == nil {
		return false
	}

	return data[0] == 1
}

// GetMaxCltvForSharedHashes iterates over all entries in sharedHashBucket and
// returns the maximum CLTV value.
func GetMaxCltvForSharedHashes(tx kvdb.RwTx, err error) (uint32, error) {
	var maximumCltv = uint32(0)
	var sharedHashes = tx.ReadBucket(sharedHashBucket)
	err = sharedHashes.ForEach(func(k, v []byte) error {
		// Deserialize the CLTV value for this entry.
		cltv := binary.BigEndian.Uint32(v)

		if cltv > maximumCltv {
			maximumCltv = cltv
		}

		return nil
	})
	if err != nil {
		log.Errorf("Error reading shared hashes: %v", err)
		return 0, err
	}

	return maximumCltv, nil
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
// NOTE: This method enforces idempotency by writing the replay set obtained
// from the first attempt for a particular batch ID, and decoding the return
// value to subsequent calls. For the indices of the replay set to be aligned
// properly, the batch MUST be constructed identically to the first attempt,
// pruning will cause the indices to become invalid.
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

		// Load the batch replay bucket, which will be used to either
		// retrieve the result of previously processing this batch, or
		// to write the result of this operation.
		batchReplayBkt := tx.ReadWriteBucket(batchReplayBucket)
		if batchReplayBkt == nil {
			return ErrDecayedLogCorrupted
		}

		// Check for the existence of this batch's id in the replay
		// bucket. If a non-nil value is found, this indicates that we
		// have already processed this batch before. We deserialize the
		// resulting and return it to ensure calls to put batch are
		// idempotent.
		replayBytes := batchReplayBkt.Get(b.ID)
		if replayBytes != nil {
			replays = sphinx.NewReplaySet()
			return replays.Decode(bytes.NewReader(replayBytes))
		}

		// The CLTV will be stored into scratch and then stored into the
		// sharedHashBucket.
		var scratch [4]byte
		var maximumExpiryHeight uint32

		replays = sphinx.NewReplaySet()
		err := b.ForEach(func(seqNum uint16, hashPrefix *sphinx.HashPrefix, cltv uint32) error {
			// Compute the maximum expiry height for the set.
			if cltv > maximumExpiryHeight {
				maximumExpiryHeight = cltv
			}

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

		// Load the replay set expiry bucket, which will be used to
		// write the expiry height for the replay set.
		replaySetExpiryBkt := tx.ReadWriteBucket(replaySetExpiryBucket)
		if replaySetExpiryBkt == nil {
			return ErrDecayedLogCorrupted
		}

		// Write the maximum expiry height to the replay set expiry
		// bucket so that batches can be removed once all entries are
		// expired.
		binary.BigEndian.PutUint32(scratch[:], maximumExpiryHeight)
		if err := replaySetExpiryBkt.Put(b.ID, scratch[:]); err != nil {
			return err
		}

		// Write the replay set under the batch identifier to the batch
		// replays bucket. This can be used during recovery to test (1)
		// that a particular batch was successfully processed and (2)
		// recover the indexes of the adds that were rejected as
		// replays.
		var replayBuf bytes.Buffer
		if err := replays.Encode(&replayBuf); err != nil {
			return err
		}

		return batchReplayBkt.Put(b.ID, replayBuf.Bytes())
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
