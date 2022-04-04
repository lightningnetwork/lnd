package migration29

import (
	"bytes"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// outpointBucket is the bucket that stores the set of outpoints we
	// know about.
	outpointBucket = []byte("outpoint-bucket")

	// chanIDBucket is the bucket that stores the set of ChannelID's we
	// know about.
	chanIDBucket = []byte("chan-id-bucket")
)

// MigrateChanID populates the ChannelID index by using the set of outpoints
// retrieved from the outpoint bucket.
func MigrateChanID(tx kvdb.RwTx) error {
	log.Info("Populating ChannelID index")

	// First we'll retrieve the set of outpoints we know about.
	ops, err := fetchOutPoints(tx)
	if err != nil {
		return err
	}

	return populateChanIDIndex(tx, ops)
}

// fetchOutPoints loops through the outpointBucket and returns each stored
// outpoint.
func fetchOutPoints(tx kvdb.RwTx) ([]*wire.OutPoint, error) {
	var ops []*wire.OutPoint

	bucket := tx.ReadBucket(outpointBucket)

	err := bucket.ForEach(func(k, _ []byte) error {
		var op wire.OutPoint
		r := bytes.NewReader(k)
		if err := readOutpoint(r, &op); err != nil {
			return err
		}

		ops = append(ops, &op)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return ops, nil
}

// populateChanIDIndex uses the set of retrieved outpoints and populates the
// ChannelID index.
func populateChanIDIndex(tx kvdb.RwTx, ops []*wire.OutPoint) error {
	bucket := tx.ReadWriteBucket(chanIDBucket)

	for _, op := range ops {
		chanID := NewChanIDFromOutPoint(op)

		if err := bucket.Put(chanID[:], []byte{}); err != nil {
			return err
		}
	}

	return nil
}
