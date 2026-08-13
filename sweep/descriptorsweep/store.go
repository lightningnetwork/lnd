package descriptorsweep

import (
	"bytes"
	"encoding/gob"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

var descriptorSweepBucket = []byte("descriptor-sweep-registrations")

const descriptorSweepStoreVersion byte = 2

type store struct {
	db kvdb.Backend
}

type recordStore interface {
	init() error
	put(*storedRecord) error
	list() ([]*storedRecord, error)
}

func newStore(db kvdb.Backend) *store {
	return &store{db: db}
}

func (s *store) init() error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(descriptorSweepBucket)
		if err == kvdb.ErrBucketExists {
			return nil
		}
		return err
	}, func() {})
}

func (s *store) put(record *storedRecord) error {
	var value bytes.Buffer
	if err := gob.NewEncoder(&value).Encode(record); err != nil {
		return fmt.Errorf("encode descriptor sweep: %w", err)
	}
	encoded := append([]byte{descriptorSweepStoreVersion}, value.Bytes()...)

	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(descriptorSweepBucket)
		if bucket == nil {
			return kvdb.ErrBucketNotFound
		}
		return bucket.Put(record.ID[:], encoded)
	}, func() {})
}

func (s *store) list() ([]*storedRecord, error) {
	var records []*storedRecord
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(descriptorSweepBucket)
		if bucket == nil {
			return kvdb.ErrBucketNotFound
		}

		return bucket.ForEach(func(_, value []byte) error {
			if len(value) == 0 || value[0] != descriptorSweepStoreVersion {
				return fmt.Errorf("unknown descriptor sweep store version")
			}
			var record storedRecord
			if err := gob.NewDecoder(bytes.NewReader(value[1:])).Decode(
				&record,
			); err != nil {
				return fmt.Errorf("decode descriptor sweep: %w", err)
			}
			if record.Preimages == nil {
				record.Preimages = make(map[string][]byte)
			}
			records = append(records, &record)
			return nil
		})
	}, func() {
		records = nil
	})

	return records, err
}
