package migration1

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// cSessionBkt is a top-level bucket storing:
	//   session-id => cSessionBody -> encoded ClientSessionBody
	//              => cSessionCommits => seqnum -> encoded CommittedUpdate
	//              => cSessionAcks => seqnum -> encoded BackupID
	cSessionBkt = []byte("client-session-bucket")

	// cSessionBody is a sub-bucket of cSessionBkt storing only the body of
	// the ClientSession.
	cSessionBody = []byte("client-session-body")

	// cTowerIDToSessionIDIndexBkt is a top-level bucket storing:
	// 	tower-id -> session-id -> 1
	cTowerIDToSessionIDIndexBkt = []byte(
		"client-tower-to-session-index-bucket",
	)

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("db not initialized")

	// ErrClientSessionNotFound signals that the requested client session
	// was not found in the database.
	ErrClientSessionNotFound = errors.New("client session not found")

	// ErrCorruptClientSession signals that the client session's on-disk
	// structure deviates from what is expected.
	ErrCorruptClientSession = errors.New("client session corrupted")
)

// MigrateTowerToSessionIndex constructs a new towerID-to-sessionID for the
// watchtower client DB.
func MigrateTowerToSessionIndex(tx kvdb.RwTx) error {
	log.Infof("Migrating the tower client db to add a " +
		"towerID-to-sessionID index")

	// First, we collect all the entries we want to add to the index.
	entries, err := getIndexEntries(tx)
	if err != nil {
		return err
	}

	// Then we create a new top-level bucket for the index.
	indexBkt, err := tx.CreateTopLevelBucket(cTowerIDToSessionIDIndexBkt)
	if err != nil {
		return err
	}

	// Finally, we add all the collected entries to the index.
	for towerID, sessions := range entries {
		// Create a sub-bucket using the tower ID.
		towerBkt, err := indexBkt.CreateBucketIfNotExists(
			towerID.Bytes(),
		)
		if err != nil {
			return err
		}

		for sessionID := range sessions {
			err := addIndex(towerBkt, sessionID)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// addIndex adds a new towerID-sessionID pair to the given bucket. The
// session ID is used as a key within the bucket and a value of []byte{1} is
// used for each session ID key.
func addIndex(towerBkt kvdb.RwBucket, sessionID SessionID) error {
	session := towerBkt.Get(sessionID[:])
	if session != nil {
		return fmt.Errorf("session %x duplicated", sessionID)
	}

	return towerBkt.Put(sessionID[:], []byte{1})
}

// getIndexEntries collects all the towerID-sessionID entries that need to be
// added to the new index.
func getIndexEntries(tx kvdb.RwTx) (map[TowerID]map[SessionID]bool, error) {
	sessions := tx.ReadBucket(cSessionBkt)
	if sessions == nil {
		return nil, ErrUninitializedDB
	}

	index := make(map[TowerID]map[SessionID]bool)
	err := sessions.ForEach(func(k, _ []byte) error {
		session, err := getClientSession(sessions, k)
		if err != nil {
			return err
		}

		if index[session.TowerID] == nil {
			index[session.TowerID] = make(map[SessionID]bool)
		}

		index[session.TowerID][session.ID] = true
		return nil
	})
	if err != nil {
		return nil, err
	}

	return index, nil
}

// getClientSession fetches the session with the given ID from the db.
func getClientSession(sessions kvdb.RBucket, idBytes []byte) (*ClientSession,
	error) {

	sessionBkt := sessions.NestedReadBucket(idBytes)
	if sessionBkt == nil {
		return nil, ErrClientSessionNotFound
	}

	// Should never have a sessionBkt without also having its body.
	sessionBody := sessionBkt.Get(cSessionBody)
	if sessionBody == nil {
		return nil, ErrCorruptClientSession
	}

	var session ClientSession
	copy(session.ID[:], idBytes)

	err := session.Decode(bytes.NewReader(sessionBody))
	if err != nil {
		return nil, err
	}

	return &session, nil
}
