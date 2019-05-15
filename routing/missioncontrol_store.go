package routing

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/coreos/bbolt"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// attemptsKey is the fixed key under which the attempts are stored.
	attemptsKey = []byte("missioncontrol-attempts")

	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian
)

type missionControlStore interface {
}

type bboltMissionControlStore struct {
	db *bbolt.DB
}

func newMissionControlStore(db *bbolt.DB) *bboltMissionControlStore {
	return &bboltMissionControlStore{
		db: db,
	}
}

// Clear removes all reports from the db.
func (b *bboltMissionControlStore) Clear() error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		return tx.DeleteBucket(attemptsKey)
	})
}

// Fetch returns all reports currently stored in the database.
func (b *bboltMissionControlStore) Fetch() ([]*paymentReport, error) {
	var reports []*paymentReport

	err := b.db.Update(func(tx *bbolt.Tx) error {
		attemptsBucket, err := tx.CreateBucketIfNotExists(attemptsKey)
		if err != nil {
			return fmt.Errorf("cannot create attempts bucket: %v",
				err)
		}

		return attemptsBucket.ForEach(func(k, v []byte) error {
			report := paymentReport{}

			b := bytes.NewReader(v)

			// Read timestamp.
			var timestamp uint64
			err := channeldb.ReadElement(b, &timestamp)
			if err != nil {
				return err
			}
			report.timestamp = time.Unix(int64(timestamp), 0).UTC()

			// Read route.
			route, err := channeldb.DeserializeRoute(b)
			if err != nil {
				return err
			}
			report.route = &route

			// Read error source index.
			var errorSourceIndex int32
			err = channeldb.ReadElement(b, &errorSourceIndex)
			if err != nil {
				return err
			}
			report.errorSourceIndex = int(errorSourceIndex)

			// Read failure.
			report.failure, err = lnwire.DecodeFailure(b, 0)
			if err != nil {
				return err
			}

			// Add to list.
			reports = append(reports, &report)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	log.Debugf("Fetched %v historical reports from db", len(reports))

	return reports, nil
}

// Add adds a new report to the db.
func (b *bboltMissionControlStore) Add(rp *paymentReport) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		attemptsBucket, err := tx.CreateBucketIfNotExists(attemptsKey)
		if err != nil {
			return errors.New("cannot create attempts bucket")
		}

		seq, err := attemptsBucket.NextSequence()
		if err != nil {
			return errors.New("cannot create next sequence number")
		}

		var seqBytes [8]byte
		byteOrder.PutUint64(seqBytes[:], seq)

		var b bytes.Buffer

		// Write timestamp.
		err = channeldb.WriteElements(&b, uint64(rp.timestamp.Unix()))
		if err != nil {
			return err
		}

		// Write route.
		if err := channeldb.SerializeRoute(&b, *rp.route); err != nil {
			return err
		}

		// Write error source.
		// TODO(joostjager): support unknown source.
		err = channeldb.WriteElements(&b, int32(rp.errorSourceIndex))
		if err != nil {
			return err
		}

		// Write failure.
		if err := lnwire.EncodeFailure(&b, rp.failure, 0); err != nil {
			return err
		}

		// Put into attempts bucket.
		return attemptsBucket.Put(seqBytes[:], b.Bytes())
	})
}
