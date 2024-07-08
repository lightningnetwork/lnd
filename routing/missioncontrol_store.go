package routing

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// resultsKey is the fixed key under which the attempt results are
	// stored.
	resultsKey = []byte("missioncontrol-results")

	// mcKey is the fixed key under which the mission control data is
	// stored.
	mcKey = []byte("missioncontrol")

	// FetchMissionControlBatchSize is the number of mission control
	// entries to be fetched in a single batch.
	FetchMissionControlBatchSize = 1000

	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian
)

const (
	// unknownFailureSourceIdx is the database encoding of an unknown error
	// source.
	unknownFailureSourceIdx = -1

	// PubKeyCompressedSize is the size of a single compressed sec pub key
	// in bytes.
	PubKeyCompressedSize = 33

	// PubKeyCompressedSizeDouble is the size of compressed sec pub keys
	// for both the source and destination nodes in the mission control
	// data pair.
	PubKeyCompressedSizeDouble = PubKeyCompressedSize * 2
)

// missionControlStore is a bolt db based implementation of a mission control
// store. It stores the raw payment attempt data from which the internal mission
// controls state can be rederived on startup. This allows the mission control
// internal data structure to be changed without requiring a database migration.
// Also changes to mission control parameters can be applied to historical data.
// Finally, it enables importing raw data from an external source.
type missionControlStore struct {
	done chan struct{}
	wg   sync.WaitGroup
	db   kvdb.Backend

	// queueCond is signalled when items are put into the queue.
	queueCond *sync.Cond

	// queue stores all pending payment results not yet added to the store.
	// Access is protected by the queueCond.L mutex.
	queue *list.List

	// keys holds the stored MC store item keys in the order of storage.
	// We use this list when adding/deleting items from the database to
	// avoid cursor use which may be slow in the remote DB case.
	keys *list.List

	// keysMap holds the stored MC store item keys. We use this map to check
	// if a new payment result has already been stored.
	keysMap map[string]struct{}

	// maxRecords is the maximum amount of records we will store in the db.
	maxRecords int

	// flushInterval is the configured interval we use to store new results
	// and delete outdated ones from the db.
	flushInterval time.Duration
}

func newMissionControlStore(db kvdb.Backend, maxRecords int,
	flushInterval time.Duration) (*missionControlStore, error) {

	var (
		keys    *list.List
		keysMap map[string]struct{}
	)

	// Create buckets if not yet existing.
	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		resultsBucket, err := tx.CreateTopLevelBucket(resultsKey)
		if err != nil {
			return fmt.Errorf("cannot create results bucket: %w",
				err)
		}

		// Collect all keys to be able to quickly calculate the
		// difference when updating the DB state.
		c := resultsBucket.ReadCursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			keys.PushBack(string(k))
			keysMap[string(k)] = struct{}{}
		}

		return nil
	}, func() {
		keys = list.New()
		keysMap = make(map[string]struct{})
	})
	if err != nil {
		return nil, err
	}

	log.Infof("Loaded %d mission control entries", len(keysMap))

	return &missionControlStore{
		done:          make(chan struct{}),
		db:            db,
		queueCond:     sync.NewCond(&sync.Mutex{}),
		queue:         list.New(),
		keys:          keys,
		keysMap:       keysMap,
		maxRecords:    maxRecords,
		flushInterval: flushInterval,
	}, nil
}

// clear removes all results from the db.
func (b *missionControlStore) clear() error {
	b.queueCond.L.Lock()
	defer b.queueCond.L.Unlock()

	err := kvdb.Update(b.db, func(tx kvdb.RwTx) error {
		if err := tx.DeleteTopLevelBucket(resultsKey); err != nil {
			return err
		}

		_, err := tx.CreateTopLevelBucket(resultsKey)
		return err
	}, func() {})

	if err != nil {
		return err
	}

	b.queue = list.New()
	return nil
}

// fetchAll returns all results currently stored in the database.
func (b *missionControlStore) fetchAll() ([]*paymentResult, error) {
	var results []*paymentResult

	err := kvdb.View(b.db, func(tx kvdb.RTx) error {
		resultBucket := tx.ReadBucket(resultsKey)
		results = make([]*paymentResult, 0)

		return resultBucket.ForEach(func(k, v []byte) error {
			result, err := deserializeResult(k, v)
			if err != nil {
				return err
			}

			results = append(results, result)

			return nil
		})

	}, func() {
		results = nil
	})
	if err != nil {
		return nil, err
	}

	return results, nil
}

// serializeResult serializes a payment result and returns a key and value byte
// slice to insert into the bucket.
func serializeResult(rp *paymentResult) ([]byte, []byte, error) {
	// Write timestamps, success status, failure source index and route.
	var b bytes.Buffer

	var dbFailureSourceIdx int32
	if rp.failureSourceIdx == nil {
		dbFailureSourceIdx = unknownFailureSourceIdx
	} else {
		dbFailureSourceIdx = int32(*rp.failureSourceIdx)
	}

	err := channeldb.WriteElements(
		&b,
		uint64(rp.timeFwd.UnixNano()),
		uint64(rp.timeReply.UnixNano()),
		rp.success, dbFailureSourceIdx,
	)
	if err != nil {
		return nil, nil, err
	}

	if err := channeldb.SerializeRoute(&b, *rp.route); err != nil {
		return nil, nil, err
	}

	// Write failure. If there is no failure message, write an empty
	// byte slice.
	var failureBytes bytes.Buffer
	if rp.failure != nil {
		err := lnwire.EncodeFailureMessage(&failureBytes, rp.failure, 0)
		if err != nil {
			return nil, nil, err
		}
	}
	err = wire.WriteVarBytes(&b, 0, failureBytes.Bytes())
	if err != nil {
		return nil, nil, err
	}

	// Compose key that identifies this result.
	key := getResultKey(rp)

	return key, b.Bytes(), nil
}

// deserializeResult deserializes a payment result.
func deserializeResult(k, v []byte) (*paymentResult, error) {
	// Parse payment id.
	result := paymentResult{
		id: byteOrder.Uint64(k[8:]),
	}

	r := bytes.NewReader(v)

	// Read timestamps, success status and failure source index.
	var (
		timeFwd, timeReply uint64
		dbFailureSourceIdx int32
	)

	err := channeldb.ReadElements(
		r, &timeFwd, &timeReply, &result.success, &dbFailureSourceIdx,
	)
	if err != nil {
		return nil, err
	}

	// Convert time stamps to local time zone for consistent logging.
	result.timeFwd = time.Unix(0, int64(timeFwd)).Local()
	result.timeReply = time.Unix(0, int64(timeReply)).Local()

	// Convert from unknown index magic number to nil value.
	if dbFailureSourceIdx != unknownFailureSourceIdx {
		failureSourceIdx := int(dbFailureSourceIdx)
		result.failureSourceIdx = &failureSourceIdx
	}

	// Read route.
	route, err := channeldb.DeserializeRoute(r)
	if err != nil {
		return nil, err
	}
	result.route = &route

	// Read failure.
	failureBytes, err := wire.ReadVarBytes(
		r, 0, math.MaxUint16, "failure",
	)
	if err != nil {
		return nil, err
	}
	if len(failureBytes) > 0 {
		result.failure, err = lnwire.DecodeFailureMessage(
			bytes.NewReader(failureBytes), 0,
		)
		if err != nil {
			return nil, err
		}
	}

	return &result, nil
}

// AddResult adds a new result to the db.
func (b *missionControlStore) AddResult(rp *paymentResult) {
	b.queueCond.L.Lock()
	b.queue.PushBack(rp)
	b.queueCond.L.Unlock()

	b.queueCond.Signal()
}

// stop stops the store ticker goroutine.
func (b *missionControlStore) stop() {
	close(b.done)

	b.queueCond.Signal()

	b.wg.Wait()
}

// run runs the MC store ticker goroutine.
func (b *missionControlStore) run() {
	b.wg.Add(1)

	go func() {
		defer b.wg.Done()

		timer := time.NewTimer(b.flushInterval)

		// Immediately stop the timer. It will be started once new
		// items are added to the store. As the doc for time.Timer
		// states, every call to Stop() done on a timer that is not
		// known to have been fired needs to be checked and the timer's
		// channel needs to be drained appropriately. This could happen
		// if the flushInterval is very small (e.g. 1 nanosecond).
		if !timer.Stop() {
			<-timer.C
		}

		for {
			// Wait for the queue to not be empty.
			b.queueCond.L.Lock()
			for b.queue.Front() == nil {
				b.queueCond.Wait()

				select {
				case <-b.done:
					b.queueCond.L.Unlock()

					return
				default:
				}
			}
			b.queueCond.L.Unlock()

			// Restart the timer.
			timer.Reset(b.flushInterval)

			select {
			case <-timer.C:
				if err := b.storeResults(); err != nil {
					log.Errorf("Failed to update mission "+
						"control store: %v", err)
				}

			case <-b.done:
				// Release the timer's resources.
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
		}
	}()
}

// storeResults stores all accumulated results.
func (b *missionControlStore) storeResults() error {
	// We copy a reference to the queue and clear the original queue to be
	// able to release the lock.
	b.queueCond.L.Lock()
	l := b.queue

	if l.Len() == 0 {
		b.queueCond.L.Unlock()

		return nil
	}
	b.queue = list.New()
	b.queueCond.L.Unlock()

	var (
		newKeys    map[string]struct{}
		delKeys    []string
		storeCount int
		pruneCount int
	)

	// Create a deduped list of new entries.
	newKeys = make(map[string]struct{}, l.Len())
	for e := l.Front(); e != nil; e = e.Next() {
		pr, ok := e.Value.(*paymentResult)
		if !ok {
			return fmt.Errorf("wrong type %T (not *paymentResult)",
				e.Value)
		}
		key := string(getResultKey(pr))
		if _, ok := b.keysMap[key]; ok {
			l.Remove(e)
			continue
		}
		if _, ok := newKeys[key]; ok {
			l.Remove(e)
			continue
		}
		newKeys[key] = struct{}{}
	}

	// Create a list of entries to delete.
	toDelete := b.keys.Len() + len(newKeys) - b.maxRecords
	if b.maxRecords > 0 && toDelete > 0 {
		delKeys = make([]string, 0, toDelete)

		// Delete as many as needed from old keys.
		for e := b.keys.Front(); len(delKeys) < toDelete && e != nil; {
			key, ok := e.Value.(string)
			if !ok {
				return fmt.Errorf("wrong type %T (not string)",
					e.Value)
			}
			delKeys = append(delKeys, key)
			e = e.Next()
		}

		// If more deletions are needed, simply do not add from the
		// list of new keys.
		for e := l.Front(); len(delKeys) < toDelete && e != nil; {
			toDelete--
			pr, ok := e.Value.(*paymentResult)
			if !ok {
				return fmt.Errorf("wrong type %T (not "+
					"*paymentResult )", e.Value)
			}
			key := string(getResultKey(pr))
			delete(newKeys, key)
			l.Remove(e)
			e = l.Front()
		}
	}

	err := kvdb.Update(b.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(resultsKey)

		for e := l.Front(); e != nil; e = e.Next() {
			pr, ok := e.Value.(*paymentResult)
			if !ok {
				return fmt.Errorf("wrong type %T (not "+
					"*paymentResult)", e.Value)
			}

			// Serialize result into key and value byte slices.
			k, v, err := serializeResult(pr)
			if err != nil {
				return err
			}

			// Put into results bucket.
			if err := bucket.Put(k, v); err != nil {
				return err
			}

			storeCount++
		}

		// Prune oldest entries.
		for _, key := range delKeys {
			if err := bucket.Delete([]byte(key)); err != nil {
				return err
			}
			pruneCount++
		}

		return nil
	}, func() {
		storeCount, pruneCount = 0, 0
	})

	if err != nil {
		return err
	}

	log.Debugf("Stored mission control results: %d added, %d deleted",
		storeCount, pruneCount)

	// DB Update was successful, update the in-memory cache.
	for _, key := range delKeys {
		delete(b.keysMap, key)
		b.keys.Remove(b.keys.Front())
	}
	for e := l.Front(); e != nil; e = e.Next() {
		pr, ok := e.Value.(*paymentResult)
		if !ok {
			return fmt.Errorf("wrong type %T (not *paymentResult)",
				e.Value)
		}
		key := string(getResultKey(pr))
		b.keys.PushBack(key)
	}

	return nil
}

// getResultKey returns a byte slice representing a unique key for this payment
// result.
func getResultKey(rp *paymentResult) []byte {
	var keyBytes [8 + 8 + 33]byte

	// Identify records by a combination of time, payment id and sender pub
	// key. This allows importing mission control data from an external
	// source without key collisions and keeps the records sorted
	// chronologically.
	byteOrder.PutUint64(keyBytes[:], uint64(rp.timeReply.UnixNano()))
	byteOrder.PutUint64(keyBytes[8:], rp.id)
	copy(keyBytes[16:], rp.route.SourcePubKey[:])

	return keyBytes[:]
}

// persistMCData stores the provided mission control snapshot in the database.
// It creates a top-level bucket if it doesn't exist and inserts key-value pairs
// for each pair of nodes in the snapshot. The keys are formed by concatenating
// the 'From' and 'To' node public keys, and the values are the serialized
// TimedPairResult data.
//
// Params:
// - mc: The mission control snapshot to be persisted.
//
// Returns:
// - error: An error if persisting data to the database fails.
func (b *missionControlStore) persistMCData(mc *MissionControlSnapshot) error {
	err := kvdb.Update(b.db, func(tx kvdb.RwTx) error {
		mcBucket, err := tx.CreateTopLevelBucket(mcKey)
		if err != nil {
			return fmt.Errorf("cannot create mission control "+
				"bucket: %w", err)
		}

		for _, pair := range mc.Pairs {
			// Create the key by concatenating From and To bytes.
			key := append([]byte{}, pair.Pair.From[:]...)
			key = append(key, pair.Pair.To[:]...)

			// Serialize TimedPairResult data.
			data, err := json.Marshal(pair.TimedPairResult)
			if err != nil {
				return err
			}

			// Put the key-value pair into the bucket
			// (will override if key exists).
			err = mcBucket.Put(key, data)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	if err != nil {
		return fmt.Errorf(
			"Error persisting mission control to disk: %w", err,
		)
	}

	return nil
}

// fetchMCData retrieves all mission control snapshots stored in the database.
// It reads from the mission control bucket and deserializes each key-value pair
// into MissionControlPairSnapshot objects. Snapshots are batched based on the
// FetchMissionControlBatchSize constant.
//
// Returns:
//   - []*MissionControlSnapshot: A slice of pointers to MissionControlSnapshot
//     objects containing the retrieved data.
//   - error: An error if fetching data from the database fails.
func (b *missionControlStore) fetchMCData() ([]*MissionControlSnapshot, error) {
	var (
		mcSnapshots []*MissionControlSnapshot
		mcData      []MissionControlPairSnapshot
	)

	err := kvdb.View(b.db, func(tx kvdb.RTx) error {
		mcBucket := tx.ReadBucket(mcKey)
		if mcBucket == nil {
			return nil
		}

		mcSnapshots = make([]*MissionControlSnapshot, 0)
		mcData = make([]MissionControlPairSnapshot, 0)

		err := mcBucket.ForEach(func(k, v []byte) error {
			mc, err := deserializeMCData(k, v)
			if err != nil {
				return err
			}

			mcData = append(mcData, mc)

			// Check if we have reached the configured batch size.
			if len(mcData) == FetchMissionControlBatchSize {
				mcSnapshots = append(
					mcSnapshots,
					&MissionControlSnapshot{Pairs: mcData},
				)
				mcData = make([]MissionControlPairSnapshot, 0)
			}

			return nil
		})
		if err != nil {
			return err
		}

		// Add the remaining data if any.
		if len(mcData) > 0 {
			mcSnapshots = append(
				mcSnapshots,
				&MissionControlSnapshot{Pairs: mcData},
			)
		}

		return nil
	}, func() {
		mcSnapshots, mcData = nil, nil
	})
	if err != nil {
		return nil, err
	}

	return mcSnapshots, nil
}

// resetMCData clears all mission control data from the database.
// It deletes all key-value pairs in the mission control bucket.
//
// Returns:
// - error: An error if resetting the mission control data fails.
func (b *missionControlStore) resetMCData() error {
	err := kvdb.Update(b.db, func(tx kvdb.RwTx) error {
		mcBucket := tx.ReadWriteBucket(mcKey)
		if mcBucket == nil {
			return nil
		}

		// Delete all key-value pairs in the mission control bucket.
		err := mcBucket.ForEach(func(k, v []byte) error {
			return mcBucket.Delete(k)
		})
		if err != nil {
			return fmt.Errorf("failed to delete mission control "+
				"data: %w", err)
		}

		return nil
	}, func() {})
	if err != nil {
		return fmt.Errorf("error resetting mission control "+
			"data: %v", err)
	}

	return nil
}

// deserializeMCData deserializes the provided key and value bytes into a
// MissionControlPairSnapshot object. The key is expected to be a concatenation
// of two 33-byte public keys representing the 'From' and 'To' nodes, and the
// value is the serialized TimedPairResult data.
//
// Params:
// - k: The key bytes representing the 'From' and 'To' nodes.
// - v: The value bytes representing the serialized TimedPairResult data.
//
// Returns:
// - MissionControlPairSnapshot: The deserialized mission control pair snapshot.
// - error: An error if deserialization fails or the key length is invalid.
func deserializeMCData(k, v []byte) (MissionControlPairSnapshot, error) {
	// Assuming the From and To are each 33 bytes.
	if len(k) != PubKeyCompressedSizeDouble {
		return MissionControlPairSnapshot{}, fmt.Errorf("invalid key "+
			"length: expected %d, got %d",
			PubKeyCompressedSizeDouble, len(k))
	}

	// Split the key into From and To.
	from := route.Vertex(k[:PubKeyCompressedSize])
	to := route.Vertex(k[PubKeyCompressedSize:])

	// Deserialize the value into TimedPairResult.
	var timedPairResult TimedPairResult
	err := json.Unmarshal(v, &timedPairResult)
	if err != nil {
		return MissionControlPairSnapshot{}, fmt.Errorf("error "+
			"deserializing TimedPairResult: %w", err)
	}

	return MissionControlPairSnapshot{
		Pair: DirectedNodePair{
			From: from,
			To:   to,
		},
		TimedPairResult: timedPairResult,
	}, nil
}
