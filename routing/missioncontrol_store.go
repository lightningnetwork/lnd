package routing

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// resultsKey is the fixed key under which the attempt results are
	// stored.
	resultsKey = []byte("missioncontrol-results")

	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian
)

const (
	// unknownFailureSourceIdx is the database encoding of an unknown error
	// source.
	unknownFailureSourceIdx = -1
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

	if err := serializeRoute(&b, rp.route); err != nil {
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

// deserializeRoute deserializes the mcRoute from the given io.Reader.
func deserializeRoute(r io.Reader) (*mcRoute, error) {
	var rt mcRoute
	if err := channeldb.ReadElements(r, &rt.totalAmount); err != nil {
		return nil, err
	}

	var pub []byte
	if err := channeldb.ReadElements(r, &pub); err != nil {
		return nil, err
	}
	copy(rt.sourcePubKey[:], pub)

	var numHops uint32
	if err := channeldb.ReadElements(r, &numHops); err != nil {
		return nil, err
	}

	var hops []*mcHop
	for i := uint32(0); i < numHops; i++ {
		hop, err := deserializeHop(r)
		if err != nil {
			return nil, err
		}
		hops = append(hops, hop)
	}
	rt.hops = hops

	return &rt, nil
}

// deserializeHop deserializes the mcHop from the given io.Reader.
func deserializeHop(r io.Reader) (*mcHop, error) {
	var h mcHop

	var pub []byte
	if err := channeldb.ReadElements(r, &pub); err != nil {
		return nil, err
	}
	copy(h.pubKeyBytes[:], pub)

	if err := channeldb.ReadElements(r,
		&h.channelID, &h.amtToFwd, &h.hasBlindingPoint,
	); err != nil {
		return nil, err
	}

	return &h, nil
}

// serializeRoute serializes a mcRouter and writes the resulting bytes to the
// given io.Writer.
func serializeRoute(w io.Writer, r *mcRoute) error {
	err := channeldb.WriteElements(w, r.totalAmount, r.sourcePubKey[:])
	if err != nil {
		return err
	}

	if err := channeldb.WriteElements(w, uint32(len(r.hops))); err != nil {
		return err
	}

	for _, h := range r.hops {
		if err := serializeHop(w, h); err != nil {
			return err
		}
	}

	return nil
}

// serializeHop serializes a mcHop and writes the resulting bytes to the given
// io.Writer.
func serializeHop(w io.Writer, h *mcHop) error {
	return channeldb.WriteElements(w,
		h.pubKeyBytes[:],
		h.channelID,
		h.amtToFwd,
		h.hasBlindingPoint,
	)
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
	route, err := deserializeRoute(r)
	if err != nil {
		return nil, err
	}
	result.route = route

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
			select {
			case <-timer.C:
			case <-b.done:
				log.Debugf("Stopping mission control store")
			}
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
					select {
					case <-timer.C:
					case <-b.done:
						log.Debugf("Mission control " +
							"store stopped")
					}
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
	copy(keyBytes[16:], rp.route.sourcePubKey[:])

	return keyBytes[:]
}
