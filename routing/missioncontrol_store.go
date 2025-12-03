package routing

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// resultsKey is the fixed key under which the attempt results are
	// stored.
	resultsKey = []byte("missioncontrol-results")

	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian
)

// missionControlDB is an interface that defines the database methods that a
// single missionControlStore has access to. It allows the missionControlStore
// to be unaware of the overall DB structure and restricts its access to the DB
// by only providing it the bucket that it needs to care about.
type missionControlDB interface {
	// update can be used to perform reads and writes on the given bucket.
	update(f func(bkt kvdb.RwBucket) error, reset func()) error

	// view can be used to perform reads on the given bucket.
	view(f func(bkt kvdb.RBucket) error, reset func()) error

	// purge will delete all the contents in this store.
	purge() error
}

// missionControlStore is a bolt db based implementation of a mission control
// store. It stores the raw payment attempt data from which the internal mission
// controls state can be rederived on startup. This allows the mission control
// internal data structure to be changed without requiring a database migration.
// Also changes to mission control parameters can be applied to historical data.
// Finally, it enables importing raw data from an external source.
type missionControlStore struct {
	done chan struct{}
	wg   sync.WaitGroup
	db   missionControlDB

	// TODO(yy): Remove the usage of sync.Cond - we are better off using
	// channes than a Cond as suggested in the official godoc.
	//
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

func newMissionControlStore(db missionControlDB, maxRecords int,
	flushInterval time.Duration) (*missionControlStore, error) {

	var (
		keys    *list.List
		keysMap map[string]struct{}
	)

	// Create buckets if not yet existing.
	err := db.update(func(resultsBucket kvdb.RwBucket) error {
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

	if err := b.db.purge(); err != nil {
		return err
	}

	b.queue = list.New()

	return nil
}

// fetchAll returns all results currently stored in the database.
// It also removes any corrupted entries that fail to deserialize from both
// the database and the in-memory tracking structures.
func (b *missionControlStore) fetchAll() ([]*paymentResult, error) {
	var results []*paymentResult
	var corruptedKeys [][]byte

	// Read all results and identify corrupted entries.
	err := b.db.view(func(resultBucket kvdb.RBucket) error {
		results = make([]*paymentResult, 0)
		corruptedKeys = make([][]byte, 0)

		err := resultBucket.ForEach(func(k, v []byte) error {
			result, err := deserializeResult(k, v)

			// In case of an error, track the key for removal.
			if err != nil {
				log.Warnf("Failed to deserialize mission "+
					"control entry (key=%x): %v", k, err)

				// Make a copy of the key since ForEach reuses
				// the slice.
				keyCopy := make([]byte, len(k))
				copy(keyCopy, k)
				corruptedKeys = append(corruptedKeys, keyCopy)

				return nil
			}

			results = append(results, result)

			return nil
		})
		if err != nil {
			return err
		}

		return nil
	}, func() {
		results = nil
		corruptedKeys = nil
	})
	if err != nil {
		return nil, err
	}

	// Delete corrupted entries from the database which were identified
	// when loading the results from the database.
	//
	// TODO: This code part should eventually be removed once we move the
	// mission control store to a native sql database and have to do a
	// full migration of the data.
	if len(corruptedKeys) > 0 {
		err = b.db.update(func(resultBucket kvdb.RwBucket) error {
			for _, key := range corruptedKeys {
				if err := resultBucket.Delete(key); err != nil {
					return fmt.Errorf("failed to delete "+
						"corrupted entry: %w", err)
				}
			}

			return nil
		}, func() {})
		if err != nil {
			return nil, err
		}

		// Build a set of corrupted keys.
		corruptedSet := make(map[string]struct{}, len(corruptedKeys))
		for _, key := range corruptedKeys {
			corruptedSet[string(key)] = struct{}{}
		}

		// Remove corrupted keys from in-memory map.
		for keyStr := range corruptedSet {
			delete(b.keysMap, keyStr)
		}

		// Remove from the keys list in a single pass.
		for e := b.keys.Front(); e != nil; {
			next := e.Next()
			keyVal, ok := e.Value.(string)
			if ok {
				_, isCorrupted := corruptedSet[keyVal]
				if isCorrupted {
					b.keys.Remove(e)
				}
			}
			e = next
		}

		log.Infof("Removed %d corrupted mission control entries",
			len(corruptedKeys))
	}

	return results, nil
}

// serializeResult serializes a payment result and returns a key and value byte
// slice to insert into the bucket.
func serializeResult(rp *paymentResult) ([]byte, []byte, error) {
	recordProducers := []tlv.RecordProducer{
		&rp.timeFwd,
		&rp.timeReply,
		&rp.route,
	}

	rp.failure.WhenSome(
		func(failure tlv.RecordT[tlv.TlvType3, paymentFailure]) {
			recordProducers = append(recordProducers, &failure)
		},
	)

	// Compose key that identifies this result.
	key := getResultKey(rp)

	var buff bytes.Buffer
	err := lnwire.EncodeRecordsTo(
		&buff, lnwire.ProduceRecordsSorted(recordProducers...),
	)
	if err != nil {
		return nil, nil, err
	}

	return key, buff.Bytes(), nil
}

// deserializeResult deserializes a payment result.
func deserializeResult(k, v []byte) (*paymentResult, error) {
	// Parse payment id.
	result := paymentResult{
		id: byteOrder.Uint64(k[8:]),
	}

	failure := tlv.ZeroRecordT[tlv.TlvType3, paymentFailure]()
	recordProducers := []tlv.RecordProducer{
		&result.timeFwd,
		&result.timeReply,
		&result.route,
		&failure,
	}

	r := bytes.NewReader(v)
	typeMap, err := lnwire.DecodeRecords(
		r, lnwire.ProduceRecordsSorted(recordProducers...)...,
	)
	if err != nil {
		return nil, err
	}

	if _, ok := typeMap[result.failure.TlvType()]; ok {
		result.failure = tlv.SomeRecordT(failure)
	}

	return &result, nil
}

// serializeRoute serializes a mcRoute and writes the resulting bytes to the
// given io.Writer.
func serializeRoute(w io.Writer, r *mcRoute) error {
	records := lnwire.ProduceRecordsSorted(
		&r.sourcePubKey,
		&r.totalAmount,
		&r.hops,
	)

	return lnwire.EncodeRecordsTo(w, records)
}

// deserializeRoute deserializes the mcRoute from the given io.Reader.
func deserializeRoute(r io.Reader) (*mcRoute, error) {
	var rt mcRoute
	records := lnwire.ProduceRecordsSorted(
		&rt.sourcePubKey,
		&rt.totalAmount,
		&rt.hops,
	)

	_, err := lnwire.DecodeRecords(r, records...)
	if err != nil {
		return nil, err
	}

	return &rt, nil
}

// deserializeHop deserializes the mcHop from the given io.Reader.
func deserializeHop(r io.Reader) (*mcHop, error) {
	var (
		h        mcHop
		blinding = tlv.ZeroRecordT[tlv.TlvType3, lnwire.TrueBoolean]()
		custom   = tlv.ZeroRecordT[tlv.TlvType4, lnwire.TrueBoolean]()
	)
	records := lnwire.ProduceRecordsSorted(
		&h.channelID,
		&h.pubKeyBytes,
		&h.amtToFwd,
		&blinding,
		&custom,
	)

	typeMap, err := lnwire.DecodeRecords(r, records...)
	if err != nil {
		return nil, err
	}

	if _, ok := typeMap[h.hasBlindingPoint.TlvType()]; ok {
		h.hasBlindingPoint = tlv.SomeRecordT(blinding)
	}

	if _, ok := typeMap[h.hasCustomRecords.TlvType()]; ok {
		h.hasCustomRecords = tlv.SomeRecordT(custom)
	}

	return &h, nil
}

// serializeHop serializes a mcHop and writes the resulting bytes to the given
// io.Writer.
func serializeHop(w io.Writer, h *mcHop) error {
	recordProducers := []tlv.RecordProducer{
		&h.channelID,
		&h.pubKeyBytes,
		&h.amtToFwd,
	}

	h.hasBlindingPoint.WhenSome(func(
		hasBlinding tlv.RecordT[tlv.TlvType3, lnwire.TrueBoolean]) {

		recordProducers = append(recordProducers, &hasBlinding)
	})

	h.hasCustomRecords.WhenSome(func(
		hasCustom tlv.RecordT[tlv.TlvType4, lnwire.TrueBoolean]) {

		recordProducers = append(recordProducers, &hasCustom)
	})

	return lnwire.EncodeRecordsTo(
		w, lnwire.ProduceRecordsSorted(recordProducers...),
	)
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
				// To make sure we can properly stop, we must
				// read the `done` channel first before
				// attempting to call `Wait()`. This is due to
				// the fact when `Signal` is called before the
				// `Wait` call, the `Wait` call will block
				// indefinitely.
				//
				// TODO(yy): replace this with channels.
				select {
				case <-b.done:
					b.queueCond.L.Unlock()

					return
				default:
				}

				b.queueCond.Wait()
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

	err := b.db.update(func(bucket kvdb.RwBucket) error {
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
	byteOrder.PutUint64(keyBytes[:], rp.timeReply.Val)
	byteOrder.PutUint64(keyBytes[8:], rp.id)
	copy(keyBytes[16:], rp.route.Val.sourcePubKey.Val[:])

	return keyBytes[:]
}

// failureMessage wraps the lnwire.FailureMessage interface such that we can
// apply a Record method and use the failureMessage in a TLV encoded type.
type failureMessage struct {
	lnwire.FailureMessage
}

// Record returns a TLV record that can be used to encode/decode a list of
// failureMessage to/from a TLV stream.
func (r *failureMessage) Record() tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := encodeFailureMessage(&b, r, &buf); err != nil {
			panic(err)
		}

		return uint64(len(b.Bytes()))
	}

	return tlv.MakeDynamicRecord(
		0, r, recordSize, encodeFailureMessage, decodeFailureMessage,
	)
}

func encodeFailureMessage(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*failureMessage); ok {
		var b bytes.Buffer
		err := lnwire.EncodeFailureMessage(&b, v.FailureMessage, 0)
		if err != nil {
			return err
		}

		_, err = w.Write(b.Bytes())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "routing.failureMessage")
}

func decodeFailureMessage(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*failureMessage); ok {
		msg, err := lnwire.DecodeFailureMessage(r, 0)
		if err != nil {
			return err
		}

		*v = failureMessage{
			FailureMessage: msg,
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "routing.failureMessage", l, l)
}
