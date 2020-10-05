package routing

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
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
	db         kvdb.Backend
	maxRecords int
	numRecords int
}

func newMissionControlStore(db kvdb.Backend, maxRecords int) (*missionControlStore, error) {
	store := &missionControlStore{
		db:         db,
		maxRecords: maxRecords,
	}

	// Create buckets if not yet existing.
	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		resultsBucket, err := tx.CreateTopLevelBucket(resultsKey)
		if err != nil {
			return fmt.Errorf("cannot create results bucket: %v",
				err)
		}

		// Count initial number of results and track this number in
		// memory to avoid calling Stats().KeyN. The reliability of
		// Stats() is doubtful and seemed to have caused crashes in the
		// past (see #1874).
		c := resultsBucket.ReadCursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			store.numRecords++
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return store, nil
}

// clear removes all results from the db.
func (b *missionControlStore) clear() error {
	return kvdb.Update(b.db, func(tx kvdb.RwTx) error {
		if err := tx.DeleteTopLevelBucket(resultsKey); err != nil {
			return err
		}

		_, err := tx.CreateTopLevelBucket(resultsKey)
		return err
	})
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
		r, 0, lnwire.FailureMessageLength, "failure",
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
func (b *missionControlStore) AddResult(rp *paymentResult) error {
	return kvdb.Update(b.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(resultsKey)

		// Prune oldest entries.
		if b.maxRecords > 0 {
			for b.numRecords >= b.maxRecords {
				cursor := bucket.ReadWriteCursor()
				cursor.First()
				if err := cursor.Delete(); err != nil {
					return err
				}

				b.numRecords--
			}
		}

		// Serialize result into key and value byte slices.
		k, v, err := serializeResult(rp)
		if err != nil {
			return err
		}

		// The store is assumed to be idempotent. It could be that the
		// same result is added twice and in that case the counter
		// shouldn't be increased.
		if bucket.Get(k) == nil {
			b.numRecords++
		}

		// Put into results bucket.
		return bucket.Put(k, v)
	})
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
