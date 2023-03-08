package migration5

import (
	"encoding/binary"
	"errors"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// cTowerBkt is a top-level bucket storing:
	//    tower-id -> encoded Tower.
	cTowerBkt = []byte("client-tower-bucket")

	// cTowerIDToSessionIDIndexBkt is a top-level bucket storing:
	// 	tower-id -> session-id -> 1
	cTowerIDToSessionIDIndexBkt = []byte(
		"client-tower-to-session-index-bucket",
	)

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("db not initialized")

	// byteOrder is the default endianness used when serializing integers.
	byteOrder = binary.BigEndian
)

// MigrateCompleteTowerToSessionIndex ensures that the tower-to-session index
// contains entries for all towers in the db. This is necessary because
// migration1 only created entries in the index for towers that the client had
// at least one session with. This migration thus makes sure that there is
// always a tower-to-sessions index entry for a tower even if there are no
// sessions with that tower.
func MigrateCompleteTowerToSessionIndex(tx kvdb.RwTx) error {
	log.Infof("Migrating the tower client db to ensure that there is an " +
		"entry in the towerID-to-sessionID index for every tower in " +
		"the db")

	// First, we collect all the towers that we should add an entry for in
	// the index.
	towerIDs, err := listTowerIDs(tx)
	if err != nil {
		return err
	}

	// Create a new top-level bucket for the index if it does not yet exist.
	indexBkt, err := tx.CreateTopLevelBucket(cTowerIDToSessionIDIndexBkt)
	if err != nil {
		return err
	}

	// Finally, ensure that there is an entry in the tower-to-session index
	// for each of our towers.
	for _, id := range towerIDs {
		// Create a sub-bucket using the tower ID.
		_, err := indexBkt.CreateBucketIfNotExists(id.Bytes())
		if err != nil {
			return err
		}
	}

	return nil
}

// listTowerIDs iterates through the cTowerBkt and collects a list of all the
// TowerIDs.
func listTowerIDs(tx kvdb.RTx) ([]*TowerID, error) {
	var ids []*TowerID
	towerBucket := tx.ReadBucket(cTowerBkt)
	if towerBucket == nil {
		return nil, ErrUninitializedDB
	}

	err := towerBucket.ForEach(func(towerIDBytes, _ []byte) error {
		id, err := TowerIDFromBytes(towerIDBytes)
		if err != nil {
			return err
		}

		ids = append(ids, &id)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return ids, nil
}
